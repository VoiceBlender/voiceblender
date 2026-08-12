package wsmedia

import (
	"io"
	"sync"
	"time"
)

// streamBuffer accepts variable-sized writes and provides paced reads. The
// recv loop writes inbound PCM here; the mixer drains it at ptime cadence.
// Capacity is bounded — writes that would exceed it discard the incoming
// bytes and increment a drop counter so the recv loop can record the loss.
//
// When playoutBytes > 0 the buffer acts as a fixed-delay WS jitter /
// playout buffer: Read returns silence until the target lead is buffered,
// then releases one frame per pace tick. After warm-up, a brief underrun
// returns silence immediately instead of blocking — that keeps the mixer
// readLoop fed so mixTick does not splice an extra digital-zero gap on
// clock-phase jitter between the WS producer and the room mixer.
//
// Adapted from internal/api/agent.go's streamBuffer with a fixed capacity
// for drop-on-overflow semantics.
type streamBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	cap      int
	dropped  int64
	closed   bool
	lastRead time.Time
	pace     time.Duration

	// playoutBytes is the warm-up / target lead (0 = passthrough: block
	// until a full Read is available, matching historical behaviour).
	playoutBytes int
	warming      bool
}

func newStreamBuffer(capBytes int, frameMs int) *streamBuffer {
	return newStreamBufferPlayout(capBytes, frameMs, 0)
}

func newStreamBufferPlayout(capBytes int, frameMs int, playoutBytes int) *streamBuffer {
	if playoutBytes < 0 {
		playoutBytes = 0
	}
	if playoutBytes > capBytes {
		playoutBytes = capBytes
	}
	sb := &streamBuffer{
		cap:          capBytes,
		pace:         time.Duration(frameMs) * time.Millisecond,
		playoutBytes: playoutBytes,
		warming:      playoutBytes > 0,
	}
	sb.cond = sync.NewCond(&sb.mu)
	return sb
}

// Write appends p to the buffer. If the buffer would exceed its capacity,
// the entire incoming write is dropped (drop-oldest would chop a frame in
// half and produce audio artifacts; whole-frame drop is the right call for
// 20ms audio frames). Always returns (len(p), nil) to satisfy io.Writer.
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

// Read blocks until len(p) bytes are buffered or the buffer is closed.
// Reads are paced: the second and later reads sleep up to pace - delta so
// the mixer's readLoop sees at most one frame per pace interval.
//
// With playout enabled, Read never blocks past the pace wait after the
// first call: warm-up and underrun return a silence frame of len(p) so the
// mixer keeps a steady cadence.
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
	defer sb.mu.Unlock()

	if sb.playoutBytes == 0 {
		for len(sb.buf) < len(p) && !sb.closed {
			sb.cond.Wait()
		}
		if len(sb.buf) == 0 && sb.closed {
			return 0, io.EOF
		}
		n := copy(p, sb.buf)
		remaining := copy(sb.buf, sb.buf[n:])
		sb.buf = sb.buf[:remaining]
		sb.lastRead = time.Now()
		return n, nil
	}

	// Playout / jitter-buffer mode.
	if sb.closed && len(sb.buf) < len(p) {
		if len(sb.buf) == 0 {
			return 0, io.EOF
		}
		// Drop a trailing partial frame on close rather than blocking.
		sb.buf = sb.buf[:0]
		return 0, io.EOF
	}
	if sb.warming && len(sb.buf) >= sb.playoutBytes {
		sb.warming = false
	}
	if sb.warming || len(sb.buf) < len(p) {
		// Leading silence while warming, or underrun after warm-up —
		// do not block; keep mixer readLoop on cadence.
		clear(p)
		sb.lastRead = time.Now()
		return len(p), nil
	}

	n := copy(p, sb.buf)
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	sb.lastRead = time.Now()
	return n, nil
}

// Close signals the reader to return io.EOF and stops accepting writes.
func (sb *streamBuffer) Close() {
	sb.mu.Lock()
	sb.closed = true
	sb.cond.Broadcast()
	sb.mu.Unlock()
}

// Dropped returns the cumulative count of bytes discarded on overflow.
func (sb *streamBuffer) Dropped() int64 {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.dropped
}
