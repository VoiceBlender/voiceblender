package stt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// SixtyDbTranscriber streams audio to 60db's realtime STT WebSocket.
//
// Uses "browser mode" (linear PCM in JSON envelopes) at 16 kHz to match
// VoiceBlender's mixer-native sample rate. The send/recv loop topology
// and lockedWriter pattern mirror stt/elevenlabs.go so this provider
// slots into the rest of the leg machinery without any glue.
// Reference: https://docs.60db.ai/websocket-api/stt
type SixtyDbTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	log     *slog.Logger
}

const (
	sixtydbSTTBase       = "https://api.60db.ai"
	sixtydbSTTSampleRate = 16000
)

// NewSixtyDb constructs a 60db STT transcriber. apiBase is read from
// SIXTYDB_API_BASE env at Start() time so per-tenant overrides are
// possible without rewiring the constructor.
func NewSixtyDb(log *slog.Logger) *SixtyDbTranscriber {
	return &SixtyDbTranscriber{log: log}
}

// Start connects to 60db /ws/stt and streams PCM frames from `reader`.
// Blocks until ctx is cancelled or the connection errors.
func (t *SixtyDbTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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

	if apiKey == "" {
		return io.ErrShortBuffer // signal misconfig — caller logs the error
	}
	base := os.Getenv("SIXTYDB_API_BASE")
	if base == "" {
		base = sixtydbSTTBase
	}
	wsBase := strings.Replace(strings.TrimRight(base, "/"), "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	q := url.Values{"apiKey": {apiKey}}
	dialURL := wsBase + "/ws/stt?" + q.Encode()

	t.log.Info("sixtydb stt dialing", "url", redactKey(dialURL))
	conn, _, _, err := ws.Dial(ctx, dialURL)
	if err != nil {
		t.log.Error("sixtydb stt dial failed", "error", err)
		return err
	}
	t.log.Info("sixtydb stt websocket connected")
	defer conn.Close()

	lw := &lockedWriter{conn: conn}

	// Wait for connection_established, then send start config. We do the
	// initial handshake synchronously so any auth/config error surfaces
	// before audio starts flowing.
	if err := t.handshake(conn, lw, opts); err != nil {
		t.log.Error("sixtydb stt handshake failed", "error", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.sendLoop(ctx, reader, lw) }()
	go func() { defer wg.Done(); t.recvLoop(ctx, conn, lw, opts.Partial, cb) }()
	wg.Wait()
	return nil
}

// Stop cancels the running transcription.
func (t *SixtyDbTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *SixtyDbTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *SixtyDbTranscriber) handshake(conn net.Conn, lw *lockedWriter, opts Options) error {
	// Read the connection_established frame.
	for {
		data, _, err := wsutil.ReadServerData(conn)
		if err != nil {
			return err
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			continue
		}
		if _, ok := probe["connection_established"]; ok {
			break
		}
	}

	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	start, _ := json.Marshal(map[string]any{
		"type":      "start",
		"languages": []string{lang},
		"config": map[string]any{
			"encoding":                  "linear",
			"sample_rate":               sixtydbSTTSampleRate,
			"utterance_end_ms":          500,
			"continuous_mode":           true,
			"interim_results_frequency": 300,
			"audio_enhancement":         "adaptive",
			"diarize":                   false,
		},
	})
	return lw.WriteText(start)
}

func (t *SixtyDbTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *lockedWriter) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			t.log.Debug("sixtydb stt sendLoop done", "sent_frames", sendCount)
			return
		default:
		}
		n, err := reader.Read(buf)
		if err != nil {
			t.log.Info("sixtydb stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}
		// 60db browser mode: {"type":"audio","audio":<base64>,"encoding":"linear","sample_rate":16000}
		msg, _ := json.Marshal(map[string]any{
			"type":        "audio",
			"audio":       base64.StdEncoding.EncodeToString(buf[:n]),
			"encoding":    "linear",
			"sample_rate": sixtydbSTTSampleRate,
		})
		if err := lw.WriteText(msg); err != nil {
			t.log.Debug("sixtydb stt send error", "error", err)
			return
		}
		sendCount++
	}
}

func (t *SixtyDbTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *lockedWriter, emitPartial bool, cb TranscriptCallback) {
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
	stopWatch := wsutilx.WatchCancel(ctx, conn)
	defer stopWatch()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout.Load())
		hdr, err := rd.NextFrame()
		if err != nil {
			t.log.Debug("sixtydb stt recv error", "error", err)
			return
		}
		if hdr.OpCode == ws.OpClose {
			return
		}
		if hdr.OpCode != ws.OpText {
			_ = rd.Discard()
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			return
		}
		var msg struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			IsFinal     bool   `json:"is_final"`
			SpeechFinal bool   `json:"speech_final"`
		}
		if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Type != "transcription" || msg.Text == "" {
			continue
		}
		// Canonical answer: is_final && speech_final.
		// Interim: !is_final (only emit when caller asked).
		if msg.IsFinal && msg.SpeechFinal {
			t.log.Info("sixtydb stt final", "text", msg.Text)
			cb(msg.Text, true)
		} else if !msg.IsFinal && emitPartial {
			cb(msg.Text, false)
		}
	}
}

// redactKey returns a URL safe to log: the apiKey query value is masked.
func redactKey(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "sixtydb-stt"
	}
	q := u.Query()
	if q.Get("apiKey") != "" {
		q.Set("apiKey", "***")
	}
	u.RawQuery = q.Encode()
	return u.String()
}
