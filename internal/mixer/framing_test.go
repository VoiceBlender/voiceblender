package mixer

import (
	"io"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// chunkReader hands out PCM in the sizes a peer's packetisation would produce,
// one Read per chunk, then EOF.
type chunkReader struct {
	chunks [][]byte
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	c := r.chunks[r.i]
	n := copy(p, c)
	if n < len(c) {
		r.chunks[r.i] = c[n:]
		return n, nil
	}
	r.i++
	return n, nil
}

// pcmChunks splits total bytes of PCM into chunks of the given sizes, cycling
// through them. Each sample carries its own index, so a test can prove not just
// how much audio arrived but that none of it was dropped or reordered.
func pcmChunks(total int, sizes ...int) ([][]byte, []byte) {
	all := make([]byte, 0, total)
	for i := 0; len(all) < total; i++ {
		all = append(all, byte(i&0xff), byte((i>>8)&0xff))
	}
	all = all[:total]

	var chunks [][]byte
	for off, k := 0, 0; off < len(all); k++ {
		size := sizes[k%len(sizes)]
		if off+size > len(all) {
			size = len(all) - off
		}
		chunks = append(chunks, all[off:off+size])
		off += size
	}
	return chunks, all
}

// collectFrames runs the read loop against src and returns every frame it
// queued. The channel is deep enough that nothing is dropped.
func collectFrames(t *testing.T, m *Mixer, src io.Reader) [][]byte {
	t.Helper()
	p := &Participant{
		ID:       "p",
		Reader:   src,
		incoming: make(chan []byte, 4096),
		done:     make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		m.readLoop(p, make(chan struct{}))
		close(done)
	}()
	<-done
	close(p.incoming)

	var frames [][]byte
	for f := range p.incoming {
		frames = append(frames, f)
	}
	return frames
}

func TestReadLoop_FramesAreWholeWhateverThePacketisation(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes() // 640 B = 20 ms at 16 kHz

	cases := []struct {
		name  string
		sizes []int
	}{
		// One Read per packet meant one queued frame per packet, so a source
		// finer than the 20 ms tick lost the surplus to drop-oldest.
		{"10ms packets", []int{frame / 2}},
		{"20ms packets", []int{frame}},
		{"30ms packets", []int{frame * 3 / 2}},
		{"40ms packets", []int{frame * 2}},
		// A peer that changes packetisation mid-stream.
		{"20ms then 10ms", []int{frame, frame, frame / 2, frame / 2}},
		// Sizes that divide into no whole number of frames at all.
		{"ragged", []int{100, 640, 32, 1000, 2}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, want := pcmChunks(frame*20, tc.sizes...)
			frames := collectFrames(t, m, &chunkReader{chunks: chunks})

			var got []byte
			for i, f := range frames {
				if len(f) != frame {
					t.Fatalf("frame %d is %d bytes, want %d — a short frame reaches "+
						"the mix and every tap as if it were a whole one", i, len(f), frame)
				}
				got = append(got, f...)
			}

			// Everything but the last partial frame, in order. Losing audio here
			// is invisible downstream: the party just said less.
			if len(got) != len(want) {
				t.Fatalf("assembled %d bytes of audio, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("audio differs at byte %d: got %d, want %d — frames were "+
						"dropped or reordered", i, got[i], want[i])
				}
			}
		})
	}
}

// A source packetised finer than one frame is legal but worth reporting.
func TestReadLoop_CountsShortReads(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes()

	p := &Participant{
		ID:       "p",
		incoming: make(chan []byte, 4096),
		done:     make(chan struct{}),
	}

	chunks, _ := pcmChunks(frame*4, frame/2)
	p.Reader = &chunkReader{chunks: chunks}
	m.readLoop(p, make(chan struct{}))

	if got := p.framesRead.Load(); got != 4 {
		t.Errorf("framesRead = %d, want 4 whole frames from 8 half-frame packets", got)
	}
	if got := p.shortReads.Load(); got != 4 {
		t.Errorf("shortReads = %d, want 4 — one per packet that did not fill a frame", got)
	}
}

// A frame still incomplete when the source ends is dropped rather than queued
// short: at most one Ptime, at hangup, and the mix never sees a short frame.
func TestReadLoop_DiscardsThePartialFrameAtEndOfStream(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	frame := m.FrameSizeBytes()

	chunks, _ := pcmChunks(frame+frame/2, frame/2)
	frames := collectFrames(t, m, &chunkReader{chunks: chunks})

	if len(frames) != 1 {
		t.Fatalf("queued %d frames, want 1 whole frame with the remainder dropped", len(frames))
	}
	if len(frames[0]) != frame {
		t.Errorf("frame is %d bytes, want %d", len(frames[0]), frame)
	}
}

// stalledReader makes no progress and never fails, which io.Reader discourages
// but does not forbid. The read loop must still be stoppable.
type stalledReader struct{ reads atomic.Uint64 }

func (r *stalledReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return 0, nil
}

func TestReadLoop_ASourceMakingNoProgressStillStops(t *testing.T) {
	m := New(slog.New(slog.DiscardHandler), 16000)
	src := &stalledReader{}
	p := &Participant{
		ID:       "p",
		Reader:   src,
		incoming: make(chan []byte, 8),
		done:     make(chan struct{}),
	}

	stopCh := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		m.readLoop(p, stopCh)
		close(returned)
	}()

	// Get into the fill loop first, so this proves the inner loop yields rather
	// than that the outer one never ran.
	for src.reads.Load() == 0 {
		runtime.Gosched()
	}
	close(stopCh)

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not stop; a source returning (0, nil) wedges it")
	}
	if got := len(p.incoming); got != 0 {
		t.Errorf("queued %d frames from a source that produced no audio", got)
	}
}
