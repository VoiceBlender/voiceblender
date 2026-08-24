package mixer

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/comfortnoise"
	"github.com/VoiceBlender/voiceblender/internal/metrics"
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

const (
	// liveQueueDepth bounds both directions for a live participant. Ingress
	// overflow drops rather than blocks, because a network source cannot be
	// slowed down; egress is kept equally shallow so a stalled writer costs a
	// dropped frame instead of accumulating mouth-to-ear latency.
	liveQueueDepth = 3
	// playbackQueueDepth gives internally generated sources slack to absorb a
	// burst. They are mixer-clocked (see MarkMixerClocked), so the depth is
	// headroom, not a drop threshold.
	playbackQueueDepth = 50
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

	// ownerClosesEgress keeps the panic path from closing this writer, because
	// the owner's own teardown closes it. Set for legs: their writer is a
	// shared egress the leg may already have carried onto a fresh participant,
	// so closing it here would silence the live successor.
	ownerClosesEgress atomic.Bool

	// mixerClocked marks a source the mixer itself paces: readLoop blocks on a
	// full queue instead of dropping, so the producer runs on the mix tick and
	// clock drift costs latency rather than audio. Only for sources that can be
	// slowed down — never a live leg, and never a bridge endpoint, whose writer
	// is driven from the peer room's mixTick.
	mixerClocked atomic.Bool

	// A participant that has gone quiet and one whose source has stopped are
	// both silence downstream; only these tell them apart.
	framesRead atomic.Uint64
	starved    atomic.Uint64
	shortReads atomic.Uint64

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
	// stopCh is replaced whenever a stopped mixer restarts, so every loop
	// captures the channel it was spawned against rather than reading this
	// field: a loop still winding down from the previous run would otherwise
	// race the restart that swaps it.
	stopCh  chan struct{}
	stopped bool
	log     *slog.Logger

	sampleRate      int
	samplesPerFrame int
	frameSizeBytes  int

	// Optional tap for room recording — receives the full mix.
	tapMu  sync.Mutex
	tapOut io.Writer

	// onParticipantPanic lets the owner finish a teardown the mixer cannot see;
	// it is a leaf and knows nothing about legs, rooms or events.
	hookMu             sync.Mutex
	onParticipantPanic func(p *Participant, loop string)

	// tickPanics counts recovered mixTick panics; tickPanicLastLog holds the
	// unix-nanos of the last one logged. Atomic to keep recoverTick lock-free.
	tickPanics       atomic.Uint64
	tickPanicLastLog atomic.Int64

	comfortNoise *comfortnoise.Generator
}

// SetOnParticipantPanic registers a callback invoked once per participant whose
// readLoop or writeLoop panicked, after the mixer has removed it. loop is
// "readLoop" or "writeLoop". Passing nil disables the hook.
//
// fn gets the participant instance, not just its ID: the same ID may already
// carry a live replacement, so the pointer is the only handle on which instance
// died. fn is invoked with no mixer lock held, on the panicking goroutine as
// its last act — anything blocking must hand off to its own goroutine.
func (m *Mixer) SetOnParticipantPanic(fn func(p *Participant, loop string)) {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	m.onParticipantPanic = fn
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

// ComfortNoiseEnabled reports whether comfort noise injection is on.
func (m *Mixer) ComfortNoiseEnabled() bool {
	return m.comfortNoise.IsEnabled()
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

// AddParticipant registers id's audio path and returns the instance created
// for it. Callers that must later distinguish this instance from a
// replacement registered under the same ID — see removeParticipantIf — keep
// the returned pointer; the rest may ignore it.
func (m *Mixer) AddParticipant(id string, reader io.Reader, writer io.Writer) *Participant {
	gw := &guardedWriter{w: writer}
	p := &Participant{
		ID:       id,
		Reader:   reader,
		Writer:   gw,
		incoming: make(chan []byte, liveQueueDepth),
		outgoing: make(chan []byte, liveQueueDepth),
		inject:   make(chan []byte, liveQueueDepth),
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
	stopCh := m.stopCh
	m.mu.Unlock()

	go m.readLoop(p, stopCh)
	go m.writeLoop(p, stopCh)
	return p
}

// MarkOwnerClosesEgress records that p's owner closes the writer itself, so the
// mixer's panic path leaves it alone. See ownerClosesEgress.
func (p *Participant) MarkOwnerClosesEgress() {
	p.ownerClosesEgress.Store(true)
}

// MarkMixerClocked makes the mixer this source's clock. See mixerClocked.
// AddPlaybackSource does it already; call it for a duplex source that is still
// internally generated, such as a room agent's speak buffer.
func (p *Participant) MarkMixerClocked() {
	p.mixerClocked.Store(true)
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
		incoming:      make(chan []byte, playbackQueueDepth),
		done:          make(chan struct{}),
	}
	p.mixerClocked.Store(true)

	m.mu.Lock()
	if m.stopped {
		m.stopCh = make(chan struct{})
		m.stopped = false
	}
	m.participants[id] = p
	stopCh := m.stopCh
	m.mu.Unlock()

	go m.readLoop(p, stopCh)
}

func (m *Mixer) RemoveParticipant(id string) {
	m.removeParticipant(id)
}

// removeParticipant detaches whatever participant is registered under id now,
// and reports whether this call was the one that removed it. The map delete is
// the exactly-once gate that keeps close(p.done) from double-closing. A caller
// holding a specific instance wants removeParticipantIf instead.
func (m *Mixer) removeParticipant(id string) bool {
	m.mu.Lock()
	p, ok := m.participants[id]
	if ok {
		delete(m.participants, id)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}

	m.teardownParticipant(p)
	return true
}

// removeParticipantIf detaches p only if p is still the instance registered
// under p.ID, and reports whether this call removed it.
//
// AddParticipant overwrites m.participants[id] without stopping the previous
// instance's loops, so an orphaned loop can outlive its replacement. Matching
// on the pointer stops that orphan evicting the live successor and escalating,
// via the panic hook, into hanging up a healthy call.
func (m *Mixer) removeParticipantIf(p *Participant) bool {
	m.mu.Lock()
	cur, ok := m.participants[p.ID]
	if !ok || cur != p {
		m.mu.Unlock()
		return false
	}
	delete(m.participants, p.ID)
	m.mu.Unlock()

	m.teardownParticipant(p)
	return true
}

// closeWriterForPanic closes p's writer so an owner parked on the read end sees
// EOF and runs its own teardown. For a ws, agent, playback or TTS source that
// EOF is the only notification it will ever get — the mixer is a leaf and
// cannot call the API layer, and muting the guard tells the owner nothing.
//
// Panic path only. An ordinary removal leaves a live participant whose owner
// keeps using it: DetachLeg feeds MoveLeg, and closing the writer there would
// hand the next room an already-dead leg.
func (m *Mixer) closeWriterForPanic(p *Participant) {
	if p.ownerClosesEgress.Load() {
		return
	}
	if p.guard == nil {
		return
	}
	_ = closeWriter(p.guard.w)
}

// closeWriter closes w under either Close shape the mixer is handed, and is a
// no-op for a writer with neither. Testing io.Closer alone would silently skip
// the API layer's egress pipe writer, whose Close returns nothing — and whose
// owner has no other wakeup.
func closeWriter(w io.Writer) error {
	switch c := w.(type) {
	case io.Closer:
		return c.Close()
	case interface{ Close() }:
		c.Close()
	}
	return nil
}

// teardownParticipant releases p's IO and stops its loops. Reachable only via a
// map delete that returned ok, which is what makes close(p.done) exactly once.
//
// It deliberately does not close p's writer: a removal is not a hangup, and a
// leg's writer is its own egress. Only closeWriterForPanic does that. It runs
// outside m.mu because the Reader's Close is arbitrary third-party IO.
func (m *Mixer) teardownParticipant(p *Participant) {
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
	stopCh := m.stopCh
	m.mu.Unlock()
	go m.mixLoop(stopCh)
}

func (m *Mixer) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	stopCh := m.stopCh
	m.mu.Unlock()
	close(stopCh)
}

// recoverParticipant removes a participant whose IO loop panicked and notifies
// the owner. The goroutine is not restarted.
//
// Removal is identity-scoped so a stale loop cannot evict the successor
// registered under the same ID, and removeParticipantIf's bool is the
// exactly-once gate when both of a participant's loops panic.
func (m *Mixer) recoverParticipant(p *Participant, loop string) {
	if r := recover(); r != nil {
		metrics.RecordPanic("mixer", loop)
		m.log.Error(loop+" panic",
			"participant_id", p.ID,
			"panic", r,
			"stack", string(debug.Stack()),
		)
		if m.removeParticipantIf(p) {
			m.closeWriterForPanic(p)
			m.hookMu.Lock()
			hook := m.onParticipantPanic
			m.hookMu.Unlock()
			if hook != nil {
				hook(p, loop)
			}
		}
	}
}

// tickPanicLogInterval bounds how often a repeating mixTick panic is logged
// after the first occurrence.
const tickPanicLogInterval = 5 * time.Second

// recoverTick lets the mix loop continue to the next tick after a panic. Must
// be deferred per tick, never around mixLoop itself — recovering the loop would
// stop the whole room instead of skipping one frame.
//
// Only the first panic carries a stack: a deterministically bad frame panics
// every tick, which at Ptime cadence is ~50 stack traces a second forever. The
// counter is never re-armed, so a later unrelated panic stays stackless.
func (m *Mixer) recoverTick() {
	r := recover()
	if r == nil {
		return
	}

	metrics.RecordPanic("mixer", "mixTick")
	total := m.tickPanics.Add(1)
	if total == 1 {
		m.tickPanicLastLog.Store(time.Now().UnixNano())
		m.log.Error("mixTick panic, skipping tick",
			"panic", r,
			"stack", string(debug.Stack()),
		)
		return
	}

	now := time.Now().UnixNano()
	last := m.tickPanicLastLog.Load()
	if now-last < int64(tickPanicLogInterval) {
		return
	}
	// Whichever caller wins the swap logs the summary; the rest stay silent
	// until the next interval.
	if !m.tickPanicLastLog.CompareAndSwap(last, now) {
		return
	}
	m.log.Error("mixTick panic, skipping tick (repeating)",
		"panic", r,
		"total_panics", total,
	)
}

// fillFrame reads until buf holds a complete mix frame, or the source fails.
// A source may hand us any packet size, so a short read is normal and is read
// again rather than queued as if it were a whole frame. A partial frame at end
// of stream is discarded with the error.
func (m *Mixer) fillFrame(p *Participant, buf []byte, stopCh <-chan struct{}) error {
	filled := 0
	for filled < len(buf) {
		n, err := p.Reader.Read(buf[filled:])
		if err != nil {
			return err
		}
		if n == 0 {
			// io.Reader discourages (0, nil) but does not forbid it, and this
			// loop would otherwise spin on it forever without looking at stopCh.
			select {
			case <-stopCh:
				return errStopped
			case <-p.done:
				return errStopped
			default:
				runtime.Gosched()
			}
			continue
		}
		if filled+n < len(buf) {
			p.shortReads.Add(1)
		}
		filled += n
	}
	return nil
}

// errStopped ends a read loop because the participant is going away, not
// because its source failed.
var errStopped = errors.New("participant stopped")

func (m *Mixer) readLoop(p *Participant, stopCh <-chan struct{}) {
	defer m.recoverParticipant(p, "readLoop")
	buf := make([]byte, m.frameSizeBytes)
	for {
		select {
		case <-stopCh:
			return
		case <-p.done:
			return
		default:
		}

		// A whole frame per queue entry: the mix loop consumes exactly one per
		// tick, so a source packetised finer than Ptime would otherwise have the
		// surplus dropped below and the remainder mixed short.
		if err := m.fillFrame(p, buf, stopCh); err != nil {
			// The participant now contributes silence for the rest of the call,
			// which recording and transcription cannot distinguish from a quiet
			// party.
			select {
			case <-p.done:
			default:
				if errors.Is(err, errStopped) {
					return
				}
				m.log.Warn("mixer: participant read loop stopped, it now contributes silence",
					"participant_id", p.ID, "error", err, "frames_read", p.framesRead.Load())
			}
			return
		}
		p.framesRead.Add(1)
		frame := make([]byte, len(buf))
		copy(frame, buf)

		// A mixer-clocked source is slowed to the mix tick rather than trimmed.
		if p.mixerClocked.Load() {
			select {
			case p.incoming <- frame:
			case <-p.done:
				return
			case <-stopCh:
				return
			}
			continue
		}

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
			case <-stopCh:
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
func (m *Mixer) writeLoop(p *Participant, stopCh <-chan struct{}) {
	defer m.recoverParticipant(p, "writeLoop")
	for {
		select {
		case <-stopCh:
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

func (m *Mixer) mixLoop(stopCh <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			m.safeMixTick()
		}
	}
}

// safeMixTick runs mixTick with a per-tick recover, so a panic skips one frame
// while the room's ticker keeps running.
func (m *Mixer) safeMixTick() {
	defer m.recoverTick()
	m.mixTick()
}

// mixTick reads one frame from each participant, computes per-listener
// filtered mixes (honoring the routing matrix), and enqueues each output.
// Never blocks on IO.
//
// Every call out of this package — tap writes, resampling, channel sends —
// must stay outside the locked sections. The locks are taken without defer, so
// a panic under one would leave the mixer wedged and safeMixTick's recover
// would keep the room ticking on a mutex nobody can acquire.
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
			p.starved.Add(1)
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

// ParticipantFeed reports how many frames a participant's source has produced
// and how many mix intervals ran with nothing queued for it. A stopped source
// shows framesRead flat while starved climbs; a quiet party shows neither.
func (m *Mixer) ParticipantFeed(id string) (framesRead, starved, shortReads uint64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.participants[id]
	if !ok {
		return 0, 0, 0, false
	}
	return p.framesRead.Load(), p.starved.Load(), p.shortReads.Load(), true
}
