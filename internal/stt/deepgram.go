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
	"net/url"
	"strings"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

const (
	deepgramWSURL  = "wss://api.deepgram.com/v1/listen"
	dgFrameBytes   = 640 // 320 samples × 2 bytes (16-bit PCM at 16kHz, 20ms)
	dgDefaultModel = "nova-3"
)

// dgFinalizeFrame flushes Deepgram's server-side buffer and emits a final
// transcript while leaving the session open.
var dgFinalizeFrame = []byte(`{"type":"Finalize"}`)

// DeepgramTranscriber streams audio to Deepgram real-time STT over WebSocket.
type DeepgramTranscriber struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	log     *slog.Logger
	// lw is the live socket writer, guarded by mu. Non-nil only between a
	// successful dial and Start returning, so Finalize can reach a socket
	// that otherwise lives entirely inside Start.
	lw *wsutilx.LockedWriter
}

func NewDeepgram(log *slog.Logger) *DeepgramTranscriber {
	return &DeepgramTranscriber{log: log}
}

func buildDeepgramV1URL(opts Options) string {
	lang := opts.Language
	if lang == "" {
		lang = "en"
	}
	model := opts.Model
	if model == "" {
		model = dgDefaultModel
	}

	var b strings.Builder
	b.WriteString(deepgramWSURL)
	b.WriteString("?encoding=linear16&sample_rate=16000&channels=1&model=")
	b.WriteString(url.QueryEscape(model))
	b.WriteString("&language=")
	b.WriteString(url.QueryEscape(lang))

	// Deepgram only emits UtteranceEnd when it is also sending interim
	// results, so utterance_end_ms forces them on the wire; recvLoop then
	// drops the interims locally unless the caller asked for partials.
	if opts.Partial || opts.UtteranceEndMs != nil {
		b.WriteString("&interim_results=true")
	}
	if opts.Endpointing != nil {
		fmt.Fprintf(&b, "&endpointing=%d", *opts.Endpointing)
	}
	if opts.UtteranceEndMs != nil {
		fmt.Fprintf(&b, "&utterance_end_ms=%d", *opts.UtteranceEndMs)
	}
	for _, kt := range opts.Keyterms {
		b.WriteString("&keyterm=")
		b.WriteString(url.QueryEscape(kt))
	}
	return b.String()
}

func (t *DeepgramTranscriber) Start(ctx context.Context, reader io.Reader, apiKey string, opts Options, cb TranscriptCallback) error {
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
			"Authorization": []string{"token " + apiKey},
		},
	}

	wsURL := buildDeepgramV1URL(opts)
	t.log.Info("deepgram stt dialing", "url", wsURL)
	conn, _, _, err := dialer.Dial(ctx, wsURL)
	if err != nil {
		t.log.Error("deepgram stt dial failed", "error", err)
		return err
	}
	t.log.Info("deepgram stt websocket connected")
	defer conn.Close()

	lw := wsutilx.NewLockedWriter(conn)
	// Registered after `defer conn.Close()` so LIFO clears the field before
	// the socket goes away; a Finalize that already copied the writer out can
	// still race the close, which is why its write error is surfaced.
	t.setWriter(lw)
	defer t.clearWriter()

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

func (t *DeepgramTranscriber) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *DeepgramTranscriber) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *DeepgramTranscriber) setWriter(lw *wsutilx.LockedWriter) {
	t.mu.Lock()
	t.lw = lw
	t.mu.Unlock()
}

func (t *DeepgramTranscriber) clearWriter() {
	t.mu.Lock()
	t.lw = nil
	t.mu.Unlock()
}

// Finalize asks Deepgram for a final transcript covering the audio buffered so
// far and keeps the socket OPEN — unlike the CloseStream frame sendLoop writes
// at teardown, which flushes AND ends the session. Fire-and-forget: the flushed
// final arrives through the existing recvLoop callback, and a silent segment
// yields no transcript at all because empty transcripts are dropped before the
// callback.
func (t *DeepgramTranscriber) Finalize(ctx context.Context) error {
	t.mu.Lock()
	lw := t.lw
	t.mu.Unlock()
	if lw == nil {
		return errors.New("deepgram stt session not connected")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return lw.WriteText(dgFinalizeFrame)
}

func (t *DeepgramTranscriber) sendLoop(ctx context.Context, reader io.Reader, lw *wsutilx.LockedWriter) {
	buf := make([]byte, dgFrameBytes)
	var sendCount int
	for {
		select {
		case <-ctx.Done():
			// Send CloseStream message to finalize.
			close := []byte(`{"type": "CloseStream"}`)
			_ = lw.WriteText(close)
			t.log.Debug("deepgram stt sendLoop context done", "sent_frames", sendCount)
			return
		default:
		}

		n, err := reader.Read(buf)
		if err != nil {
			// Send CloseStream on reader close too.
			close := []byte(`{"type": "CloseStream"}`)
			_ = lw.WriteText(close)
			t.log.Info("deepgram stt sendLoop reader closed", "error", err, "sent_frames", sendCount)
			return
		}
		if n == 0 {
			continue
		}

		if sendCount == 0 {
			t.log.Info("deepgram stt sendLoop first audio read", "bytes", n)
		}

		// Deepgram accepts raw binary PCM frames.
		if err := lw.WriteBinary(buf[:n]); err != nil {
			t.log.Debug("deepgram stt send error", "error", err, "sent_frames", sendCount)
			return
		}
		sendCount++
		if sendCount%250 == 0 {
			t.log.Debug("deepgram stt sendLoop progress", "sent_frames", sendCount)
		}
	}
}

// dgEnvelope discriminates the message type before the full payload is
// decoded. Two-stage parsing is required because "channel" is an object on
// Results and an array of indexes on UtteranceEnd — one struct cannot hold both.
type dgEnvelope struct {
	Type string `json:"type"`
}

// dgResult represents the Deepgram streaming response.
type dgResult struct {
	Type    string `json:"type"`
	Channel struct {
		Alternatives []struct {
			Transcript string `json:"transcript"`
		} `json:"alternatives"`
	} `json:"channel"`
	IsFinal     bool `json:"is_final"`
	SpeechFinal bool `json:"speech_final"`
	// Where in the stream this result covers, in seconds from the first audio.
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
}

// dgUtteranceEnd is sent when utterance_end_ms is configured and Deepgram
// detects the speaker has stopped. It carries no transcript.
type dgUtteranceEnd struct {
	LastWordEnd float64 `json:"last_word_end"`
}

func (t *DeepgramTranscriber) recvLoop(ctx context.Context, conn net.Conn, lw *wsutilx.LockedWriter, opts Options, cb TranscriptCallback) {
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
				t.log.Debug("deepgram stt recvLoop context done")
			default:
				t.log.Debug("deepgram stt recv error", "error", err)
			}
			return
		}

		if hdr.OpCode == ws.OpClose {
			payload, perr := io.ReadAll(rd)
			if perr != nil {
				t.log.Debug("deepgram stt close payload read error", "error", perr)
				return
			}
			code, reason := ws.ParseCloseFrameData(payload)
			t.log.Info("deepgram stt recv close frame", "code", int(code), "reason", reason)
			return
		}
		if hdr.OpCode != ws.OpText {
			if err := rd.Discard(); err != nil {
				t.log.Debug("deepgram stt discard error", "error", err)
				return
			}
			continue
		}

		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rd); err != nil {
			t.log.Debug("deepgram stt read error", "error", err)
			return
		}

		raw := buf.Bytes()
		t.log.Debug("deepgram stt recv msg", "raw", string(raw[:min(len(raw), 300)]))

		var env dgEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.log.Debug("deepgram stt parse error", "error", err)
			continue
		}

		switch env.Type {
		case "Results":
			var result dgResult
			if err := json.Unmarshal(raw, &result); err != nil {
				t.log.Debug("deepgram stt results parse error", "error", err)
				continue
			}
			if len(result.Channel.Alternatives) == 0 {
				continue
			}
			text := result.Channel.Alternatives[0].Transcript
			if text == "" {
				continue
			}
			// A speech_final always closes an utterance, so never let one
			// through as an interim even if is_final were somehow unset.
			isFinal := result.IsFinal || result.SpeechFinal
			if !isFinal && !opts.Partial {
				continue
			}
			t.log.Debug("deepgram stt transcript", "text", text, "is_final", isFinal, "speech_final", result.SpeechFinal)
			emitTranscript(opts, cb, TranscriptEvent{
				Text:        text,
				IsFinal:     isFinal,
				SpeechFinal: result.SpeechFinal,
				AudioStart:  result.Start,
				AudioEnd:    result.Start + result.Duration,
			})

		case "UtteranceEnd":
			var ue dgUtteranceEnd
			if err := json.Unmarshal(raw, &ue); err != nil {
				t.log.Debug("deepgram stt utterance end parse error", "error", err)
				continue
			}
			t.log.Debug("deepgram stt utterance end", "last_word_end", ue.LastWordEnd)
			emitTurn(opts, TurnEvent{Event: TurnUtteranceEnd, LastWordEnd: ue.LastWordEnd})
		}
	}
}
