package agent

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
	elevenlabsWSURL = "wss://api.elevenlabs.io/v1/convai/conversation"
	frameBytes      = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)
)

// ElevenLabsSession manages a WebSocket connection to the ElevenLabs ConvAI API.
type ElevenLabsSession struct {
	mu             sync.Mutex
	running        bool
	cancel         context.CancelFunc
	conversationID string
	log            *slog.Logger
}

func NewElevenLabs(log *slog.Logger) *ElevenLabsSession {
	return &ElevenLabsSession{log: log}
}

// Start dials the ElevenLabs ConvAI WebSocket and streams audio bidirectionally.
// reader provides 16kHz 16-bit PCM mono (what humans say).
// writer receives 16kHz 16-bit PCM mono (agent's spoken audio).
// Blocks until the context is cancelled or an error occurs.
func (s *ElevenLabsSession) Start(ctx context.Context, reader io.Reader, writer io.Writer, apiKey string, opts Options, cb Callbacks) error {
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
		s.mu.Unlock()
		if cb.OnDisconnected != nil {
			cb.OnDisconnected()
		}
	}()

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"xi-api-key": []string{apiKey},
		},
	}

	url := elevenlabsWSURL + "?agent_id=" + opts.AgentID
	s.log.Info("agent dialing", "url", url)
	conn, _, _, err := dialer.Dial(ctx, url)
	if err != nil {
		s.log.Error("agent dial failed", "error", err)
		return err
	}
	s.log.Info("agent websocket connected")
	defer conn.Close()

	lw := &lockedWriter{conn: conn}

	// Send initiation data if overrides are present.
	if err := s.sendInitiation(lw, opts); err != nil {
		s.log.Error("agent send initiation failed", "error", err)
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

// Stop cancels the running agent session.
func (s *ElevenLabsSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

// Running returns whether the session is active.
func (s *ElevenLabsSession) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ConversationID returns the conversation ID assigned by ElevenLabs.
func (s *ElevenLabsSession) ConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationID
}

func (s *ElevenLabsSession) sendInitiation(lw *lockedWriter, opts Options) error {
	if opts.FirstMessage == "" && opts.Language == "" && len(opts.DynamicVariables) == 0 {
		return nil
	}

	msg := map[string]interface{}{
		"type": "conversation_initiation_client_data",
	}

	configOverride := map[string]interface{}{}
	if opts.FirstMessage != "" {
		configOverride["agent"] = map[string]interface{}{
			"first_message": opts.FirstMessage,
		}
	}
	if opts.Language != "" {
		configOverride["language"] = opts.Language
	}
	if len(configOverride) > 0 {
		msg["conversation_config_override"] = configOverride
	}
	if len(opts.DynamicVariables) > 0 {
		msg["dynamic_variables"] = opts.DynamicVariables
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.log.Debug("agent sending initiation", "data", string(data))
	return lw.WriteText(data)
}

type audioChunkMsg struct {
	UserAudioChunk string `json:"user_audio_chunk"`
}

func (s *ElevenLabsSession) sendLoop(ctx context.Context, reader io.Reader, lw *lockedWriter) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			s.log.Debug("agent sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			s.log.Info("agent sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			s.log.Info("agent sendLoop first audio read", "bytes", n)
		}

		msg := audioChunkMsg{
			UserAudioChunk: base64.StdEncoding.EncodeToString(buf[:n]),
		}
		data, _ := json.Marshal(msg)

		if err := lw.WriteText(data); err != nil {
			s.log.Debug("agent send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			s.log.Debug("agent sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// convaiResponse is a generic envelope for all server messages.
type convaiResponse struct {
	Type string `json:"type"`
}

type audioEvent struct {
	AudioBase64 string `json:"audio_base_64"`
	EventID     int64  `json:"event_id"`
}

type audioMessage struct {
	AudioEvent audioEvent `json:"audio_event"`
}

type initiationMetadata struct {
	ConversationID string `json:"conversation_id"`
}

type userTranscriptMessage struct {
	UserTranscript struct {
		Text string `json:"user_transcript"`
	} `json:"user_transcription_event"`
}

type agentResponseMessage struct {
	AgentResponse string `json:"agent_response_event"`
}

// agentResponseEvent wraps the agent_response server message.
type agentResponseEvent struct {
	AgentResponseEvent struct {
		Text string `json:"agent_response"`
	} `json:"agent_response_event"`
}

type pingMessage struct {
	PingEvent struct {
		EventID int64 `json:"event_id"`
	} `json:"ping_event"`
}

func (s *ElevenLabsSession) recvLoop(ctx context.Context, conn net.Conn, lw *lockedWriter, writer io.Writer, cb Callbacks) {
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
				s.log.Debug("agent recvLoop context done")
			default:
				s.log.Debug("agent recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			s.log.Info("agent recv close frame")
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				s.log.Debug("agent discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			s.log.Debug("agent read error", "error", err)
			return
		}

		raw := buf.Bytes()
		s.log.Debug("agent recv msg", "raw", string(raw[:min(len(raw), 300)]))

		var envelope convaiResponse
		if err := json.Unmarshal(raw, &envelope); err != nil {
			s.log.Debug("agent parse error", "error", err)
			continue
		}

		switch envelope.Type {
		case "conversation_initiation_metadata":
			var meta struct {
				ConversationID string `json:"conversation_id"`
			}
			if err := json.Unmarshal(raw, &meta); err == nil && meta.ConversationID != "" {
				s.mu.Lock()
				s.conversationID = meta.ConversationID
				s.mu.Unlock()
				s.log.Info("agent conversation started", "conversation_id", meta.ConversationID)
				if cb.OnConnected != nil {
					cb.OnConnected(meta.ConversationID)
				}
			}

		case "audio":
			var msg audioMessage
			if err := json.Unmarshal(raw, &msg); err != nil {
				s.log.Debug("agent audio parse error", "error", err)
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(msg.AudioEvent.AudioBase64)
			if err != nil {
				s.log.Debug("agent audio base64 decode error", "error", err)
				continue
			}
			if _, err := writer.Write(pcm); err != nil {
				s.log.Debug("agent audio write error", "error", err)
			}

		case "user_transcript":
			var msg userTranscriptMessage
			if err := json.Unmarshal(raw, &msg); err == nil && msg.UserTranscript.Text != "" {
				s.log.Info("agent user transcript", "text", msg.UserTranscript.Text)
				if cb.OnUserTranscript != nil {
					cb.OnUserTranscript(msg.UserTranscript.Text)
				}
			}

		case "agent_response":
			var msg agentResponseEvent
			if err := json.Unmarshal(raw, &msg); err == nil && msg.AgentResponseEvent.Text != "" {
				s.log.Info("agent response", "text", msg.AgentResponseEvent.Text)
				if cb.OnAgentResponse != nil {
					cb.OnAgentResponse(msg.AgentResponseEvent.Text)
				}
			}

		case "ping":
			var msg pingMessage
			if err := json.Unmarshal(raw, &msg); err == nil {
				pong := map[string]interface{}{
					"type":     "pong",
					"event_id": msg.PingEvent.EventID,
				}
				data, _ := json.Marshal(pong)
				if err := lw.WriteText(data); err != nil {
					s.log.Debug("agent pong send error", "error", err)
				}
			}

		case "interruption":
			s.log.Info("agent interruption received")

		default:
			s.log.Debug("agent unknown message type", "type", envelope.Type)
		}
	}
}

// InjectMessage is not supported by ElevenLabs.
func (s *ElevenLabsSession) InjectMessage(ctx context.Context, message string) error {
	return ErrNotSupported
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
