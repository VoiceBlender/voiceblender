package lkmedia

import (
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// remoteMixer sum-mixes the decoded PCM streams from N subscribed LiveKit
// remote tracks into a single PCM16 stream exposed to the VoiceBlender
// room mixer. The VoiceBlender room sees the entire LK room as one
// participant, so this layer collapses N participants → 1.
//
// The mix runs on a single 20 ms ticker goroutine: each tick reads one
// frame from every lane (silence if a lane has no data ready), sums them
// with saturating int16 arithmetic to prevent wraparound clipping, and
// writes the frame to the output streamBuffer.
type remoteMixer struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	lanes map[string]*lane

	out *streamBuffer

	cancel chan struct{}
	done   chan struct{}

	started   atomic.Bool
	startOnce sync.Once
	closeOnce sync.Once

	// telemetry
	frameSizeBytes int
	frameSamples   int
}

// lane is the per-LK-remote-track decode pipe. Writers (decoder
// goroutines) push PCM16-LE bytes; the mix ticker reads exactly one frame
// at a time. Bounded ring with drop-oldest semantics so a slow decoder
// cannot stall the mixer.
type lane struct {
	mu       sync.Mutex
	frames   [][]byte // each entry is exactly frameSizeBytes
	cap      int
	carry    []byte // partial accumulator: writes need not be frame-aligned
	dropped  uint64
	identity string
}

func newRemoteMixer(cfg Config, log *slog.Logger) *remoteMixer {
	return &remoteMixer{
		cfg:            cfg,
		log:            log,
		lanes:          map[string]*lane{},
		out:            newStreamBuffer(cfg.IngressBufferBytes(), cfg.FrameMs),
		cancel:         make(chan struct{}),
		done:           make(chan struct{}),
		frameSizeBytes: cfg.FrameBytesPCM(),
		frameSamples:   cfg.FrameSamples(),
	}
}

// Start begins the mix ticker. Idempotent — safe to call from multiple
// places without coordination.
func (m *remoteMixer) Start() {
	m.startOnce.Do(func() {
		m.started.Store(true)
		go m.run()
	})
}

func (m *remoteMixer) run() {
	defer close(m.done)
	t := time.NewTicker(time.Duration(m.cfg.FrameMs) * time.Millisecond)
	defer t.Stop()
	scratch := make([]int32, m.frameSamples)
	frame := make([]byte, m.frameSizeBytes)
	for {
		select {
		case <-m.cancel:
			return
		case <-t.C:
			m.tick(scratch, frame)
		}
	}
}

// tick reads one frame from each lane (silence if empty), sum-mixes with
// saturating int16 add, and writes the result to the output buffer.
func (m *remoteMixer) tick(scratch []int32, frame []byte) {
	for i := range scratch {
		scratch[i] = 0
	}

	m.mu.Lock()
	lanes := make([]*lane, 0, len(m.lanes))
	for _, ln := range m.lanes {
		lanes = append(lanes, ln)
	}
	m.mu.Unlock()

	if len(lanes) == 0 {
		// No lanes — emit silence so the room mixer still gets a paced stream.
		for i := range frame {
			frame[i] = 0
		}
		_, _ = m.out.Write(frame)
		return
	}

	for _, ln := range lanes {
		buf := ln.popFrame(m.frameSizeBytes)
		if buf == nil {
			continue
		}
		for i := 0; i < m.frameSamples; i++ {
			s := int16(binary.LittleEndian.Uint16(buf[i*2:]))
			scratch[i] += int32(s)
		}
	}

	// Saturate to int16 range and write out.
	for i, v := range scratch {
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v < math.MinInt16 {
			v = math.MinInt16
		}
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(v)))
	}
	_, _ = m.out.Write(frame)
}

// AddLane registers a new subscriber-side decode pipe keyed by track SID.
// Returns the lane's Writer that decoder goroutines push PCM16-LE bytes
// to. Idempotent: re-adding an existing SID returns the existing lane.
func (m *remoteMixer) AddLane(sid, identity string) io.Writer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ln, ok := m.lanes[sid]; ok {
		return laneWriter{lane: ln, frameSize: m.frameSizeBytes}
	}
	ln := &lane{
		cap:      m.cfg.LaneCapacityFrames(),
		identity: identity,
	}
	m.lanes[sid] = ln
	return laneWriter{lane: ln, frameSize: m.frameSizeBytes}
}

// RemoveLane drops the lane for a track SID. Any buffered frames are
// discarded.
func (m *remoteMixer) RemoveLane(sid string) {
	m.mu.Lock()
	delete(m.lanes, sid)
	m.mu.Unlock()
}

// LaneCount returns the number of currently active lanes.
func (m *remoteMixer) LaneCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lanes)
}

// Output is the io.Reader the leg's AudioReader returns. PCM16-LE bytes at
// the configured sample rate, paced at FrameMs cadence.
func (m *remoteMixer) Output() io.Reader { return m.out }

// Close stops the mix ticker and signals EOF on the output. Idempotent;
// safe to call even if Start was never invoked.
func (m *remoteMixer) Close() {
	m.closeOnce.Do(func() {
		close(m.cancel)
		if m.started.Load() {
			<-m.done
		}
		m.out.Close()
	})
}

// LaneDrops returns total dropped bytes across all lanes (best-effort
// snapshot for observability).
func (m *remoteMixer) LaneDrops() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total uint64
	for _, ln := range m.lanes {
		ln.mu.Lock()
		total += ln.dropped
		ln.mu.Unlock()
	}
	return total
}

// laneWriter implements io.Writer by accumulating PCM bytes into the
// lane's carry buffer and pushing complete frames into the ring.
type laneWriter struct {
	lane      *lane
	frameSize int
}

func (lw laneWriter) Write(p []byte) (int, error) {
	lw.lane.mu.Lock()
	defer lw.lane.mu.Unlock()
	lw.lane.carry = append(lw.lane.carry, p...)
	for len(lw.lane.carry) >= lw.frameSize {
		frame := make([]byte, lw.frameSize)
		copy(frame, lw.lane.carry[:lw.frameSize])
		lw.lane.carry = lw.lane.carry[lw.frameSize:]
		// Drop oldest on overflow.
		if len(lw.lane.frames) >= lw.lane.cap {
			dropped := lw.lane.frames[0]
			lw.lane.frames = lw.lane.frames[1:]
			lw.lane.dropped += uint64(len(dropped))
		}
		lw.lane.frames = append(lw.lane.frames, frame)
	}
	return len(p), nil
}

// popFrame returns one frame's worth of PCM bytes, or nil if the lane is
// empty. The returned slice is owned by the caller — safe to mutate.
func (ln *lane) popFrame(frameSize int) []byte {
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if len(ln.frames) == 0 {
		return nil
	}
	f := ln.frames[0]
	ln.frames = ln.frames[1:]
	if len(f) != frameSize {
		// Lane was created with a different config; should not happen.
		return nil
	}
	return f
}
