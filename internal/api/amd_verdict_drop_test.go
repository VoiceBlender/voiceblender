package api

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// amdRaceLeg is an amdLeg whose ownership check can inject work at the exact
// moment the readLoop consults it. A machine-plus-beep verdict releases d.mu,
// checks ownership, then publishes; the deadline race lives in that window, so
// running watch from OwnsAMDTap reproduces the losing interleaving
// deterministically without a sleep.
type amdRaceLeg struct {
	mu        sync.Mutex
	installed io.Writer
	cleared   int
	// onOwnsCheck, if set, runs once just before the ownership read, standing
	// in for watch firing between the machine verdict and its publish.
	onOwnsCheck func()
	fired       bool
}

func (l *amdRaceLeg) ID() string    { return "leg-amd-race" }
func (l *amdRaceLeg) AppID() string { return "app-amd-race" }

func (l *amdRaceLeg) ClearAMDTapIf(w io.Writer) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.installed != w {
		return false
	}
	l.installed = nil
	l.cleared++
	return true
}

func (l *amdRaceLeg) OwnsAMDTap(w io.Writer) bool {
	l.mu.Lock()
	hook := l.onOwnsCheck
	if hook != nil && !l.fired {
		l.fired = true
		// Release the leg lock so the injected watch can clear the tap through
		// ClearAMDTapIf, exactly as a real concurrent watch would.
		l.mu.Unlock()
		hook()
		l.mu.Lock()
	}
	owns := l.installed == w
	l.mu.Unlock()
	return owns
}

func (l *amdRaceLeg) clearedCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cleared
}

func newAMDRaceDriver(s *Server, l *amdRaceLeg, params amd.Params) *amdDriver {
	d := &amdDriver{s: s, l: l, analyzer: amd.New(params)}
	d.tap = d
	l.mu.Lock()
	l.installed = d
	l.mu.Unlock()
	return d
}

func amdMachineBeepParams() amd.Params {
	params := amd.DefaultParams()
	params.GreetingDuration = 200 * time.Millisecond // reachable mid-feed
	params.TotalAnalysisTime = 5000 * time.Millisecond
	params.BeepTimeout = 3000 * time.Millisecond // a machine verdict opens a beep window
	return params
}

// TestAMDDriver_MachineBeepVerdictNotDroppedByDeadline is the regression for the
// dropped verdict: the readLoop reaches a machine-plus-beep verdict, sets its
// beep-window state, and releases d.mu; if watch's deadline fires before the
// verdict's ownership-gated publish, watch must not clear the tap and swallow
// the publish, leaving neither goroutine to emit the result. Here the ownership
// check runs watch in that precise window, so exactly one amd.result must still
// be published.
func TestAMDDriver_MachineBeepVerdictNotDroppedByDeadline(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdRaceLeg{}
	d := newAMDRaceDriver(s, l, amdMachineBeepParams())

	// When the readLoop checks ownership on its machine verdict, fire watch's
	// deadline in the same goroutine — the losing order in which watch races
	// ahead of the pending publish.
	l.onOwnsCheck = func() { d.watch(context.Background(), 0) }

	speech := amdSpeechFrame()
	for i := 0; i < 40; i++ {
		if _, err := d.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := rec.count(events.AMDResult); got != 1 {
		t.Fatalf("machine verdict raced by the deadline must publish exactly 1 amd.result, got %d", got)
	}
	got := rec.resultData()
	if len(got) != 1 || got[0].Result != string(amd.ResultMachine) {
		t.Fatalf("expected a single machine verdict, got %+v", got)
	}
}

// TestAMDDriver_MachineBeepVerdictPublishesOnceThenDeadline pins the normal
// ordering: with no deadline racing the publish, the machine verdict publishes
// exactly once and keeps its tap installed for the beep window, and a later
// deadline expiring the window must not republish.
func TestAMDDriver_MachineBeepVerdictPublishesOnceThenDeadline(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdRaceLeg{}
	d := newAMDRaceDriver(s, l, amdMachineBeepParams())

	speech := amdSpeechFrame()
	for i := 0; i < 40; i++ {
		if _, err := d.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := rec.count(events.AMDResult); got != 1 {
		t.Fatalf("expected the machine verdict to publish exactly 1 amd.result, got %d", got)
	}
	if l.clearedCount() != 0 {
		t.Fatal("tap must stay installed through the beep window")
	}

	// The beep window stalls and the budget expires: no beep, no republish.
	d.watch(context.Background(), 0)

	if got := rec.count(events.AMDResult); got != 1 {
		t.Errorf("the beep-window deadline must not republish, got %d", got)
	}
	if got := rec.count(events.AMDBeep); got != 0 {
		t.Errorf("expected no amd.beep when the window expires unresolved, got %d", got)
	}
	if l.clearedCount() == 0 {
		t.Error("expected the tap to be cleared once the beep window expires")
	}
}
