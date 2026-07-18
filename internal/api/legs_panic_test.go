package api

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/agent"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// panicReader panics on its first Read, standing in for a leg whose inbound
// audio path blows up inside the mixer's readLoop.
type panicReader struct{}

func (panicReader) Read(p []byte) (int, error) { panic("simulated read panic") }

// panicAudioLeg is an apiMockLeg with a real, panicking audio path.
// apiMockLeg hardcodes AudioReader() to nil, which the mixer never reads from,
// so the overrides are what let the panic come through the real mixer — the
// whole point of the test.
type panicAudioLeg struct {
	*apiMockLeg
}

func (p *panicAudioLeg) AudioReader() io.Reader { return panicReader{} }
func (p *panicAudioLeg) AudioWriter() io.Writer { return io.Discard }
func (p *panicAudioLeg) SampleRate() int        { return 16000 }

// TestPanickedLegPublishesDisconnect drives a mixer IO panic through the real
// wiring NewServer installs — real leg.Manager, real room.Manager, real bus —
// and asserts the API layer finishes the teardown the room layer started.
//
// Before the callback existed, such a leg emitted leg.left_room and nothing
// else: no CDR, an unended span, and it stayed in the leg manager forever, so
// GET /v1/legs/{id} kept serving a dead leg.
func TestPanickedLegPublishesDisconnect(t *testing.T) {
	s := newTestServer(t)

	var mu sync.Mutex
	var gotReason string
	var gotDisconnect bool
	s.Bus.Subscribe(func(e events.Event) {
		if e.Type != events.LegDisconnected {
			return
		}
		d, ok := e.Data.(*events.LegDisconnectedData)
		if !ok || d.LegID != "panic-leg" {
			return
		}
		mu.Lock()
		gotDisconnect = true
		gotReason = d.CDR.Reason
		mu.Unlock()
	})

	l := &panicAudioLeg{apiMockLeg: &apiMockLeg{id: "panic-leg", createdAt: time.Now()}}
	s.LegMgr.Add(l)
	if _, err := s.RoomMgr.Create("r1", "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}
	if err := s.RoomMgr.AddLeg("r1", "panic-leg"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := gotDisconnect
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no leg.disconnected published for a leg killed by a mixer IO panic")
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	reason := gotReason
	mu.Unlock()
	if reason != "mixer_panic" {
		t.Fatalf("cdr.reason = %q, want %q", reason, "mixer_panic")
	}

	// cleanupLeg must have run too, or the leg leaks in the leg manager and
	// GET /v1/legs/{id} keeps serving it.
	if _, ok := s.LegMgr.Get("panic-leg"); ok {
		t.Fatal("panicked leg still registered in the leg manager")
	}
}

// stoppableProvider is a do-nothing agent.Provider that records whether Stop
// was called, standing in for a live vendor session on a room agent.
type stoppableProvider struct {
	mu      sync.Mutex
	stopped bool
}

func (p *stoppableProvider) Start(context.Context, io.Reader, io.Writer, string,
	agent.Options, agent.Callbacks) error {
	return nil
}
func (p *stoppableProvider) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}
func (p *stoppableProvider) Running() bool          { return false }
func (p *stoppableProvider) ConversationID() string { return "" }
func (p *stoppableProvider) InjectMessage(context.Context, string) error {
	return nil
}

func (p *stoppableProvider) wasStopped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopped
}

// TestPanickedLastLegRunsRoomScopedCleanup pins the room-scoped half of the
// mixer-panic teardown.
//
// The room layer clears the leg's RoomID as part of removing it, so by the time
// the API hook runs, cleanupLeg's room block no longer fires. Every other
// leg-removal path reaches the room-scoped cleanup through that block, which
// left the panic path as the only one that skipped it: the room's agent stayed
// in roomAgents with its vendor session live, against a room with no legs left.
// Nothing reaps it — Manager.Delete has one caller, DELETE /v1/rooms/{id} — so
// a billed websocket stayed open until someone deleted the room by hand.
//
// The hook now carries the roomID for exactly this reason.
func TestPanickedLastLegRunsRoomScopedCleanup(t *testing.T) {
	const roomID = "r-panic-cleanup"

	s := newTestServer(t)

	if _, err := s.RoomMgr.Create(roomID, "", 0); err != nil {
		t.Fatalf("Create room: %v", err)
	}

	// Stand a room agent up directly in the registry: the assertion is about
	// the teardown path, not about how the agent got there.
	prov := &stoppableProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan struct{})
	go func() { <-ctx.Done(); close(cancelled) }()

	roomAgents.Lock()
	roomAgents.m[roomID] = &agentInfo{
		session:  prov,
		sourceID: "agent-" + roomID,
		roomID:   roomID,
		cancel:   cancel,
	}
	roomAgents.Unlock()
	t.Cleanup(func() {
		roomAgents.Lock()
		delete(roomAgents.m, roomID)
		roomAgents.Unlock()
		cancel()
	})

	l := &panicAudioLeg{apiMockLeg: &apiMockLeg{id: "panic-last-leg", createdAt: time.Now()}}
	s.LegMgr.Add(l)
	if err := s.RoomMgr.AddLeg(roomID, "panic-last-leg"); err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		roomAgents.Lock()
		_, still := roomAgents.m[roomID]
		roomAgents.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("room agent still registered after the room's last leg died " +
				"in a mixer IO panic: its vendor session leaks until the room is " +
				"deleted by hand")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !prov.wasStopped() {
		t.Fatal("room agent deregistered but its vendor session was never stopped")
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("room agent's context was never cancelled")
	}
}
