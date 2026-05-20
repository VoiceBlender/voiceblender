package mixer

import (
	"encoding/binary"
	"io"
	"log/slog"
	"testing"
	"time"
)

// constantToneReader emits 20ms PCM frames of a single int16 constant value.
type constantToneReader struct {
	value int16
	frame []byte
}

func newConstantToneReader(value int16, samplesPerFrame int) *constantToneReader {
	buf := make([]byte, samplesPerFrame*2)
	for i := 0; i < samplesPerFrame; i++ {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(value))
	}
	return &constantToneReader{value: value, frame: buf}
}

// Read blocks 20ms per call to simulate a real-time PCM source pacing.
func (r *constantToneReader) Read(p []byte) (int, error) {
	time.Sleep(time.Duration(Ptime) * time.Millisecond)
	n := copy(p, r.frame)
	return n, nil
}

// frameAvg computes the mean absolute sample value over a captured byte buffer.
func frameAvg(b []byte) float64 {
	if len(b) < 2 {
		return 0
	}
	var sum int64
	n := len(b) / 2
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(b[i*2:]))
		if v < 0 {
			v = -v
		}
		sum += int64(v)
	}
	return float64(sum) / float64(n)
}

// containsValue returns true if any 16-bit sample in b is approximately `want`.
func containsValue(b []byte, want int16, tolerance int) bool {
	n := len(b) / 2
	for i := 0; i < n; i++ {
		v := int16(binary.LittleEndian.Uint16(b[i*2:]))
		diff := int(v) - int(want)
		if diff < 0 {
			diff = -diff
		}
		if diff <= tolerance {
			return true
		}
	}
	return false
}

// setupThreeParticipants builds a 16kHz mixer with three participants A/B/C,
// each emitting a distinct constant tone. Captures each participant's
// outgoing mix into a captureWriter and returns the writers.
func setupThreeParticipants(t *testing.T) (*Mixer, *captureWriter, *captureWriter, *captureWriter, int16, int16, int16) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(log, DefaultSampleRate)
	m.Start()
	t.Cleanup(m.Stop)

	const (
		toneA int16 = 1000
		toneB int16 = 2000
		toneC int16 = 4000
	)
	capA := &captureWriter{}
	capB := &captureWriter{}
	capC := &captureWriter{}

	m.AddParticipant("A", newConstantToneReader(toneA, m.samplesPerFrame), capA)
	m.AddParticipant("B", newConstantToneReader(toneB, m.samplesPerFrame), capB)
	m.AddParticipant("C", newConstantToneReader(toneC, m.samplesPerFrame), capC)

	return m, capA, capB, capC, toneA, toneB, toneC
}

func TestMixer_Hears_NilIsFullMesh(t *testing.T) {
	m, capA, capB, _, toneA, toneB, toneC := setupThreeParticipants(t)
	_ = m

	// No Hears set anywhere — every participant should hear every other.
	time.Sleep(200 * time.Millisecond)

	bufA := capA.Bytes()
	bufB := capB.Bytes()
	if len(bufA) == 0 || len(bufB) == 0 {
		t.Fatal("expected audio in both participant outputs (full mesh)")
	}
	// A hears B + C = 2000 + 4000 = 6000 (constant).
	if !containsValue(bufA, toneB+toneC, 5) {
		t.Errorf("A's mix should contain B+C=%d in full-mesh mode; avg=%.1f", toneB+toneC, frameAvg(bufA))
	}
	// B hears A + C = 1000 + 4000 = 5000.
	if !containsValue(bufB, toneA+toneC, 5) {
		t.Errorf("B's mix should contain A+C=%d in full-mesh mode; avg=%.1f", toneA+toneC, frameAvg(bufB))
	}
}

func TestMixer_Hears_Whitelist_AsymmetricSupervisor(t *testing.T) {
	m, capA, capB, capC, toneA, toneB, toneC := setupThreeParticipants(t)

	// Supervisor scenario:
	//   A = customer: hears only B (agent).
	//   B = agent:    hears A (customer) + C (supervisor).
	//   C = supervisor: hears A + B but is heard only by B (not A).
	// Translated to per-listener Hears sets (which sources to include in
	// this listener's mix):
	m.SetParticipantHears("A", map[string]struct{}{"B": {}})
	m.SetParticipantHears("B", map[string]struct{}{"A": {}, "C": {}})
	m.SetParticipantHears("C", map[string]struct{}{"A": {}, "B": {}})

	// Drain the warm-up frames produced before the whitelists were applied
	// so the assertions only see post-routing audio.
	time.Sleep(120 * time.Millisecond)
	capA.mu.Lock()
	capA.data = nil
	capA.mu.Unlock()
	capB.mu.Lock()
	capB.data = nil
	capB.mu.Unlock()
	capC.mu.Lock()
	capC.data = nil
	capC.mu.Unlock()

	time.Sleep(300 * time.Millisecond)

	bufA := capA.Bytes()
	bufB := capB.Bytes()
	bufC := capC.Bytes()

	// A must hear only B (value = toneB = 2000); must NOT contain B+C and
	// must NOT contain C alone — those would prove supervisor bleed.
	if !containsValue(bufA, toneB, 5) {
		t.Errorf("A should hear B alone; avg=%.1f", frameAvg(bufA))
	}
	if containsValue(bufA, toneB+toneC, 5) || containsValue(bufA, toneC, 5) {
		t.Errorf("A must not hear supervisor (C); avg=%.1f", frameAvg(bufA))
	}

	// B hears A + C = 1000 + 4000 = 5000.
	if !containsValue(bufB, toneA+toneC, 5) {
		t.Errorf("B should hear A+C=%d; avg=%.1f", toneA+toneC, frameAvg(bufB))
	}

	// C hears A + B = 1000 + 2000 = 3000.
	if !containsValue(bufC, toneA+toneB, 5) {
		t.Errorf("C should hear A+B=%d; avg=%.1f", toneA+toneB, frameAvg(bufC))
	}
}

func TestMixer_Hears_EmptySetIsIsolated(t *testing.T) {
	m, capA, _, _, _, _, _ := setupThreeParticipants(t)
	// A whitelist that allows nothing → A receives silence even though B
	// and C are speaking.
	m.SetParticipantHears("A", map[string]struct{}{})

	time.Sleep(120 * time.Millisecond)
	capA.mu.Lock()
	capA.data = nil
	capA.mu.Unlock()

	time.Sleep(200 * time.Millisecond)

	if avg := frameAvg(capA.Bytes()); avg > 5 {
		t.Errorf("isolated listener should receive silence; got avg=%.1f", avg)
	}
}

func TestMixer_Hears_MuteAndDeafStillRespected(t *testing.T) {
	m, capA, capB, _, _, _, _ := setupThreeParticipants(t)

	// Full mesh — but A is muted (source silence) and B is deaf (no output).
	m.SetParticipantMuted("A", true)
	m.SetParticipantDeaf("B", true)

	time.Sleep(120 * time.Millisecond)
	capA.mu.Lock()
	capA.data = nil
	capA.mu.Unlock()
	capB.mu.Lock()
	capB.data = nil
	capB.mu.Unlock()

	time.Sleep(200 * time.Millisecond)

	// B is deaf → no output reaches it.
	if avg := frameAvg(capB.Bytes()); avg > 5 {
		t.Errorf("deaf participant should receive no audio; got avg=%.1f", avg)
	}
	// A is muted → A's audio doesn't reach anyone, but A still hears others
	// (full mesh). Without mute, A would hear B+C; with B speaking, A's
	// mix must still contain B's tone.
	if len(capA.Bytes()) == 0 {
		t.Errorf("A (muted listener, but not deaf) should still receive others' audio")
	}
}
