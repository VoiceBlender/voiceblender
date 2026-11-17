package lifecycle

import (
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

type recorder struct {
	mu  sync.Mutex
	got []*events.InstanceStatusChangedData
}

func (r *recorder) handle(e events.Event) {
	if e.Type != events.InstanceStatusChanged {
		return
	}
	r.mu.Lock()
	r.got = append(r.got, e.Data.(*events.InstanceStatusChangedData))
	r.mu.Unlock()
}

func (r *recorder) transitions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.got))
	for i, d := range r.got {
		out[i] = d.From + ">" + d.To
	}
	return out
}

func newTest(t *testing.T, cfg Config) (*Controller, *recorder) {
	t.Helper()
	bus := events.NewBus("test")
	rec := &recorder{}
	bus.Subscribe(rec.handle)
	return New(bus, cfg), rec
}

func dur(d time.Duration) *time.Duration { return &d }

func waitState(t *testing.T, c *Controller, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", c.State(), want)
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStartsInStartingAndBecomesReady(t *testing.T) {
	c, rec := newTest(t, Config{})
	if c.State() != Starting {
		t.Fatalf("initial state = %s", c.State())
	}
	if c.AcceptingSIP() {
		t.Fatal("SIP accepted while starting")
	}
	c.MarkReady()
	c.MarkReady()
	if !c.AcceptingSIP() || !c.AcceptingNew() {
		t.Fatal("ready instance refuses work")
	}
	if got := rec.transitions(); !equal(got, []string{"starting>ready"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestDrainWithLegsWaitsThenDrains(t *testing.T) {
	c, rec := newTest(t, Config{EmptyGrace: 30 * time.Millisecond})
	c.MarkReady()
	c.LegCountChanged(2)

	if _, err := c.StartDrain(DrainOptions{Source: SourceAPI}); err != nil {
		t.Fatal(err)
	}
	if c.State() != Draining || c.AcceptingSIP() || !c.AcceptingNew() {
		t.Fatalf("draining: state=%s sip=%v new=%v", c.State(), c.AcceptingSIP(), c.AcceptingNew())
	}
	c.LegCountChanged(1)
	c.LegCountChanged(0)
	waitState(t, c, Drained)
	if c.AcceptingNew() {
		t.Fatal("drained instance still accepts new legs")
	}
	if closed(c.ShutdownRequested()) {
		t.Fatal("API drain requested shutdown")
	}
	if got := rec.transitions(); !equal(got, []string{"starting>ready", "ready>draining", "draining>drained"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestGraceCancelledByNewLeg(t *testing.T) {
	c, _ := newTest(t, Config{EmptyGrace: 80 * time.Millisecond})
	c.MarkReady()
	c.StartDrain(DrainOptions{Source: SourceAPI})
	time.Sleep(40 * time.Millisecond)
	c.LegCountChanged(1)
	time.Sleep(80 * time.Millisecond)
	if c.State() != Draining {
		t.Fatalf("state = %s, want draining", c.State())
	}
	c.LegCountChanged(0)
	waitState(t, c, Drained)
}

func TestLegAfterDrainedReturnsToDraining(t *testing.T) {
	c, _ := newTest(t, Config{})
	c.MarkReady()
	c.StartDrain(DrainOptions{Source: SourceAPI})
	if c.State() != Drained {
		t.Fatalf("state = %s, want drained with zero grace", c.State())
	}
	c.LegCountChanged(1)
	if c.State() != Draining {
		t.Fatalf("state = %s, want draining", c.State())
	}
}

func TestExitDrainRequestsShutdownWhenDrained(t *testing.T) {
	c, _ := newTest(t, Config{EmptyGrace: 10 * time.Millisecond})
	c.MarkReady()
	c.LegCountChanged(1)
	c.StartDrain(DrainOptions{Source: SourceSignal, ExitWhenDrained: true})
	if closed(c.ShutdownRequested()) {
		t.Fatal("shutdown requested with a live leg")
	}
	c.LegCountChanged(0)
	select {
	case <-c.ShutdownRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown not requested")
	}
}

func TestSignalDuringAPIDrainSetsExitAndKeepsDeadline(t *testing.T) {
	c, _ := newTest(t, Config{DefaultDrainTimeout: time.Hour})
	c.MarkReady()
	c.LegCountChanged(1)
	st, _ := c.StartDrain(DrainOptions{Source: SourceAPI, Timeout: dur(time.Minute)})
	first := st.DrainDeadline
	st, err := c.StartDrain(DrainOptions{Source: SourceSignal, ExitWhenDrained: true})
	if err != nil {
		t.Fatal(err)
	}
	if !st.ExitWhenDrained || st.DrainSource != SourceAPI {
		t.Fatalf("status = %+v", st)
	}
	if !st.DrainDeadline.Equal(first) {
		t.Fatalf("deadline changed: %v -> %v", first, st.DrainDeadline)
	}
}

func TestDeadlineRequestsShutdown(t *testing.T) {
	c, _ := newTest(t, Config{})
	c.MarkReady()
	c.LegCountChanged(1)
	st, _ := c.StartDrain(DrainOptions{Source: SourceAPI, Timeout: dur(30 * time.Millisecond)})
	if st.DrainDeadline.IsZero() {
		t.Fatal("no deadline set")
	}
	select {
	case <-c.ShutdownRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not request shutdown")
	}
}

func TestDefaultDeadlineAndZeroMeansNoLimit(t *testing.T) {
	c, _ := newTest(t, Config{DefaultDrainTimeout: time.Hour})
	c.MarkReady()
	c.LegCountChanged(1)
	st, _ := c.StartDrain(DrainOptions{Source: SourceSignal})
	if st.DrainDeadline.IsZero() {
		t.Fatal("default deadline not applied")
	}
	st, _ = c.StartDrain(DrainOptions{Source: SourceAPI, Timeout: dur(0)})
	if !st.DrainDeadline.IsZero() {
		t.Fatalf("timeout 0 kept deadline %v", st.DrainDeadline)
	}
}

func TestRepeatedStartDrainUpdatesDeadline(t *testing.T) {
	c, _ := newTest(t, Config{})
	c.MarkReady()
	c.LegCountChanged(1)
	c.StartDrain(DrainOptions{Source: SourceAPI, Timeout: dur(time.Hour)})
	c.StartDrain(DrainOptions{Source: SourceAPI, Timeout: dur(30 * time.Millisecond)})
	select {
	case <-c.ShutdownRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("updated deadline did not fire")
	}
}

func TestCancelDrainClearsEverything(t *testing.T) {
	c, rec := newTest(t, Config{})
	c.MarkReady()
	c.LegCountChanged(1)
	c.StartDrain(DrainOptions{Source: SourceSignal, ExitWhenDrained: true, Timeout: dur(40 * time.Millisecond)})
	st, err := c.CancelDrain()
	if err != nil {
		t.Fatal(err)
	}
	if st.State != Ready || st.ExitWhenDrained || !st.DrainDeadline.IsZero() || st.DrainSource != "" {
		t.Fatalf("status after cancel = %+v", st)
	}
	time.Sleep(80 * time.Millisecond)
	if closed(c.ShutdownRequested()) {
		t.Fatal("cancelled drain still fired its deadline")
	}
	c.LegCountChanged(0)
	if c.State() != Ready {
		t.Fatalf("state = %s", c.State())
	}
	if got := rec.transitions(); !equal(got, []string{"starting>ready", "ready>draining", "draining>ready"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestCancelWhenNotDraining(t *testing.T) {
	c, _ := newTest(t, Config{})
	c.MarkReady()
	if _, err := c.CancelDrain(); err != ErrNotDraining {
		t.Fatalf("err = %v, want ErrNotDraining", err)
	}
}

func TestStoppingIsFinal(t *testing.T) {
	c, rec := newTest(t, Config{})
	c.MarkReady()
	if !c.BeginStop() {
		t.Fatal("first BeginStop returned false")
	}
	if c.BeginStop() {
		t.Fatal("second BeginStop returned true")
	}
	if _, err := c.StartDrain(DrainOptions{Source: SourceAPI}); err != ErrStopping {
		t.Fatalf("StartDrain err = %v", err)
	}
	if _, err := c.CancelDrain(); err != ErrStopping {
		t.Fatalf("CancelDrain err = %v", err)
	}
	if c.AcceptingNew() || c.AcceptingSIP() {
		t.Fatal("stopping instance accepts work")
	}
	if got := rec.transitions(); !equal(got, []string{"starting>ready", "ready>stopping"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestDrainHooksRunInOrder(t *testing.T) {
	c, _ := newTest(t, Config{})
	c.MarkReady()
	c.LegCountChanged(1)
	var mu sync.Mutex
	var calls []string
	done := make(chan struct{}, 4)
	c.SetDrainHooks(
		func() {
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			calls = append(calls, "start")
			mu.Unlock()
			done <- struct{}{}
		},
		func() { mu.Lock(); calls = append(calls, "cancel"); mu.Unlock(); done <- struct{}{} },
	)
	c.StartDrain(DrainOptions{Source: SourceAPI})
	c.CancelDrain()
	c.StartDrain(DrainOptions{Source: SourceAPI})
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("hooks did not run")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !equal(calls, []string{"start", "cancel", "start"}) {
		t.Fatalf("hook order = %v", calls)
	}
}

func TestEventCarriesCountsAndDeadline(t *testing.T) {
	c, rec := newTest(t, Config{RoomCount: func() int { return 3 }})
	c.MarkReady()
	c.LegCountChanged(2)
	c.StartDrain(DrainOptions{Source: SourceSignal, ExitWhenDrained: true, Timeout: dur(time.Hour)})
	rec.mu.Lock()
	defer rec.mu.Unlock()
	last := rec.got[len(rec.got)-1]
	if last.Legs != 2 || last.Rooms != 3 || last.DrainSource != SourceSignal || !last.ExitWhenDrained || last.DrainDeadline == "" {
		t.Fatalf("event = %+v", last)
	}
}
