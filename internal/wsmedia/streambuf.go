package wsmedia

import (
	"io"
	"sync"
	"time"
)

// streamBuffer accepts variable-sized writes and provides paced or blocking
// reads. Capacity is bounded — overflow drops the whole write.
//
// When SoleMixerClock is false (default), reads Sleep to frameMs and playout
// mode returns silence on warm-up/underrun so the mixer readLoop stays fed.
// When SoleMixerClock is true, reads block for real PCM only (no Sleep, no
// invented frames); pair with a deeper mixer live queue.
type streamBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	cap      int
	dropped  int64
	closed   bool
	lastRead time.Time
	pace     time.Duration

	playoutBytes   int
	warming        bool
	soleMixerClock bool
}

func newStreamBuffer(capBytes int, frameMs int) *streamBuffer {
	return newStreamBufferPlayout(capBytes, frameMs, 0, false)
}

func newStreamBufferPlayout(capBytes int, frameMs int, playoutBytes int, soleMixerClock bool) *streamBuffer {
	if playoutBytes < 0 {
		playoutBytes = 0
	}
	// playoutBytes == cap makes warm-up unreachable (Write drops at > cap).
	if playoutBytes > 0 && playoutBytes >= capBytes {
		playoutBytes = capBytes - 1
		if playoutBytes < 0 {
			playoutBytes = 0
		}
	}
	sb := &streamBuffer{
		cap:            capBytes,
		pace:           time.Duration(frameMs) * time.Millisecond,
		playoutBytes:   playoutBytes,
		warming:        playoutBytes > 0,
		soleMixerClock: soleMixerClock,
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

	if !sb.soleMixerClock && !sb.lastRead.IsZero() {
		wait := sb.pace - time.Since(sb.lastRead)
		if wait > 0 {
			time.Sleep(wait)
		}
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()

	if sb.soleMixerClock {
		return sb.readSoleClock(p)
	}
	return sb.readPaced(p)
}

func (sb *streamBuffer) readSoleClock(p []byte) (int, error) {
	if sb.playoutBytes > 0 && sb.warming {
		for len(sb.buf) < sb.playoutBytes && !sb.closed {
			sb.cond.Wait()
		}
		if sb.closed && len(sb.buf) < sb.playoutBytes {
			sb.buf = sb.buf[:0]
			return 0, io.EOF
		}
		sb.warming = false
	}

	for len(sb.buf) < len(p) && !sb.closed {
		sb.cond.Wait()
	}
	if len(sb.buf) == 0 && sb.closed {
		return 0, io.EOF
	}
	// Short read on close with a partial frame (historical behavior).
	if sb.closed && len(sb.buf) < len(p) {
		n := copy(p, sb.buf)
		sb.buf = sb.buf[:0]
		return n, nil
	}

	n := copy(p, sb.buf)
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	return n, nil
}

func (sb *streamBuffer) readPaced(p []byte) (int, error) {
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

	if sb.closed && len(sb.buf) < len(p) {
		if len(sb.buf) == 0 {
			return 0, io.EOF
		}
		n := copy(p, sb.buf)
		sb.buf = sb.buf[:0]
		sb.lastRead = time.Now()
		return n, nil
	}
	if sb.warming && len(sb.buf) >= sb.playoutBytes {
		sb.warming = false
	}
	if sb.warming || len(sb.buf) < len(p) {
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
