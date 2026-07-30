package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// SixtyDb implements Provider against the 60db cloud TTS API.
// Three transports are supported behind one struct so the leg layer
// can pick the best fit per use case:
//
//	TransportWS     - wss://api.60db.ai/ws/tts (default; LINEAR16 @ 16k,
//	                  mixer-native, lowest latency)
//	TransportStream - POST /tts-stream (NDJSON of base64 chunks; mp3)
//	TransportREST   - POST /tts-synthesize (one-shot base64; mp3)
//
// Reference: https://docs.60db.ai
type SixtyDb struct {
	apiKey    string
	apiBase   string
	transport Transport
	voiceID   string
	client    *http.Client
	log       *slog.Logger
}

// Transport selects which 60db TTS surface to use.
type Transport int

const (
	TransportWS Transport = iota
	TransportStream
	TransportREST
)

const (
	sixtydbDefaultBase    = "https://api.60db.ai"
	sixtydbDefaultVoice   = "fbb75ed2-975a-40c7-9e06-38e30524a9a1"
	sixtydbTTSSampleRate  = 16000
	sixtydbWSAudioEncoding = "LINEAR16"
)

// NewSixtyDb creates a 60db TTS provider with the WebSocket transport
// (mixer-native PCM 16k). Use NewSixtyDbWith() to pick a different transport.
func NewSixtyDb(apiKey string, log *slog.Logger) *SixtyDb {
	return NewSixtyDbWith(apiKey, TransportWS, log)
}

// NewSixtyDbWith creates a 60db TTS provider with a specific transport.
// apiBase + voiceID can be overridden by env (SIXTYDB_API_BASE, SIXTYDB_TTS_VOICE_ID).
func NewSixtyDbWith(apiKey string, transport Transport, log *slog.Logger) *SixtyDb {
	return &SixtyDb{
		apiKey:    apiKey,
		apiBase:   sixtydbDefaultBase,
		transport: transport,
		voiceID:   sixtydbDefaultVoice,
		client:    &http.Client{},
		log:       log,
	}
}

// SetBase overrides the API base URL. Useful for tests or staging.
func (s *SixtyDb) SetBase(base string) {
	if base != "" {
		s.apiBase = strings.TrimRight(base, "/")
	}
}

// SetDefaultVoice changes the voice id used when Options.Voice is empty.
func (s *SixtyDb) SetDefaultVoice(voice string) {
	if voice != "" {
		s.voiceID = voice
	}
}

func (s *SixtyDb) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = s.apiKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("sixtydb: no API key provided")
	}
	voice := opts.Voice
	if voice == "" {
		voice = s.voiceID
	}

	switch s.transport {
	case TransportREST:
		return s.synthesizeREST(ctx, text, voice, apiKey)
	case TransportStream:
		return s.synthesizeStream(ctx, text, voice, apiKey)
	default:
		return s.synthesizeWS(ctx, text, voice, apiKey)
	}
}

// ---- REST sync (POST /tts-synthesize → mp3) ----------------------------

func (s *SixtyDb) synthesizeREST(ctx context.Context, text, voice, apiKey string) (*Result, error) {
	payload := map[string]any{
		"text":          text,
		"voice_id":      voice,
		"enhance":       true,
		"speed":         1,
		"stability":     50,
		"similarity":    75,
		"output_format": "mp3",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.apiBase+"/tts-synthesize", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sixtydb /tts-synthesize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("sixtydb /tts-synthesize: status %d: %s", resp.StatusCode, preview)
	}

	var data struct {
		Success     bool   `json:"success"`
		AudioBase64 string `json:"audio_base64"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("sixtydb: decode response: %w", err)
	}
	if !data.Success || data.AudioBase64 == "" {
		return nil, fmt.Errorf("sixtydb: empty audio (%s)", data.Message)
	}
	mp3, err := base64.StdEncoding.DecodeString(data.AudioBase64)
	if err != nil {
		return nil, fmt.Errorf("sixtydb: decode base64: %w", err)
	}
	return &Result{
		Audio:    io.NopCloser(bytes.NewReader(mp3)),
		MimeType: "audio/mpeg",
	}, nil
}

// ---- NDJSON stream (POST /tts-stream → mp3 chunks) ---------------------

func (s *SixtyDb) synthesizeStream(ctx context.Context, text, voice, apiKey string) (*Result, error) {
	payload := map[string]any{
		"text":          text,
		"voice_id":      voice,
		"output_format": "mp3",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.apiBase+"/tts-stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sixtydb /tts-stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("sixtydb /tts-stream: status %d: %s", resp.StatusCode, preview)
	}

	// Spawn a goroutine that parses NDJSON lines and pipes decoded mp3
	// bytes into the consumer side of an io.Pipe. The leg can start
	// playback as soon as the first chunk arrives.
	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		dec := json.NewDecoder(resp.Body)
		for {
			if ctx.Err() != nil {
				return
			}
			var msg struct {
				Type         string `json:"type"`
				AudioContent string `json:"audioContent"`
			}
			if err := dec.Decode(&msg); err != nil {
				if err != io.EOF {
					pw.CloseWithError(err)
				}
				return
			}
			if msg.Type == "error" {
				pw.CloseWithError(fmt.Errorf("sixtydb /tts-stream emitted error"))
				return
			}
			if msg.Type == "complete" {
				return
			}
			if msg.AudioContent == "" {
				continue
			}
			chunk, err := base64.StdEncoding.DecodeString(msg.AudioContent)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("base64 decode: %w", err))
				return
			}
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()
	return &Result{Audio: pr, MimeType: "audio/mpeg"}, nil
}

// ---- WebSocket (wss://.../ws/tts → LINEAR16 PCM @ 16k) -----------------

func (s *SixtyDb) synthesizeWS(ctx context.Context, text, voice, apiKey string) (*Result, error) {
	wsBase := strings.Replace(s.apiBase, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	q := url.Values{"apiKey": {apiKey}}
	dialURL := wsBase + "/ws/tts?" + q.Encode()

	conn, _, _, err := ws.Dial(ctx, dialURL)
	if err != nil {
		return nil, fmt.Errorf("sixtydb /ws/tts dial: %w", err)
	}

	pr, pw := io.Pipe()
	contextID := fmt.Sprintf("ctx-%d", time.Now().UnixNano())
	go func() {
		defer conn.Close()
		defer pw.Close()
		for {
			data, _, err := wsutil.ReadServerData(conn)
			if err != nil {
				if err != io.EOF {
					pw.CloseWithError(err)
				}
				return
			}
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if _, ok := msg["connection_established"]; ok {
				create := map[string]any{
					"create_context": map[string]any{
						"context_id": contextID,
						"voice_id":   voice,
						"audio_config": map[string]any{
							"audio_encoding":    sixtydbWSAudioEncoding,
							"sample_rate_hertz": sixtydbTTSSampleRate,
						},
					},
				}
				b, _ := json.Marshal(create)
				if err := wsutil.WriteClientText(conn, b); err != nil {
					pw.CloseWithError(err)
					return
				}
				continue
			}
			if _, ok := msg["context_created"]; ok {
				send, _ := json.Marshal(map[string]any{
					"send_text": map[string]any{"context_id": contextID, "text": text},
				})
				_ = wsutil.WriteClientText(conn, send)
				flush, _ := json.Marshal(map[string]any{
					"flush_context": map[string]any{"context_id": contextID},
				})
				_ = wsutil.WriteClientText(conn, flush)
				continue
			}
			if chunkRaw, ok := msg["audio_chunk"]; ok {
				var chunk struct {
					AudioContent string `json:"audioContent"`
				}
				if json.Unmarshal(chunkRaw, &chunk) == nil && chunk.AudioContent != "" {
					pcm, err := base64.StdEncoding.DecodeString(chunk.AudioContent)
					if err != nil {
						pw.CloseWithError(err)
						return
					}
					if _, err := pw.Write(pcm); err != nil {
						return
					}
				}
				continue
			}
			if _, ok := msg["flush_completed"]; ok {
				closeMsg, _ := json.Marshal(map[string]any{
					"close_context": map[string]any{"context_id": contextID},
				})
				_ = wsutil.WriteClientText(conn, closeMsg)
				return
			}
		}
	}()

	return &Result{
		Audio:    pr,
		MimeType: fmt.Sprintf("audio/pcm;rate=%d", sixtydbTTSSampleRate),
	}, nil
}

