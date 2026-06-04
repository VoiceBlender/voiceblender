package lkmedia

import (
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"
)

// testCfg returns a validated Config suitable for mixer unit tests.
func testCfg(t *testing.T) Config {
	t.Helper()
	c := Config{Log: slog.Default()}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// pcmFrame returns a frame of `samples` int16 samples, all equal to v,
// encoded as PCM16-LE.
func pcmFrame(samples int, v int16) []byte {
	b := make([]byte, samples*2)
	for i := 0; i < samples; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v))
	}
	return b
}

// readFrame blocks until a full frame is available on r, or the timeout
// fires. Returns nil on timeout.
func readFrame(t *testing.T, r io.Reader, n int, timeout time.Duration) []byte {
	t.Helper()
	buf := make([]byte, n)
	deadline := time.Now().Add(timeout)
	off := 0
	for off < n {
		_ = deadline
		got, err := r.Read(buf[off:])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		off += got
		if time.Now().After(deadline) {
			return nil
		}
	}
	return buf
}

// allSamplesEqual returns the unique sample value if every PCM16-LE
// sample in buf equals it, else ok=false.
func allSamplesEqual(buf []byte) (int16, bool) {
	if len(buf) < 2 || len(buf)%2 != 0 {
		return 0, false
	}
	first := int16(binary.LittleEndian.Uint16(buf[:2]))
	for i := 2; i < len(buf); i += 2 {
		if int16(binary.LittleEndian.Uint16(buf[i:i+2])) != first {
			return 0, false
		}
	}
	return first, true
}

func TestRemoteMixer_SilentWithNoLanes(t *testing.T) {
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	m.Start()
	t.Cleanup(m.Close)

	buf := readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
	if buf == nil {
		t.Fatal("no frame produced")
	}
	v, ok := allSamplesEqual(buf)
	if !ok || v != 0 {
		t.Errorf("expected silence frame, got first=%d ok=%v", v, ok)
	}
}

func TestRemoteMixer_SumsTwoLanes(t *testing.T) {
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	m.Start()
	t.Cleanup(m.Close)

	w1 := m.AddLane("TR_a", "alice")
	w2 := m.AddLane("TR_b", "bob")

	// Pre-fill several frames per lane so the ticker reliably finds data
	// even with some scheduling jitter.
	for i := 0; i < 5; i++ {
		if _, err := w1.Write(pcmFrame(c.FrameSamples(), 100)); err != nil {
			t.Fatal(err)
		}
		if _, err := w2.Write(pcmFrame(c.FrameSamples(), 200)); err != nil {
			t.Fatal(err)
		}
	}

	// Skip the first emitted frame (it may have been produced before our
	// writes landed); inspect the next one.
	_ = readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
	buf := readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)

	v, ok := allSamplesEqual(buf)
	if !ok {
		t.Fatalf("frame not uniform: %v", buf[:8])
	}
	if v != 300 {
		t.Errorf("sum = %d, want 300 (100+200)", v)
	}
}

func TestRemoteMixer_Saturates(t *testing.T) {
	c := testCfg(t)

	check := func(t *testing.T, sample int16, want int16) {
		t.Helper()
		m := newRemoteMixer(c, slog.Default())
		m.Start()
		t.Cleanup(m.Close)
		w1 := m.AddLane("TR_hot1", "noisy1")
		w2 := m.AddLane("TR_hot2", "noisy2")
		for i := 0; i < 5; i++ {
			_, _ = w1.Write(pcmFrame(c.FrameSamples(), sample))
			_, _ = w2.Write(pcmFrame(c.FrameSamples(), sample))
		}
		// Skip one frame to dodge the boundary where the first tick may
		// have fired before our writes landed.
		_ = readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
		buf := readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
		v, ok := allSamplesEqual(buf)
		if !ok {
			t.Fatalf("frame not uniform: first 16 bytes = %v", buf[:16])
		}
		if v != want {
			t.Errorf("sample=%d: got %d, want %d", sample, v, want)
		}
	}

	// Two lanes at +30000 sum to +60000 → saturates to MaxInt16.
	check(t, 30000, math.MaxInt16)
	// Two lanes at -30000 sum to -60000 → saturates to MinInt16.
	check(t, -30000, math.MinInt16)
}

func TestRemoteMixer_RemoveLane(t *testing.T) {
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	m.Start()
	t.Cleanup(m.Close)

	m.AddLane("TR_a", "alice")
	m.AddLane("TR_b", "bob")
	if got := m.LaneCount(); got != 2 {
		t.Errorf("LaneCount = %d, want 2", got)
	}
	m.RemoveLane("TR_a")
	if got := m.LaneCount(); got != 1 {
		t.Errorf("LaneCount after remove = %d, want 1", got)
	}
	m.RemoveLane("TR_b")
	if got := m.LaneCount(); got != 0 {
		t.Errorf("LaneCount after both removed = %d, want 0", got)
	}
}

func TestRemoteMixer_LaneOverflowDropsOldest(t *testing.T) {
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	// Don't Start — we want the lane to fill without being drained.
	t.Cleanup(m.Close)

	w := m.AddLane("TR_slow", "slow")
	cap := c.LaneCapacityFrames()
	// Push capacity + 5 frames; the oldest 5 should be dropped.
	for i := 0; i < cap+5; i++ {
		if _, err := w.Write(pcmFrame(c.FrameSamples(), int16(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.LaneDrops(); got == 0 {
		t.Errorf("expected drops > 0, got %d", got)
	}
	// Inspect the ring: the first 5 frames (values 1..5) should be gone.
	// Pop one frame and check its uniform sample value is > 5.
	ln := m.lanes["TR_slow"]
	f := ln.popFrame(c.FrameBytesPCM())
	if f == nil {
		t.Fatal("no frame in lane")
	}
	v, ok := allSamplesEqual(f)
	if !ok {
		t.Fatalf("non-uniform frame")
	}
	if v <= 5 {
		t.Errorf("oldest frame should have been dropped, got value %d", v)
	}
}

func TestRemoteMixer_PartialWriteAccumulates(t *testing.T) {
	// Writers don't have to push frame-aligned bytes — the laneWriter
	// must accumulate across multiple Write calls.
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	m.Start()
	t.Cleanup(m.Close)

	w := m.AddLane("TR_a", "alice")
	full := pcmFrame(c.FrameSamples(), 1000)

	// Write in three uneven chunks.
	chunks := [][]byte{full[:100], full[100:1700], full[1700:]}
	for _, ch := range chunks {
		if _, err := w.Write(ch); err != nil {
			t.Fatal(err)
		}
	}

	// Pre-fill a few more frames so the ticker can drain reliably.
	for i := 0; i < 4; i++ {
		_, _ = w.Write(pcmFrame(c.FrameSamples(), 1000))
	}

	_ = readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
	buf := readFrame(t, m.Output(), c.FrameBytesPCM(), 200*time.Millisecond)
	v, ok := allSamplesEqual(buf)
	if !ok || v != 1000 {
		t.Errorf("got v=%d ok=%v, want 1000 true", v, ok)
	}
}

func TestRemoteMixer_CloseIsIdempotent(t *testing.T) {
	c := testCfg(t)
	m := newRemoteMixer(c, slog.Default())
	m.Start()
	m.Close()
	m.Close() // second close must not panic or deadlock
}
