package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
)

const azFrameBytes = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)

// AzureTranscriber streams audio to Azure Speech Services real-time STT over WebSocket.
type AzureTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	region  string
	log     *slog.Logger
}

func NewAzure(region string, log *slog.Logger) *AzureTranscriber {
	return &AzureTranscriber{region: region, log: log}
}

func (t *AzureTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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
		lang = "en-US"
	}

	wsURL := fmt.Sprintf("wss://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1?language=%s&format=detailed", t.region, lang)
	if opts.Partial {
		wsURL += "&interim=true"
	}

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"Ocp-Apim-Subscription-Key": []string{apiKey},
		},
	}

	t.log.Info("azure stt dialing", "url", wsURL)
	conn, _, _, err := dialer.Dial(ctx, wsURL)
	if err != nil {
		t.log.Error("azure stt dial failed", "error", err)
		return err
	}
	t.log.Info("azure stt websocket connected")
	defer conn.Close()

	lw := &azLockedWriter{conn: conn}
	requestID := strings.ReplaceAll(uuid.New().String(), "-", "")

	// Send speech.config message.
	if err := t.sendConfig(lw, requestID); err != nil {
		t.log.Error("azure stt send config failed", "error", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		t.sendLoop(ctx, reader, lw, requestID)
	}()

	go func() {
		defer wg.Done()
		t.recvLoop(ctx, conn, lw, cb, opts.Partial)
	}()

	wg.Wait()
	return nil
}

func (t *AzureTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *AzureTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *AzureTranscriber) sendConfig(lw *azLockedWriter, requestID string) error {
	configJSON := `{"context":{"system":{"version":"1.0.0"},"os":{"platform":"Go","name":"VoiceBlender"},"audio":{"source":{"connectivity":"Unknown","manufacturer":"Unknown","model":"Unknown","type":"Unknown"},"format":{"encoding":"pcm","sampleRate":16000,"bitsPerSample":16,"channels":1}}}}`

	msg := buildAzureTextFrame("speech.config", requestID, "application/json", []byte(configJSON))
	return lw.WriteText(msg)
}

func (t *AzureTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *azLockedWriter, requestID string) {
	buf := make([]byte, azFrameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			// Send empty audio frame to signal end of stream.
			frame := buildAzureBinaryFrame("audio", requestID, "audio/x-wav", nil)
			_ = lw.WriteBinary(frame)
			t.log.Debug("azure stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			// Send empty audio frame on reader close too.
			frame := buildAzureBinaryFrame("audio", requestID, "audio/x-wav", nil)
			_ = lw.WriteBinary(frame)
			t.log.Info("azure stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			t.log.Info("azure stt sendLoop first audio read", "bytes", n)
		}

		frame := buildAzureBinaryFrame("audio", requestID, "audio/x-wav", buf[:n])
		if err := lw.WriteBinary(frame); err != nil {
			t.log.Debug("azure stt send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			t.log.Debug("azure stt sendLoop progress", "sent_frames", sendCount)
		}
	}
}

type azSpeechHypothesis struct {
	Text string `json:"Text"`
}

type azSpeechPhrase struct {
	RecognitionStatus string `json:"RecognitionStatus"`
	DisplayText       string `json:"DisplayText"`
}

func (t *AzureTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *azLockedWriter, cb TranscriptCallback, partial bool) {
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
				t.log.Debug("azure stt recvLoop context done")
			default:
				t.log.Debug("azure stt recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			t.log.Info("azure stt recv close frame")
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("azure stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("azure stt read error", "error", err)
			return
		}

		raw := buf.String()
		t.log.Debug("azure stt recv msg", "raw", raw[:min(len(raw), 300)])

		// Azure text frames: headers\r\n\r\nbody
		path, body := parseAzureTextFrame(raw)

		switch path {
		case "speech.hypothesis":
			if !partial {
				continue
			}
			var h azSpeechHypothesis
			if err := json.Unmarshal([]byte(body), &h); err != nil {
				t.log.Debug("azure stt hypothesis parse error", "error", err)
				continue
			}
			if h.Text != "" {
				t.log.Info("azure stt interim transcript", "text", h.Text)
				cb(h.Text, false)
			}
		case "speech.phrase":
			var p azSpeechPhrase
			if err := json.Unmarshal([]byte(body), &p); err != nil {
				t.log.Debug("azure stt phrase parse error", "error", err)
				continue
			}
			if p.RecognitionStatus == "Success" && p.DisplayText != "" {
				t.log.Info("azure stt final transcript", "text", p.DisplayText)
				cb(p.DisplayText, true)
			}
		default:
			t.log.Debug("azure stt ignored path", "path", path)
		}
	}
}

// parseAzureTextFrame splits an Azure WebSocket text frame into its Path header value and JSON body.
func parseAzureTextFrame(raw string) (path, body string) {
	idx := strings.Index(raw, "\r\n\r\n")
	if idx < 0 {
		return "", raw
	}
	headers := raw[:idx]
	body = raw[idx+4:]
	for _, line := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(line, "Path:") {
			path = strings.TrimPrefix(line, "Path:")
			break
		}
	}
	return path, body
}

// buildAzureTextFrame constructs an Azure WebSocket text frame with headers and JSON body.
func buildAzureTextFrame(path, requestID, contentType string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Path:%s\r\nX-RequestId:%s\r\nX-Timestamp:%s\r\nContent-Type:%s\r\n\r\n",
		path, requestID, time.Now().UTC().Format(time.RFC3339Nano), contentType)
	buf.Write(payload)
	return buf.Bytes()
}

// buildAzureBinaryFrame constructs an Azure WebSocket binary frame with a 2-byte header-length
// prefix, ASCII headers, and audio payload.
func buildAzureBinaryFrame(path, requestID, contentType string, payload []byte) []byte {
	header := fmt.Sprintf("Path:%s\r\nX-RequestId:%s\r\nX-Timestamp:%s\r\nContent-Type:%s\r\n",
		path, requestID, time.Now().UTC().Format(time.RFC3339Nano), contentType)

	headerBytes := []byte(header)
	buf := make([]byte, 2+len(headerBytes)+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(headerBytes)))
	copy(buf[2:], headerBytes)
	copy(buf[2+len(headerBytes):], payload)
	return buf
}

// azLockedWriter serializes all WebSocket frame writes to a net.Conn.
type azLockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *azLockedWriter) WriteBinary(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientBinary(lw.conn, data)
}

func (lw *azLockedWriter) WriteText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientText(lw.conn, data)
}

func (lw *azLockedWriter) WriteControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientMessage(lw.conn, op, payload)
}
