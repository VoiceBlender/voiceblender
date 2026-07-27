package api

import (
	"io"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// OwnsAMDTap mirrors the leg's single tap slot without clearing it, so a
// superseded driver checking ownership sees the same no-op it would on a
// SIPLeg. It is defined here rather than beside amdFakeLeg so the shared mock
// keeps satisfying the amdLeg interface without editing that file.
func (l *amdFakeLeg) OwnsAMDTap(w io.Writer) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.installed == w
}

// TestAMDDriver_SupersededMachineBeepPublishesNothing pins the ownership gate on
// the machine-plus-beep verdict. With beep detection enabled a machine verdict
// keeps its tap installed for the beep window, so it cannot claim ownership by
// clearing the tap the way a terminal verdict does. A frame already in flight
// can still drive a superseded driver's FSM to that machine verdict after a
// later AMD start replaced the tap; the verdict must not become a stale
// amd.result, and the driver must not keep listening on a tap it no longer owns.
func TestAMDDriver_SupersededMachineBeepPublishesNothing(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdFakeLeg{}

	params := amd.DefaultParams()
	params.GreetingDuration = 200 * time.Millisecond // reachable mid-feed
	params.TotalAnalysisTime = 5000 * time.Millisecond
	params.BeepTimeout = 3000 * time.Millisecond // machine verdict opens a beep window

	d1 := newAMDTestDriver(s, l, params)

	// d1 accumulates speech but stops short of the greeting threshold.
	speech := amdSpeechFrame()
	for i := 0; i < 5; i++ {
		if _, err := d1.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if rec.count(events.AMDResult) != 0 {
		t.Fatal("no verdict should have been reached yet")
	}

	// A second AMD start supersedes d1, taking over the leg's tap.
	d2 := newAMDTestDriver(s, l, params)

	// The frame the readLoop snapshotted before the swap now lands on d1 and
	// drives its FSM past the greeting threshold to a machine verdict.
	for i := 0; i < 10; i++ {
		if _, err := d1.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := rec.count(events.AMDResult); got != 0 {
		t.Fatalf("a superseded machine+beep verdict must not publish, got %d", got)
	}
	if l.liveTap() != io.Writer(d2) {
		t.Fatal("a superseded driver must not disturb the live driver's tap")
	}

	// The superseded driver must have stopped rather than staying in the beep
	// window on a tap it no longer owns.
	d1.mu.Lock()
	stopped := d1.done && !d1.beeping
	d1.mu.Unlock()
	if !stopped {
		t.Fatal("a superseded machine+beep verdict must stop the driver, not keep it listening")
	}

	// The live driver still owns the leg: its own machine verdict publishes and
	// keeps its tap installed for the beep window.
	for i := 0; i < 15; i++ {
		if _, err := d2.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := rec.count(events.AMDResult); got != 1 {
		t.Fatalf("expected the live driver to publish exactly 1 amd.result, got %d", got)
	}
	if got := rec.resultData(); len(got) != 1 || got[0].Result != string(amd.ResultMachine) {
		t.Fatalf("expected a single machine verdict, got %+v", got)
	}
	if l.clearedCount() != 0 {
		t.Error("the live driver's tap must stay installed through its beep window")
	}
}
