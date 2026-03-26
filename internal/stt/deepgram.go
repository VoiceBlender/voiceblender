package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	deepgramWSURL  = "wss://api.deepgram.com/v1/listen"
	dgFrameBytes   = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)
)

// DeepgramTranscriber streams audio to Deepgram real-time STT over WebSocket.
type DeepgramTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	log     *slog.Logger
}

func NewDeepgram(log *slog.Logger) *DeepgramTranscriber {
	return &DeepgramTranscriber{log: log}
}

func (t *DeepgramTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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

	lang := opts.Language
	if lang == "" {
		lang = "en"
	}

	url := deepgramWSURL + "?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=" + lang
	if opts.Partial {
		url += "&interim_results=true"
	}

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"Authorization": []string{"token " + apiKey},
		},
	}

	t.log.Info("deepgram stt dialing", "url", url)
	conn, _, _, err := dialer.Dial(ctx, url)
	if err != nil {
		t.log.Error("deepgram stt dial failed", "error", err)
		return err
	}
	t.log.Info("deepgram stt websocket connected")
	defer conn.Close()

	lw := &dgLockedWriter{conn: conn}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		t.sendLoop(ctx, reader, lw)
	}()

	go func() {
		defer wg.Done()
		t.recvLoop(ctx, conn, lw, cb)
	}()

	wg.Wait()
	return nil
}

func (t *DeepgramTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *DeepgramTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *DeepgramTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *dgLockedWriter) {
	buf := make([]byte, dgFrameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			// Send CloseStream message to finalize.
			close := []byte(`{"type": "CloseStream"}`)
			_ = lw.WriteText(close)
			t.log.Debug("deepgram stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			// Send CloseStream on reader close too.
			close := []byte(`{"type": "CloseStream"}`)
			_ = lw.WriteText(close)
			t.log.Info("deepgram stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			t.log.Info("deepgram stt sendLoop first audio read", "bytes", n)
		}

		// Deepgram accepts raw binary PCM frames.
		if err := lw.WriteBinary(buf[:n]); err != nil {
			t.log.Debug("deepgram stt send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			t.log.Debug("deepgram stt sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// dgResult represents the Deepgram streaming response.
type dgResult struct {
	Type    string `json:"type"`
	Channel struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
	IsFinal   bool `json:"is_final"`
	SpeechFinal bool `json:"speech_final"`
}

func (t *DeepgramTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *dgLockedWriter, cb TranscriptCallback) {
	rd := &wsutil.Reader{
		Source: conn,
		State:  ws.StateClientSide,
		OnIntermediate: func(hdr ws.Header, r io.Reader) error {
			payload, err := io.ReadAll(r)
			if err != nil {
				return err
			}
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
				t.log.Debug("deepgram stt recvLoop context done")
			default:
				t.log.Debug("deepgram stt recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			t.log.Info("deepgram stt recv close frame")
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("deepgram stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("deepgram stt read error", "error", err)
			return
		}

		raw := buf.Bytes()
		t.log.Debug("deepgram stt recv msg", "raw", string(raw[:min(len(raw), 300)]))

		var result dgResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.log.Debug("deepgram stt parse error", "error", err)
			continue
		}

		if result.Type != "Results" {
			continue
		}

		if len(result.Channel.Alternatives) == 0 {
			continue
		}

		text := result.Channel.Alternatives[0].Transcript
		if text == "" {
			continue
		}

		if result.IsFinal {
			t.log.Info("deepgram stt final transcript", "text", text)
			cb(text, true)
		} else {
			t.log.Info("deepgram stt interim transcript", "text", text)
			cb(text, false)
		}
	}
}

// dgLockedWriter serializes all WebSocket frame writes to a net.Conn.
type dgLockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *dgLockedWriter) WriteBinary(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientBinary(lw.conn, data)
}

func (lw *dgLockedWriter) WriteText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientText(lw.conn, data)
}

func (lw *dgLockedWriter) WriteControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientMessage(lw.conn, op, payload)
}
