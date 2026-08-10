package stt

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// sttScriptServer spins a WebSocket server that writes the given text frames to
// the client and then closes, so a recvLoop under test terminates on its own.
func sttScriptServer(t *testing.T, frames []string) net.Conn {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		for _, f := range frames {
			if err := wsutil.WriteServerText(conn, []byte(f)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, _, err := ws.Dialer{}.Dial(context.Background(), wsURL)
	if err != nil {
		t.Fatalf("dial test websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// collector accumulates whatever a provider emits, so a test can assert on the
// whole sequence rather than one callback at a time.
type collector struct {
	mu          sync.Mutex
	transcripts []TranscriptEvent
	turns       []TurnEvent
	legacy      []TranscriptEvent // via the positional TranscriptCallback
}

func (c *collector) sinks(opts Options) Options {
	opts.OnTranscript = func(ev TranscriptEvent) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.transcripts = append(c.transcripts, ev)
	}
	opts.OnTurn = func(ev TurnEvent) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.turns = append(c.turns, ev)
	}
	return opts
}

func (c *collector) cb() TranscriptCallback {
	return func(text string, isFinal bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.legacy = append(c.legacy, TranscriptEvent{Text: text, IsFinal: isFinal})
	}
}

func (c *collector) turnEvents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.turns))
	for i, tv := range c.turns {
		out[i] = tv.Event
	}
	return out
}

// OnTranscript supersedes the positional callback rather than doubling up: a
// caller that wants speech_final must never also get the lossy form.
func TestEmitTranscript_SinkSelection(t *testing.T) {
	t.Run("detail_callback_wins", func(t *testing.T) {
		var c collector
		emitTranscript(c.sinks(Options{}), c.cb(), TranscriptEvent{Text: "hi", IsFinal: true, SpeechFinal: true})
		if len(c.transcripts) != 1 || !c.transcripts[0].SpeechFinal {
			t.Fatalf("OnTranscript got %+v, want one event with SpeechFinal", c.transcripts)
		}
		if len(c.legacy) != 0 {
			t.Errorf("positional callback also fired (%+v); it must not double-report", c.legacy)
		}
	})

	t.Run("falls_back_to_positional", func(t *testing.T) {
		var c collector
		emitTranscript(Options{}, c.cb(), TranscriptEvent{Text: "hi", IsFinal: true})
		if len(c.legacy) != 1 || c.legacy[0].Text != "hi" || !c.legacy[0].IsFinal {
			t.Fatalf("positional callback got %+v, want one final 'hi'", c.legacy)
		}
	})

	t.Run("tolerates_no_sink", func(t *testing.T) {
		emitTranscript(Options{}, nil, TranscriptEvent{Text: "hi"})
		emitTurn(Options{}, TurnEvent{Event: TurnEndOfTurn})
	})
}

func intp(v int) *int         { return &v }
func f64p(v float64) *float64 { return &v }

func TestBuildDeepgramV1URL(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			// Byte-identical to what shipped before the tuning params existed:
			// an unchanged request must produce an unchanged session.
			name: "defaults",
			opts: Options{},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en",
		},
		{
			name: "partial_and_language",
			opts: Options{Language: "es", Partial: true},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=es&interim_results=true",
		},
		{
			name: "model_override",
			opts: Options{Model: "nova-2"},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-2&language=en",
		},
		{
			// Deepgram only emits UtteranceEnd alongside interim results, so
			// they are requested even though the caller did not ask for them.
			name: "utterance_end_forces_interim",
			opts: Options{UtteranceEndMs: intp(1000)},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en&interim_results=true&utterance_end_ms=1000",
		},
		{
			name: "endpointing_zero_is_sent",
			opts: Options{Endpointing: intp(0)},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en&endpointing=0",
		},
		{
			name: "keyterms_are_escaped",
			opts: Options{Keyterms: []string{"VoiceBlender", "co pilot"}},
			want: "wss://api.deepgram.com/v1/listen?encoding=linear16&sample_rate=16000&channels=1&model=nova-3&language=en&keyterm=VoiceBlender&keyterm=co+pilot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildDeepgramV1URL(DefaultDeepgramWSURL, tc.opts); got != tc.want {
				t.Errorf("buildDeepgramV1URL()\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func runDeepgramRecv(t *testing.T, opts Options, frames []string) *collector {
	t.Helper()
	conn := sttScriptServer(t, frames)
	var c collector
	tr := &DeepgramTranscriber{log: slog.Default()}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), c.sinks(opts), nil)
	return &c
}

func TestDeepgramDispatch_SpeechFinalAndUtteranceEnd(t *testing.T) {
	const interim = `{"type":"Results","is_final":false,"speech_final":false,"channel":{"alternatives":[{"transcript":"hello th"}]}}`
	const segment = `{"type":"Results","is_final":true,"speech_final":false,"channel":{"alternatives":[{"transcript":"hello there"}]}}`
	const stopped = `{"type":"Results","is_final":true,"speech_final":true,"channel":{"alternatives":[{"transcript":"how are you"}]}}`
	// channel is an array here, not the object Results uses — the parser must
	// not choke on the shape change.
	const utterEnd = `{"type":"UtteranceEnd","channel":[0,1],"last_word_end":2.395}`

	t.Run("distinguishes_segment_final_from_speech_final", func(t *testing.T) {
		c := runDeepgramRecv(t, Options{Partial: true}, []string{interim, segment, stopped})
		if len(c.transcripts) != 3 {
			t.Fatalf("got %d transcripts, want 3: %+v", len(c.transcripts), c.transcripts)
		}
		want := []TranscriptEvent{
			{Text: "hello th", IsFinal: false, SpeechFinal: false},
			{Text: "hello there", IsFinal: true, SpeechFinal: false},
			{Text: "how are you", IsFinal: true, SpeechFinal: true},
		}
		for i, w := range want {
			if c.transcripts[i] != w {
				t.Errorf("transcript[%d] = %+v, want %+v", i, c.transcripts[i], w)
			}
		}
	})

	t.Run("utterance_end_is_a_turn_not_a_transcript", func(t *testing.T) {
		c := runDeepgramRecv(t, Options{UtteranceEndMs: intp(1000)}, []string{utterEnd})
		if len(c.transcripts) != 0 {
			t.Errorf("UtteranceEnd produced transcripts %+v; it carries no text", c.transcripts)
		}
		if len(c.turns) != 1 || c.turns[0].Event != TurnUtteranceEnd {
			t.Fatalf("turns = %+v, want one utterance_end", c.turns)
		}
		if c.turns[0].LastWordEnd != 2.395 {
			t.Errorf("LastWordEnd = %v, want 2.395", c.turns[0].LastWordEnd)
		}
	})

	// utterance_end_ms turns interim results on at the provider. An app that
	// did not ask for partials must not start receiving them.
	t.Run("forced_interims_are_suppressed_locally", func(t *testing.T) {
		c := runDeepgramRecv(t, Options{UtteranceEndMs: intp(1000)}, []string{interim, segment, utterEnd})
		if len(c.transcripts) != 1 {
			t.Fatalf("got %d transcripts, want only the final one: %+v", len(c.transcripts), c.transcripts)
		}
		if c.transcripts[0].Text != "hello there" || !c.transcripts[0].IsFinal {
			t.Errorf("transcript = %+v, want the final 'hello there'", c.transcripts[0])
		}
	})
}
