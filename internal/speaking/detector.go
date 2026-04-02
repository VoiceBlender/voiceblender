package speaking

import (
	"encoding/binary"
	"math"
	"sync"
	"time"
)

const (
	// Threshold is the RMS energy level (int16 samples) above which a
	// frame is considered voiced. Typical speech RMS is 1000–10000;
	// background noise sits below 200.
	Threshold = 300

	// OnFrames is the number of consecutive voiced frames required
	// before emitting a speaking-started event (3 × 20ms = 60ms).
	OnFrames = 3

	// OffFrames is the number of consecutive silent frames required
	// before emitting a speaking-stopped event (15 × 20ms = 300ms).
	// A longer release avoids flicker during natural speech pauses.
	OffFrames = 15

	// ptime is the frame duration in milliseconds.
	ptime = 20
)

// Event is emitted when a leg starts or stops speaking.
type Event struct {
	LegID    string
	Speaking bool
}

// state tracks voice activity with debouncing.
type state struct {
	speaking     bool
	activeFrames int // consecutive frames above threshold
	silentFrames int // consecutive frames below threshold
}

// update feeds a new frame's RMS energy into the state machine.
// Returns true if the speaking state changed.
func (s *state) update(rms float64) bool {
	if rms >= Threshold {
		s.activeFrames++
		s.silentFrames = 0
	} else {
		s.silentFrames++
		s.activeFrames = 0
	}

	prev := s.speaking
	if !s.speaking && s.activeFrames >= OnFrames {
		s.speaking = true
	} else if s.speaking && s.silentFrames >= OffFrames {
		s.speaking = false
	}
	return s.speaking != prev
}

// ComputeRMS returns the root-mean-square of int16 samples.
func ComputeRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// Detector performs voice activity detection on a leg's audio stream.
// It implements io.Writer to receive PCM from the leg's speaking tap,
// and runs a 20ms ticker goroutine to process accumulated audio.
type Detector struct {
	legID      string
	sampleRate int
	frameBytes int // bytes per 20ms frame at the leg's native sample rate
	st         state
	muted      func() bool
	onEvent    func(Event)

	mu   sync.Mutex
	buf  []byte
	stop chan struct{}
	done chan struct{}
}

// New creates a Detector for the given leg. The muted function is called
// each tick to check whether the leg is muted (muted legs don't emit
// speaking events). onEvent is called whenever speaking state changes.
func New(legID string, sampleRate int, muted func() bool, onEvent func(Event)) *Detector {
	samplesPerFrame := sampleRate * ptime / 1000
	return &Detector{
		legID:      legID,
		sampleRate: sampleRate,
		frameBytes: samplesPerFrame * 2, // 16-bit samples
		muted:      muted,
		onEvent:    onEvent,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// Start launches the background tick goroutine. Must be called once.
func (d *Detector) Start() {
	go d.run()
}

// Stop terminates the detector. If the leg was speaking, a speaking-stopped
// event is emitted. Safe to call multiple times.
func (d *Detector) Stop() {
	select {
	case <-d.stop:
		return // already stopped
	default:
	}
	close(d.stop)
	<-d.done
}

// Write implements io.Writer. It buffers incoming PCM from the leg's
// readLoop. Non-blocking — the tick goroutine drains the buffer.
func (d *Detector) Write(p []byte) (int, error) {
	d.mu.Lock()
	d.buf = append(d.buf, p...)
	d.mu.Unlock()
	return len(p), nil
}

func (d *Detector) run() {
	defer close(d.done)
	ticker := time.NewTicker(ptime * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			// Emit speaking-stopped on shutdown if needed.
			if d.st.speaking {
				d.onEvent(Event{LegID: d.legID, Speaking: false})
			}
			return
		case <-ticker.C:
			d.tick()
		}
	}
}

func (d *Detector) tick() {
	d.mu.Lock()
	data := d.buf
	d.buf = nil
	d.mu.Unlock()

	if len(data) == 0 {
		// No audio received — treat as silence.
		d.processSamples(nil)
		return
	}

	// Process all complete frames in the accumulated buffer.
	for len(data) >= d.frameBytes {
		frame := data[:d.frameBytes]
		data = data[d.frameBytes:]
		samples := bytesToSamples(frame)
		d.processSamples(samples)
	}
	// Remaining partial frame is discarded (next tick will get fresh data).
}

func (d *Detector) processSamples(samples []int16) {
	muted := d.muted()

	// If muted while speaking, force stop.
	if muted && d.st.speaking {
		d.st.speaking = false
		d.st.activeFrames = 0
		d.st.silentFrames = 0
		d.onEvent(Event{LegID: d.legID, Speaking: false})
		return
	}

	if muted {
		return
	}

	rms := ComputeRMS(samples)
	if d.st.update(rms) {
		d.onEvent(Event{LegID: d.legID, Speaking: d.st.speaking})
	}
}

func bytesToSamples(b []byte) []int16 {
	n := len(b) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}
