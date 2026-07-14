package api

import (
	"io"
	"testing"
)

// TestPipeReader_TryRead covers the non-blocking read contract: nothing ready
// yields (0, nil) without waiting, a buffered remainder is served before the
// channel, and io.EOF appears only after Close plus a full drain.
func TestPipeReader_TryRead(t *testing.T) {
	t.Run("empty returns zero without blocking", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 0 || err != nil {
			t.Fatalf("TryRead on empty pipe = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("serves a queued frame", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 4 || err != nil {
			t.Fatalf("TryRead = (%d, %v), want (4, nil)", n, err)
		}
		if string(p) != string([]byte{1, 2, 3, 4}) {
			t.Fatalf("TryRead payload = %v, want [1 2 3 4]", p)
		}
	})

	t.Run("serves buffered remainder before the channel", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{1, 2, 3, 4})
		pw.Write([]byte{9, 9})

		// A short read leaves a remainder buffered on the reader.
		small := make([]byte, 2)
		n, err := pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("first TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 1 || small[1] != 2 {
			t.Fatalf("first TryRead payload = %v, want [1 2]", small)
		}

		// The remainder of frame one must win over the queued frame two.
		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("second TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 3 || small[1] != 4 {
			t.Fatalf("remainder served out of order: got %v, want [3 4]", small)
		}

		n, err = pr.TryRead(small)
		if n != 2 || err != nil {
			t.Fatalf("third TryRead = (%d, %v), want (2, nil)", n, err)
		}
		if small[0] != 9 || small[1] != 9 {
			t.Fatalf("third TryRead payload = %v, want [9 9]", small)
		}
	})

	t.Run("EOF only after close and drain", func(t *testing.T) {
		pr, pw := createPipe()
		pw.Write([]byte{7, 7})
		pw.Close()

		// Still queued data: must be served, not swallowed by the close.
		p := make([]byte, 4)
		n, err := pr.TryRead(p)
		if n != 2 || err != nil {
			t.Fatalf("TryRead after Close with data queued = (%d, %v), want (2, nil)", n, err)
		}

		n, err = pr.TryRead(p)
		if n != 0 || err != io.EOF {
			t.Fatalf("TryRead after Close and drain = (%d, %v), want (0, io.EOF)", n, err)
		}
	})

	t.Run("open and empty is not EOF", func(t *testing.T) {
		pr, _ := createPipe()
		p := make([]byte, 4)
		for i := 0; i < 3; i++ {
			n, err := pr.TryRead(p)
			if n != 0 || err != nil {
				t.Fatalf("TryRead on open empty pipe = (%d, %v), want (0, nil)", n, err)
			}
		}
	})
}
