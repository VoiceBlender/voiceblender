package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// SixtyDbSession composes 60db's STT WS + Chat Completions + TTS WS into
// the same agent.Provider shape as ElevenLabs ConvAI.
//
// 60db ships the three pieces separately — there is no managed
// conversational AI endpoint — so this file IS the orchestration. The
// state machine mirrors the Twilio bridge work in the elevenlabs-twilio
// repo: STT yields canonical finals → LLM call → TTS chunks → writer.
//
// Audio contract:
//   reader: 16 kHz PCM 16-bit mono   (from the leg's inbound mixer tap)
//   writer: 16 kHz PCM 16-bit mono   (back into the leg's outbound queue)
//
// References:
//   https://docs.60db.ai/websocket-api/stt
//   https://docs.60db.ai/websocket-api/tts
//   https://docs.60db.ai/api-reference/llm/chat-completion
type SixtyDbSession struct {
	mu             sync.Mutex
	running        bool
	cancel         context.CancelFunc
	conversationID string
	log            *slog.Logger

	// LLM history is owned by the session so InjectMessage can land in
	// the same conversation context the user is having.
	histMu  sync.Mutex
	history []chatMessage

	// TTS WS pieces shared between turn-handler and InjectMessage.
	ttsConn   net.Conn
	ttsLW     *lockedWriter
	ttsCtxID  string
	ttsReady  chan struct{}
	ttsReadyO sync.Once
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const (
	sixtydbAgentBase       = "https://api.60db.ai"
	sixtydbAgentSampleRate = 16000
	sixtydbDefaultLLMModel = "60db-tiny"
)

// NewSixtyDb constructs a 60db-composed agent provider.
func NewSixtyDb(log *slog.Logger) *SixtyDbSession {
	return &SixtyDbSession{log: log}
}

func (s *SixtyDbSession) Start(ctx context.Context, reader io.Reader, writer io.Writer, apiKey string, opts Options, cb Callbacks) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.running = true
	s.conversationID = fmt.Sprintf("60db-conv-%d", time.Now().UnixNano())
	s.history = []chatMessage{{
		Role:    "system",
		Content: systemPromptFromOpts(opts),
	}}
	s.ttsReady = make(chan struct{})
	s.ttsReadyO = sync.Once{}
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

	if apiKey == "" {
		return fmt.Errorf("sixtydb agent: API key required")
	}

	base := os.Getenv("SIXTYDB_API_BASE")
	if base == "" {
		base = sixtydbAgentBase
	}
	base = strings.TrimRight(base, "/")

	// 1. Open STT WS + TTS WS in parallel.
	sttConn, err := s.openSttWS(ctx, base, apiKey, opts)
	if err != nil {
		return fmt.Errorf("stt dial: %w", err)
	}
	defer sttConn.Close()

	ttsConn, ttsLW, ttsCtxID, err := s.openTtsWS(ctx, base, apiKey)
	if err != nil {
		return fmt.Errorf("tts dial: %w", err)
	}
	defer ttsConn.Close()
	s.mu.Lock()
	s.ttsConn = ttsConn
	s.ttsLW = ttsLW
	s.ttsCtxID = ttsCtxID
	s.mu.Unlock()

	if cb.OnConnected != nil {
		cb.OnConnected(s.conversationID)
	}

	// 2. Greet caller with FirstMessage if supplied.
	if opts.FirstMessage != "" {
		go func() {
			<-s.ttsReady
			s.speak(opts.FirstMessage)
			s.appendHistory("assistant", opts.FirstMessage)
			if cb.OnAgentResponse != nil {
				cb.OnAgentResponse(opts.FirstMessage)
			}
		}()
	}

	// 3. Run the 4 goroutines (stt send/recv, tts recv, llm turn handler).
	llmCh := make(chan string, 4)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); s.sttSendLoop(ctx, reader, sttConn) }()
	go func() { defer wg.Done(); s.sttRecvLoop(ctx, sttConn, llmCh, cb) }()
	go func() { defer wg.Done(); s.ttsRecvLoop(ctx, ttsConn, writer) }()
	go func() { defer wg.Done(); s.turnLoop(ctx, llmCh, base, apiKey, opts, cb) }()
	wg.Wait()
	return nil
}

func (s *SixtyDbSession) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *SixtyDbSession) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *SixtyDbSession) ConversationID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conversationID
}

// InjectMessage feeds a system/user message into the conversation
// out-of-band (e.g. CRM lookup, tool result). The next LLM call will
// see it; if the agent is currently speaking, the new context applies
// to the turn after it.
func (s *SixtyDbSession) InjectMessage(_ context.Context, message string) error {
	s.appendHistory("user", message)
	return nil
}

// ---- STT WS ------------------------------------------------------------

func (s *SixtyDbSession) openSttWS(ctx context.Context, base, apiKey string, opts Options) (net.Conn, error) {
	wsBase := strings.Replace(base, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	q := url.Values{"apiKey": {apiKey}}
	conn, _, _, err := ws.Dial(ctx, wsBase+"/ws/stt?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// Wait for connection_established, then send start config.
	for {
		data, _, err := wsutil.ReadServerData(conn)
		if err != nil {
			conn.Close()
			return nil, err
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
			"sample_rate":               sixtydbAgentSampleRate,
			"utterance_end_ms":          500,
			"continuous_mode":           true,
			"interim_results_frequency": 300,
			"audio_enhancement":         "adaptive",
		},
	})
	if err := wsutil.WriteClientText(conn, start); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (s *SixtyDbSession) sttSendLoop(ctx context.Context, reader io.Reader, conn net.Conn) {
	buf := make([]byte, frameBytes)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := reader.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		msg, _ := json.Marshal(map[string]any{
			"type":        "audio",
			"audio":       base64.StdEncoding.EncodeToString(buf[:n]),
			"encoding":    "linear",
			"sample_rate": sixtydbAgentSampleRate,
		})
		if err := wsutil.WriteClientText(conn, msg); err != nil {
			return
		}
	}
}

func (s *SixtyDbSession) sttRecvLoop(ctx context.Context, conn net.Conn, llmCh chan<- string, cb Callbacks) {
	rd := &wsutil.Reader{Source: conn, State: ws.StateClientSide}
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
		if msg.IsFinal && msg.SpeechFinal {
			if cb.OnUserTranscript != nil {
				cb.OnUserTranscript(msg.Text)
			}
			select {
			case llmCh <- msg.Text:
			case <-ctx.Done():
				return
			}
		}
	}
}

// ---- TTS WS ------------------------------------------------------------

func (s *SixtyDbSession) openTtsWS(ctx context.Context, base, apiKey string) (net.Conn, *lockedWriter, string, error) {
	wsBase := strings.Replace(base, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	q := url.Values{"apiKey": {apiKey}}
	conn, _, _, err := ws.Dial(ctx, wsBase+"/ws/tts?"+q.Encode())
	if err != nil {
		return nil, nil, "", err
	}
	lw := &lockedWriter{conn: conn}
	ctxID := fmt.Sprintf("ttsctx-%d", time.Now().UnixNano())

	// Wait for connection_established, then create the context.
	for {
		data, _, err := wsutil.ReadServerData(conn)
		if err != nil {
			conn.Close()
			return nil, nil, "", err
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(data, &probe); err != nil {
			continue
		}
		if _, ok := probe["connection_established"]; ok {
			break
		}
	}
	create, _ := json.Marshal(map[string]any{
		"create_context": map[string]any{
			"context_id": ctxID,
			"voice_id":   getVoiceID(),
			"audio_config": map[string]any{
				"audio_encoding":    "LINEAR16",
				"sample_rate_hertz": sixtydbAgentSampleRate,
			},
		},
	})
	if err := lw.WriteText(create); err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	return conn, lw, ctxID, nil
}

func (s *SixtyDbSession) ttsRecvLoop(ctx context.Context, conn net.Conn, writer io.Writer) {
	rd := &wsutil.Reader{Source: conn, State: ws.StateClientSide}
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
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(buf.Bytes(), &msg); err != nil {
			continue
		}
		if _, ok := msg["context_created"]; ok {
			s.ttsReadyO.Do(func() { close(s.ttsReady) })
			continue
		}
		if chunkRaw, ok := msg["audio_chunk"]; ok {
			var chunk struct {
				AudioContent string `json:"audioContent"`
			}
			if json.Unmarshal(chunkRaw, &chunk) == nil && chunk.AudioContent != "" {
				pcm, err := base64.StdEncoding.DecodeString(chunk.AudioContent)
				if err == nil {
					_, _ = writer.Write(pcm)
				}
			}
			continue
		}
		// flush_completed → ready for the next utterance; nothing to do.
	}
}

func (s *SixtyDbSession) speak(text string) {
	s.mu.Lock()
	lw := s.ttsLW
	ctxID := s.ttsCtxID
	s.mu.Unlock()
	if lw == nil || ctxID == "" {
		return
	}
	send, _ := json.Marshal(map[string]any{"send_text": map[string]any{"context_id": ctxID, "text": text}})
	_ = lw.WriteText(send)
	flush, _ := json.Marshal(map[string]any{"flush_context": map[string]any{"context_id": ctxID}})
	_ = lw.WriteText(flush)
}

// ---- LLM turn loop -----------------------------------------------------

func (s *SixtyDbSession) turnLoop(ctx context.Context, llmCh <-chan string, base, apiKey string, opts Options, cb Callbacks) {
	model := os.Getenv("SIXTYDB_LLM_MODEL")
	if model == "" {
		model = sixtydbDefaultLLMModel
	}
	for {
		select {
		case <-ctx.Done():
			return
		case user := <-llmCh:
			s.appendHistory("user", user)
			reply, err := s.callLLM(ctx, base, apiKey, model)
			if err != nil {
				s.log.Error("sixtydb agent LLM error", "error", err)
				continue
			}
			if reply == "" {
				continue
			}
			s.appendHistory("assistant", reply)
			if cb.OnAgentResponse != nil {
				cb.OnAgentResponse(reply)
			}
			<-s.ttsReady
			s.speak(reply)
		}
	}
}

func (s *SixtyDbSession) callLLM(ctx context.Context, base, apiKey, model string) (string, error) {
	s.histMu.Lock()
	histCopy := make([]chatMessage, len(s.history))
	copy(histCopy, s.history)
	s.histMu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"model":                model,
		"messages":             histCopy,
		"stream":               false,
		"top_k":                20,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("LLM status %d: %s", resp.StatusCode, preview)
	}
	var data struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if len(data.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	return strings.TrimSpace(data.Choices[0].Message.Content), nil
}

func (s *SixtyDbSession) appendHistory(role, content string) {
	s.histMu.Lock()
	s.history = append(s.history, chatMessage{Role: role, Content: content})
	s.histMu.Unlock()
}

// ---- helpers -----------------------------------------------------------

func systemPromptFromOpts(opts Options) string {
	if opts.Settings != nil {
		if v, ok := opts.Settings["system_prompt"].(string); ok && v != "" {
			return v
		}
	}
	if v := os.Getenv("SIXTYDB_SYSTEM_PROMPT"); v != "" {
		return v
	}
	return "You are a helpful voice agent. Reply in short, clear sentences suitable for a phone call."
}

func getVoiceID() string {
	if v := os.Getenv("SIXTYDB_TTS_VOICE_ID"); v != "" {
		return v
	}
	return "fbb75ed2-975a-40c7-9e06-38e30524a9a1"
}

// Note: lockedWriter is defined in agent/elevenlabs.go (same package);
// reusing it here serializes all WS writes (audio sends + pong replies).
