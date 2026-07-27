package api

import (
	"io"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestAMDDriver_SupersededWritePublishesNothing pins the ownership gate on the
// frame-driven verdict. The leg's readLoop snapshots the AMD tap under RLock
// and writes to it with the lock released, so a frame already in flight can
// reach a superseded driver after a later AMD start replaced the tap. That
// frame must not turn into a second amd.result for the leg.
func TestAMDDriver_SupersededWritePublishesNothing(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdFakeLeg{}

	params := amd.DefaultParams()
	params.GreetingDuration = 200 * time.Millisecond // reachable mid-feed
	params.TotalAnalysisTime = 5000 * time.Millisecond

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
		t.Fatalf("a superseded driver must not publish a verdict from a late frame, got %d", got)
	}
	if l.liveTap() != io.Writer(d2) {
		t.Fatal("a superseded driver must not clear the live driver's tap")
	}

	// The live driver still owns the leg and publishes the one verdict.
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
}

// TestAMDDriver_SupersededBeepWritePublishesNothing covers the same gate on the
// beep window: a superseded driver mid-window owns no amd.beep for the leg
// either.
func TestAMDDriver_SupersededBeepWritePublishesNothing(t *testing.T) {
	s := newTestServer(t)
	rec := recordAMDEvents(t, s)
	l := &amdFakeLeg{}

	params := amd.DefaultParams()
	params.GreetingDuration = 200 * time.Millisecond
	params.TotalAnalysisTime = 5000 * time.Millisecond
	params.BeepTimeout = 3000 * time.Millisecond

	d1 := newAMDTestDriver(s, l, params)

	// Speech past the greeting threshold → machine, which opens d1's beep
	// window and leaves its tap installed.
	speech := amdSpeechFrame()
	for i := 0; i < 15; i++ {
		if _, err := d1.Write(speech); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := rec.count(events.AMDResult); got != 1 {
		t.Fatalf("expected the machine verdict to publish, got %d amd.result", got)
	}

	// A second AMD start supersedes d1 mid-beep-window.
	d2 := newAMDTestDriver(s, l, params)

	// A late frame carrying the beep tone lands on d1.
	beep := amdBeepFrame()
	for i := 0; i < 10; i++ {
		if _, err := d1.Write(beep); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := rec.count(events.AMDBeep); got != 0 {
		t.Fatalf("a superseded driver must not publish a beep, got %d", got)
	}
	if l.liveTap() != io.Writer(d2) {
		t.Fatal("a superseded driver must not clear the live driver's tap")
	}
}
