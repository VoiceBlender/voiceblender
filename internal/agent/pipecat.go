package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	pb "github.com/VoiceBlender/voiceblender/internal/agent/pipecatpb"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"google.golang.org/protobuf/proto"
)

const pipecatSampleRate = 16000

// PipecatSession manages a WebSocket connection to a Pipecat bot.
// The agent_id in Options is the WebSocket URL of the running Pipecat bot
// (e.g. "ws://my-pipecat-bot:8765").
type PipecatSession struct {
	mu             sync.Mutex
	running        bool
	cancel         context.CancelFunc
	conversationID string
	lw             *pipecatLockedWriter
	log            *slog.Logger
}

// pipecatLockedWriter serializes all WebSocket binary frame writes to a net.Conn.
type pipecatLockedWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (lw *pipecatLockedWriter) WriteBinary(data []byte) error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return wsutil.WriteClientBinary(lw.conn, data)
}

func NewPipecat(log *slog.Logger) *PipecatSession {
	return &PipecatSession{log: log}
}

// Start connects to the Pipecat bot WebSocket and streams audio bidirectionally.
// The apiKey parameter is unused (Pipecat bots don't require platform API keys).
// opts.AgentID must be the WebSocket URL of the Pipecat bot.
func (p *PipecatSession) Start(ctx context.Context, reader io.Reader, writer io.Writer, apiKey string, opts Options, cb Callbacks) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.running = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.running = false
		p.cancel = nil
		p.lw = nil
		p.mu.Unlock()
		if cb.OnDisconnected != nil {
			cb.OnDisconnected()
		}
	}()

	wsURL := opts.AgentID
	p.log.Info("pipecat dialing", "url", wsURL)
	conn, _, _, err := ws.Dial(ctx, wsURL)
	if err != nil {
		p.log.Error("pipecat dial failed", "error", err)
		return err
	}
	p.log.Info("pipecat websocket connected")
	defer conn.Close()

	lw := &pipecatLockedWriter{conn: conn}

	// Pipecat has no handshake; connection is immediately live.
	// Use the WebSocket URL as the conversation ID.
	p.mu.Lock()
	p.conversationID = wsURL
	p.lw = lw
	p.mu.Unlock()
	if cb.OnConnected != nil {
		cb.OnConnected(wsURL)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		p.sendLoop(ctx, reader, lw)
	}()

	go func() {
		defer wg.Done()
		p.recvLoop(ctx, conn, writer, cb)
	}()

	wg.Wait()
	return nil
}

// Stop cancels the running Pipecat session.
func (p *PipecatSession) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
}

// Running returns whether the session is active.
func (p *PipecatSession) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// ConversationID returns the Pipecat bot URL used as the conversation identifier.
func (p *PipecatSession) ConversationID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conversationID
}

func (p *PipecatSession) sendLoop(ctx context.Context, reader io.Reader, lw *pipecatLockedWriter) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			p.log.Debug("pipecat sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			p.log.Info("pipecat sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			p.log.Info("pipecat sendLoop first audio read", "bytes", n)
		}

		// Wrap PCM audio in a protobuf Frame with AudioRawFrame.
		frame := &pb.Frame{
			Frame: &pb.Frame_Audio{
				Audio: &pb.AudioRawFrame{
					Audio:       buf[:n],
					SampleRate:  pipecatSampleRate,
					NumChannels: 1,
				},
			},
		}
		data, err := proto.Marshal(frame)
		if err != nil {
			p.log.Debug("pipecat proto marshal error", "error", err)
			continue
		}

		if err := lw.WriteBinary(data); err != nil {
			p.log.Debug("pipecat send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			p.log.Debug("pipecat sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// InjectMessage sends a TextFrame to the Pipecat bot.
func (p *PipecatSession) InjectMessage(ctx context.Context, message string) error {
	p.mu.Lock()
	lw := p.lw
	running := p.running
	p.mu.Unlock()

	if !running || lw == nil {
		return fmt.Errorf("agent session not running")
	}

	frame := &pb.Frame{
		Frame: &pb.Frame_Text{
			Text: &pb.TextFrame{
				Text: message,
			},
		},
	}
	data, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	return lw.WriteBinary(data)
}

// pipecatRTVIMessage represents a JSON message inside a Pipecat MessageFrame.
type pipecatRTVIMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// pipecatTranscriptData is the data payload for transcript-related RTVI messages.
type pipecatTranscriptData struct {
	Text string `json:"text"`
	Role string `json:"role"`
}

func (p *PipecatSession) recvLoop(ctx context.Context, conn net.Conn, writer io.Writer, cb Callbacks) {
	rd := &wsutil.Reader{
		Source: conn,
		State:  ws.StateClientSide,
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
				p.log.Debug("pipecat recvLoop context done")
			default:
				p.log.Debug("pipecat recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			p.log.Info("pipecat recv close frame")
			return
		}

		payload, err := io.ReadAll(rd)
		if err != nil {
			p.log.Debug("pipecat read error", "error", err)
			return
		}

		if hdr.OpCode != ws.OpBinary {
			p.log.Debug("pipecat ignoring non-binary frame", "opcode", hdr.OpCode)
			continue
		}

		// Deserialize protobuf Frame.
		var frame pb.Frame
		if err := proto.Unmarshal(payload, &frame); err != nil {
			p.log.Debug("pipecat proto unmarshal error", "error", err)
			continue
		}

		switch f := frame.Frame.(type) {
		case *pb.Frame_Audio:
			// Write raw PCM audio to the writer.
			if f.Audio != nil && len(f.Audio.Audio) > 0 {
				if _, err := writer.Write(f.Audio.Audio); err != nil {
					p.log.Debug("pipecat audio write error", "error", err)
				}
			}

		case *pb.Frame_Transcription:
			if f.Transcription != nil && f.Transcription.Text != "" {
				p.log.Info("pipecat transcription", "text", f.Transcription.Text, "user_id", f.Transcription.UserId)
				if cb.OnUserTranscript != nil {
					cb.OnUserTranscript(f.Transcription.Text)
				}
			}

		case *pb.Frame_Text:
			if f.Text != nil && f.Text.Text != "" {
				p.log.Info("pipecat text frame", "text", f.Text.Text)
				if cb.OnAgentResponse != nil {
					cb.OnAgentResponse(f.Text.Text)
				}
			}

		case *pb.Frame_Message:
			if f.Message != nil && f.Message.Message != "" {
				p.handleMessageFrame(f.Message.Message, cb)
			}
		}
	}
}

// handleMessageFrame parses JSON RTVI messages from Pipecat MessageFrames.
func (p *PipecatSession) handleMessageFrame(raw string, cb Callbacks) {
	var msg pipecatRTVIMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		p.log.Debug("pipecat message parse error", "error", err, "raw", raw[:min(len(raw), 200)])
		return
	}

	switch msg.Type {
	case "user-transcript", "user-transcription":
		var data pipecatTranscriptData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.Text != "" {
			p.log.Info("pipecat user transcript (rtvi)", "text", data.Text)
			if cb.OnUserTranscript != nil {
				cb.OnUserTranscript(data.Text)
			}
		}

	case "bot-transcript", "bot-transcription":
		var data pipecatTranscriptData
		if err := json.Unmarshal(msg.Data, &data); err == nil && data.Text != "" {
			p.log.Info("pipecat bot transcript (rtvi)", "text", data.Text)
			if cb.OnAgentResponse != nil {
				cb.OnAgentResponse(data.Text)
			}
		}

	default:
		p.log.Debug("pipecat rtvi message", "type", msg.Type, "raw", raw[:min(len(raw), 200)])
	}
}
