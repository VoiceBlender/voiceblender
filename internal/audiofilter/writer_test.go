package audiofilter

import (
	"bytes"
	"sync"
	"testing"
)

type countingWriter struct {
	mu sync.Mutex
	n  int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.n += len(p)
	c.mu.Unlock()
	return len(p), nil
}

func (c *countingWriter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func newBandpassWriter(t *testing.T, dst *countingWriter) *Writer {
	t.Helper()
	w, err := NewWriter(dst, 8000, 16000, []Spec{{Type: "bandpass"}})
	if err != nil {
		t.Fatal(err)
	}
	fw, ok := w.(*Writer)
	if !ok {
		t.Fatalf("a bandpass chain must build a filtering Writer, got %T", w)
	}
	return fw
}

func TestWriter_WriteAfterCloseIsDiscarded(t *testing.T) {
	dst := &countingWriter{}
	w := newBandpassWriter(t, dst)
	frame := bytes.Repeat([]byte{0x10, 0x20}, 160)
	for i := 0; i < 10; i++ {
		if _, err := w.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	if dst.total() == 0 {
		t.Fatal("an open writer must pass filtered audio on")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	before := dst.total()
	n, err := w.Write(frame)
	if err != nil || n != len(frame) {
		t.Fatalf("Write after Close = %d, %v; want %d, nil", n, err, len(frame))
	}
	if dst.total() != before {
		t.Fatal("a closed writer must not run its released stages")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWriter_CloseRacesWrite(t *testing.T) {
	dst := &countingWriter{}
	w := newBandpassWriter(t, dst)
	frame := bytes.Repeat([]byte{0x10, 0x20}, 160)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = w.Write(frame)
		}
	}()
	_ = w.Close()
	wg.Wait()
}
