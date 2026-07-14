package api

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// amdFakeLeg stands in for the slice of a leg the AMD driver touches, so the
// driver's concurrency contract can be tested without a live SIP dialog.
type amdFakeLeg struct {
	mu      sync.Mutex
	cleared int
}

func (l *amdFakeLeg) ID() string    { return "leg-amd" }
func (l *amdFakeLeg) AppID() string { return "app-amd" }

func (l *amdFakeLeg) ClearAMDTap() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleared++
}

func (l *amdFakeLeg) clearedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleared
}

// amdRecorder counts AMD events published on the bus. The bus invokes handlers
// synchronously on the publishing goroutine, so it locks.
type amdRecorder struct {
	mu     sync.Mutex
	counts map[events.EventType]int
}

func recordAMDEvents(t *testing.T, s *Server) *amdRecorder {
	t.Helper()
	r := &amdRecorder{counts: map[events.EventType]int{}}
	unsub := s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.AMDResult && e.Type != events.AMDBeep {
			return
		}
		r.mu.Lock()
		r.counts[e.Type]++
		r.mu.Unlock()
	})
	t.Cleanup(unsub)
	return r
}

func (r *amdRecorder) count(typ events.EventType) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[typ]
}

// amdSilentFrame returns 20 ms of 16 kHz silence.
func amdSilentFrame() []byte { return make([]byte, 640) }

// amdSpeechFrame returns 20 ms of 16 kHz speech-like tone, well above the AMD
// speech threshold.
func amdSpeechFrame() []byte { return sineFrame(16000, 440, 20) }

// amdBeepFrame returns 20 ms of the 1000 Hz voicemail beep tone.
func amdBeepFrame() []byte { return sineFrame(16000, 1000, 20) }

func newAMDTestDriver(s *Server, l amdLeg, params amd.Params) *amdDriver {
	return &amdDriver{s: s, l: l, analyzer: amd.New(params)}
}

// TestAMDDriver_PublishesExactlyOneResultUnderConcurrency drives Feed from the
// readLoop's role while the deadline goroutine fires OnDeadline concurrently.
// Under -race this proves the mutex serializes the analyzer; the count proves
// the sync.Once gates the terminal publish so the leak fix cannot trade itself
// for a duplicate amd.result.
func TestAMDDriver_PublishesExactlyOneResultUnderConcurrency(t *testing.T) {
	params := amd.DefaultParams()
	params.GreetingDuration = 200 * time.Millisecond // reachable mid-feed
	params.TotalAnalysisTime = 5000 * time.Millisecond

	// Repeat so the scheduler explores different interleavings of the two
	// goroutines racing for the terminal publish.
	for i := 0; i < 50; i++ {
		s := newTestServer(t)
		rec := recordAMDEvents(t, s)
		l := &amdFakeLeg{}
		d := newAMDTestDriver(s, l, params)

		ctx, cancel := context.WithCancel(context.Background())

		var wg sync.WaitGroup
		gate := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			// A zero budget fires the deadline immediately, so OnDeadline races
			// the frames being fed below.
			d.watch(ctx, 0)
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			frame := amdSpeechFrame()
			for j := 0; j < 40; j++ {
				if _, err := d.Write(frame); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()

		close(gate)
		wg.Wait()
		cancel()

		if got := rec.count(events.AMDResult); got != 1 {
			t.Fatalf("iteration %d: expected exactly 1 amd.result, got %d", i, got)
		}
	}
}

// TestAMDDriver_WatchExitsOnLegTeardown is the regression for the goroutine
// leak: the analysis goroutine used to park forever on a read once the leg
// stopped feeding it. The watch goroutine selects only on a timer and the leg
// context, so teardown mid-analysis must return the count to baseline.
func TestAMDDriver_WatchExitsOnLegTeardown(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdFakeLeg{}

	params := amd.DefaultParams()
	// Budgets long enough that only teardown — never the timer — can end this.
	params.TotalAnalysisTime = time.Hour
	params.BeepTimeout = time.Hour
	d := newAMDTestDriver(s, l, params)

	ctx, cancel := context.WithCancel(context.Background())

	// Let any lazily-started server goroutines settle before snapshotting.
	waitForGoroutines(t, runtime.NumGoroutine())
	baseline := runtime.NumGoroutine()

	go d.watch(ctx, params.TotalAnalysisTime+params.BeepTimeout)

	// Analysis is genuinely in flight: frames fed, no verdict reached.
	frame := amdSpeechFrame()
	for i := 0; i < 10; i++ {
		if _, err := d.Write(frame); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if rec.count(events.AMDResult) != 0 {
		t.Fatal("no verdict should have been reached yet")
	}

	// The leg tears down mid-analysis.
	cancel()

	if !waitForGoroutines(t, baseline) {
		t.Fatalf("goroutine count did not return to baseline %d, still %d — watch leaked",
			baseline, runtime.NumGoroutine())
	}
	if l.clearedCount() == 0 {
		t.Error("expected the AMD tap to be cleared on teardown")
	}
	if got := rec.count(events.AMDResult); got != 0 {
		t.Errorf("expected no amd.result for a torn-down leg, got %d", got)
	}
	if got := rec.count(events.AMDBeep); got != 0 {
		t.Errorf("expected no amd.beep for a torn-down leg, got %d", got)
	}
}

// waitForGoroutines polls until the goroutine count drops to at most want,
// reporting whether it got there.
func waitForGoroutines(t *testing.T, want int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= want
}

// TestAMDDriver_MachineKeepsTapUntilBeepResolves pins the corrected beep
// behavior: a machine verdict with beep detection enabled must leave the tap
// installed so the beep window still receives frames.
func TestAMDDriver_MachineKeepsTapUntilBeepResolves(t *testing.T) {
	params := amd.DefaultParams()
	params.GreetingDuration = 500 * time.Millisecond
	params.TotalAnalysisTime = 5000 * time.Millisecond
	params.BeepTimeout = 3000 * time.Millisecond

	t.Run("beep detected", func(t *testing.T) {
		s := newTestServer(t)
		rec := recordAMDEvents(t, s)
		l := &amdFakeLeg{}
		d := newAMDTestDriver(s, l, params)

		// 600 ms of speech crosses the 500 ms greeting threshold → machine.
		speech := amdSpeechFrame()
		for i := 0; i < 30; i++ {
			if _, err := d.Write(speech); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if got := rec.count(events.AMDResult); got != 1 {
			t.Fatalf("expected the machine verdict to publish, got %d amd.result", got)
		}
		if l.clearedCount() != 0 {
			t.Fatal("tap must stay installed through the beep window")
		}

		// Now the voicemail beep arrives.
		beep := amdBeepFrame()
		for i := 0; i < 10; i++ {
			if _, err := d.Write(beep); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if got := rec.count(events.AMDBeep); got != 1 {
			t.Fatalf("expected exactly 1 amd.beep, got %d", got)
		}
		if l.clearedCount() == 0 {
			t.Error("tap should be cleared once the beep is confirmed")
		}
		if got := rec.count(events.AMDResult); got != 1 {
			t.Errorf("beep window must not republish a result, got %d", got)
		}
	})

	t.Run("beep window times out", func(t *testing.T) {
		s := newTestServer(t)
		rec := recordAMDEvents(t, s)
		l := &amdFakeLeg{}

		short := params
		short.BeepTimeout = 200 * time.Millisecond
		d := newAMDTestDriver(s, l, short)

		speech := amdSpeechFrame()
		for i := 0; i < 30; i++ {
			if _, err := d.Write(speech); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if l.clearedCount() != 0 {
			t.Fatal("tap must stay installed through the beep window")
		}

		// Silence outlasting the beep timeout resolves the window without a beep.
		silent := amdSilentFrame()
		for i := 0; i < 20; i++ {
			if _, err := d.Write(silent); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		if got := rec.count(events.AMDBeep); got != 0 {
			t.Errorf("expected no amd.beep on timeout, got %d", got)
		}
		if l.clearedCount() == 0 {
			t.Error("tap should be cleared once the beep window times out")
		}
	})
}

// TestAMDDriver_DeadlinePublishesAccumulatedVerdict covers the stalled-RTP
// path: frames stop arriving, and the wall-clock deadline still produces a
// verdict from the state Feed accumulated.
func TestAMDDriver_DeadlinePublishesAccumulatedVerdict(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdFakeLeg{}

	params := amd.DefaultParams()
	params.TotalAnalysisTime = 5000 * time.Millisecond
	d := newAMDTestDriver(s, l, params)

	// 200 ms of silence, well under initial_silence_timeout, then the feed
	// stalls — no frame-driven verdict is ever reached.
	silent := amdSilentFrame()
	for i := 0; i < 10; i++ {
		if _, err := d.Write(silent); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if rec.count(events.AMDResult) != 0 {
		t.Fatal("no verdict should have been reached yet")
	}

	d.watch(context.Background(), 0)

	if got := rec.count(events.AMDResult); got != 1 {
		t.Fatalf("expected the deadline to publish exactly 1 amd.result, got %d", got)
	}
	if l.clearedCount() == 0 {
		t.Error("expected the tap to be cleared once the deadline fires")
	}

	// Frames arriving after the terminal state must not publish again.
	for i := 0; i < 300; i++ {
		if _, err := d.Write(silent); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := rec.count(events.AMDResult); got != 1 {
		t.Errorf("expected no republish after the deadline, got %d", got)
	}
}
