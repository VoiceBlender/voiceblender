package mixer

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// panicAfterReader panics on the (limit+1)th call to Read; before that it
// returns a fixed frame. Used to simulate a participant whose Reader.Read
// panics on a malformed frame after N good reads.
type panicAfterReader struct {
	n     int32
	limit int32
	frame []byte
}

func (r *panicAfterReader) Read(p []byte) (int, error) {
	if atomic.AddInt32(&r.n, 1) > r.limit {
		panic("simulated read panic")
	}
	copy(p, r.frame)
	return len(p), nil
}

// silenceReader always returns a fixed (silent) frame, never panics. Used as
// the "other side" of a participant whose Writer is the one under test.
type silenceReader struct {
	frame []byte
}

func (r *silenceReader) Read(p []byte) (int, error) {
	copy(p, r.frame)
	return len(p), nil
}

// panicAfterWriter panics on the (limit+1)th call to Write; before that it
// accepts the write silently.
type panicAfterWriter struct {
	n     int32
	limit int32
}

func (w *panicAfterWriter) Write(p []byte) (int, error) {
	if atomic.AddInt32(&w.n, 1) > w.limit {
		panic("simulated write panic")
	}
	return len(p), nil
}

// panicOnceWriter panics on its first Write call and behaves normally
// afterward. Used to force exactly one bad mixTick.
type panicOnceWriter struct {
	fired atomic.Bool
}

func (w *panicOnceWriter) Write(p []byte) (int, error) {
	if w.fired.CompareAndSwap(false, true) {
		panic("simulated tap panic")
	}
	return len(p), nil
}

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls cond until it's true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMixer_ReadLoopPanicRemovesParticipant verifies that a panic inside a
// participant's Reader.Read is recovered, removes exactly that participant,
// and does not take down the mixer — other participants keep receiving
// mixed output afterward.
func TestMixer_ReadLoopPanicRemovesParticipant(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	victimReader := &panicAfterReader{limit: 2, frame: make([]byte, fsz)}
	m.AddParticipant("victim", victimReader, io.Discard)

	survivorReader, survivorFeeder := io.Pipe()
	survivorCapture := &captureWriter{}
	m.AddParticipant("survivor", survivorReader, survivorCapture)
	stopFeed := make(chan struct{})
	defer close(stopFeed)
	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeed:
				return
			case <-ticker.C:
				if _, err := survivorFeeder.Write(silence); err != nil {
					return
				}
			}
		}
	}()
	defer survivorFeeder.Close()

	waitFor(t, 2*time.Second, "victim removed after read panic", func() bool {
		return m.ParticipantCount() == 1
	})

	before := len(survivorCapture.Bytes())
	time.Sleep(200 * time.Millisecond)
	after := len(survivorCapture.Bytes())
	if after <= before {
		t.Fatalf("survivor stopped receiving audio after victim's read panic: before=%d after=%d", before, after)
	}
}

// TestMixer_WriteLoopPanicRemovesParticipant verifies that a panic inside a
// participant's Writer.Write is recovered, removes exactly that
// participant, and the mixer keeps ticking for the rest.
func TestMixer_WriteLoopPanicRemovesParticipant(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	victimReader := &silenceReader{frame: make([]byte, fsz)}
	victimWriter := &panicAfterWriter{limit: 2}
	m.AddParticipant("victim", victimReader, victimWriter)

	survivorReader, survivorFeeder := io.Pipe()
	survivorCapture := &captureWriter{}
	m.AddParticipant("survivor", survivorReader, survivorCapture)
	stopFeed := make(chan struct{})
	defer close(stopFeed)
	go func() {
		silence := make([]byte, fsz)
		ticker := time.NewTicker(time.Duration(Ptime) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeed:
				return
			case <-ticker.C:
				if _, err := survivorFeeder.Write(silence); err != nil {
					return
				}
			}
		}
	}()
	defer survivorFeeder.Close()

	waitFor(t, 2*time.Second, "victim removed after write panic", func() bool {
		return m.ParticipantCount() == 1
	})

	before := len(survivorCapture.Bytes())
	time.Sleep(200 * time.Millisecond)
	after := len(survivorCapture.Bytes())
	if after <= before {
		t.Fatalf("survivor stopped receiving audio after victim's write panic: before=%d after=%d", before, after)
	}
}

// TestMixer_SafeMixTickRecoversAndContinues asserts the per-tick contract of
// safeMixTick in isolation: a panic on one call is swallowed with no output
// for that tick, and the very next call completes normally and produces
// output. Calling safeMixTick directly keeps this deterministic — no ticker,
// no timing.
//
// Scope, deliberately: this covers the helper, NOT the room-survival
// guarantee. It says nothing about whether mixLoop routes its ticker case
// through safeMixTick — rewiring the ticker to a bare mixTick() leaves this
// test green while the process regains its crash. That wiring is what
// TestMixer_MixLoopTickerPanicKeepsRoomRunning exists to catch; do not read
// a green result here as proof the room survives.
func TestMixer_SafeMixTickRecoversAndContinues(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	fsz := m.frameSizeBytes
	spf := m.samplesPerFrame

	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       "listener",
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
	}
	// tap.Write is called early in mixTick, before the per-listener mix loop
	// that produces output — so a panic here proves the whole tick is
	// skipped, not just this participant's slice of it.
	panicTap := &panicOnceWriter{}
	p.tap = panicTap

	m.mu.Lock()
	m.participants["listener"] = p
	m.mu.Unlock()

	frame := make([]byte, fsz)

	// Tick 1: tap panics. safeMixTick must recover; no output for this tick.
	p.incoming <- frame
	m.safeMixTick()

	select {
	case <-p.outgoing:
		t.Fatal("expected no output on the panicking tick")
	default:
	}

	// Tick 2: tap no longer panics (fired once); mixTick must complete
	// normally and produce output — proving mixLoop's ticker case survived
	// the previous panic.
	p.incoming <- frame
	m.safeMixTick()

	select {
	case out := <-p.outgoing:
		if len(out) != spf*2 {
			t.Fatalf("unexpected output length = %d, want %d", len(out), spf*2)
		}
	default:
		t.Fatal("expected output on the tick after recovery")
	}
}

// TestMixer_MixLoopTickerPanicKeepsRoomRunning drives the real mix loop —
// Start(), the real 20ms ticker, a panic on a real tick — rather than
// calling safeMixTick by hand. This is what pins mixLoop's ticker case to
// safeMixTick: if it were rewired to a bare mixTick(), the panic would
// unwind mixLoop and take the process down with it, so this test cannot
// pass without the recover actually being on the live path.
func TestMixer_MixLoopTickerPanicKeepsRoomRunning(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	fsz := m.frameSizeBytes
	capture := &captureWriter{}
	m.AddParticipant("listener", &silenceReader{frame: make([]byte, fsz)}, capture)

	// tap.Write runs early in mixTick, before any output is produced, so the
	// tick that hits it is skipped whole.
	panicTap := &panicOnceWriter{}
	m.SetParticipantTap("listener", panicTap)

	m.Start()
	defer m.Stop()

	waitFor(t, 2*time.Second, "a real ticker tick to reach the panicking tap", func() bool {
		return panicTap.fired.Load()
	})
	waitFor(t, 2*time.Second, "recoverTick to record the tick panic", func() bool {
		return m.tickPanics.Load() >= 1
	})

	// Baseline after the panicking tick: any growth from here is produced by
	// ticks the loop ran *after* it recovered.
	before := len(capture.Bytes())
	waitFor(t, 2*time.Second, "a later ticker tick to still produce output", func() bool {
		return len(capture.Bytes()) > before
	})

	if got := m.ParticipantCount(); got != 1 {
		t.Errorf("participant count = %d, want 1 — a tick panic must not evict anyone", got)
	}
}

// TestMixer_ParticipantPanicHookFires verifies the owner is notified when a
// participant's IO loop panics — removing the participant from the mixer's
// map is only half of teardown, and without this the leg behind it would be
// left connected but deaf and mute.
func TestMixer_ParticipantPanicHookFires(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	type call struct{ id, loop string }
	calls := make(chan call, 4)
	m.SetOnParticipantPanic(func(id, loop string) {
		calls <- call{id, loop}
	})

	victimReader := &panicAfterReader{limit: 1, frame: make([]byte, m.frameSizeBytes)}
	m.AddParticipant("victim", victimReader, io.Discard)

	select {
	case c := <-calls:
		if c.id != "victim" {
			t.Errorf("hook participant id = %q, want victim", c.id)
		}
		if c.loop != "readLoop" {
			t.Errorf("hook loop = %q, want readLoop", c.loop)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the participant panic hook to fire")
	}
}

// TestMixer_ParticipantPanicHookFiresExactlyOnce is the assertion that buys
// removeParticipant's returned bool: when both IO loops panic for the same
// participant, the map delete under m.mu elects a single teardown owner, so
// the owner is notified once — and close(p.done) is never double-closed.
func TestMixer_ParticipantPanicHookFiresExactlyOnce(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	var fired atomic.Int32
	m.SetOnParticipantPanic(func(id, loop string) {
		fired.Add(1)
		if id != "victim" {
			t.Errorf("hook participant id = %q, want victim", id)
		}
	})

	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       "victim",
		Reader:   &silenceReader{frame: make([]byte, m.frameSizeBytes)},
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
	}
	m.mu.Lock()
	m.participants["victim"] = p
	m.mu.Unlock()

	// Both of this participant's IO loops fail at once. Driving
	// recoverParticipant directly makes the race deterministic: a real
	// readLoop panic closes p.done, which would usually retire writeLoop
	// before it ever got the chance to panic too.
	var wg sync.WaitGroup
	for _, loop := range []string{"readLoop", "writeLoop"} {
		wg.Add(1)
		go func(loop string) {
			defer wg.Done()
			defer m.recoverParticipant(p, loop)
			panic("simulated " + loop + " panic")
		}(loop)
	}
	wg.Wait()

	if got := fired.Load(); got != 1 {
		t.Fatalf("hook fired %d times, want exactly 1", got)
	}
	if m.ParticipantCount() != 0 {
		t.Fatalf("participant count = %d, want 0", m.ParticipantCount())
	}
}

// TestMixer_StaleLoopPanicDoesNotEvictSuccessor pins teardown to the
// participant *instance* rather than to its ID.
//
// AddParticipant overwrites m.participants[id] without stopping the previous
// instance's loops, and the mixer's unblock trick (closing a Reader that is an
// io.Closer) misses a rate-mismatched leg entirely — NewResampleReader wraps it
// in a *resampleReader, which has no Close. So an orphaned readLoop can stay
// parked in Read long after its replacement is live. When that orphan finally
// panics, keying removal on the ID would delete the healthy successor and fire
// the hook for it, escalating into a RemoveLeg + Hangup on a good call.
//
// Driving recoverParticipant directly keeps this deterministic — no real
// panicking reader, no timing.
func TestMixer_StaleLoopPanicDoesNotEvictSuccessor(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)

	var fired atomic.Int32
	m.SetOnParticipantPanic(func(id, loop string) {
		fired.Add(1)
	})

	// pOld is the orphaned instance: registered under "victim", then replaced.
	gwOld := &guardedWriter{w: io.Discard}
	pOld := &Participant{
		ID:       "victim",
		Reader:   &silenceReader{frame: make([]byte, m.frameSizeBytes)},
		Writer:   gwOld,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gwOld,
	}
	m.mu.Lock()
	m.participants["victim"] = pOld
	m.mu.Unlock()

	// The re-add: overwrites "victim" with a live instance, orphaning pOld.
	m.AddParticipant("victim", &silenceReader{frame: make([]byte, m.frameSizeBytes)}, io.Discard)

	m.mu.Lock()
	pNew := m.participants["victim"]
	m.mu.Unlock()
	if pNew == pOld {
		t.Fatal("AddParticipant did not replace the registered instance")
	}
	defer m.removeParticipantIf(pNew)

	// The orphan's parked Read finally panics.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer m.recoverParticipant(pOld, "readLoop")
		panic("stale loop")
	}()
	<-done

	if got := fired.Load(); got != 0 {
		t.Errorf("hook fired %d times for a stale loop, want 0 — the owner must not be told to tear down a live call", got)
	}
	if got := m.ParticipantCount(); got != 1 {
		t.Fatalf("participant count = %d, want 1 — the successor must survive", got)
	}

	m.mu.Lock()
	cur := m.participants["victim"]
	m.mu.Unlock()
	if cur != pNew {
		t.Fatal("the stale loop's panic evicted the live successor")
	}

	select {
	case <-pNew.done:
		t.Fatal("the successor's done was closed by the stale loop's teardown")
	default:
	}
}

// TestMixer_ParticipantPanicHookNilIsNoop verifies a mixer with no hook
// registered (the mixer is usable standalone) still recovers and removes.
func TestMixer_ParticipantPanicHookNilIsNoop(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	victimReader := &panicAfterReader{limit: 1, frame: make([]byte, m.frameSizeBytes)}
	m.AddParticipant("victim", victimReader, io.Discard)

	waitFor(t, 2*time.Second, "victim removed with no hook registered", func() bool {
		return m.ParticipantCount() == 0
	})
}

// alwaysPanicWriter panics on every Write, simulating a deterministically
// bad frame that makes every single mix tick panic.
type alwaysPanicWriter struct{}

func (alwaysPanicWriter) Write(p []byte) (int, error) { panic("simulated tap panic") }

// countingHandler records how many log records carried a "stack" attribute
// and how many were emitted in total.
type countingHandler struct {
	mu          sync.Mutex
	total       int
	withStack   int
	lastMessage string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total++
	h.lastMessage = r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "stack" {
			h.withStack++
			return false
		}
		return true
	})
	return nil
}

func (h *countingHandler) counts() (total, withStack int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total, h.withStack
}

// TestMixer_TickPanicLogsAreRateLimited verifies that a deterministically
// panicking frame does not flood the log. A bad frame recurs on every tick,
// so an unbounded recoverTick would emit a multi-KB stack ~50x a second
// forever. Exactly one stack-bearing line may be emitted; the rest collapse
// into at most one stackless summary per interval.
func TestMixer_TickPanicLogsAreRateLimited(t *testing.T) {
	h := &countingHandler{}
	m := New(slog.New(h), DefaultSampleRate)

	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       "listener",
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
		tap:      alwaysPanicWriter{},
	}
	m.mu.Lock()
	m.participants["listener"] = p
	m.mu.Unlock()

	// Far more consecutive panicking ticks than a 5s interval could ever
	// permit a second log line for.
	const ticks = 500
	for i := 0; i < ticks; i++ {
		m.safeMixTick()
	}

	total, withStack := h.counts()
	if withStack != 1 {
		t.Errorf("stack-bearing log lines = %d, want exactly 1 (got %d lines total)", withStack, total)
	}
	if total != 1 {
		t.Errorf("log lines = %d, want 1 — %d ticks inside one interval must collapse", total, ticks)
	}
	if got := m.tickPanics.Load(); got != ticks {
		t.Errorf("tickPanics = %d, want %d — every panic must still be counted", got, ticks)
	}
}

// TestMixer_TickPanicSummaryLogsAfterInterval verifies the rate limit lets a
// stackless summary through once the interval has elapsed, so an ongoing
// fault stays visible rather than going silent after the first line.
func TestMixer_TickPanicSummaryLogsAfterInterval(t *testing.T) {
	h := &countingHandler{}
	m := New(slog.New(h), DefaultSampleRate)

	gw := &guardedWriter{w: io.Discard}
	p := &Participant{
		ID:       "listener",
		Writer:   gw,
		incoming: make(chan []byte, 3),
		outgoing: make(chan []byte, 3),
		inject:   make(chan []byte, 3),
		done:     make(chan struct{}),
		guard:    gw,
		tap:      alwaysPanicWriter{},
	}
	m.mu.Lock()
	m.participants["listener"] = p
	m.mu.Unlock()

	m.safeMixTick() // first panic: full stack
	m.safeMixTick() // suppressed

	if total, withStack := h.counts(); total != 1 || withStack != 1 {
		t.Fatalf("after 2 ticks: total=%d withStack=%d, want 1 and 1", total, withStack)
	}

	// Age the last-log stamp past the interval rather than sleeping for it.
	m.tickPanicLastLog.Store(time.Now().Add(-2 * tickPanicLogInterval).UnixNano())
	m.safeMixTick()

	total, withStack := h.counts()
	if total != 2 {
		t.Errorf("log lines = %d, want 2 — a summary is due after the interval", total)
	}
	if withStack != 1 {
		t.Errorf("stack-bearing lines = %d, want 1 — the summary must not carry a stack", withStack)
	}
	h.mu.Lock()
	msg := h.lastMessage
	h.mu.Unlock()
	if msg != "mixTick panic, skipping tick (repeating)" {
		t.Errorf("summary message = %q", msg)
	}
}

// TestMixer_ReadLoopPanicGoroutinesExit verifies that after a read-panic
// removes a participant, that participant's readLoop/writeLoop goroutines
// actually exit (via the close(p.done) path in RemoveParticipant) rather
// than leaking.
func TestMixer_ReadLoopPanicGoroutinesExit(t *testing.T) {
	m := New(testLog(), DefaultSampleRate)
	m.Start()
	defer m.Stop()

	fsz := m.frameSizeBytes

	// Let the mixer settle before taking the goroutine-count baseline.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	victimReader := &panicAfterReader{limit: 1, frame: make([]byte, fsz)}
	m.AddParticipant("victim", victimReader, io.Discard)

	waitFor(t, 2*time.Second, "victim removed after read panic", func() bool {
		return m.ParticipantCount() == 0
	})

	waitFor(t, 2*time.Second, "victim's readLoop/writeLoop goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= before
	})
}
