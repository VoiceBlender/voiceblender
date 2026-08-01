package wsutilx

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gobwas/ws"
)

// TestLockedWriter_WriteDeadlineUnblocks asserts every LockedWriter method
// arms a write deadline before handing the conn to gobwas, so a peer that
// never drains cannot pin the calling goroutine.
//
// stuckConn.Write blocks on <-s.closed forever unless a deadline was
// stored, so a missing SetWriteDeadline call turns into a goroutine that
// never delivers.
func TestLockedWriter_WriteDeadlineUnblocks(t *testing.T) {
	prev := DefaultWriteTimeout.Load()
	DefaultWriteTimeout.Store(80 * time.Millisecond)
	t.Cleanup(func() { DefaultWriteTimeout.Store(prev) })

	cases := []struct {
		name  string
		write func(lw *LockedWriter) error
	}{
		{"WriteText", func(lw *LockedWriter) error { return lw.WriteText([]byte("hello")) }},
		{"WriteBinary", func(lw *LockedWriter) error { return lw.WriteBinary([]byte{1, 2, 3}) }},
		{"WriteControl", func(lw *LockedWriter) error { return lw.WriteControl(ws.OpPong, nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := newStuckConn()
			t.Cleanup(func() { _ = sc.Close() })
			lw := NewLockedWriter(sc)

			errCh := make(chan error, 1)
			go func() { errCh <- tc.write(lw) }()

			select {
			case err := <-errCh:
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Fatalf("%s err = %v, want os.ErrDeadlineExceeded", tc.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not unblock: no write deadline was armed", tc.name)
			}
		})
	}
}

// recordingConn counts concurrent Write calls and records the byte stream.
type recordingConn struct {
	inFlight    atomic.Int32
	maxInFlight atomic.Int32

	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *recordingConn) Write(p []byte) (int, error) {
	n := c.inFlight.Add(1)
	for {
		m := c.maxInFlight.Load()
		if n <= m || c.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	// Wide enough that unlocked writers are guaranteed to overlap.
	time.Sleep(5 * time.Millisecond)
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	c.inFlight.Add(-1)
	return len(p), nil
}

func (c *recordingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordingConn) Close() error                     { return nil }
func (c *recordingConn) LocalAddr() net.Addr              { return nil }
func (c *recordingConn) RemoteAddr() net.Addr             { return nil }
func (c *recordingConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }

// TestLockedWriter_SerializesFrames asserts the mutex still makes whole
// frames atomic — the deadline must not have been bought at the cost of
// the frame corruption the writer exists to prevent.
//
// gobwas emits a frame as a header write followed by a payload write, so
// without the lock the 8 writers interleave inside conn.Write.
func TestLockedWriter_SerializesFrames(t *testing.T) {
	const writers = 8

	rc := &recordingConn{}
	lw := NewLockedWriter(rc)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct payload length per writer.
			payload := bytes.Repeat([]byte{byte('a' + i)}, i+1)
			if err := lw.WriteBinary(payload); err != nil {
				t.Errorf("WriteBinary(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := rc.maxInFlight.Load(); got != 1 {
		t.Fatalf("max concurrent conn.Write = %d, want 1: frame writes are interleaving", got)
	}

	// The recorded stream must parse back as exactly 8 intact frames.
	rc.mu.Lock()
	raw := rc.buf.Bytes()
	rc.mu.Unlock()

	rd := bytes.NewReader(raw)
	lengths := map[int]int{}
	for i := 0; i < writers; i++ {
		f, err := ws.ReadFrame(rd)
		if err != nil {
			t.Fatalf("frame %d: %v (stream corrupted)", i, err)
		}
		if f.Header.OpCode != ws.OpBinary {
			t.Fatalf("frame %d opcode = %v, want binary", i, f.Header.OpCode)
		}
		lengths[len(f.Payload)]++
	}
	if rd.Len() != 0 {
		t.Fatalf("%d trailing bytes after 8 frames", rd.Len())
	}
	for i := 0; i < writers; i++ {
		if lengths[i+1] != 1 {
			t.Fatalf("payload length %d seen %d times, want 1", i+1, lengths[i+1])
		}
	}
}

// failingConn fails every Write and counts how many it was asked to make.
type failingConn struct {
	writes atomic.Int32
	err    error
}

func (c *failingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return 0, c.err
}

func (c *failingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *failingConn) Close() error                     { return nil }
func (c *failingConn) LocalAddr() net.Addr              { return nil }
func (c *failingConn) RemoteAddr() net.Addr             { return nil }
func (c *failingConn) SetDeadline(time.Time) error      { return nil }
func (c *failingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failingConn) SetWriteDeadline(time.Time) error { return nil }

// TestLockedWriter_LatchesFirstError asserts a failed write poisons the
// writer. The failure leaves a partial frame on the wire, so anything
// written afterwards — a pong from the read path, say — would be parsed by
// the peer as a continuation of that frame.
func TestLockedWriter_LatchesFirstError(t *testing.T) {
	boom := errors.New("write boom")
	fc := &failingConn{err: boom}
	lw := NewLockedWriter(fc)

	if err := lw.WriteText([]byte("hello")); !errors.Is(err, boom) {
		t.Fatalf("first write err = %v, want %v", err, boom)
	}
	if got := fc.writes.Load(); got != 1 {
		t.Fatalf("conn.Write calls after first frame = %d, want 1", got)
	}

	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"WriteText", func() error { return lw.WriteText([]byte("hello")) }},
		{"WriteBinary", func() error { return lw.WriteBinary([]byte{1, 2, 3}) }},
		{"WriteControl", func() error { return lw.WriteControl(ws.OpPong, nil) }},
	} {
		if err := tc.write(); !errors.Is(err, boom) {
			t.Errorf("%s after failure: err = %v, want the latched %v", tc.name, err, boom)
		}
	}
	if got := fc.writes.Load(); got != 1 {
		t.Errorf("conn.Write calls after the latch = %d, want 1: bytes were appended to a broken frame", got)
	}
	if err := lw.Err(); !errors.Is(err, boom) {
		t.Errorf("Err() = %v, want %v", err, boom)
	}
}
