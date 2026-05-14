package moqmedia

import (
	"io"
	"sync"
	"time"
)

// streamBuffer accepts variable-sized writes and provides paced reads. The
// recv loop writes decoded PCM here; the mixer drains it at ptime cadence.
// Capacity is bounded — writes that would exceed it discard the incoming
// frame whole and increment a drop counter.
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
