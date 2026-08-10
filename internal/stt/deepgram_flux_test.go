package stt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// /v2/listen has no Finalize message, so the provider must not advertise one —
// doFinalizeSTTLeg relies on the type assertion failing to answer 501.
func TestFluxDoesNotImplementFinalizer(t *testing.T) {
	if _, ok := Provider(NewDeepgramFlux(slog.Default())).(Finalizer); ok {
		t.Error("*FluxTranscriber implements Finalizer; Deepgram Flux has no mid-stream flush")
	}
}

func TestBuildFluxURL(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "defaults",
			opts: Options{},
			want: "wss://api.deepgram.com/v2/listen?encoding=linear16&sample_rate=16000&model=flux-general-en",
		},
		{
			name: "eager_and_thresholds",
			opts: Options{EagerEOTThreshold: f64p(0.4), EOTThreshold: f64p(0.75), EOTTimeoutMs: intp(4000)},
			want: "wss://api.deepgram.com/v2/listen?encoding=linear16&sample_rate=16000&model=flux-general-en&eager_eot_threshold=0.4&eot_threshold=0.75&eot_timeout_ms=4000",
		},
		{
			name: "multi_with_language_hints",
			opts: Options{Model: "flux-general-multi", LanguageHints: []string{"en", "es"}},
			want: "wss://api.deepgram.com/v2/listen?encoding=linear16&sample_rate=16000&model=flux-general-multi&language_hint=en&language_hint=es",
		},
		{
			name: "keyterms_are_escaped",
			opts: Options{Keyterms: []string{"co pilot"}},
			want: "wss://api.deepgram.com/v2/listen?encoding=linear16&sample_rate=16000&model=flux-general-en&keyterm=co+pilot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildFluxURL(tc.opts); got != tc.want {
				t.Errorf("buildFluxURL()\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

func turnFrame(event, transcript string, turnIndex int) string {
	return fmt.Sprintf(`{"type":"TurnInfo","event":%q,"turn_index":%d,"transcript":%q,`+
		`"audio_window_start":0.0,"audio_window_end":1.5,"end_of_turn_confidence":0.85,`+
		`"words":[{"word":"hello","confidence":0.99,"start":0.1,"end":0.4}]}`,
		event, turnIndex, transcript)
}

func runFluxRecv(t *testing.T, opts Options, frames []string) *collector {
	t.Helper()
	conn := sttScriptServer(t, frames)
	var c collector
	tr := NewDeepgramFlux(slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), c.sinks(opts), nil)
	return &c
}

// One eager/resume cycle followed by a real end of turn — the sequence the
// preflight loop is built around.
func TestFluxDispatch_TurnSequence(t *testing.T) {
	frames := []string{
		`{"type":"Connected","request_id":"req-1","sequence_id":0}`,
		turnFrame("StartOfTurn", "how", 0),
		turnFrame("Update", "how do", 0),
		turnFrame("EagerEndOfTurn", "how do I", 0),
		turnFrame("TurnResumed", "how do I", 0),
		turnFrame("Update", "how do I reset", 0),
		turnFrame("EagerEndOfTurn", "how do I reset it", 0),
		turnFrame("EndOfTurn", "how do I reset it", 0),
	}

	t.Run("with_partials", func(t *testing.T) {
		c := runFluxRecv(t, Options{Partial: true}, frames)

		wantTurns := []string{
			TurnStartOfTurn, TurnUpdate, TurnEagerEndOfTurn,
			TurnResumed, TurnUpdate, TurnEagerEndOfTurn, TurnEndOfTurn,
		}
		if got := c.turnEvents(); !equalStrings(got, wantTurns) {
			t.Fatalf("turn events = %v, want %v", got, wantTurns)
		}

		// TurnResumed carries no stable text, so it yields no transcript.
		if len(c.transcripts) != 6 {
			t.Fatalf("got %d transcripts, want 6: %+v", len(c.transcripts), c.transcripts)
		}
		for i, ev := range c.transcripts[:5] {
			if ev.IsFinal {
				t.Errorf("transcript[%d] (%q) is final; only end_of_turn may be", i, ev.Text)
			}
		}
		last := c.transcripts[5]
		if !last.IsFinal || !last.SpeechFinal || last.Text != "how do I reset it" {
			t.Errorf("last transcript = %+v, want the final speech_final 'how do I reset it'", last)
		}
	})

	t.Run("without_partials", func(t *testing.T) {
		c := runFluxRecv(t, Options{}, frames)

		// update fires ~4x/second; the boundaries an app acts on do not.
		wantTurns := []string{
			TurnStartOfTurn, TurnEagerEndOfTurn, TurnResumed, TurnEagerEndOfTurn, TurnEndOfTurn,
		}
		if got := c.turnEvents(); !equalStrings(got, wantTurns) {
			t.Fatalf("turn events = %v, want %v", got, wantTurns)
		}
		if len(c.transcripts) != 1 {
			t.Fatalf("got %d transcripts, want exactly one per turn: %+v", len(c.transcripts), c.transcripts)
		}
		if !c.transcripts[0].IsFinal || !c.transcripts[0].SpeechFinal {
			t.Errorf("transcript = %+v, want final and speech_final", c.transcripts[0])
		}
	})
}

// EagerEndOfTurn is revoked by TurnResumed. An app that accumulates on
// is_final would corrupt its transcript if it ever arrived as a final.
func TestFluxDispatch_EagerIsNeverFinal(t *testing.T) {
	c := runFluxRecv(t, Options{Partial: true}, []string{turnFrame("EagerEndOfTurn", "maybe done", 0)})
	if len(c.transcripts) != 1 {
		t.Fatalf("got %d transcripts, want 1: %+v", len(c.transcripts), c.transcripts)
	}
	if c.transcripts[0].IsFinal || c.transcripts[0].SpeechFinal {
		t.Errorf("eager transcript = %+v, want neither is_final nor speech_final", c.transcripts[0])
	}
}

func TestFluxDispatch_CarriesTurnDetail(t *testing.T) {
	c := runFluxRecv(t, Options{}, []string{turnFrame("EndOfTurn", "hello", 3)})
	if len(c.turns) != 1 {
		t.Fatalf("turns = %+v, want 1", c.turns)
	}
	tv := c.turns[0]
	if tv.TurnIndex != 3 || tv.EndOfTurnConfidence != 0.85 || tv.AudioWindowEnd != 1.5 {
		t.Errorf("turn = %+v, want turn_index 3, confidence 0.85, window end 1.5", tv)
	}
	if len(tv.Words) != 1 || tv.Words[0].Word != "hello" || tv.Words[0].Start != 0.1 {
		t.Errorf("words = %+v, want one 'hello' starting at 0.1", tv.Words)
	}
}

// Deepgram closes the socket itself on a fatal error; bailing out on an error
// frame would drop a session that is still live.
func TestFluxDispatch_ErrorFrameDoesNotStopTheLoop(t *testing.T) {
	c := runFluxRecv(t, Options{}, []string{
		`{"type":"Error","sequence_id":3,"code":"INTERNAL_SERVER_ERROR","description":"boom"}`,
		turnFrame("EndOfTurn", "still here", 0),
	})
	if len(c.transcripts) != 1 || c.transcripts[0].Text != "still here" {
		t.Fatalf("transcripts = %+v, want the turn that followed the error frame", c.transcripts)
	}
}

// fluxSendServer collects the binary frames a sendLoop puts on the wire.
func fluxSendServer(t *testing.T) (net.Conn, chan []byte) {
	t.Helper()
	frames := make(chan []byte, 32)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			data, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if op == ws.OpBinary {
				frames <- data
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
	return conn, frames
}

// chunkedReader hands back at most one 20ms mixer frame per Read, the way the
// room pipe and the resample reader upstream of STT actually behave.
type chunkedReader struct {
	data  []byte
	chunk int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(min(len(p), r.chunk), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// Flux is tuned for 80ms chunks. A plain Read against the upstream pipe would
// yield 640-byte frames, so the sendLoop has to fill its buffer itself.
func TestFluxSendLoop_Sends80msFrames(t *testing.T) {
	conn, frames := fluxSendServer(t)

	// Ten 80ms frames' worth of audio, delivered 20ms at a time.
	src := &chunkedReader{data: bytes.Repeat([]byte{0xAB}, fluxFrameBytes*10), chunk: 640}

	tr := NewDeepgramFlux(slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.sendLoop(ctx, src, wsutilx.NewLockedWriter(conn))

	deadline := time.After(3 * time.Second)
	for i := 0; i < 10; i++ {
		select {
		case f := <-frames:
			if len(f) != fluxFrameBytes {
				t.Fatalf("frame %d is %d bytes, want %d — the 80ms cadence was not honoured", i, len(f), fluxFrameBytes)
			}
		case <-deadline:
			t.Fatalf("timed out after %d frames, want 10", i)
		}
	}
}

// A stream that ends mid-frame must still deliver its tail rather than drop it.
func TestFluxSendLoop_FlushesPartialTail(t *testing.T) {
	conn, frames := fluxSendServer(t)

	src := &chunkedReader{data: bytes.Repeat([]byte{0xCD}, fluxFrameBytes+640), chunk: 640}

	tr := NewDeepgramFlux(slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.sendLoop(ctx, src, wsutilx.NewLockedWriter(conn))

	if f := recvFrame(t, frames); len(f) != fluxFrameBytes {
		t.Fatalf("first frame is %d bytes, want %d", len(f), fluxFrameBytes)
	}
	if f := recvFrame(t, frames); len(f) != 640 {
		t.Fatalf("tail frame is %d bytes, want the remaining 640", len(f))
	}
}

func recvFrame(t *testing.T, ch chan []byte) []byte {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an audio frame")
		return nil
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
