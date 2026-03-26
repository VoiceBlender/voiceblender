package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const deepgramAgentURL = "wss://agent.deepgram.com/v1/agent/converse"

// DeepgramSession manages a WebSocket connection to the Deepgram Voice Agent API.
type DeepgramSession struct {
	mu             sync.Mutex
	running        bool
	cancel         context.CancelFunc
	conversationID string
	lw             *dgAgentLockedWriter
	log            *slog.Logger
}

func NewDeepgram(log *slog.Logger) *DeepgramSession {
	return &DeepgramSession{log: log}
}

// Start dials the Deepgram Voice Agent WebSocket and streams audio bidirectionally.
// reader provides 16kHz 16-bit PCM mono (what humans say).
// writer receives 16kHz 16-bit PCM mono (agent's spoken audio).
// Blocks until the context is cancelled or an error occurs.
func (s *DeepgramSession) Start(ctx context.Context, reader io.Reader, writer io.Writer, apiKey string, opts Options, cb Callbacks) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		s.lw = nil
		s.mu.Unlock()
		if cb.OnDisconnected != nil {
			cb.OnDisconnected()
		}
	}()

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"Authorization": []string{"token " + apiKey},
		},
	}

	s.log.Info("deepgram agent dialing", "url", deepgramAgentURL)
	conn, _, _, err := dialer.Dial(ctx, deepgramAgentURL)
	if err != nil {
		s.log.Error("deepgram agent dial failed", "error", err)
		return err
	}
	s.log.Info("deepgram agent websocket connected")
	defer conn.Close()

	lw := &dgAgentLockedWriter{conn: conn}

	s.mu.Lock()
	s.lw = lw
	s.mu.Unlock()

	// Send settings message after connection.
	if err := s.sendSettings(lw, opts); err != nil {
		s.log.Error("deepgram agent send settings failed", "error", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		s.sendLoop(ctx, reader, lw)
	}()

	go func() {
		defer wg.Done()
		s.recvLoop(ctx, conn, lw, writer, cb)
	}()

	wg.Wait()
	return nil
}

func (s *DeepgramSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *DeepgramSession) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *DeepgramSession) ConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationID
}

func (s *DeepgramSession) sendSettings(lw *dgAgentLockedWriter, opts Options) error {
	// Audio config forced to 16kHz — VoiceBlender's audio pipeline always
	// delivers/expects 16-bit PCM at 16kHz.
	audioConfig := map[string]interface{}{
		"input": map[string]interface{}{
			"encoding":    "linear16",
			"sample_rate": 16000,
		},
		"output": map[string]interface{}{
			"encoding":    "linear16",
			"sample_rate": 16000,
			"container":   "none",
		},
	}

	var settings map[string]interface{}

	if opts.Settings != nil {
		// User provided a full settings object — use it, but force audio config.
		settings = make(map[string]interface{})
		for k, v := range opts.Settings {
			settings[k] = v
		}
		settings["type"] = "Settings"
		settings["audio"] = audioConfig
	} else {
		// Build default settings.
		settings = map[string]interface{}{
			"type":  "Settings",
			"audio": audioConfig,
			"agent": map[string]interface{}{
				"listen": map[string]interface{}{
					"provider": map[string]interface{}{
						"type":  "deepgram",
						"model": "nova-3",
					},
				},
				"think": map[string]interface{}{
					"provider": map[string]interface{}{
						"type":  "open_ai",
						"model": "gpt-4o-mini",
					},
				},
				"speak": map[string]interface{}{
					"provider": map[string]interface{}{
						"type":  "deepgram",
						"model": "aura-2-asteria-en",
					},
				},
			},
		}
	}

	// Apply language/greeting overrides into agent config.
	if opts.Language != "" || opts.FirstMessage != "" {
		if agent, ok := settings["agent"].(map[string]interface{}); ok {
			if opts.Language != "" {
				agent["language"] = opts.Language
			}
			if opts.FirstMessage != "" {
				agent["greeting"] = opts.FirstMessage
			}
		}
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	s.log.Debug("deepgram agent sending settings", "data", string(data))
	return lw.WriteText(data)
}

func (s *DeepgramSession) sendLoop(ctx context.Context, reader io.Reader, lw *dgAgentLockedWriter) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			s.log.Debug("deepgram agent sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			s.log.Info("deepgram agent sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			s.log.Info("deepgram agent sendLoop first audio read", "bytes", n)
		}

		// Deepgram Voice Agent accepts raw binary PCM frames.
		if err := lw.WriteBinary(buf[:n]); err != nil {
			s.log.Debug("deepgram agent send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			s.log.Debug("deepgram agent sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// InjectMessage sends an InjectAgentMessage command to the Deepgram Voice Agent.
func (s *DeepgramSession) InjectMessage(ctx context.Context, message string) error {
	s.mu.Lock()
	lw := s.lw
	running := s.running
	s.mu.Unlock()

	if !running || lw == nil {
		return fmt.Errorf("agent session not running")
	}

	msg := map[string]string{
		"type":    "InjectAgentMessage",
		"message": message,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return lw.WriteText(data)
}

// dgAgentServerMessage is a generic envelope for Deepgram Voice Agent text messages.
type dgAgentServerMessage struct {
	Type string `json:"type"`
}

// dgAgentWelcome is the initial welcome message with session info.
type dgAgentWelcome struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

// dgAgentConversationText holds transcript data.
type dgAgentConversationText struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *DeepgramSession) recvLoop(ctx context.Context, conn net.Conn, lw *dgAgentLockedWriter, writer io.Writer, cb Callbacks) {
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
				s.log.Debug("deepgram agent recvLoop context done")
			default:
				s.log.Debug("deepgram agent recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			s.log.Info("deepgram agent recv close frame")
			return
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			s.log.Debug("deepgram agent read error", "error", err)
			return
		}

		switch hdr.OpCode {
		case ws.OpBinary:
			// Raw PCM audio from the agent.
			if _, err := writer.Write(buf.Bytes()); err != nil {
				s.log.Debug("deepgram agent audio write error", "error", err)
			}

		case ws.OpText:
			raw := buf.Bytes()
			s.log.Debug("deepgram agent recv text", "raw", string(raw[:min(len(raw), 300)]))

			var envelope dgAgentServerMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				s.log.Debug("deepgram agent parse error", "error", err)
				continue
			}

			switch envelope.Type {
			case "Welcome":
				var welcome dgAgentWelcome
				if err := json.Unmarshal(raw, &welcome); err == nil && welcome.SessionID != "" {
					s.mu.Lock()
					s.conversationID = welcome.SessionID
					s.mu.Unlock()
					s.log.Info("deepgram agent session started", "session_id", welcome.SessionID)
					if cb.OnConnected != nil {
						cb.OnConnected(welcome.SessionID)
					}
				}

			case "ConversationText":
				var msg dgAgentConversationText
				if err := json.Unmarshal(raw, &msg); err == nil && msg.Content != "" {
					switch msg.Role {
					case "user":
						s.log.Info("deepgram agent user transcript", "text", msg.Content)
						if cb.OnUserTranscript != nil {
							cb.OnUserTranscript(msg.Content)
						}
					case "assistant":
						s.log.Info("deepgram agent response", "text", msg.Content)
						if cb.OnAgentResponse != nil {
							cb.OnAgentResponse(msg.Content)
						}
					}
				}

			case "UserStartedSpeaking":
				s.log.Debug("deepgram agent user started speaking")

			case "AgentStartedSpeaking":
				s.log.Debug("deepgram agent started speaking")

			case "AgentAudioDone":
				s.log.Debug("deepgram agent audio done")

			case "Error":
				s.log.Error("deepgram agent error", "raw", string(raw[:min(len(raw), 500)]))

			default:
				s.log.Debug("deepgram agent unknown message type", "type", envelope.Type)
			}
		}
	}
}

// dgAgentLockedWriter serializes all WebSocket frame writes to a net.Conn.
type dgAgentLockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *dgAgentLockedWriter) WriteBinary(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientBinary(lw.conn, data)
}

func (lw *dgAgentLockedWriter) WriteText(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientText(lw.conn, data)
}

func (lw *dgAgentLockedWriter) WriteControl(op ws.OpCode, payload []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientMessage(lw.conn, op, payload)
}
