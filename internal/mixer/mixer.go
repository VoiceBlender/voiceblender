package mixer

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/comfortnoise"
)

var errWriterClosed = errors.New("writer closed")

// guardedWriter wraps an io.Writer with an atomic closed flag.
// Once closed, all Write calls return immediately without touching
// the underlying writer. This prevents writes to a dead connection
// after a participant is removed.
type guardedWriter struct {
	w      io.Writer
	closed atomic.Bool
}

func (g *guardedWriter) Write(p []byte) (int, error) {
	if g.closed.Load() {
		return 0, errWriterClosed
	}
	return g.w.Write(p)
}

func (g *guardedWriter) Close() {
	g.closed.Store(true)
}

const (
	Ptime             = 20    // ms
	DefaultSampleRate = 16000 // Hz
)

func ValidSampleRate(rate int) bool {
	return rate == 8000 || rate == 16000 || rate == 48000
}

// Participant represents a single audio participant in the mixer.
type Participant struct {
	ID        string
	Reader    io.Reader
	Writer    io.Writer
	WriteOnly bool // playback sources have no writer output

	// incoming holds PCM frames read from this participant (read goroutine → mixer).
	incoming chan []byte
	// outgoing holds mixed-minus-self frames to send (mixer → write goroutine).
	outgoing chan []byte
	// done is closed when this participant is removed, stopping its goroutines.
	done chan struct{}
	// guard wraps Writer to prevent writes after removal.
	guard *guardedWriter

	// Muted prevents this participant's audio from contributing to the mix
	// and suppresses speaking events. Lock-free via atomic.
	Muted atomic.Bool

	// Deaf prevents this participant from receiving mixed-minus-self output.
	// The participant can still speak (contribute audio) but cannot hear others.
	Deaf atomic.Bool

	// Hears, when non-nil, is a whitelist of source participant IDs whose
	// audio is included in this participant's outgoing mix. nil means
	// full-mesh: hear every other participant (legacy behavior).
	// Guarded by Mixer.mu; read inside the mixTick snapshot.
	Hears map[string]struct{}

	// BypassRouting marks this participant as a room-wide audio source that
	// every listener hears regardless of their Hears whitelist. Used for
	// playback sources and inter-room bridge endpoints — sources that are
	// not legs and therefore never appear in the room's role-derived
	// allow-sets. Listener-side filters (Deaf) still apply.
	BypassRouting bool

	// inject receives PCM frames that are mixed into this participant's
	// output only (not heard by others). Used for per-leg playback while
	// the leg is in a room — the playback audio is added to the
	// mixed-minus-self output inside mixTick, avoiding channel contention.
	inject chan []byte

	// tap receives a copy of this participant's raw incoming PCM (for STT).
	tap io.Writer
	// outTap receives a copy of this participant's mixed-minus-self PCM (for stereo recording).
	outTap io.Writer
	// recordTap receives a copy of this participant's raw incoming PCM (for per-participant recording).
	// Separate from tap so STT/agent and multi-channel recording can run simultaneously.
	recordTap io.Writer
}

// Mixer performs multi-party audio mixing with mixed-minus-self.
// Inspired by the GetStream reference mixer: the mix tick never does IO.
// Dedicated read/write goroutines per participant handle all blocking IO,
// communicating with the mix loop via buffered channels.
type Mixer struct {
	mu           sync.Mutex
	participants map[string]*Participant
	stopCh       chan struct{}
	stopped      bool
	log          *slog.Logger

	sampleRate      int
	samplesPerFrame int
	frameSizeBytes  int

	// Optional tap for room recording — receives the full mix.
	tapMu  sync.Mutex
	tapOut io.Writer

	comfortNoise *comfortnoise.Generator
}

func New(log *slog.Logger, sampleRate int) *Mixer {
	if sampleRate == 0 {
		sampleRate = DefaultSampleRate
	}
	spf := sampleRate * Ptime / 1000
	return &Mixer{
		participants:    make(map[string]*Participant),
		stopCh:          make(chan struct{}),
		log:             log,
		sampleRate:      sampleRate,
		samplesPerFrame: spf,
		frameSizeBytes:  spf * 2,
		comfortNoise:    comfortnoise.NewGenerator(),
	}
}

func (m *Mixer) SampleRate() int      { return m.sampleRate }
func (m *Mixer) SamplesPerFrame() int { return m.samplesPerFrame }
func (m *Mixer) FrameSizeBytes() int  { return m.frameSizeBytes }

// SetComfortNoise enables or disables comfort noise injection during silence.
func (m *Mixer) SetComfortNoise(enabled bool) {
	m.comfortNoise.SetEnabled(enabled)
}

func (m *Mixer) SetTap(w io.Writer) {
	m.tapMu.Lock()
	defer m.tapMu.Unlock()
	m.tapOut = w
}

// SetParticipantTap sets an io.Writer that receives a copy of the participant's
// raw incoming PCM frames (before mixing). Used for per-participant SST.
func (m *Mixer) SetParticipantTap(id string, w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.tap = w
	}
}

// ClearParticipantTap removes the per-participant tap writer.
func (m *Mixer) ClearParticipantTap(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.tap = nil
	}
}

// SetParticipantOutTap sets an io.Writer that receives a copy of the
// mixed-minus-self PCM frames sent to this participant. Used for stereo
// leg recording (right channel = what the participant hears).
func (m *Mixer) SetParticipantOutTap(id string, w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.outTap = w
	}
}

// SetParticipantMuted sets the muted state for a participant. When muted,
// the participant's audio is replaced with silence in the mix and speaking
// events are suppressed. If the participant was speaking when muted, a
// SpeakingStopped event is emitted.
func (m *Mixer) SetParticipantMuted(id string, muted bool) {
	m.mu.Lock()
	p, ok := m.participants[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	p.Muted.Store(muted)
	m.mu.Unlock()
}

// SetParticipantDeaf sets the deaf state for a participant. When deaf,
// the participant does not receive mixed-minus-self output (cannot hear
// other participants). The participant can still speak.
func (m *Mixer) SetParticipantDeaf(id string, deaf bool) {
	m.mu.Lock()
	p, ok := m.participants[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	p.Deaf.Store(deaf)
}

// SetParticipantHears sets the per-listener source whitelist. Passing nil
// restores full-mesh behavior. Used by the room layer to materialize a
// routing-matrix decision into a flat set of allowed source IDs.
func (m *Mixer) SetParticipantHears(id string, hears map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.participants[id]
	if !ok {
		return
	}
	p.Hears = hears
}

// ApplyHearsBatch applies whitelists to multiple participants under a single
// mixer-mutex acquisition. mixTick snapshots Hears under the same mutex, so
// no tick can observe a partially-updated routing matrix. A nil value
// restores full mesh for that participant. Participants not present in
// updates are untouched. IDs in updates that no longer exist are ignored.
func (m *Mixer) ApplyHearsBatch(updates map[string]map[string]struct{}) {
	if len(updates) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, hears := range updates {
		if p, ok := m.participants[id]; ok {
			p.Hears = hears
		}
	}
}

// SetParticipantBypassRouting marks (or unmarks) a participant as a
// room-wide source that bypasses every listener's routing whitelist. Use
// for inter-room bridge endpoints and other non-leg sources added through
// AddParticipant. (AddPlaybackSource sets this automatically.)
func (m *Mixer) SetParticipantBypassRouting(id string, bypass bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.BypassRouting = bypass
	}
}

// ParticipantHears returns a snapshot of a participant's source whitelist.
// (nil, true) means full mesh; (nil, false) means the participant is not in
// this mixer. Intended for introspection and tests.
func (m *Mixer) ParticipantHears(id string) (map[string]struct{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.participants[id]
	if !ok {
		return nil, false
	}
	if p.Hears == nil {
		return nil, true
	}
	out := make(map[string]struct{}, len(p.Hears))
	for k := range p.Hears {
		out[k] = struct{}{}
	}
	return out, true
}

// InjectWriter returns an io.Writer that feeds PCM frames into the
// participant's private inject channel. The mixer adds injected frames
// to this participant's mixed-minus-self output only — other participants
// do not hear it. Used for per-leg playback while the leg is in a room.
// Returns nil if the participant is not found.
func (m *Mixer) InjectWriter(id string) io.Writer {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.participants[id]
	if !ok {
		return nil
	}
	return &injectWriter{ch: p.inject, done: p.done}
}

// injectWriter is an io.Writer that sends PCM frames into a participant's
// inject channel. Non-blocking: drops frames if the channel is full.
type injectWriter struct {
	ch   chan []byte
	done chan struct{}
}

func (w *injectWriter) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case <-w.done:
		return 0, io.ErrClosedPipe
	case w.ch <- frame:
		return len(p), nil
	default:
		// Drop frame rather than block the playback ticker.
		return len(p), nil
	}
}

// SetParticipantRecordTap sets an io.Writer that receives a copy of the
// participant's raw incoming PCM frames. Unlike SetParticipantTap (used by
// STT/agent), this tap is dedicated to per-participant recording and can
// coexist with the STT tap.
func (m *Mixer) SetParticipantRecordTap(id string, w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.recordTap = w
	}
}

// ClearParticipantRecordTap removes the per-participant recording tap writer.
func (m *Mixer) ClearParticipantRecordTap(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.recordTap = nil
	}
}

// ClearParticipantOutTap removes the per-participant outgoing tap writer.
func (m *Mixer) ClearParticipantOutTap(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.participants[id]; ok {
		p.outTap = nil
	}
}

func (m *Mixer) AddParticipant(id string, reader io.Reader, writer io.Writer) {
	gw := &guardedWriter{w: writer}
	p := &Participant{
		ID:       id,
		Reader:   reader,
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
	}

	m.mu.Lock()
	// Reset stop state so goroutines spawned below don't exit immediately.
	// This makes the mixer restartable after Stop() was called when the
	// last participant left.
	if m.stopped {
		m.stopCh = make(chan struct{})
		m.stopped = false
	}
	m.participants[id] = p
	m.mu.Unlock()

	go m.readLoop(p)
	go m.writeLoop(p)
}

// AddPlaybackSource adds a read-only source into the mix (e.g. audio file).
// It is mixed into everyone's output but receives no mixed-minus-self back.
// Playback is room-wide audio and bypasses the routing matrix.
func (m *Mixer) AddPlaybackSource(id string, reader io.Reader) {
	p := &Participant{
		ID:            id,
		Reader:        reader,
		WriteOnly:     true,
		BypassRouting: true,
		incoming:      make(chan []byte, 50),
		done:          make(chan struct{}),
	}

	m.mu.Lock()
	if m.stopped {
		m.stopCh = make(chan struct{})
		m.stopped = false
	}
	m.participants[id] = p
	m.mu.Unlock()

	go m.readLoop(p)
}

func (m *Mixer) RemoveParticipant(id string) {
	m.mu.Lock()
	p, ok := m.participants[id]
	if ok {
		delete(m.participants, id)
		if p.guard != nil {
			p.guard.Close() // prevent any further writes to the network
		}
		// Closing p.done alone does not unblock readLoop when it is parked
		// inside p.Reader.Read (the select runs only between iterations).
		// If the reader implements io.Closer, close it so the in-flight
		// Read returns and readLoop observes p.done on the next iteration.
		if rc, ok := p.Reader.(io.Closer); ok {
			_ = rc.Close()
		}
		close(p.done) // signal readLoop/writeLoop to stop
	}
	m.mu.Unlock()
}

func (m *Mixer) ParticipantCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.participants)
}

func (m *Mixer) Start() {
	m.mu.Lock()
	if m.stopped {
		m.stopCh = make(chan struct{})
		m.stopped = false
	}
	m.mu.Unlock()
	go m.mixLoop()
}

func (m *Mixer) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	m.mu.Unlock()
	close(m.stopCh)
}

// recoverParticipant logs a panic on a per-participant IO loop (readLoop or
// writeLoop) and removes the participant, mirroring SIPLeg.recoverLoop. The
// panicking goroutine is not restarted; RemoveParticipant is idempotent
// (mixer.go:391-409) so this is safe even if teardown is already underway.
func (m *Mixer) recoverParticipant(p *Participant, loop string) {
	if r := recover(); r != nil {
		m.log.Error(loop+" panic",
			"participant_id", p.ID,
			"panic", r,
			"stack", string(debug.Stack()),
		)
		m.RemoveParticipant(p.ID)
	}
}

// recoverTick logs a panic from a single mixTick invocation and lets the
// mix loop continue to the next tick. Must be deferred at the per-tick call
// site (mixLoop's ticker case), never around mixLoop itself — recovering the
// loop would stop the whole room instead of skipping one tick. Frame
// contents are deliberately not logged.
func (m *Mixer) recoverTick() {
	if r := recover(); r != nil {
		m.log.Error("mixTick panic, skipping tick",
			"panic", r,
			"stack", string(debug.Stack()),
		)
	}
}

// readLoop continuously reads PCM frames from a participant's Reader
// and buffers them for the mix loop. Blocks on IO (RTP receive).
func (m *Mixer) readLoop(p *Participant) {
	buf := make([]byte, m.frameSizeBytes)
	for {
		select {
		case <-m.stopCh:
			return
		case <-p.done:
			return
		default:
		}

		n, err := p.Reader.Read(buf)
		if err != nil {
			return
		}
		frame := make([]byte, n)
		copy(frame, buf[:n])

		// Buffer the frame. If full, drop oldest to prevent lag.
		select {
		case p.incoming <- frame:
		case <-p.done:
			return
		default:
			select {
			case <-p.incoming:
			default:
			}
			select {
			case p.incoming <- frame:
			case <-m.stopCh:
				return
			case <-p.done:
				return
			}
		}
	}
}

// writeLoop continuously drains mixed audio from the outgoing channel
// and writes to the participant's Writer. Blocks on IO (RTP send).
// This runs on its own goroutine so the mix tick never blocks.
func (m *Mixer) writeLoop(p *Participant) {
	for {
		select {
		case <-m.stopCh:
			return
		case <-p.done:
			return
		case frame := <-p.outgoing:
			if _, err := p.Writer.Write(frame); err != nil {
				if !errors.Is(err, errWriterClosed) {
					m.log.Debug("write error", "id", p.ID, "error", err)
				}
				return
			}
		}
	}
}

func (m *Mixer) mixLoop() {
	ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.mixTick()
		}
	}
}

// mixTick reads one frame from each participant, computes per-listener
// filtered mixes (honoring the routing matrix), and enqueues each output.
// Never blocks on IO.
func (m *Mixer) mixTick() {
	m.mu.Lock()
	parts := make([]*Participant, 0, len(m.participants))
	taps := make([]io.Writer, 0, len(m.participants))
	outTaps := make([]io.Writer, 0, len(m.participants))
	recordTaps := make([]io.Writer, 0, len(m.participants))
	hearsList := make([]map[string]struct{}, 0, len(m.participants))
	for _, p := range m.participants {
		parts = append(parts, p)
		taps = append(taps, p.tap)
		outTaps = append(outTaps, p.outTap)
		recordTaps = append(recordTaps, p.recordTap)
		hearsList = append(hearsList, p.Hears)
	}
	m.mu.Unlock()

	if len(parts) == 0 {
		return
	}

	// Collect latest frames from each participant (non-blocking)
	frames := make([][]int16, len(parts))
	muted := make([]bool, len(parts))
	for i, p := range parts {
		muted[i] = p.Muted.Load()
		var raw []byte
		select {
		case raw = <-p.incoming:
		default:
			raw = make([]byte, m.frameSizeBytes) // silence
		}
		// Write raw PCM to per-participant tap (for STT) before conversion.
		// Tap still receives audio even when muted (recording/STT of own audio).
		if taps[i] != nil {
			taps[i].Write(raw)
		}
		// Write raw PCM to per-participant recording tap (separate from STT tap).
		if recordTaps[i] != nil {
			recordTaps[i].Write(raw)
		}
		if muted[i] {
			frames[i] = make([]int16, m.samplesPerFrame) // silence — don't contribute to mix
		} else {
			frames[i] = bytesToSamples(raw)
		}
	}

	numSamples := m.samplesPerFrame

	// Room-level full-mix tap is independent of per-listener routing: it
	// captures everything happening in the room. Compute the global sum
	// only when the tap is actually attached.
	m.tapMu.Lock()
	tap := m.tapOut
	m.tapMu.Unlock()
	cnEnabled := m.comfortNoise.IsEnabled()
	if tap != nil {
		globalSum := make([]int32, numSamples)
		for _, f := range frames {
			for j := 0; j < numSamples && j < len(f); j++ {
				globalSum[j] += int32(f[j])
			}
		}
		if cnEnabled {
			hasAudio := false
			for j := 0; j < numSamples; j++ {
				if globalSum[j] != 0 {
					hasAudio = true
					break
				}
			}
			if !hasAudio {
				cnFrame := m.comfortNoise.Generate(numSamples)
				for j := 0; j < numSamples; j++ {
					globalSum[j] += int32(cnFrame[j])
				}
			}
		}
		fullMix := make([]byte, numSamples*2)
		for j := 0; j < numSamples; j++ {
			s := clamp16(globalSum[j])
			binary.LittleEndian.PutUint16(fullMix[j*2:], uint16(s))
		}
		tap.Write(fullMix)
	}

	// Per-listener filtered mix. Self is excluded by skipping k == i, so no
	// separate "minus-self" subtraction is needed. The routing matrix is
	// applied by checking each listener's Hears whitelist; nil whitelist
	// means full mesh (legacy behavior). Sources with BypassRouting (room
	// playback, inter-room bridges) are always heard regardless of the
	// whitelist.
	for i, p := range parts {
		if p.WriteOnly || p.Writer == nil || p.Deaf.Load() {
			continue
		}
		hears := hearsList[i]
		listenerSum := make([]int32, numSamples)
		for k, src := range parts {
			if k == i {
				continue
			}
			if hears != nil && !src.BypassRouting {
				if _, ok := hears[src.ID]; !ok {
					continue
				}
			}
			f := frames[k]
			for j := 0; j < numSamples && j < len(f); j++ {
				listenerSum[j] += int32(f[j])
			}
		}
		// Comfort noise per listener when their personal mix is silent.
		if cnEnabled {
			hasAudio := false
			for j := 0; j < numSamples; j++ {
				if listenerSum[j] != 0 {
					hasAudio = true
					break
				}
			}
			if !hasAudio {
				cnFrame := m.comfortNoise.Generate(numSamples)
				for j := 0; j < numSamples; j++ {
					listenerSum[j] += int32(cnFrame[j])
				}
			}
		}
		out := make([]byte, numSamples*2)
		for j := 0; j < numSamples; j++ {
			s := clamp16(listenerSum[j])
			binary.LittleEndian.PutUint16(out[j*2:], uint16(s))
		}
		// Mix in any privately-injected audio (per-leg playback).
		var injRaw []byte
		select {
		case injRaw = <-p.inject:
		default:
		}
		if injRaw != nil {
			injSamples := bytesToSamples(injRaw)
			for j := 0; j < numSamples && j < len(injSamples); j++ {
				cur := int16(binary.LittleEndian.Uint16(out[j*2:]))
				mixed := clamp16(int32(cur) + int32(injSamples[j]))
				binary.LittleEndian.PutUint16(out[j*2:], uint16(mixed))
			}
		}
		// Write mixed-minus-self to per-participant outgoing tap (for stereo recording).
		if outTaps[i] != nil {
			outTaps[i].Write(out)
		}
		// Non-blocking send. Skip if participant was removed or write goroutine
		// is behind (drop frame rather than stall the mixer).
		select {
		case <-p.done:
			// Participant removed since we took the snapshot; skip.
		case p.outgoing <- out:
		default:
			m.log.Debug("write buffer full, dropping frame", "id", p.ID)
		}
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

func clamp16(s int32) int16 {
	if s > math.MaxInt16 {
		return math.MaxInt16
	}
	if s < math.MinInt16 {
		return math.MinInt16
	}
	return int16(s)
}
