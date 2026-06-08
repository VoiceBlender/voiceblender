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
	lastRead time.Time
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
	if !sb.lastRead.IsZero() {
		wait := sb.pace - time.Since(sb.lastRead)
		if wait > 0 {
			time.Sleep(wait)
		}
	}

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

	sb.lastRead = time.Now()
	return n, nil
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
