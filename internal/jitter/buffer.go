// Package jitter implements a simple fixed-delay RTP jitter buffer.
//
// The buffer decouples RTP packet arrival jitter from downstream consumption.
// Producers push decoded PCM frames keyed by their 16-bit RTP sequence
// number (wraparound-aware); consumers call Pop at a fixed cadence (20 ms in
// VoiceBlender) and receive the next in-order frame or a nil slice on
// underrun, which the caller is expected to replace with silence of the
// right length.
package jitter

import (
	"sync"
)

// SeqLess reports whether RTP sequence number a is strictly before b using
// RFC 1982 / RTP circular comparison (a difference of more than 32768 is
// treated as a wraparound).
func SeqLess(a, b uint16) bool {
	return a != b && uint16(b-a) < 0x8000
}

// frame is one queued packet with its sequence number.
type frame struct {
	seq uint16
	pcm []byte
}

// Buffer is a fixed-delay, seqnum-ordered PCM frame reorder buffer.
//
// Semantics:
//   - Push stores frames keyed by seqnum and discards duplicates and
//     frames older than the current play cursor.
//   - Pop returns the next in-order frame or nil if the buffer is still
//     warming up or underrunning. Callers must substitute silence when
//     they receive nil.
//   - The buffer warms up after the first Push until it holds at least
//     TargetDelayFrames frames; before that Pop returns nil.
//   - If the queue grows beyond MaxDelayFrames, the oldest frames are
//     dropped (catch-up behaviour after a long stall).
//
// Buffer is safe for concurrent Push/Pop from different goroutines.
type Buffer struct {
	mu sync.Mutex

	// Target queue depth (frames) before Pop starts emitting. One frame =
	// one call to Push; VoiceBlender uses 20 ms frames so the delay in ms
	// is TargetDelayFrames * 20.
	targetFrames int
	maxFrames    int

	frames []frame // seqnum-ordered slice; small, inserted with linear scan

	hasCursor bool   // true once Pop has emitted at least one frame
	cursor    uint16 // expected next seqnum

	warming bool // true until we first reach targetFrames
}

// New returns a buffer with the given target and max depth in frames.
// Typical values for 20 ms frames: target=3 (60 ms), max=15 (300 ms).
// target must be >= 1 and max >= target.
func New(targetFrames, maxFrames int) *Buffer {
	if targetFrames < 1 {
		targetFrames = 1
	}
	if maxFrames < targetFrames {
		maxFrames = targetFrames
	}
	return &Buffer{
		targetFrames: targetFrames,
		maxFrames:    maxFrames,
		warming:      true,
	}
}

// NewMs is a convenience constructor taking durations. frameMs is the
// length of one frame in milliseconds (20 for VoiceBlender's SIP legs).
func NewMs(targetMs, maxMs, frameMs int) *Buffer {
	if frameMs < 1 {
		frameMs = 20
	}
	target := targetMs / frameMs
	if target < 1 {
		target = 1
	}
	max := maxMs / frameMs
	if max < target {
		max = target
	}
	return New(target, max)
}

// Push inserts a frame. Out-of-order inserts are placed in seqnum order;
// duplicates and frames older than the play cursor are dropped. If the
// queue exceeds max depth, the oldest frame is evicted.
func (b *Buffer) Push(seq uint16, pcm []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Discard frames older than or equal to the play cursor — they'd play
	// out of order and the listener has already heard silence for that
	// slot.
	if b.hasCursor && !SeqLess(b.cursor, seq) && seq != b.cursor {
		// seq <= cursor-1: already played.
		return
	}

	// Find insertion point. Most packets arrive in order so we scan from
	// the tail.
	i := len(b.frames)
	for i > 0 {
		prev := b.frames[i-1].seq
		if prev == seq {
			// Duplicate — drop.
			return
		}
		if SeqLess(prev, seq) {
			break
		}
		i--
	}
	// Copy PCM so the caller can reuse its buffer.
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	b.frames = append(b.frames, frame{})
	copy(b.frames[i+1:], b.frames[i:])
	b.frames[i] = frame{seq: seq, pcm: cp}

	// Evict oldest frames while over the max depth.
	for len(b.frames) > b.maxFrames {
		b.frames = b.frames[1:]
		if b.hasCursor {
			b.cursor = b.frames[0].seq
		}
	}

	if b.warming && len(b.frames) >= b.targetFrames {
		b.warming = false
	}
}

// Pop returns the next frame in seqnum order.
//
// Return contract:
//   - (pcm, true):  a real frame was returned.
//   - (nil, false): the buffer is warming up, empty, or the next expected
//     seqnum hasn't arrived yet. The caller should output a silence frame
//     of the codec's native size.
//
// On the first Pop after warm-up, the play cursor is anchored at the
// smallest buffered seqnum.
func (b *Buffer) Pop() ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.warming || len(b.frames) == 0 {
		return nil, false
	}

	head := b.frames[0]
	if !b.hasCursor {
		b.hasCursor = true
		b.cursor = head.seq
	}

	if head.seq == b.cursor {
		b.frames = b.frames[1:]
		b.cursor++
		return head.pcm, true
	}

	// Head is ahead of the cursor — the expected packet is late or lost.
	// Advance the cursor (substitute silence) so the stream catches up.
	b.cursor++
	return nil, false
}

// Len returns the current queue depth in frames.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.frames)
}

// Reset clears the buffer and returns it to the warming state. Useful
// after long silences or re-INVITE codec changes where the RTP stream
// restarts.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames = b.frames[:0]
	b.hasCursor = false
	b.warming = true
}
