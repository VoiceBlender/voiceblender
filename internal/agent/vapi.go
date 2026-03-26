package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const vapiCallURL = "https://api.vapi.ai/call"

// VAPISession manages a WebSocket connection to the VAPI conversational AI API.
type VAPISession struct {
	mu             sync.Mutex
	running        bool
	cancel         context.CancelFunc
	conversationID string
	controlURL     string
	apiKeyStored   string
	log            *slog.Logger
}

func NewVAPI(log *slog.Logger) *VAPISession {
	return &VAPISession{log: log}
}

// vapiCallRequest is the payload for POST /call.
type vapiCallRequest struct {
	AssistantID        string                 `json:"assistantId"`
	Transport          vapiTransport          `json:"transport"`
	AssistantOverrides map[string]interface{} `json:"assistantOverrides,omitempty"`
}

type vapiTransport struct {
	Provider    string          `json:"provider"`
	AudioFormat vapiAudioFormat `json:"audioFormat"`
}

type vapiAudioFormat struct {
	Format     string `json:"format"`
	Container  string `json:"container"`
	SampleRate int    `json:"sampleRate"`
}

// vapiCallResponse is the response from POST /call.
type vapiCallResponse struct {
	ID         string `json:"id"`
	ListenURL  string `json:"listenUrl"`
	ControlURL string `json:"controlUrl"`
}

// Start creates a VAPI call, connects to the returned WebSocket, and streams
// audio bidirectionally. Blocks until the context is cancelled or an error occurs.
func (v *VAPISession) Start(ctx context.Context, reader io.Reader, writer io.Writer, apiKey string, opts Options, cb Callbacks) error {
	v.mu.Lock()
	if v.running {
		v.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	v.cancel = cancel
	v.running = true
	v.mu.Unlock()

	defer func() {
		v.mu.Lock()
		v.running = false
		v.cancel = nil
		v.controlURL = ""
		v.apiKeyStored = ""
		v.mu.Unlock()
		if cb.OnDisconnected != nil {
			cb.OnDisconnected()
		}
	}()

	// Create call via REST API.
	callResp, err := v.createCall(ctx, apiKey, opts)
	if err != nil {
		return fmt.Errorf("vapi create call: %w", err)
	}

	v.mu.Lock()
	v.conversationID = callResp.ID
	v.controlURL = callResp.ControlURL
	v.apiKeyStored = apiKey
	v.mu.Unlock()

	v.log.Info("vapi call created", "call_id", callResp.ID, "listen_url", callResp.ListenURL)
	if cb.OnConnected != nil {
		cb.OnConnected(callResp.ID)
	}

	// Connect WebSocket.
	conn, _, _, err := ws.Dial(ctx, callResp.ListenURL)
	if err != nil {
		v.log.Error("vapi websocket dial failed", "error", err)
		return fmt.Errorf("vapi ws dial: %w", err)
	}
	v.log.Info("vapi websocket connected")
	defer conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		v.sendLoop(ctx, reader, conn)
	}()

	go func() {
		defer wg.Done()
		v.recvLoop(ctx, conn, writer, cb)
	}()

	wg.Wait()
	return nil
}

// Stop cancels the running VAPI session.
func (v *VAPISession) Stop() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.cancel != nil {
		v.cancel()
	}
}

// Running returns whether the session is active.
func (v *VAPISession) Running() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running
}

// ConversationID returns the call ID assigned by VAPI.
func (v *VAPISession) ConversationID() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.conversationID
}

// InjectMessage sends a system message to the VAPI call via the control URL.
func (v *VAPISession) InjectMessage(ctx context.Context, message string) error {
	v.mu.Lock()
	controlURL := v.controlURL
	apiKey := v.apiKeyStored
	running := v.running
	v.mu.Unlock()

	if !running {
		return fmt.Errorf("agent session not running")
	}
	if controlURL == "" {
		return fmt.Errorf("no control URL available")
	}

	payload := map[string]interface{}{
		"type": "add-message",
		"message": map[string]string{
			"role":    "system",
			"content": message,
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vapi control API returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (v *VAPISession) createCall(ctx context.Context, apiKey string, opts Options) (*vapiCallResponse, error) {
	body := vapiCallRequest{
		AssistantID: opts.AgentID,
		Transport: vapiTransport{
			Provider: "vapi.websocket",
			AudioFormat: vapiAudioFormat{
				Format:     "pcm_s16le",
				Container:  "raw",
				SampleRate: 16000,
			},
		},
	}

	overrides := map[string]interface{}{}
	if opts.FirstMessage != "" {
		overrides["firstMessage"] = opts.FirstMessage
	}
	if len(opts.DynamicVariables) > 0 {
		overrides["variableValues"] = opts.DynamicVariables
	}
	if len(overrides) > 0 {
		body.AssistantOverrides = overrides
	}

	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vapiCallURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("vapi call API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var callResp vapiCallResponse
	if err := json.NewDecoder(resp.Body).Decode(&callResp); err != nil {
		return nil, fmt.Errorf("decode call response: %w", err)
	}
	if callResp.ListenURL == "" {
		return nil, fmt.Errorf("vapi call response missing listenUrl")
	}

	return &callResp, nil
}

func (v *VAPISession) sendLoop(ctx context.Context, reader io.Reader, conn net.Conn) {
	buf := make([]byte, frameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			v.log.Debug("vapi sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			v.log.Info("vapi sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			v.log.Info("vapi sendLoop first audio read", "bytes", n)
		}

		// VAPI expects raw PCM as binary WebSocket frames.
		if err := wsutil.WriteClientBinary(conn, buf[:n]); err != nil {
			v.log.Debug("vapi send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			v.log.Debug("vapi sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// vapiServerMessage is a generic envelope for VAPI text messages.
type vapiServerMessage struct {
	Type string `json:"type"`
}

// vapiTranscriptMessage holds transcript data from VAPI.
type vapiTranscriptMessage struct {
	Type       string `json:"type"`
	Role       string `json:"role"`
	Transcript string `json:"transcript"`
}

// vapiStatusUpdate holds status update data from VAPI.
type vapiStatusUpdate struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

func (v *VAPISession) recvLoop(ctx context.Context, conn net.Conn, writer io.Writer, cb Callbacks) {
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
				v.log.Debug("vapi recvLoop context done")
			default:
				v.log.Debug("vapi recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			v.log.Info("vapi recv close frame")
			return
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			v.log.Debug("vapi read error", "error", err)
			return
		}

		switch hdr.OpCode {
		case ws.OpBinary:
			// Raw PCM audio from VAPI.
			if _, err := writer.Write(buf.Bytes()); err != nil {
				v.log.Debug("vapi audio write error", "error", err)
			}

		case ws.OpText:
			raw := buf.Bytes()
			v.log.Debug("vapi recv text", "raw", string(raw[:min(len(raw), 300)]))

			var envelope vapiServerMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				v.log.Debug("vapi parse error", "error", err)
				continue
			}

			switch envelope.Type {
			case "transcript":
				var msg vapiTranscriptMessage
				if err := json.Unmarshal(raw, &msg); err == nil && msg.Transcript != "" {
					switch msg.Role {
					case "user":
						v.log.Info("vapi user transcript", "text", msg.Transcript)
						if cb.OnUserTranscript != nil {
							cb.OnUserTranscript(msg.Transcript)
						}
					case "assistant":
						v.log.Info("vapi agent response", "text", msg.Transcript)
						if cb.OnAgentResponse != nil {
							cb.OnAgentResponse(msg.Transcript)
						}
					}
				}

			case "status-update":
				var msg vapiStatusUpdate
				if err := json.Unmarshal(raw, &msg); err == nil {
					v.log.Info("vapi status update", "status", msg.Status)
					if msg.Status == "ended" {
						return
					}
				}

			case "speech-update":
				v.log.Debug("vapi speech update", "raw", string(raw[:min(len(raw), 200)]))

			default:
				v.log.Debug("vapi unknown message type", "type", envelope.Type)
			}
		}
	}
}
