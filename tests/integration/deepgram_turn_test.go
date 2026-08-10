//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/stt"
	"github.com/VoiceBlender/voiceblender/internal/tts"
)

const deepgramTurnPhrase = "How do I reset my password?"

// pacedReader hands out audio at wall-clock speed and then a tail of silence,
// so a real-time turn detector sees a plausible utterance followed by a pause
// rather than a whole file arriving at once.
type pacedReader struct {
	audio   []byte
	silence int // bytes of trailing silence
	pos     int
	chunk   int
	last    time.Time
}

func newPacedReader(audio []byte, silence time.Duration) *pacedReader {
	return &pacedReader{
		audio:   audio,
		silence: int(silence.Seconds() * 32000), // 16kHz mono PCM16
		chunk:   640,                            // 20ms, matching the mixer frame the real path delivers
	}
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if !r.last.IsZero() {
		if wait := 20*time.Millisecond - time.Since(r.last); wait > 0 {
			time.Sleep(wait)
		}
	}
	r.last = time.Now()

	n := min(len(p), r.chunk)
	if r.pos < len(r.audio) {
		n = min(n, len(r.audio)-r.pos)
		copy(p, r.audio[r.pos:r.pos+n])
		r.pos += n
		return n, nil
	}
	if r.silence <= 0 {
		return 0, io.EOF
	}
	n = min(n, r.silence)
	for i := range p[:n] {
		p[i] = 0
	}
	r.silence -= n
	return n, nil
}

// synthesizeDeepgramSpeech produces real 16kHz PCM speech to feed the
// recognizers, so both tests need only DEEPGRAM_API_KEY.
func synthesizeDeepgramSpeech(t *testing.T, key, text string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := tts.NewDeepgram(key, slog.Default()).Synthesize(ctx, text, tts.Options{Voice: "aura-2-asteria-en"})
	if err != nil {
		t.Fatalf("synthesize fixture speech: %v", err)
	}
	defer res.Audio.Close()

	audio, err := io.ReadAll(res.Audio)
	if err != nil {
		t.Fatalf("read fixture speech: %v", err)
	}
	if len(audio) == 0 {
		t.Fatal("fixture speech is empty")
	}
	return audio
}

type turnRecorder struct {
	mu          sync.Mutex
	turns       []stt.TurnEvent
	transcripts []stt.TranscriptEvent
}

func (r *turnRecorder) attach(opts stt.Options) stt.Options {
	opts.OnTurn = func(ev stt.TurnEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.turns = append(r.turns, ev)
	}
	opts.OnTranscript = func(ev stt.TranscriptEvent) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.transcripts = append(r.transcripts, ev)
	}
	return opts
}

func (r *turnRecorder) snapshot() ([]stt.TurnEvent, []stt.TranscriptEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]stt.TurnEvent(nil), r.turns...), append([]stt.TranscriptEvent(nil), r.transcripts...)
}

// TestDeepgramFlux_TurnLifecycle drives real speech through /v2/listen and
// checks the turn state machine the preflight loop depends on.
func TestDeepgramFlux_TurnLifecycle(t *testing.T) {
	key := os.Getenv("DEEPGRAM_API_KEY")
	if key == "" {
		t.Skip("DEEPGRAM_API_KEY not set, skipping Deepgram Flux integration test")
	}

	audio := synthesizeDeepgramSpeech(t, key, deepgramTurnPhrase)

	var rec turnRecorder
	eager := 0.4
	opts := rec.attach(stt.Options{EagerEOTThreshold: &eager})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := stt.NewDeepgramFlux(slog.Default()).
		Start(ctx, newPacedReader(audio, 3*time.Second), key, opts, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	turns, transcripts := rec.snapshot()
	t.Logf("flux produced %d turn events and %d transcripts", len(turns), len(transcripts))
	for _, tv := range turns {
		t.Logf("  %-18s turn=%d conf=%.2f %q", tv.Event, tv.TurnIndex, tv.EndOfTurnConfidence, tv.Transcript)
	}

	var lastEager string
	var endOfTurn *stt.TurnEvent
	for i, tv := range turns {
		switch tv.Event {
		case stt.TurnEagerEndOfTurn:
			lastEager = tv.Transcript
		case stt.TurnEndOfTurn:
			endOfTurn = &turns[i]
		}
	}

	if endOfTurn == nil {
		t.Fatal("no end_of_turn event; the turn never closed")
	}
	if endOfTurn.Transcript == "" {
		t.Error("end_of_turn carried an empty transcript")
	}
	// Deepgram guarantees this, and the preflight loop relies on it: the reply
	// drafted on the eager event is the reply for the turn that just ended.
	if lastEager != "" && lastEager != endOfTurn.Transcript {
		t.Errorf("end_of_turn transcript %q differs from the preceding eager_end_of_turn %q",
			endOfTurn.Transcript, lastEager)
	}

	// Without partials, exactly one transcript per turn reaches an app that
	// only reads stt.text.
	finals := 0
	for _, tr := range transcripts {
		if tr.IsFinal {
			finals++
			if !tr.SpeechFinal {
				t.Errorf("final transcript %q is not marked speech_final", tr.Text)
			}
		}
	}
	if finals != 1 {
		t.Errorf("got %d final transcripts, want exactly one for a single turn", finals)
	}
}

// TestDeepgramV1_SpeechFinalAndUtteranceEnd is the v1 fallback mode: an app
// that cannot use Flux still gets a "caller stopped talking" signal.
func TestDeepgramV1_SpeechFinalAndUtteranceEnd(t *testing.T) {
	key := os.Getenv("DEEPGRAM_API_KEY")
	if key == "" {
		t.Skip("DEEPGRAM_API_KEY not set, skipping Deepgram v1 turn integration test")
	}

	audio := synthesizeDeepgramSpeech(t, key, deepgramTurnPhrase)

	var rec turnRecorder
	utteranceEnd := 1000
	// partial stays false: utterance_end_ms forces interim results on the
	// wire, and they must not leak to a caller who did not ask for them.
	opts := rec.attach(stt.Options{UtteranceEndMs: &utteranceEnd})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := stt.NewDeepgram(slog.Default()).
		Start(ctx, newPacedReader(audio, 3*time.Second), key, opts, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	turns, transcripts := rec.snapshot()
	t.Logf("deepgram v1 produced %d turn events and %d transcripts", len(turns), len(transcripts))

	sawUtteranceEnd := false
	for _, tv := range turns {
		if tv.Event == stt.TurnUtteranceEnd {
			sawUtteranceEnd = true
		}
	}
	if !sawUtteranceEnd {
		t.Error("no utterance_end turn event despite utterance_end_ms being set")
	}

	sawSpeechFinal := false
	for _, tr := range transcripts {
		if !tr.IsFinal {
			t.Errorf("interim transcript %q leaked despite partial=false", tr.Text)
		}
		if tr.SpeechFinal {
			sawSpeechFinal = true
		}
	}
	if len(transcripts) == 0 {
		t.Fatal("no transcripts at all")
	}
	if !sawSpeechFinal {
		t.Error("no transcript carried speech_final; the speaker-stopped signal never surfaced")
	}
}
