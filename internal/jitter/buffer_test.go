package jitter

import (
	"bytes"
	"testing"
)

func frameBytes(tag byte) []byte {
	// 320 byte "frame" tagged with a single byte so the test can assert
	// which input came out.
	b := make([]byte, 320)
	b[0] = tag
	return b
}

func TestSeqLess(t *testing.T) {
	cases := []struct {
		a, b uint16
		want bool
	}{
		{0, 1, true},
		{1, 0, false},
		{100, 200, true},
		{65535, 0, true},  // wraparound
		{0, 65535, false}, // wraparound
		{32767, 32768, true},
		{5, 5, false}, // equal
	}
	for _, c := range cases {
		if got := SeqLess(c.a, c.b); got != c.want {
			t.Errorf("SeqLess(%d,%d)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestBuffer_WarmUp verifies that Pop returns nil until the buffer has
// reached target depth.
func TestBuffer_WarmUp(t *testing.T) {
	b := New(3, 10)
	b.Push(100, frameBytes(1))
	if _, ok := b.Pop(); ok {
		t.Fatal("pop before warm-up should return false")
	}
	b.Push(101, frameBytes(2))
	if _, ok := b.Pop(); ok {
		t.Fatal("still warming after 2 pushes")
	}
	b.Push(102, frameBytes(3))
	// Now warmed.
	f, ok := b.Pop()
	if !ok || f[0] != 1 {
		t.Fatalf("first pop: ok=%v tag=%d", ok, f[0])
	}
}

// TestBuffer_Reorder verifies out-of-order packets are reordered.
func TestBuffer_Reorder(t *testing.T) {
	b := New(2, 10)
	b.Push(100, frameBytes(1))
	b.Push(102, frameBytes(3))
	b.Push(101, frameBytes(2))
	b.Push(103, frameBytes(4))

	want := []byte{1, 2, 3, 4}
	for i, exp := range want {
		f, ok := b.Pop()
		if !ok {
			t.Fatalf("pop %d: expected data", i)
		}
		if f[0] != exp {
			t.Fatalf("pop %d: got tag %d want %d", i, f[0], exp)
		}
	}
}

// TestBuffer_DropDuplicate verifies duplicate seqnums are ignored.
func TestBuffer_DropDuplicate(t *testing.T) {
	b := New(1, 10)
	b.Push(100, frameBytes(1))
	b.Push(100, frameBytes(2)) // duplicate, should be dropped
	if l := b.Len(); l != 1 {
		t.Fatalf("len = %d, want 1", l)
	}
	f, ok := b.Pop()
	if !ok || f[0] != 1 {
		t.Fatalf("expected original frame tag 1, got ok=%v tag=%d", ok, f[0])
	}
}

// TestBuffer_LateArrivalAfterCursor verifies that a packet arriving after
// its slot was already popped (with silence) is dropped.
func TestBuffer_LateArrivalAfterCursor(t *testing.T) {
	b := New(1, 10)
	b.Push(100, frameBytes(1))
	if _, ok := b.Pop(); !ok {
		t.Fatal("first pop should succeed")
	}
	// Cursor is now 101. A push with 100 or earlier should be dropped.
	b.Push(100, frameBytes(99))
	b.Push(99, frameBytes(99))
	if l := b.Len(); l != 0 {
		t.Fatalf("len after late pushes = %d, want 0", l)
	}
}

// TestBuffer_UnderrunEmitsSilence verifies that when the next seqnum is
// missing, Pop returns (nil, false) and advances the cursor.
func TestBuffer_UnderrunEmitsSilence(t *testing.T) {
	b := New(1, 10)
	b.Push(100, frameBytes(1))
	b.Push(102, frameBytes(3)) // gap at 101

	f, ok := b.Pop()
	if !ok || f[0] != 1 {
		t.Fatalf("pop 1: ok=%v tag=%d", ok, f[0])
	}
	// Cursor 101 is missing — Pop should return (nil,false) and advance.
	if _, ok := b.Pop(); ok {
		t.Fatal("pop 2 should underrun")
	}
	// Now cursor == 102 and the buffered frame should come out.
	f, ok = b.Pop()
	if !ok || f[0] != 3 {
		t.Fatalf("pop 3: ok=%v tag=%d", ok, f[0])
	}
}

// TestBuffer_Wraparound verifies correct ordering across the uint16 rollover.
func TestBuffer_Wraparound(t *testing.T) {
	b := New(2, 10)
	b.Push(65534, frameBytes(1))
	b.Push(0, frameBytes(3))
	b.Push(65535, frameBytes(2))
	b.Push(1, frameBytes(4))

	want := []byte{1, 2, 3, 4}
	for i, exp := range want {
		f, ok := b.Pop()
		if !ok || f[0] != exp {
			t.Fatalf("pop %d: ok=%v tag=%d want %d", i, ok, f[0], exp)
		}
	}
}

// TestBuffer_MaxDepthEvicts verifies oldest-frame eviction when the buffer
// exceeds the max depth without ever being popped.
func TestBuffer_MaxDepthEvicts(t *testing.T) {
	b := New(2, 3)
	for i := uint16(0); i < 10; i++ {
		b.Push(100+i, frameBytes(byte(i+1)))
	}
	if l := b.Len(); l != 3 {
		t.Fatalf("len = %d, want 3", l)
	}
	// The three youngest frames survive: tags 8, 9, 10.
	f, ok := b.Pop()
	if !ok || f[0] != 8 {
		t.Fatalf("first surviving pop: ok=%v tag=%d, want 8", ok, f[0])
	}
}

// TestBuffer_Reset clears state.
func TestBuffer_Reset(t *testing.T) {
	b := New(1, 10)
	b.Push(100, frameBytes(1))
	b.Pop() // consume
	b.Reset()
	if l := b.Len(); l != 0 {
		t.Fatalf("len after reset = %d", l)
	}
	// After reset, buffer is warming again.
	b.Push(500, frameBytes(9))
	f, ok := b.Pop()
	if !ok || f[0] != 9 {
		t.Fatalf("post-reset pop: ok=%v tag=%d", ok, f[0])
	}
}

// TestBuffer_CopiesInput verifies Push defensively copies the PCM so
// caller-reused buffers don't corrupt queued frames.
func TestBuffer_CopiesInput(t *testing.T) {
	b := New(1, 10)
	buf := frameBytes(7)
	b.Push(100, buf)
	// Mutate caller's buffer.
	buf[0] = 99
	f, ok := b.Pop()
	if !ok {
		t.Fatal("pop failed")
	}
	if f[0] != 7 {
		t.Fatalf("queued frame got corrupted: got %d want 7", f[0])
	}
	if bytes.Equal(f, buf) {
		t.Fatal("expected independent slices after caller mutation")
	}
}
