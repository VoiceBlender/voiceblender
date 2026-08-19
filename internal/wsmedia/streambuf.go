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

	// playoutBytes is the lead Read withholds before handing out audio; 0
	// disables jitter buffering entirely. warming is true whenever the lead
	// still has to be (re)accumulated.
	playoutBytes int
	warming      bool
	underruns    int64
}

func newStreamBuffer(capBytes int, frameMs int, playoutBytes int) *streamBuffer {
	if playoutBytes < 0 || playoutBytes >= capBytes {
		// A lead the buffer cannot hold would never warm up. Config.Validate
		// keeps capacity above the lead; this only guards direct callers.
		playoutBytes = 0
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
// With a playout lead configured, Read also withholds audio until the lead is
// buffered, and re-arms that wait after every underrun — a lead spent once and
// never rebuilt stops absorbing jitter. It blocks rather than returning an
// invented silent frame, so the gap still shows up in the mixer's per
// participant starvation counters instead of looking like a quiet talker.
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
	if sb.playoutBytes > 0 && !sb.closed {
		if !sb.warming && len(sb.buf) < len(p) {
			sb.warming = true
			sb.underruns++
		}
		for sb.warming && len(sb.buf) < sb.playoutBytes && !sb.closed {
			sb.cond.Wait()
		}
		if !sb.closed {
			sb.warming = false
		}
	}
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

// Underruns returns how many times the playout lead ran dry and had to be
// rebuilt. Always 0 when jitter buffering is disabled.
func (sb *streamBuffer) Underruns() int64 {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.underruns
}
