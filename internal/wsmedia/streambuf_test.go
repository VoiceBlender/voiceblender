package wsmedia

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func TestStreamBufferRoundTrip(t *testing.T) {
	sb := newStreamBuffer(1024, 20, 0, 16000)
	in := bytes.Repeat([]byte{0xAB}, 320)
	n, err := sb.Write(in)
	if err != nil || n != 320 {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	out := make([]byte, 320)
	n, err = sb.Read(out)
	if err != nil || n != 320 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	if !bytes.Equal(in, out) {
		t.Fatal("data mismatch")
	}
}

func TestStreamBufferDropsOnOverflow(t *testing.T) {
	sb := newStreamBuffer(640, 20, 0, 16000) // 2 frames at 20ms@16kHz
	frame := bytes.Repeat([]byte{0xCD}, 640)
	// First write fits exactly.
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Second write would exceed capacity — must be dropped silently.
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	if got := sb.Dropped(); got != 640 {
		t.Fatalf("drops=%d, want 640", got)
	}
	// Read should still see only the first frame's worth.
	out := make([]byte, 640)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("first frame corrupted")
	}
}

func TestStreamBufferCloseUnblocksRead(t *testing.T) {
	sb := newStreamBuffer(1024, 20, 0, 16000)
	out := make([]byte, 100)
	done := make(chan error, 1)
	go func() {
		_, err := sb.Read(out)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	sb.Close()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("want io.EOF, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestStreamBufferPacesReads(t *testing.T) {
	sb := newStreamBuffer(4096, 20, 0, 16000)
	frame := bytes.Repeat([]byte{1}, 100)
	for i := 0; i < 3; i++ {
		if _, err := sb.Write(frame); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	out := make([]byte, 100)
	// First read returns immediately; subsequent reads should be paced.
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read 1: %v", err)
	}
	start := time.Now()
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 15*time.Millisecond {
		t.Fatalf("expected pacing ≥15ms, got %v", elapsed)
	}
}

func TestStreamBufferPlayoutWarmsBeforeFirstRead(t *testing.T) {
	const frameBytes = 640
	sb := newStreamBuffer(4096, 20, 2*frameBytes, 16000)
	frame := bytes.Repeat([]byte{0x11}, frameBytes)

	out := make([]byte, frameBytes)
	done := make(chan int, 1)
	go func() {
		n, err := sb.Read(out)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		done <- n
	}()

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	select {
	case <-done:
		t.Fatal("read returned before the playout lead was buffered")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	select {
	case n := <-done:
		if n != frameBytes {
			t.Fatalf("read n=%d, want %d", n, frameBytes)
		}
	case <-time.After(time.Second):
		t.Fatal("read did not release once the lead was buffered")
	}
	if got := sb.Drift().Underruns; got != 0 {
		t.Fatalf("underruns=%d, want 0 for the initial warm-up", got)
	}
}

// voicedFrame builds a frame well above the speech floor; quietFrame builds one
// below it. The drift corrections only touch quiet frames, so tests need both.
func voicedFrame(n int) []byte {
	b := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(int16(6000)))
	}
	return b
}

func quietFrame(n int) []byte { return make([]byte, n*2) }

func TestStreamBufferPlayoutConcealsUnderrunThenWaits(t *testing.T) {
	const samples = 160 // 320 bytes
	frameBytes := samples * 2
	sb := newStreamBuffer(4096, 0, 2*frameBytes, 16000) // no pacing, isolate the lead
	voiced := voicedFrame(samples)
	out := make([]byte, frameBytes)

	for i := 0; i < 2; i++ {
		if _, err := sb.Write(voiced); err != nil {
			t.Fatalf("warm write %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("warm read %d: %v", i, err)
		}
	}

	// Buffer is dry mid-speech: the next reads must be concealed, not blocked
	// and not zero-filled, because a hard step to zero is what clicks.
	for i := 1; i <= maxConcealFrames; i++ {
		done := make(chan struct{})
		go func() { sb.Read(out); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("conceal %d blocked instead of inventing a frame", i)
		}
		if got := sb.Drift().Concealed; got != int64(i) {
			t.Fatalf("Concealed=%d, want %d", got, i)
		}
	}
	if got := sb.Drift().Underruns; got != 1 {
		t.Fatalf("Underruns=%d, want 1 event", got)
	}

	// Budget spent: now it must wait for real audio so the gap is visible.
	done := make(chan struct{})
	go func() { sb.Read(out); close(done) }()
	select {
	case <-done:
		t.Fatal("read returned past the concealment budget; the gap must surface")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := sb.Write(voiced); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read did not resume once real audio arrived")
	}
}

func TestStreamBufferConcealFadesToSilence(t *testing.T) {
	const samples = 160
	frameBytes := samples * 2
	sb := newStreamBuffer(4096, 0, frameBytes, 16000)
	voiced := voicedFrame(samples)
	out := make([]byte, frameBytes)

	if _, err := sb.Write(voiced); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}

	var energies []float64
	for i := 0; i < maxConcealFrames; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("conceal read %d: %v", i, err)
		}
		var sum float64
		for j := 0; j < samples; j++ {
			v := float64(int16(binary.LittleEndian.Uint16(out[j*2:])))
			sum += v * v
		}
		energies = append(energies, sum)
	}
	if energies[0] == 0 {
		t.Fatal("first concealed frame is digital silence — that is the click being fixed")
	}
	for i := 1; i < len(energies); i++ {
		if energies[i] >= energies[i-1] {
			t.Fatalf("concealment energy did not fall: %v", energies)
		}
	}
}

func TestStreamBufferTrimsQuietFrameWhenRunningAhead(t *testing.T) {
	const samples = 160
	frameBytes := samples * 2
	sb := newStreamBuffer(16000, 0, 2*frameBytes, 16000)
	quiet := quietFrame(samples)

	// Well past the trim threshold, all of it silence.
	for i := 0; i < 10; i++ {
		if _, err := sb.Write(quiet); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	out := make([]byte, frameBytes)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := sb.Drift().Trimmed; got != 1 {
		t.Fatalf("Trimmed=%d, want 1 — a backlog of silence should be shed", got)
	}
}

func TestStreamBufferKeepsSpeechWhenRunningAhead(t *testing.T) {
	const samples = 160
	frameBytes := samples * 2
	sb := newStreamBuffer(16000, 0, 2*frameBytes, 16000)
	voiced := voicedFrame(samples)

	for i := 0; i < 10; i++ {
		if _, err := sb.Write(voiced); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	out := make([]byte, frameBytes)
	for i := 0; i < 5; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := sb.Drift().Trimmed; got != 0 {
		t.Fatalf("Trimmed=%d, want 0 — speech must never be dropped to shed backlog", got)
	}
}

func TestStreamBufferInsertsQuietFrameWhenFallingBehind(t *testing.T) {
	const samples = 160
	frameBytes := samples * 2
	sb := newStreamBuffer(16000, 0, 4*frameBytes, 16000)
	quiet := quietFrame(samples)

	for i := 0; i < 4; i++ {
		if _, err := sb.Write(quiet); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	out := make([]byte, frameBytes)
	for i := 0; i < 3; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := sb.Drift().Inserted; got == 0 {
		t.Fatal("Inserted=0 — a draining level should be topped up during silence")
	}
	if got := sb.Drift().Underruns; got != 0 {
		t.Fatalf("Underruns=%d, want 0 — the insert should have pre-empted the underrun", got)
	}
}

func TestStreamBufferInsertRunIsBounded(t *testing.T) {
	const samples = 160
	frameBytes := samples * 2
	sb := newStreamBuffer(16000, 0, 4*frameBytes, 16000)
	quiet := quietFrame(samples)

	// Enough to warm the lead, then nothing ever again: a producer that died
	// mid-silence. Duplicating its last quiet frame forever would keep the leg
	// sounding merely quiet instead of gone.
	for i := 0; i < 5; i++ {
		if _, err := sb.Write(quiet); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	out := make([]byte, frameBytes)
	go func() {
		for i := 0; i < 30; i++ {
			if _, err := sb.Read(out); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sb.Drift().Underruns > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	d := sb.Drift()
	t.Fatalf("no underrun surfaced for a dead producer (inserted=%d, concealed=%d) — the buffer is stretching silence forever",
		d.Inserted, d.Concealed)
}

func TestStreamBufferPlayoutCloseDuringWarmUpKeepsAudio(t *testing.T) {
	const frameBytes = 320
	sb := newStreamBuffer(4096, 0, 4*frameBytes, 16000)
	partial := bytes.Repeat([]byte{0x33}, frameBytes)
	if _, err := sb.Write(partial); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := make([]byte, frameBytes)
	done := make(chan int, 1)
	go func() {
		n, err := sb.Read(out)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		done <- n
	}()
	time.Sleep(20 * time.Millisecond)
	sb.Close()

	select {
	case n := <-done:
		if n != frameBytes {
			t.Fatalf("read n=%d, want the buffered %d bytes back", n, frameBytes)
		}
		if !bytes.Equal(out, partial) {
			t.Fatal("buffered audio discarded on close")
		}
	case <-time.After(time.Second):
		t.Fatal("read did not unblock after Close")
	}
}

func TestStreamBufferPlayoutDisabledIsPassthrough(t *testing.T) {
	sb := newStreamBuffer(4096, 0, 0, 16000)
	frame := bytes.Repeat([]byte{0x44}, 320)
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 320)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, frame) {
		t.Fatal("data mismatch")
	}
	if got := sb.Drift().Underruns; got != 0 {
		t.Fatalf("underruns=%d, want 0 when jitter buffering is off", got)
	}
}

func TestStreamBufferPlayoutLargerThanCapacityIsIgnored(t *testing.T) {
	sb := newStreamBuffer(320, 0, 640, 16000)
	frame := bytes.Repeat([]byte{0x55}, 320)
	if _, err := sb.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := make([]byte, 320)
	if _, err := sb.Read(out); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestStreamBufferReadPacingDoesNotDrift(t *testing.T) {
	// A writer on an absolute-schedule ticker never drifts, so any lag the
	// reader accumulates is its own. Pacing from the end of the previous read
	// used to add ~300µs per frame, which the mixer sees as a whole 20ms frame
	// missing roughly once a second.
	const frames = 100
	sb := newStreamBuffer(64000, 20, 0, 16000)
	frame := make([]byte, 640)

	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for i := 0; i < frames+25; i++ {
			<-tk.C
			sb.Write(frame)
		}
	}()

	out := make([]byte, 640)
	start := time.Now()
	for i := 0; i < frames; i++ {
		if _, err := sb.Read(out); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	drift := time.Since(start) - frames*20*time.Millisecond

	const budget = 10 * time.Millisecond // the old scheme burned ~30ms here
	t.Logf("drift over %d frames: %v (%v per frame)", frames, drift, drift/frames)
	if drift > budget {
		t.Fatalf("read pacing drifted %v over %d frames (budget %v) — the mixer will starve every %.1fs",
			drift, frames, budget,
			(20*time.Millisecond).Seconds()/(drift.Seconds()/frames)*0.02)
	}
}
