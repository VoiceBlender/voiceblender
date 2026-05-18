// Package bridge provides an in-memory duplex audio conduit used to join
// two room mixers together. It has no knowledge of mixers or rooms; it is a
// pure transport primitive shaped to satisfy the mixer's participant
// reader/writer contract.
package bridge

import (
	"io"
	"sync"
	"sync/atomic"
)

// DefaultBufFrames matches the mixer's per-participant incoming buffer depth.
const DefaultBufFrames = 3

// Endpoint is one side of a duplex conduit. It implements io.Reader,
// io.Writer and io.Closer so it can be passed to Mixer.AddParticipant as
// both the reader and the writer of a synthetic bridge participant.
//
// Write is non-blocking and drops the oldest frame under backpressure
// (matching the mixer's lossy philosophy). Read blocks while the conduit is
// open and returns io.EOF only after Close — never (0, nil) — because the
// mixer's readLoop permanently exits on any read error.
//
// The two endpoints of a pair share a single close signal: closing either
// endpoint tears the whole conduit down and unblocks the peer's Read.
type Endpoint struct {
	in   chan []byte // frames this endpoint reads (written by the peer)
	out  chan []byte // frames this endpoint writes (read by the peer)
	done chan struct{}
	once *sync.Once
	st   *atomic.Bool // shared closed flag
	rbuf []byte       // leftover from a frame larger than the Read buffer
}

// NewPair returns the two endpoints of a duplex conduit. Frames written to
// one endpoint become readable on the other. bufFrames is the per-direction
// buffer depth; values <= 0 use DefaultBufFrames.
func NewPair(bufFrames int) (a, b *Endpoint) {
	if bufFrames <= 0 {
		bufFrames = DefaultBufFrames
	}
	aToB := make(chan []byte, bufFrames)
	bToA := make(chan []byte, bufFrames)
	done := make(chan struct{})
	once := &sync.Once{}
	st := &atomic.Bool{}
	a = &Endpoint{in: bToA, out: aToB, done: done, once: once, st: st}
	b = &Endpoint{in: aToB, out: bToA, done: done, once: once, st: st}
	return a, b
}

// Read returns audio sent by the peer endpoint. It blocks until a frame is
// available or the conduit is closed; on close it returns io.EOF.
func (e *Endpoint) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(e.rbuf) > 0 {
		n := copy(p, e.rbuf)
		e.rbuf = e.rbuf[n:]
		return n, nil
	}
	select {
	case frame := <-e.in:
		n := copy(p, frame)
		if n < len(frame) {
			e.rbuf = append(e.rbuf[:0], frame[n:]...)
		}
		return n, nil
	case <-e.done:
		return 0, io.EOF
	}
}

// Write hands a frame to the peer endpoint. It never blocks: if the peer is
// not keeping up the oldest buffered frame is dropped to make room. The
// frame is copied, so the caller may reuse p immediately.
func (e *Endpoint) Write(p []byte) (int, error) {
	if e.st.Load() {
		return 0, io.ErrClosedPipe
	}
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case <-e.done:
		return 0, io.ErrClosedPipe
	case e.out <- frame:
		return len(p), nil
	default:
	}
	// Buffer full: drop the oldest frame, then enqueue the newest.
	select {
	case <-e.out:
	default:
	}
	select {
	case <-e.done:
		return 0, io.ErrClosedPipe
	case e.out <- frame:
	default:
	}
	return len(p), nil
}

// Close tears down both halves of the conduit. It is idempotent and safe to
// call from either endpoint; the peer's blocked Read unblocks with io.EOF.
func (e *Endpoint) Close() error {
	e.once.Do(func() {
		e.st.Store(true)
		close(e.done)
	})
	return nil
}
