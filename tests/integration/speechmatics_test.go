//go:build integration

package integration

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/stt"
)

const speechmaticsPhrase = "How do I reset my password?"

func speechmaticsKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("SPEECHMATICS_API_KEY")
	if key == "" {
		t.Skip("SPEECHMATICS_API_KEY not set, skipping Speechmatics integration test")
	}
	return key
}

// speechmaticsSpeech borrows Deepgram TTS for a real 16kHz PCM fixture, so the
// recognizer is fed speech rather than silence.
func speechmaticsSpeech(t *testing.T) []byte {
	t.Helper()
	dg := os.Getenv("DEEPGRAM_API_KEY")
	if dg == "" {
		t.Skip("DEEPGRAM_API_KEY not set (used only to synthesize the fixture), skipping")
	}
	return synthesizeDeepgramSpeech(t, dg, speechmaticsPhrase)
}

// TestSpeechmaticsSTT_Connectivity is the cheap check: the handshake is
// accepted, the session runs, and EndOfStream tears it down cleanly.
func TestSpeechmaticsSTT_Connectivity(t *testing.T) {
	key := speechmaticsKey(t)

	transcriber := stt.NewSpeechmatics(os.Getenv("SPEECHMATICS_URL"), slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ~2 seconds of silence at 16kHz mono PCM16.
	reader := strings.NewReader(strings.Repeat("\x00", 640*100))
	if err := transcriber.Start(ctx, reader, key, stt.Options{}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if transcriber.Running() {
		t.Error("Running() is still true after Start returned")
	}
}

// TestSpeechmatics_TurnLifecycle drives real speech through the realtime API
// and checks that the end-of-utterance detector closes exactly one turn.
func TestSpeechmatics_TurnLifecycle(t *testing.T) {
	key := speechmaticsKey(t)
	audio := speechmaticsSpeech(t)

	var rec turnRecorder
	opts := rec.attach(stt.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := stt.NewSpeechmatics(os.Getenv("SPEECHMATICS_URL"), slog.Default()).
		Start(ctx, newPacedReader(audio, 4*time.Second), key, opts, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	turns, transcripts := rec.snapshot()
	t.Logf("speechmatics produced %d turn events and %d transcripts", len(turns), len(transcripts))
	for _, tv := range turns {
		t.Logf("  %-14s turn=%d words=%d %q", tv.Event, tv.TurnIndex, len(tv.Words), tv.Transcript)
	}

	var endOfTurn *stt.TurnEvent
	for i, tv := range turns {
		if tv.Event != stt.TurnEndOfTurn {
			t.Errorf("unexpected turn event %q; Speechmatics only reports end_of_turn", tv.Event)
			continue
		}
		if endOfTurn == nil {
			endOfTurn = &turns[i]
		}
	}
	if endOfTurn == nil {
		t.Fatal("no end_of_turn event; the utterance never closed")
	}
	if endOfTurn.Transcript == "" {
		t.Error("end_of_turn carried an empty transcript")
	}
	if len(endOfTurn.Words) == 0 {
		t.Error("end_of_turn carried no word timings")
	}
	if endOfTurn.AudioWindowEnd <= endOfTurn.AudioWindowStart {
		t.Errorf("end_of_turn window = [%v, %v], want a positive span",
			endOfTurn.AudioWindowStart, endOfTurn.AudioWindowEnd)
	}

	// partial is false, so nothing interim may reach a caller who did not ask.
	if len(transcripts) == 0 {
		t.Fatal("no transcripts at all")
	}
	for _, tr := range transcripts {
		if !tr.IsFinal {
			t.Errorf("interim transcript %q leaked despite partial=false", tr.Text)
		}
		// The final always precedes EndOfUtterance, so it cannot know the
		// speaker stopped; stt.turn is the signal for that.
		if tr.SpeechFinal {
			t.Errorf("transcript %q claims speech_final", tr.Text)
		}
	}

	joined := joinFinals(transcripts)
	if !strings.Contains(strings.ToLower(joined), "password") {
		t.Errorf("transcript %q does not contain the spoken phrase", joined)
	}
}

// TestSpeechmatics_ForceEndOfUtterance is the /stt/finalize path: the flush
// closes a turn mid-speech, well before the silence trigger would.
func TestSpeechmatics_ForceEndOfUtterance(t *testing.T) {
	key := speechmaticsKey(t)
	audio := speechmaticsSpeech(t)

	transcriber := stt.NewSpeechmatics(os.Getenv("SPEECHMATICS_URL"), slog.Default())
	finalizer, ok := stt.Provider(transcriber).(stt.Finalizer)
	if !ok {
		t.Fatal("*SpeechmaticsTranscriber does not implement stt.Finalizer")
	}

	var rec turnRecorder
	// A slow natural trigger, so a turn that closes early can only be the
	// forced one.
	silenceTrigger := 2000
	opts := rec.attach(stt.Options{UtteranceEndMs: &silenceTrigger})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- transcriber.Start(ctx, newPacedReader(audio, 5*time.Second), key, opts, nil)
	}()

	// Long enough to connect and stream the first second of speech, short
	// enough that the phrase is still being spoken.
	time.Sleep(2 * time.Second)
	if err := finalizer.Finalize(ctx); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Start did not return")
	}

	turns, transcripts := rec.snapshot()
	t.Logf("speechmatics produced %d turn events and %d transcripts", len(turns), len(transcripts))
	for _, tv := range turns {
		t.Logf("  %-14s turn=%d %q", tv.Event, tv.TurnIndex, tv.Transcript)
	}

	// The forced flush plus the natural end of the tail: two closed turns,
	// where an unforced run of the same audio closes only one.
	if len(turns) < 2 {
		t.Errorf("got %d turn events, want at least 2 (the forced flush and the natural one)", len(turns))
	}
	if joined := joinFinals(transcripts); joined == "" {
		t.Error("no final transcript survived the forced flush")
	}
}

func joinFinals(transcripts []stt.TranscriptEvent) string {
	var b bytes.Buffer
	for _, tr := range transcripts {
		if !tr.IsFinal {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(tr.Text)
	}
	return b.String()
}
