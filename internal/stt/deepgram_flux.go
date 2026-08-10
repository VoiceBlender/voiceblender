package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	fluxWSURL = "wss://api.deepgram.com/v2/listen"
	// 1280 samples × 2 bytes = 80ms at 16kHz, the chunk size Deepgram
	// recommends for Flux's turn detector.
	fluxFrameBytes   = 2560
	fluxDefaultModel = "flux-general-en"
)

var fluxCloseFrame = []byte(`{"type":"CloseStream"}`)

// FluxTranscriber streams audio to Deepgram Flux (/v2/listen) over WebSocket.
// Flux reports a conversational turn state machine rather than plain interim
// and final segments. It deliberately does NOT implement Finalizer: /v2 has no
// Finalize message.
type FluxTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	log     *slog.Logger
}

func NewDeepgramFlux(log *slog.Logger) *FluxTranscriber {
	return &FluxTranscriber{log: log}
}

func buildFluxURL(opts Options) string {
	model := opts.Model
	if model == "" {
		model = fluxDefaultModel
	}

	var b strings.Builder
	b.WriteString(fluxWSURL)
	b.WriteString("?encoding=linear16&sample_rate=16000&model=")
	b.WriteString(url.QueryEscape(model))

	if opts.EagerEOTThreshold != nil {
		b.WriteString("&eager_eot_threshold=")
		b.WriteString(strconv.FormatFloat(*opts.EagerEOTThreshold, 'g', -1, 64))
	}
	if opts.EOTThreshold != nil {
		b.WriteString("&eot_threshold=")
		b.WriteString(strconv.FormatFloat(*opts.EOTThreshold, 'g', -1, 64))
	}
	if opts.EOTTimeoutMs != nil {
		fmt.Fprintf(&b, "&eot_timeout_ms=%d", *opts.EOTTimeoutMs)
	}
	for _, h := range opts.LanguageHints {
		b.WriteString("&language_hint=")
		b.WriteString(url.QueryEscape(h))
	}
	for _, kt := range opts.Keyterms {
		b.WriteString("&keyterm=")
		b.WriteString(url.QueryEscape(kt))
	}
	return b.String()
}

func (t *FluxTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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

	dialer := ws.Dialer{
		Header: ws.HandshakeHeaderHTTP{
			"Authorization": []string{"Token " + apiKey},
		},
	}

	wsURL := buildFluxURL(opts)
	t.log.Info("deepgram flux stt dialing", "url", wsURL)
	conn, _, _, err := dialer.Dial(ctx, wsURL)
	if err != nil {
		t.log.Error("deepgram flux stt dial failed", "error", err)
		return err
	}
	t.log.Info("deepgram flux stt websocket connected")
	defer conn.Close()

	lw := wsutilx.NewLockedWriter(conn)

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

func (t *FluxTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *FluxTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *FluxTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *wsutilx.LockedWriter) {
	buf := make([]byte, fluxFrameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			_ = lw.WriteText(fluxCloseFrame)
			t.log.Debug("deepgram flux stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		// ReadFull, not Read: the upstream pipe and resampler both hand back
		// one 20ms mixer frame per call, so a bare Read would send 640-byte
		// chunks and defeat the 80ms cadence Flux expects.
		n, err := io.ReadFull(reader, buf)
		if n > 0 {
			if sendCount == 0 {
				t.log.Info("deepgram flux stt sendLoop first audio read", "bytes", n)
			}
			if werr := lw.WriteBinary(buf[:n]); werr != nil {
				t.log.Debug("deepgram flux stt send error", "error", werr, "sent_frames", sendCount)
				return
			}
			sendCount++
			if sendCount%63 == 0 { // every ~5s at 80ms frames
				t.log.Debug("deepgram flux stt sendLoop progress", "sent_frames", sendCount)
			}
		}
		if err != nil {
			_ = lw.WriteText(fluxCloseFrame)
			t.log.Info("deepgram flux stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
	}
}

// fluxMessage covers every server frame Flux sends. TurnInfo carries the work;
// the rest are connection lifecycle and errors.
type fluxMessage struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`

	// TurnInfo
	Event               string     `json:"event"`
	TurnIndex           int        `json:"turn_index"`
	Transcript          string     `json:"transcript"`
	Words               []fluxWord `json:"words"`
	EndOfTurnConfidence float64    `json:"end_of_turn_confidence"`
	AudioWindowStart    float64    `json:"audio_window_start"`
	AudioWindowEnd      float64    `json:"audio_window_end"`
	Languages           []string   `json:"languages"`

	// Error
	Code        string `json:"code"`
	Description string `json:"description"`
}

type fluxWord struct {
	Word       string  `json:"word"`
	Confidence float64 `json:"confidence"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
}

// fluxTurnEvents maps the wire event names onto the dialect-neutral constants.
var fluxTurnEvents = map[string]string{
	"StartOfTurn":    TurnStartOfTurn,
	"Update":         TurnUpdate,
	"EagerEndOfTurn": TurnEagerEndOfTurn,
	"TurnResumed":    TurnResumed,
	"EndOfTurn":      TurnEndOfTurn,
}

func (t *FluxTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *wsutilx.LockedWriter, opts Options, cb TranscriptCallback) {
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
			select {
			case <-ctx.Done():
				t.log.Debug("deepgram flux stt recvLoop context done")
			default:
				t.log.Debug("deepgram flux stt recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			payload, perr := io.ReadAll(rd)
			if perr != nil {
				t.log.Debug("deepgram flux stt close payload read error", "error", perr)
				return
			}
			code, reason := ws.ParseCloseFrameData(payload)
			t.log.Info("deepgram flux stt recv close frame", "code", int(code), "reason", reason)
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("deepgram flux stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("deepgram flux stt read error", "error", err)
			return
		}

		raw := buf.Bytes()
		t.log.Debug("deepgram flux stt recv msg", "raw", string(raw[:min(len(raw), 300)]))

		var msg fluxMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.log.Debug("deepgram flux stt parse error", "error", err)
			continue
		}

		switch msg.Type {
		case "TurnInfo":
			t.dispatchTurn(msg, opts, cb)
		case "Connected":
			t.log.Info("deepgram flux stt session started", "request_id", msg.RequestID)
		case "Error", "ConfigureFailure":
			// Deepgram closes the socket itself on a fatal error, so keep
			// reading — ending here would drop a still-live session.
			t.log.Error("deepgram flux stt error frame", "type", msg.Type, "code", msg.Code, "description", msg.Description)
		}
	}
}

func (t *FluxTranscriber) dispatchTurn(msg fluxMessage, opts Options, cb TranscriptCallback) {
	event, ok := fluxTurnEvents[msg.Event]
	if !ok {
		t.log.Debug("deepgram flux stt unknown turn event", "event", msg.Event)
		return
	}

	// Update fires ~4x/second; without partials the caller only wants the
	// turn boundaries.
	if event != TurnUpdate || opts.Partial {
		emitTurn(opts, TurnEvent{
			Event:               event,
			TurnIndex:           msg.TurnIndex,
			Transcript:          msg.Transcript,
			EndOfTurnConfidence: msg.EndOfTurnConfidence,
			AudioWindowStart:    msg.AudioWindowStart,
			AudioWindowEnd:      msg.AudioWindowEnd,
			Words:               fluxWords(msg.Words),
			Languages:           msg.Languages,
		})
	}

	if msg.Transcript == "" {
		return
	}

	switch event {
	case TurnEndOfTurn:
		emitTranscript(opts, cb, TranscriptEvent{Text: msg.Transcript, IsFinal: true, SpeechFinal: true})
	case TurnResumed:
		// The turn is still open and its text is about to change; nothing
		// stable to report on the transcript channel.
	default:
		// EagerEndOfTurn included: it is revoked by TurnResumed, so it must
		// never reach a caller that accumulates on is_final.
		if opts.Partial {
			emitTranscript(opts, cb, TranscriptEvent{Text: msg.Transcript})
		}
	}
}

func fluxWords(in []fluxWord) []TurnWord {
	if len(in) == 0 {
		return nil
	}
	out := make([]TurnWord, len(in))
	for i, w := range in {
		out[i] = TurnWord{Word: w.Word, Confidence: w.Confidence, Start: w.Start, End: w.End}
	}
	return out
}
