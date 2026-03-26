package mixer

import (
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"
)

func TestComputeRMS(t *testing.T) {
	// Silence
	silence := make([]int16, SamplesPerFrame)
	if rms := computeRMS(silence); rms != 0 {
		t.Errorf("silence RMS = %f, want 0", rms)
	}

	// Constant signal
	constant := make([]int16, SamplesPerFrame)
	for i := range constant {
		constant[i] = 1000
	}
	if rms := computeRMS(constant); math.Abs(rms-1000) > 0.1 {
		t.Errorf("constant RMS = %f, want 1000", rms)
	}

	// Empty
	if rms := computeRMS(nil); rms != 0 {
		t.Errorf("nil RMS = %f, want 0", rms)
	}
}

func TestSpeakingState_Debounce(t *testing.T) {
	s := &speakingState{}

	// Below threshold — no change
	for i := 0; i < 10; i++ {
		if changed := s.update(100); changed {
			t.Fatalf("frame %d: unexpected state change to speaking=%v", i, s.speaking)
		}
	}
	if s.speaking {
		t.Fatal("should not be speaking after silence")
	}

	// Above threshold — need speakingOnFrames consecutive frames
	for i := 0; i < speakingOnFrames-1; i++ {
		if changed := s.update(500); changed {
			t.Fatalf("frame %d: premature speaking start", i)
		}
	}
	if s.speaking {
		t.Fatal("should not be speaking yet (one frame short)")
	}

	// One more above threshold → starts speaking
	if changed := s.update(500); !changed {
		t.Fatal("expected state change to speaking")
	}
	if !s.speaking {
		t.Fatal("should be speaking now")
	}

	// Below threshold — need speakingOffFrames consecutive frames
	for i := 0; i < speakingOffFrames-1; i++ {
		if changed := s.update(100); changed {
			t.Fatalf("frame %d: premature speaking stop", i)
		}
	}
	if !s.speaking {
		t.Fatal("should still be speaking (one frame short of off threshold)")
	}

	// One more below threshold → stops speaking
	if changed := s.update(100); !changed {
		t.Fatal("expected state change to not speaking")
	}
	if s.speaking {
		t.Fatal("should not be speaking now")
	}
}

func TestSpeakingState_InterruptedSilence(t *testing.T) {
	s := &speakingState{}

	// Start speaking
	for i := 0; i < speakingOnFrames; i++ {
		s.update(500)
	}
	if !s.speaking {
		t.Fatal("should be speaking")
	}

	// Almost reach the off threshold, then a voiced frame resets the counter
	for i := 0; i < speakingOffFrames-2; i++ {
		s.update(100)
	}
	if !s.speaking {
		t.Fatal("should still be speaking")
	}

	// Voiced frame resets silent counter
	s.update(500)
	if !s.speaking {
		t.Fatal("should still be speaking after voiced frame")
	}

	// Now need full speakingOffFrames again
	for i := 0; i < speakingOffFrames-1; i++ {
		s.update(100)
	}
	if !s.speaking {
		t.Fatal("should still be speaking (one short)")
	}

	s.update(100)
	if s.speaking {
		t.Fatal("should have stopped speaking")
	}
}

func TestMixer_SpeakingEvents(t *testing.T) {
	log := slog.Default()
	m := New(log)

	var mu sync.Mutex
	var events []SpeakingEvent
	m.OnSpeaking(func(e SpeakingEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	m.Start()
	defer m.Stop()

	// Create a participant that will "speak" (send loud audio)
	speakerReader, speakerFeeder := io.Pipe()
	speakerOut := &discardWriter{}
	m.AddParticipant("speaker1", speakerReader, speakerOut)

	// Generate loud frames (RMS well above threshold)
	loudFrame := makeToneFrame(5000)
	silentFrame := make([]byte, FrameSizeBytes)

	// Feed enough loud frames to trigger speaking start
	// (speakingOnFrames + some margin for mixer tick alignment)
	go func() {
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()

		// Loud frames to start speaking
		for i := 0; i < speakingOnFrames+5; i++ {
			<-ticker.C
			speakerFeeder.Write(loudFrame)
		}

		// Silent frames to stop speaking
		for i := 0; i < speakingOffFrames+5; i++ {
			<-ticker.C
			speakerFeeder.Write(silentFrame)
		}

		speakerFeeder.Close()
	}()

	// Wait for all frames to be processed
	totalFrames := speakingOnFrames + 5 + speakingOffFrames + 5
	time.Sleep(time.Duration((totalFrames+5)*Ptime) * time.Millisecond)

	mu.Lock()
	got := make([]SpeakingEvent, len(events))
	copy(got, events)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events (start+stop), got %d: %+v", len(got), got)
	}

	// First event should be speaking started
	if !got[0].Speaking {
		t.Errorf("event[0]: expected Speaking=true, got false")
	}
	if got[0].ParticipantID != "speaker1" {
		t.Errorf("event[0]: expected ParticipantID=speaker1, got %s", got[0].ParticipantID)
	}

	// Last event should be speaking stopped
	last := got[len(got)-1]
	if last.Speaking {
		t.Errorf("last event: expected Speaking=false, got true")
	}
}

func TestMixer_SpeakingStopOnRemove(t *testing.T) {
	log := slog.Default()
	m := New(log)

	var mu sync.Mutex
	var events []SpeakingEvent
	m.OnSpeaking(func(e SpeakingEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	m.Start()
	defer m.Stop()

	speakerReader, speakerFeeder := io.Pipe()
	speakerOut := &discardWriter{}
	m.AddParticipant("speaker2", speakerReader, speakerOut)

	loudFrame := makeToneFrame(5000)

	// Feed loud frames to start speaking
	go func() {
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for i := 0; i < speakingOnFrames+5; i++ {
			<-ticker.C
			speakerFeeder.Write(loudFrame)
		}
	}()

	// Wait for speaking to start
	time.Sleep(time.Duration((speakingOnFrames+8)*Ptime) * time.Millisecond)

	// Remove participant while speaking
	m.RemoveParticipant("speaker2")
	speakerFeeder.Close()

	time.Sleep(50 * time.Millisecond) // let events propagate

	mu.Lock()
	got := make([]SpeakingEvent, len(events))
	copy(got, events)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events (start from mixer + stop from remove), got %d: %+v", len(got), got)
	}

	// Last event should be speaking stopped (from RemoveParticipant)
	last := got[len(got)-1]
	if last.Speaking {
		t.Error("last event should be Speaking=false after remove")
	}
	if last.ParticipantID != "speaker2" {
		t.Errorf("last event ParticipantID = %s, want speaker2", last.ParticipantID)
	}
}

// makeToneFrame creates a 20ms frame of a sine tone at 16kHz with the given amplitude.
func makeToneFrame(amplitude int16) []byte {
	frame := make([]byte, FrameSizeBytes)
	for i := 0; i < SamplesPerFrame; i++ {
		s := int16(float64(amplitude) * math.Sin(2*math.Pi*440*float64(i)/float64(SampleRate)))
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(s))
	}
	return frame
}

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
