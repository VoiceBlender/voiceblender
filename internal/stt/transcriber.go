package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	wsURL      = "wss://api.elevenlabs.io/v1/speech-to-text/realtime"
	frameBytes = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)
)

// TranscriptCallback is called for each transcript result.
type TranscriptCallback func(text string, isFinal bool)

// Transcriber streams audio to ElevenLabs real-time STT over WebSocket.
type Transcriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	log     *slog.Logger
}

func New(log *slog.Logger) *Transcriber {
	return &Transcriber{log: log}
}

// Options configures the transcription session.
type Options struct {
	Language string // ISO-639-1 language code (default "en")
	Partial  bool   // emit partial transcripts
}

// Start connects to ElevenLabs STT and streams PCM from reader.
// It blocks until the context is cancelled or an error occurs.
func (t *Transcriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel
	t.running = true
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		t.running = false
		t.cancel = nil
		t.mu.Unlock()
	}()

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"xi-api-key": []string{apiKey},
		},
	}

	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	url := wsURL + "?model_id=scribe_v2_realtime&language_code=" + lang + "&audio_format=pcm_16000&commit_strategy=vad"
	t.log.Info("stt dialing", "url", url)
	conn, _, _, err := dialer.Dial(ctx, url)
	if err != nil {
		t.log.Error("stt dial failed", "error", err)
		return err
	}
	t.log.Info("stt websocket connected")
	defer conn.Close()

	// lockedWriter serializes ALL writes to the WebSocket connection,
	// including pong responses from the read path and audio sends.
	lw := &lockedWriter{conn: conn}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		t.sendLoop(ctx, reader, lw)
	}()

	go func() {
		defer wg.Done()
		t.recvLoop(ctx, conn, lw, opts.Partial, cb)
	}()

	wg.Wait()
	return nil
}

// Stop cancels the running transcription.
func (t *Transcriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

// Running returns whether the transcriber is active.
func (t *Transcriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

type audioChunkMsg struct {
	MessageType string `json:"message_type"`
	AudioBase64 string `json:"audio_base_64"`
}

func (t *Transcriber) sendLoop(ctx context.Context, reader io.Reader, lw *lockedWriter) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			t.log.Debug("stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			t.log.Info("stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			t.log.Info("stt sendLoop first audio read", "bytes", n)
		}

		msg := audioChunkMsg{
			MessageType: "input_audio_chunk",
			AudioBase64: base64.StdEncoding.EncodeToString(buf[:n]),
		}
		data, _ := json.Marshal(msg)

		if err := lw.WriteText(data); err != nil {
			t.log.Debug("stt send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 { // every ~5s at 20ms frames
			t.log.Debug("stt sendLoop progress", "sent_frames", sendCount)
		}
	}
}

type sttResponse struct {
	MessageType string `json:"message_type"`
	Text        string `json:"text"`
}

func (t *Transcriber) recvLoop(ctx context.Context, conn net.Conn, lw *lockedWriter, emitPartial bool, cb TranscriptCallback) {
	// Use wsutil.Reader directly so that control frame responses (pong)
	// go through our lockedWriter instead of writing to conn directly.
	// Without this, pong writes and sendLoop writes race on the conn,
	// corrupting WebSocket frames.
	rd := &wsutil.Reader{
		Source: conn,
		State:  ws.StateClientSide,
		OnIntermediate: func(hdr ws.Header, r io.Reader) error {
			// Read the control frame payload.
			payload, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			// Respond to pings with pongs through the locked writer.
			if hdr.OpCode == ws.OpPing {
				return lw.WriteControl(ws.OpPong, payload)
			}
			return nil
		},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		hdr, err := rd.NextFrame()
		if err != nil {
			select {
			case <-ctx.Done():
				t.log.Debug("stt recvLoop context done")
			default:
				t.log.Debug("stt recv error", "error", err)
			}
			return
		}

		t.log.Debug("stt recv frame", "opcode", hdr.OpCode, "length", hdr.Length, "fin", hdr.Fin)

		// Skip non-text frames (binary, etc).
		if hdr.OpCode == ws.OpClose {
			t.log.Info("stt recv close frame")
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("stt read error", "error", err)
			return
		}

		raw := buf.String()
		t.log.Debug("stt recv msg", "raw", raw[:min(len(raw), 300)])

		var resp sttResponse
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.log.Debug("stt parse error", "error", err, "raw", raw[:min(len(raw), 200)])
			continue
		}

		t.log.Debug("stt recv parsed", "message_type", resp.MessageType, "text", resp.Text)

		switch resp.MessageType {
		case "partial_transcript":
			if resp.Text != "" && emitPartial {
				t.log.Info("stt partial transcript", "text", resp.Text)
				cb(resp.Text, false)
			}
		case "committed_transcript":
			if resp.Text != "" {
				t.log.Info("stt committed transcript", "text", resp.Text)
				cb(resp.Text, true)
			}
		}
	}
}

// lockedWriter serializes all WebSocket frame writes to a net.Conn.
type lockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *lockedWriter) WriteText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientText(lw.conn, data)
}

func (lw *lockedWriter) WriteControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientMessage(lw.conn, op, payload)
}
