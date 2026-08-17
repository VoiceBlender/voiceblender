package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	smDefaultURL = "wss://eu2.rt.speechmatics.com/v2"
	smFrameBytes = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)

	// smDefaultEOUSec turns on Speechmatics' end-of-utterance detector by
	// default, which is what makes the provider report turn boundaries at all.
	smDefaultEOUSec = 0.6
	smMaxEOUSec     = 2.0

	smMinMaxDelaySec = 0.7
	smMaxMaxDelaySec = 4.0
)

// smForceEOUFrame closes the current utterance and emits a final transcript
// while leaving the session open.
var smForceEOUFrame = []byte(`{"message":"ForceEndOfUtterance"}`)

// SpeechmaticsTranscriber streams audio to the Speechmatics Realtime v2 API
// over WebSocket.
type SpeechmaticsTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	url     string
	log     *slog.Logger
	// lw is the live socket writer, guarded by mu. Non-nil only between a
	// successful dial and Start returning, so Finalize can reach a socket
	// that otherwise lives entirely inside Start.
	lw *wsutilx.LockedWriter
}

// NewSpeechmatics builds a transcriber for the given endpoint. An empty url
// selects the default SaaS region; a self-hosted container or another region
// is reached by passing its wss:// address.
func NewSpeechmatics(url string, log *slog.Logger) *SpeechmaticsTranscriber {
	if url == "" {
		url = smDefaultURL
	}
	return &SpeechmaticsTranscriber{url: url, log: log}
}

type smAudioFormat struct {
	Type       string `json:"type"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type smConversationConfig struct {
	EndOfUtteranceSilenceTrigger float64 `json:"end_of_utterance_silence_trigger"`
}

type smTranscriptionConfig struct {
	Language           string                `json:"language"`
	Model              string                `json:"model,omitempty"`
	EnablePartials     bool                  `json:"enable_partials,omitempty"`
	MaxDelay           float64               `json:"max_delay,omitempty"`
	AdditionalVocab    []string              `json:"additional_vocab,omitempty"`
	ConversationConfig *smConversationConfig `json:"conversation_config,omitempty"`
}

type smStartRecognition struct {
	Message             string                `json:"message"`
	AudioFormat         smAudioFormat         `json:"audio_format"`
	TranscriptionConfig smTranscriptionConfig `json:"transcription_config"`
}

// buildStartRecognition maps Options onto the StartRecognition handshake.
// Out-of-range values are clamped rather than rejected: the same request body
// is accepted by every provider, so a value that is legal for Deepgram must
// not fail here.
func buildStartRecognition(opts Options) ([]byte, error) {
	lang := opts.Language
	if lang == "" {
		lang = "en"
	}

	cfg := smTranscriptionConfig{
		Language:        lang,
		Model:           opts.Model,
		EnablePartials:  opts.Partial,
		AdditionalVocab: opts.Keyterms,
	}

	// Deepgram's endpointing is the nearest equivalent of max_delay: both are
	// how long the server waits before finalizing a segment. endpointing=0
	// disables Deepgram's, which Speechmatics cannot express, so it falls back
	// to the vendor default.
	if opts.Endpointing != nil && *opts.Endpointing > 0 {
		cfg.MaxDelay = clampFloat(float64(*opts.Endpointing)/1000, smMinMaxDelaySec, smMaxMaxDelaySec)
	}

	eou := smDefaultEOUSec
	if opts.UtteranceEndMs != nil {
		eou = clampFloat(float64(*opts.UtteranceEndMs)/1000, 0, smMaxEOUSec)
	}
	if eou > 0 {
		cfg.ConversationConfig = &smConversationConfig{EndOfUtteranceSilenceTrigger: eou}
	}

	return json.Marshal(smStartRecognition{
		Message: "StartRecognition",
		AudioFormat: smAudioFormat{
			Type:       "raw",
			Encoding:   "pcm_s16le",
			SampleRate: 16000,
		},
		TranscriptionConfig: cfg,
	})
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (t *SpeechmaticsTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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

	start, err := buildStartRecognition(opts)
	if err != nil {
		return err
	}

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"Authorization": []string{"Bearer " + apiKey},
		},
	}

	t.log.Info("speechmatics stt dialing", "url", t.url)
	conn, _, _, err := dialer.Dial(ctx, t.url)
	if err != nil {
		t.log.Error("speechmatics stt dial failed", "error", err)
		return err
	}
	t.log.Info("speechmatics stt websocket connected")
	defer conn.Close()

	lw := wsutilx.NewLockedWriter(conn)
	// Registered after `defer conn.Close()` so LIFO clears the field before
	// the socket goes away; a Finalize that already copied the writer out can
	// still race the close, which is why its write error is surfaced.
	t.setWriter(lw)
	defer t.clearWriter()

	// No wait for RecognitionStarted: the socket preserves ordering, so audio
	// queued behind this frame still arrives after it.
	if err := lw.WriteText(start); err != nil {
		t.log.Error("speechmatics stt start recognition failed", "error", err)
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Either loop exiting cancels the other: a write that fails on its
	// deadline must not leave Start waiting out the recv read timeout.
	go func() {
		defer wg.Done()
		defer cancel()
		t.sendLoop(ctx, reader, lw)
	}()

	go func() {
		defer wg.Done()
		defer cancel()
		t.recvLoop(ctx, conn, lw, opts, cb)
	}()

	wg.Wait()
	return nil
}

func (t *SpeechmaticsTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *SpeechmaticsTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *SpeechmaticsTranscriber) setWriter(lw *wsutilx.LockedWriter) {
	t.mu.Lock()
	t.lw = lw
	t.mu.Unlock()
}

func (t *SpeechmaticsTranscriber) clearWriter() {
	t.mu.Lock()
	t.lw = nil
	t.mu.Unlock()
}

// Finalize closes the current utterance and keeps the socket OPEN — unlike the
// EndOfStream frame sendLoop writes at teardown, which flushes AND ends the
// session. Fire-and-forget: the forced final and its EndOfUtterance arrive
// through the existing recvLoop callbacks.
func (t *SpeechmaticsTranscriber) Finalize(ctx context.Context) error {
	t.mu.Lock()
	lw := t.lw
	t.mu.Unlock()
	if lw == nil {
		return errors.New("speechmatics stt session not connected")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return lw.WriteText(smForceEOUFrame)
}

func (t *SpeechmaticsTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *wsutilx.LockedWriter) {
	buf := make([]byte, smFrameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			t.writeEndOfStream(lw, sendCount)
			t.log.Debug("speechmatics stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			t.writeEndOfStream(lw, sendCount)
			t.log.Info("speechmatics stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			t.log.Info("speechmatics stt sendLoop first audio read", "bytes", n)
		}

		if err := lw.WriteBinary(buf[:n]); err != nil {
			t.log.Debug("speechmatics stt send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 { // every ~5s at 20ms frames
			t.log.Debug("speechmatics stt sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// writeEndOfStream tells the server no more audio is coming. last_seq_no is the
// count of audio chunks sent, which is what the server acknowledged on AudioAdded.
func (t *SpeechmaticsTranscriber) writeEndOfStream(lw *wsutilx.LockedWriter, sendCount int) {
	_ = lw.WriteText([]byte(fmt.Sprintf(`{"message":"EndOfStream","last_seq_no":%d}`, sendCount)))
}

type smMetadata struct {
	Transcript string  `json:"transcript"`
	StartTime  float64 `json:"start_time"`
	EndTime    float64 `json:"end_time"`
}

type smAlternative struct {
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
}

type smResult struct {
	Type         string          `json:"type"`
	StartTime    float64         `json:"start_time"`
	EndTime      float64         `json:"end_time"`
	Alternatives []smAlternative `json:"alternatives"`
}

// smMessage covers every server frame. AddTranscript and EndOfUtterance carry
// the work; the rest are session lifecycle, flow control and diagnostics.
type smMessage struct {
	Message string `json:"message"`

	// RecognitionStarted
	ID string `json:"id"`

	// AddPartialTranscript / AddTranscript / EndOfUtterance
	Metadata smMetadata `json:"metadata"`
	Results  []smResult `json:"results"`
	Forced   bool       `json:"forced"`

	// Error / Warning / Info
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Code   int    `json:"code"`
}

// smUtterance accumulates the AddTranscript segments that make up one turn.
// Speechmatics finalizes a segment every max_delay, so a single utterance
// usually spans several of them before EndOfUtterance closes it.
type smUtterance struct {
	segments  []string
	words     []TurnWord
	startTime float64
	started   bool
}

func (u *smUtterance) add(msg smMessage) {
	if !u.started {
		u.startTime = msg.Metadata.StartTime
		u.started = true
	}
	if seg := strings.TrimSpace(msg.Metadata.Transcript); seg != "" {
		u.segments = append(u.segments, seg)
	}
	u.words = append(u.words, smWords(msg.Results)...)
}

func (u *smUtterance) text() string { return strings.Join(u.segments, " ") }

func (t *SpeechmaticsTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *wsutilx.LockedWriter, opts Options, cb TranscriptCallback) {
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

	// recvLoop is the only goroutine touching these, so no lock is needed.
	var utt smUtterance
	var turnIndex int

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		wsutilx.SetReadDeadline(conn, wsutilx.DefaultReadTimeout.Load())

		hdr, err := rd.NextFrame()
		if err != nil {
			select {
			case <-ctx.Done():
				t.log.Debug("speechmatics stt recvLoop context done")
			default:
				t.log.Debug("speechmatics stt recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			payload, perr := io.ReadAll(rd)
			if perr != nil {
				t.log.Debug("speechmatics stt close payload read error", "error", perr)
				return
			}
			code, reason := ws.ParseCloseFrameData(payload)
			t.log.Info("speechmatics stt recv close frame", "code", int(code), "reason", reason)
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("speechmatics stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("speechmatics stt read error", "error", err)
			return
		}

		raw := buf.Bytes()
		t.log.Debug("speechmatics stt recv msg", "raw", string(raw[:min(len(raw), 300)]))

		var msg smMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.log.Debug("speechmatics stt parse error", "error", err)
			continue
		}

		switch msg.Message {
		case "RecognitionStarted":
			t.log.Info("speechmatics stt session started", "id", msg.ID)
		case "AudioAdded":
			// Flow-control ack, one per 20ms chunk; nothing to do.
		case "AddPartialTranscript":
			text := strings.TrimSpace(msg.Metadata.Transcript)
			if text == "" || !opts.Partial {
				continue
			}
			t.log.Debug("speechmatics stt partial transcript", "text", text)
			emitTranscript(opts, cb, TranscriptEvent{Text: text})
		case "AddTranscript":
			utt.add(msg)
			text := strings.TrimSpace(msg.Metadata.Transcript)
			if text == "" {
				continue
			}
			t.log.Debug("speechmatics stt final transcript", "text", text)
			// SpeechFinal stays false: EndOfUtterance is what says the speaker
			// stopped, and it only arrives after this segment.
			emitTranscript(opts, cb, TranscriptEvent{Text: text, IsFinal: true})
		case "EndOfUtterance":
			t.log.Info("speechmatics stt end of utterance", "turn_index", turnIndex, "forced", msg.Forced, "text_len", len(utt.text()))
			emitTurn(opts, TurnEvent{
				Event:            TurnEndOfTurn,
				TurnIndex:        turnIndex,
				Transcript:       utt.text(),
				AudioWindowStart: utt.startTime,
				AudioWindowEnd:   msg.Metadata.EndTime,
				Words:            utt.words,
			})
			utt = smUtterance{}
			turnIndex++
		case "EndOfTranscript":
			t.log.Info("speechmatics stt end of transcript")
			return
		case "Error":
			// Speechmatics closes the socket itself on a fatal error, so keep
			// reading — ending here would drop a still-live session.
			t.log.Error("speechmatics stt error frame", "type", msg.Type, "code", msg.Code, "reason", msg.Reason)
		case "Warning", "Info":
			t.log.Info("speechmatics stt notice", "frame", msg.Message, "type", msg.Type, "reason", msg.Reason)
		}
	}
}

// smWords lifts the word-level results of one transcript segment onto the
// dialect-neutral TurnWord. Punctuation and entity results are skipped; the
// first alternative is the one Speechmatics ranks highest.
func smWords(in []smResult) []TurnWord {
	var out []TurnWord
	for _, r := range in {
		if r.Type != "word" || len(r.Alternatives) == 0 {
			continue
		}
		out = append(out, TurnWord{
			Word:       r.Alternatives[0].Content,
			Confidence: r.Alternatives[0].Confidence,
			Start:      r.StartTime,
			End:        r.EndTime,
		})
	}
	return out
}
