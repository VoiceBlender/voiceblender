package lkmedia

import (
	"io"
	"sync"
	"time"
)

// streamBuffer accepts variable-sized writes and provides paced reads.
// Copied from internal/moqmedia/streambuf.go to avoid exporting that
// package's internals; the implementations should be kept in sync if one
// is changed.
type streamBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	cap      int
	dropped  int64
	closed   bool
	nextRead time.Time
	pace     time.Duration
}

func newStreamBuffer(capBytes int, frameMs int) *streamBuffer {
	sb := &streamBuffer{
		cap:  capBytes,
		pace: time.Duration(frameMs) * time.Millisecond,
	}
	sb.cond = sync.NewCond(&sb.mu)
	return sb
}

func (sb *streamBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.closed {
		return len(p), nil
	}
	if len(sb.buf)+len(p) > sb.cap {
		sb.dropped += int64(len(p))
		return len(p), nil
	}
	sb.buf = append(sb.buf, p...)
	sb.cond.Signal()
	return len(p), nil
}

func (sb *streamBuffer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	sb.awaitSlot()

	sb.mu.Lock()
	for len(sb.buf) < len(p) && !sb.closed {
		sb.cond.Wait()
	}
	if len(sb.buf) == 0 && sb.closed {
		sb.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, sb.buf)
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	sb.mu.Unlock()

	return n, nil
}

// awaitSlot sleeps until this read's slot on a fixed schedule. The schedule is
// absolute on purpose: pacing from the end of the previous read charges every
// interval for that read's own cost plus the sleep's overshoot, and a few
// hundred microseconds per frame accumulates into a whole frame missed roughly
// once a second — which the mixer fills with a 20ms hole. Single-reader only.
func (sb *streamBuffer) awaitSlot() {
	if sb.pace <= 0 {
		return
	}
	now := time.Now()
	switch {
	case sb.nextRead.IsZero():
		sb.nextRead = now
	case now.Before(sb.nextRead):
		time.Sleep(sb.nextRead.Sub(now))
	case now.Sub(sb.nextRead) > sb.pace:
		// More than a frame late: the consumer stalled, so resync instead of
		// firing a catch-up burst that the mixer would only drop.
		sb.nextRead = now
	}
	sb.nextRead = sb.nextRead.Add(sb.pace)
}

// tryRead is a non-blocking variant: returns 0 when fewer than len(p)
// bytes are available rather than waiting for them. Used by the send
// loop where pacing is driven by an external ticker — blocking would
// stall paced WriteSample calls.
func (sb *streamBuffer) tryRead(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.buf) < len(p) {
		if sb.closed {
			return 0, io.EOF
		}
		return 0, nil
	}
	n := copy(p, sb.buf)
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	return n, nil
}

func (sb *streamBuffer) Close() {
	sb.mu.Lock()
	sb.closed = true
	sb.cond.Broadcast()
	sb.mu.Unlock()
}

func (sb *streamBuffer) Dropped() int64 {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.dropped
}
