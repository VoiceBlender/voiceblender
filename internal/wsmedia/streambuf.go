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
	nextRead time.Time
	pace     time.Duration

	// playoutBytes is the lead Read withholds before handing out audio; 0
	// disables jitter buffering entirely. warming is true whenever the lead
	// still has to be (re)accumulated.
	playoutBytes int
	warming      bool
	sampleRate   int

	// lastFrame is the most recent real frame, kept as the source material for
	// concealment. concealRun counts how many invented frames have gone out
	// back to back.
	lastFrame  []byte
	concealRun int
	insertRun  int
	drift      DriftStats
}

func newStreamBuffer(capBytes int, frameMs int, playoutBytes int, sampleRate int) *streamBuffer {
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
		sampleRate:   sampleRate,
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
// With a playout lead configured it also compensates for clock drift between
// the producer and the mixer — see readPlayout.
func (sb *streamBuffer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	sb.awaitSlot()

	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.playoutBytes > 0 {
		return sb.readPlayout(p)
	}
	return sb.readPassthrough(p)
}

// readPassthrough is the historical behaviour, used when jitter buffering is
// off: block for a whole frame, short-read whatever is left on close.
func (sb *streamBuffer) readPassthrough(p []byte) (int, error) {
	for len(sb.buf) < len(p) && !sb.closed {
		sb.cond.Wait()
	}
	if len(sb.buf) == 0 && sb.closed {
		return 0, io.EOF
	}
	return sb.takeFrame(p), nil
}

// readPlayout serves one frame from a jitter-buffered stream and keeps the
// buffer level near the configured lead.
//
// Two clocks feed this buffer — the producer's and the mixer's — and they never
// run at exactly the same rate. A lead only defers the reckoning: a producer
// even a fraction of a percent slow eventually leaves the buffer empty, and one
// that is fast eventually overflows it. Something has to give a frame back or
// take one away.
//
// The cheap place to do that is a pause. Duplicating or dropping 20 ms of
// silence is inaudible, so whenever the level drifts outside its band and the
// frame at the head is below the speech floor, the correction happens there and
// costs nothing. Only when the buffer runs dry mid-speech, with no pause to
// spend, does it invent a frame — faded, and bounded by maxConcealFrames, after
// which it waits for real audio and lets the mixer record the gap.
func (sb *streamBuffer) readPlayout(p []byte) (int, error) {
	f := len(p)

	// Initial warm-up: hold everything back until the lead exists. Later
	// shortfalls are handled below; rebuilding the whole lead after each one
	// would trade a 20 ms gap for a gap the length of the lead.
	if sb.warming {
		target := sb.playoutBytes
		if target < f {
			target = f
		}
		for len(sb.buf) < target && !sb.closed {
			sb.cond.Wait()
		}
		if !sb.closed {
			sb.warming = false
		}
	}

	if sb.closed && len(sb.buf) < f {
		if len(sb.buf) == 0 {
			return 0, io.EOF
		}
		return sb.drainTail(p), nil
	}

	if len(sb.buf) < f {
		if sb.concealRun == 0 {
			sb.drift.Underruns++
		}
		if sb.concealRun < maxConcealFrames && len(sb.lastFrame) == f {
			sb.concealRun++
			sb.drift.Concealed++
			gStart, gEnd := concealGains(sb.concealRun)
			writeConceal(p, sb.lastFrame, gStart, gEnd, sb.sampleRate)
			return f, nil
		}
		for len(sb.buf) < f && !sb.closed {
			sb.cond.Wait()
		}
		if len(sb.buf) == 0 && sb.closed {
			return 0, io.EOF
		}
		if sb.closed && len(sb.buf) < f {
			return sb.drainTail(p), nil
		}
	}
	sb.concealRun = 0

	// Running ahead: drop a quiet frame so the level stops climbing toward the
	// capacity bound, where whole writes get discarded mid-speech instead.
	if len(sb.buf) >= sb.trimAbove(f)+f && frameIsQuiet(sb.buf[:f]) {
		sb.discardFrame(f)
		sb.drift.Trimmed++
	}
	// Falling behind: repeat a quiet frame now, while there is still a pause to
	// spend, rather than concealing mid-word later. The repeat is not consumed,
	// so the level recovers by one frame.
	if sb.insertRun < maxInsertRun && len(sb.buf) <= sb.insertBelow(f) && frameIsQuiet(sb.buf[:f]) {
		copy(p, sb.buf[:f])
		sb.rememberFrame(p[:f])
		sb.insertRun++
		sb.drift.Inserted++
		return f, nil
	}

	sb.insertRun = 0
	n := sb.takeFrame(p)
	sb.rememberFrame(p[:n])
	return n, nil
}

// drainTail hands back a partial frame left over at close.
func (sb *streamBuffer) drainTail(p []byte) int {
	n := copy(p, sb.buf)
	sb.buf = sb.buf[:0]
	return n
}

// trimAbove and insertBelow bracket the level the buffer aims to hold.
//
// The target is the configured lead, and insertBelow sits one frame under it so
// a pause restores the level to the target rather than to some fraction of it —
// the whole lead is then available as jitter margin, which is what an operator
// sizing WS_JITTER_BUFFER_MS expects to be buying. trimAbove has more room
// because a level that is too high only costs latency, and shedding on ordinary
// jitter would fight the producer for no benefit.
func (sb *streamBuffer) trimAbove(frame int) int {
	slack := sb.playoutBytes / 2
	if slack < 2*frame {
		slack = 2 * frame
	}
	return sb.playoutBytes + slack
}

func (sb *streamBuffer) insertBelow(frame int) int {
	if n := sb.playoutBytes - frame; n > frame {
		return n
	}
	return frame
}

// takeFrame copies one frame out of the buffer and compacts what is left.
func (sb *streamBuffer) takeFrame(p []byte) int {
	n := copy(p, sb.buf)
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	return n
}

func (sb *streamBuffer) discardFrame(f int) {
	remaining := copy(sb.buf, sb.buf[f:])
	sb.buf = sb.buf[:remaining]
}

func (sb *streamBuffer) rememberFrame(frame []byte) {
	if cap(sb.lastFrame) < len(frame) {
		sb.lastFrame = make([]byte, len(frame))
	}
	sb.lastFrame = sb.lastFrame[:len(frame)]
	copy(sb.lastFrame, frame)
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

// Drift reports the clock-drift corrections made so far. All zero when jitter
// buffering is disabled.
func (sb *streamBuffer) Drift() DriftStats {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.drift
}
