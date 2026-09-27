package mixer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
)

type frameLog struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *frameLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.frames = append(c.frames, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *frameLog) last() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames[len(c.frames)-1]
}

func newTickParticipant(m *Mixer, id string) *Participant {
	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       id,
		Writer:   gw,
		incoming: make(chan []byte, 1),
		outgoing: make(chan []byte, 1),
		inject:   make(chan []byte, 1),
		done:     make(chan struct{}),
		guard:    gw,
	}
	m.participants[id] = p
	return p
}

func encodeFrame(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

func randFrame(r *rand.Rand, n int, amp int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(r.Intn(2*amp+1) - amp)
	}
	return s
}

// referenceMix is the naive per-listener sum mixTick must reproduce.
func referenceMix(n int, srcs [][]int16, include func(k int) bool, inject []int16) []byte {
	sum := make([]int32, n)
	for k, f := range srcs {
		if f == nil || !include(k) {
			continue
		}
		for j := range sum {
			sum[j] += int32(f[j])
		}
	}
	out := make([]int16, n)
	for j := range out {
		out[j] = clamp16(sum[j])
		if inject != nil {
			out[j] = clamp16(int32(out[j]) + int32(inject[j]))
		}
	}
	return encodeFrame(out)
}

func TestMixTickMatchesReferenceMix(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(log, 8000)
	n := m.samplesPerFrame
	r := rand.New(rand.NewSource(1))

	const count = 5
	parts := make([]*Participant, count)
	outTaps := make([]*frameLog, count)
	for i := range parts {
		parts[i] = newTickParticipant(m, fmt.Sprintf("p%d", i))
		outTaps[i] = &frameLog{}
		parts[i].outTap = outTaps[i]
	}
	parts[1].Muted.Store(true)
	parts[3].Hears = map[string]struct{}{"p0": {}, "p1": {}}
	roomTap := &frameLog{}
	m.SetTap(roomTap)

	for tick := 0; tick < 5; tick++ {
		srcs := make([][]int16, count)
		for i, p := range parts {
			// Large amplitudes so the sum clips and the clamp path is covered.
			f := randFrame(r, n, 20000)
			p.incoming <- encodeFrame(f)
			if !p.Muted.Load() {
				srcs[i] = f
			}
		}
		inject := randFrame(r, n, 20000)
		parts[2].inject <- encodeFrame(inject)

		m.mixTick()

		all := func(int) bool { return true }
		if got, want := roomTap.last(), referenceMix(n, srcs, all, nil); !bytes.Equal(got, want) {
			t.Fatalf("tick %d: room tap mismatch", tick)
		}
		for i, p := range parts {
			include := func(k int) bool {
				if k == i {
					return false
				}
				if p.Hears == nil {
					return true
				}
				_, ok := p.Hears[parts[k].ID]
				return ok
			}
			var inj []int16
			if i == 2 {
				inj = inject
			}
			want := referenceMix(n, srcs, include, inj)
			if got := outTaps[i].last(); !bytes.Equal(got, want) {
				t.Fatalf("tick %d: listener %s mismatch", tick, p.ID)
			}
			if got := <-p.outgoing; !bytes.Equal(got, want) {
				t.Fatalf("tick %d: listener %s outgoing mismatch", tick, p.ID)
			}
		}
	}
}

// Outgoing frames are handed to other goroutines, so a later tick must not
// overwrite a frame an earlier tick already sent.
func TestMixTickOutgoingFramesAreNotReused(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(log, 8000)
	n := m.samplesPerFrame
	a := newTickParticipant(m, "a")
	b := newTickParticipant(m, "b")
	a.outgoing = make(chan []byte, 2)

	b.incoming <- encodeFrame(randFrame(rand.New(rand.NewSource(1)), n, 1000))
	m.mixTick()
	first := <-a.outgoing
	snapshot := append([]byte(nil), first...)

	b.incoming <- encodeFrame(randFrame(rand.New(rand.NewSource(2)), n, 1000))
	m.mixTick()
	<-a.outgoing
	if !bytes.Equal(first, snapshot) {
		t.Fatal("first outgoing frame was overwritten by the next tick")
	}
}

// Comfort noise is generated once per tick and shared, so every silent
// listener gets the same non-silent frame and a listener with audio gets none.
func TestMixTickComfortNoiseSharedPerTick(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(log, 8000)
	m.SetComfortNoise(true)
	n := m.samplesPerFrame
	parts := make([]*Participant, 3)
	for i := range parts {
		parts[i] = newTickParticipant(m, fmt.Sprintf("p%d", i))
	}
	// p0 only hears p1, which is silent; p2 hears everyone, including p0's tone.
	parts[0].Hears = map[string]struct{}{"p1": {}}
	parts[1].Hears = map[string]struct{}{"p1": {}}
	tone := make([]int16, n)
	for i := range tone {
		tone[i] = 1000
	}
	parts[0].incoming <- encodeFrame(tone)

	m.mixTick()

	a, b, c := <-parts[0].outgoing, <-parts[1].outgoing, <-parts[2].outgoing
	if bytes.Equal(a, make([]byte, n*2)) {
		t.Fatal("silent listener got no comfort noise")
	}
	if !bytes.Equal(a, b) {
		t.Fatal("silent listeners in one tick got different comfort-noise frames")
	}
	if !bytes.Equal(c, encodeFrame(tone)) {
		t.Fatal("listener with real audio had comfort noise added")
	}
}
