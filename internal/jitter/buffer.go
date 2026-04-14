// Package jitter implements a fixed-delay RTP jitter buffer (non-adaptive by design).
package jitter

import (
	"sync"
)

// SeqLess does RFC 1982 circular comparison on uint16 sequence numbers.
func SeqLess(a, b uint16) bool {
	return a != b && uint16(b-a) < 0x8000
}

type frame struct {
	seq uint16
	pcm []byte
}

// Buffer is a fixed-delay seqnum-ordered PCM reorder buffer; safe for concurrent Push/Pop.
type Buffer struct {
	mu sync.Mutex

	targetFrames int
	maxFrames    int

	frames []frame

	hasCursor bool
	cursor    uint16

	warming bool
}

// New takes target/max depth in frames (one frame = one Push, typically 20 ms).
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

// NewMs is New with millisecond inputs (frameMs typically 20).
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

	if b.hasCursor && !SeqLess(b.cursor, seq) && seq != b.cursor {
		return
	}

	i := len(b.frames)
	for i > 0 {
		prev := b.frames[i-1].seq
		if prev == seq {
			return
		}
		if SeqLess(prev, seq) {
			break
		}
		i--
	}
	cp := make([]byte, len(pcm))
	copy(cp, pcm)
	b.frames = append(b.frames, frame{})
	copy(b.frames[i+1:], b.frames[i:])
	b.frames[i] = frame{seq: seq, pcm: cp}

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

// Pop returns the next in-order frame, or (nil,false) when warming/underrunning
// (caller should emit silence). First Pop after warm-up anchors the play cursor.
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

	// Expected seqnum missing: advance and let caller emit silence.
	b.cursor++
	return nil, false
}

func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.frames)
}

// Reset clears the buffer and re-enters the warming state.
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.frames = b.frames[:0]
	b.hasCursor = false
	b.warming = true
}
