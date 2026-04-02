package speaking

import (
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"
)

func TestComputeRMS(t *testing.T) {
	// Silence
	silence := make([]int16, 320)
	if rms := ComputeRMS(silence); rms != 0 {
		t.Errorf("silence RMS = %f, want 0", rms)
	}

	// Constant signal
	constant := make([]int16, 320)
	for i := range constant {
		constant[i] = 1000
	}
	if rms := ComputeRMS(constant); math.Abs(rms-1000) > 0.1 {
		t.Errorf("constant RMS = %f, want 1000", rms)
	}

	// Empty
	if rms := ComputeRMS(nil); rms != 0 {
		t.Errorf("nil RMS = %f, want 0", rms)
	}
}

func TestState_Debounce(t *testing.T) {
	s := &state{}

	// Below threshold — no change
	for i := 0; i < 10; i++ {
		if changed := s.update(100); changed {
			t.Fatalf("frame %d: unexpected state change to speaking=%v", i, s.speaking)
		}
	}
	if s.speaking {
		t.Fatal("should not be speaking after silence")
	}

	// Above threshold — need OnFrames consecutive frames
	for i := 0; i < OnFrames-1; i++ {
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

	// Below threshold — need OffFrames consecutive frames
	for i := 0; i < OffFrames-1; i++ {
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

func TestState_InterruptedSilence(t *testing.T) {
	s := &state{}

	// Start speaking
	for i := 0; i < OnFrames; i++ {
		s.update(500)
	}
	if !s.speaking {
		t.Fatal("should be speaking")
	}

	// Almost reach the off threshold, then a voiced frame resets the counter
	for i := 0; i < OffFrames-2; i++ {
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

	// Now need full OffFrames again
	for i := 0; i < OffFrames-1; i++ {
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

// makeToneFrame creates a 20ms frame of a sine tone at the given sample rate
// with the given amplitude, returned as little-endian int16 PCM bytes.
func makeToneFrame(sampleRate int, amplitude int16) []byte {
	samplesPerFrame := sampleRate * ptime / 1000
	frame := make([]byte, samplesPerFrame*2)
	for i := 0; i < samplesPerFrame; i++ {
		s := int16(float64(amplitude) * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(s))
	}
	return frame
}

func TestDetector_SpeakingEvents(t *testing.T) {
	var mu sync.Mutex
	var evts []Event
	muted := false

	det := New("leg-1", 16000, func() bool { return muted }, func(e Event) {
		mu.Lock()
		evts = append(evts, e)
		mu.Unlock()
	})
	det.Start()

	loudFrame := makeToneFrame(16000, 5000)
	silentFrame := make([]byte, 640) // 320 samples × 2 bytes

	// Feed loud frames to start speaking
	for i := 0; i < OnFrames+5; i++ {
		det.Write(loudFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	// Feed silent frames to stop speaking
	for i := 0; i < OffFrames+5; i++ {
		det.Write(silentFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	det.Stop()

	mu.Lock()
	got := make([]Event, len(evts))
	copy(got, evts)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events (start+stop), got %d: %+v", len(got), got)
	}

	// First event should be speaking started
	if !got[0].Speaking {
		t.Errorf("event[0]: expected Speaking=true, got false")
	}
	if got[0].LegID != "leg-1" {
		t.Errorf("event[0]: expected LegID=leg-1, got %s", got[0].LegID)
	}

	// Last event should be speaking stopped
	last := got[len(got)-1]
	if last.Speaking {
		t.Errorf("last event: expected Speaking=false, got true")
	}
}

func TestDetector_StopEmitsSpeakingStopped(t *testing.T) {
	var mu sync.Mutex
	var evts []Event

	det := New("leg-2", 16000, func() bool { return false }, func(e Event) {
		mu.Lock()
		evts = append(evts, e)
		mu.Unlock()
	})
	det.Start()

	loudFrame := makeToneFrame(16000, 5000)

	// Feed loud frames to start speaking
	for i := 0; i < OnFrames+5; i++ {
		det.Write(loudFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	// Stop while speaking — should emit speaking stopped
	det.Stop()

	mu.Lock()
	got := make([]Event, len(evts))
	copy(got, evts)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(got), got)
	}

	last := got[len(got)-1]
	if last.Speaking {
		t.Error("last event should be Speaking=false after Stop()")
	}
}

func TestDetector_MuteSuppressesSpeaking(t *testing.T) {
	var mu sync.Mutex
	var evts []Event
	muted := false

	det := New("leg-3", 16000, func() bool { return muted }, func(e Event) {
		mu.Lock()
		evts = append(evts, e)
		mu.Unlock()
	})
	det.Start()

	loudFrame := makeToneFrame(16000, 5000)

	// Feed loud frames to start speaking
	for i := 0; i < OnFrames+5; i++ {
		det.Write(loudFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	// Mute the leg — should emit speaking stopped
	muted = true

	// Feed more loud frames while muted — should not emit speaking started
	for i := 0; i < OnFrames+5; i++ {
		det.Write(loudFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	det.Stop()

	mu.Lock()
	got := make([]Event, len(evts))
	copy(got, evts)
	mu.Unlock()

	// Should have: speaking started, speaking stopped (from mute)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d: %+v", len(got), got)
	}

	if !got[0].Speaking {
		t.Error("event[0] should be Speaking=true")
	}
	if got[1].Speaking {
		t.Error("event[1] should be Speaking=false (muted while speaking)")
	}

	// No additional speaking started events from muted loud frames
	for i := 2; i < len(got); i++ {
		if got[i].Speaking {
			t.Errorf("event[%d] unexpected Speaking=true while muted", i)
		}
	}
}

func TestDetector_8kHzSampleRate(t *testing.T) {
	var mu sync.Mutex
	var evts []Event

	det := New("leg-8k", 8000, func() bool { return false }, func(e Event) {
		mu.Lock()
		evts = append(evts, e)
		mu.Unlock()
	})
	det.Start()

	// 8kHz: 160 samples per 20ms frame = 320 bytes
	loudFrame := makeToneFrame(8000, 5000)
	silentFrame := make([]byte, 320)

	for i := 0; i < OnFrames+5; i++ {
		det.Write(loudFrame)
		time.Sleep(ptime * time.Millisecond)
	}
	for i := 0; i < OffFrames+5; i++ {
		det.Write(silentFrame)
		time.Sleep(ptime * time.Millisecond)
	}

	det.Stop()

	mu.Lock()
	got := make([]Event, len(evts))
	copy(got, evts)
	mu.Unlock()

	if len(got) < 2 {
		t.Fatalf("expected at least 2 events at 8kHz, got %d: %+v", len(got), got)
	}
	if !got[0].Speaking {
		t.Error("event[0] should be Speaking=true")
	}
	last := got[len(got)-1]
	if last.Speaking {
		t.Error("last event should be Speaking=false")
	}
}
