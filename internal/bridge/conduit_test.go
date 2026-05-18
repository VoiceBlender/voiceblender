package bridge

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

func TestPairWiring(t *testing.T) {
	a, b := NewPair(3)

	want := []byte{1, 2, 3, 4}
	if _, err := a.Write(want); err != nil {
		t.Fatalf("a.Write: %v", err)
	}
	got := make([]byte, 4)
	n, err := b.Read(got)
	if err != nil {
		t.Fatalf("b.Read: %v", err)
	}
	if n != 4 || !bytes.Equal(got, want) {
		t.Fatalf("b.Read = %v (n=%d), want %v", got[:n], n, want)
	}

	// Reverse direction.
	rev := []byte{9, 8, 7}
	if _, err := b.Write(rev); err != nil {
		t.Fatalf("b.Write: %v", err)
	}
	got = make([]byte, 3)
	if n, err = a.Read(got); err != nil || n != 3 || !bytes.Equal(got, rev) {
		t.Fatalf("a.Read = %v (n=%d, err=%v), want %v", got[:n], n, err, rev)
	}
}

func TestReadBlocksUntilData(t *testing.T) {
	a, b := NewPair(3)

	readDone := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4)
		n, err := b.Read(buf)
		if err != nil {
			readDone <- nil
			return
		}
		readDone <- buf[:n]
	}()

	select {
	case <-readDone:
		t.Fatal("Read returned before any data was written")
	case <-time.After(50 * time.Millisecond):
	}

	a.Write([]byte{5, 6, 7, 8})
	select {
	case got := <-readDone:
		if !bytes.Equal(got, []byte{5, 6, 7, 8}) {
			t.Fatalf("Read = %v, want [5 6 7 8]", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after data was written")
	}
}

func TestCloseUnblocksPeerReadWithEOF(t *testing.T) {
	a, b := NewPair(3)

	res := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 4))
		res <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-res:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Read after Close err = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not unblock after Close")
	}
}

func TestReadAfterCloseReturnsEOFNotSpin(t *testing.T) {
	a, b := NewPair(3)
	a.Close()

	for i := 0; i < 3; i++ {
		n, err := b.Read(make([]byte, 4))
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("Read after Close = (n=%d, err=%v), want (0, io.EOF)", n, err)
		}
	}
}

func TestWriteAfterCloseErrors(t *testing.T) {
	a, b := NewPair(3)
	b.Close()

	if _, err := a.Write([]byte{1, 2}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after Close err = %v, want io.ErrClosedPipe", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	a, b := NewPair(3)
	if err := a.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("peer Close after Close: %v", err)
	}
}

func TestWriteDropsOldestUnderBackpressure(t *testing.T) {
	a, b := NewPair(2) // buffer depth 2; no reader draining

	a.Write([]byte{1})
	a.Write([]byte{2})
	a.Write([]byte{3}) // overflow: oldest (1) dropped
	a.Write([]byte{4}) // overflow: oldest (2) dropped

	// The two most-recent frames must survive in order.
	first := make([]byte, 1)
	if n, err := b.Read(first); err != nil || n != 1 || first[0] != 3 {
		t.Fatalf("first surviving frame = %v (n=%d, err=%v), want [3]", first[:n], n, err)
	}
	second := make([]byte, 1)
	if n, err := b.Read(second); err != nil || n != 1 || second[0] != 4 {
		t.Fatalf("second surviving frame = %v (n=%d, err=%v), want [4]", second[:n], n, err)
	}
}

func TestReadLeftoverWhenBufferSmallerThanFrame(t *testing.T) {
	a, b := NewPair(3)
	a.Write([]byte{1, 2, 3, 4, 5})

	p1 := make([]byte, 2)
	if n, err := b.Read(p1); err != nil || n != 2 || !bytes.Equal(p1, []byte{1, 2}) {
		t.Fatalf("Read#1 = %v (n=%d, err=%v), want [1 2]", p1[:n], n, err)
	}
	p2 := make([]byte, 2)
	if n, err := b.Read(p2); err != nil || n != 2 || !bytes.Equal(p2, []byte{3, 4}) {
		t.Fatalf("Read#2 = %v (n=%d, err=%v), want [3 4]", p2[:n], n, err)
	}
	p3 := make([]byte, 2)
	if n, err := b.Read(p3); err != nil || n != 1 || p3[0] != 5 {
		t.Fatalf("Read#3 = %v (n=%d, err=%v), want [5]", p3[:n], n, err)
	}
}

func TestWriteCopiesCallerBuffer(t *testing.T) {
	a, b := NewPair(3)
	buf := []byte{1, 2, 3}
	a.Write(buf)
	buf[0], buf[1], buf[2] = 9, 9, 9 // mutate after Write

	got := make([]byte, 3)
	if n, err := b.Read(got); err != nil || n != 3 || !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("Read = %v (n=%d, err=%v), want [1 2 3] (Write must copy)", got[:n], n, err)
	}
}
