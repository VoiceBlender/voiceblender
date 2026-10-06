package api

import (
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

func TestPendingRoomJoins(t *testing.T) {
	var p pendingRoomJoins
	if got := p.legsFor("r1"); len(got) != 0 {
		t.Fatalf("zero value legsFor = %v, want empty", got)
	}
	p.clear("missing")

	p.set("a", "r1")
	p.set("b", "r1")
	p.set("c", "r2")
	if got := p.legsFor("r1"); len(got) != 2 {
		t.Errorf("legsFor(r1) = %v, want 2 legs", got)
	}
	if got := p.legsFor("r2"); len(got) != 1 || got[0] != "c" {
		t.Errorf("legsFor(r2) = %v, want [c]", got)
	}

	p.clear("a")
	if got := p.legsFor("r1"); len(got) != 1 || got[0] != "b" {
		t.Errorf("legsFor(r1) after clear = %v, want [b]", got)
	}
}

// disconnectReasons records the reason of every leg.disconnected by leg ID.
func disconnectReasons(s *Server) func(legID string) []string {
	var mu sync.Mutex
	got := map[string][]string{}
	s.Bus.Subscribe(func(e events.Event) {
		d, ok := e.Data.(*events.LegDisconnectedData)
		if !ok {
			return
		}
		mu.Lock()
		got[d.LegID] = append(got[d.LegID], d.CDR.Reason)
		mu.Unlock()
	})
	return func(legID string) []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got[legID]...)
	}
}

func addPendingLeg(t *testing.T, s *Server, roomID string) leg.Leg {
	t.Helper()
	l := leg.NewWebSocketOutboundPendingLeg(16000, false, slog.Default())
	s.LegMgr.Add(l)
	s.pendingRooms.set(l.ID(), roomID)
	return l
}

func TestDeleteRoom_CancelsPendingLegs(t *testing.T) {
	s := newTestServer(t)
	reasons := disconnectReasons(s)
	for _, id := range []string{"r1", "r2"} {
		if _, err := s.RoomMgr.Create(id, "", 16000); err != nil {
			t.Fatalf("create room %s: %v", id, err)
		}
	}
	pending := addPendingLeg(t, s, "r1")
	other := addPendingLeg(t, s, "r2")

	if err := s.doDeleteRoom("r1"); err != nil {
		t.Fatalf("doDeleteRoom: %v", err)
	}

	if pending.State() != leg.StateHungUp {
		t.Errorf("pending leg state = %s, want hung_up", pending.State())
	}
	if _, ok := s.LegMgr.Get(pending.ID()); ok {
		t.Error("pending leg still registered after its room was deleted")
	}
	if got := reasons(pending.ID()); len(got) != 1 || got[0] != "room_deleted" {
		t.Errorf("pending leg disconnect reasons = %v, want [room_deleted]", got)
	}
	if got := s.pendingRooms.legsFor("r1"); len(got) != 0 {
		t.Errorf("pending entries left for r1: %v", got)
	}

	if other.State() != leg.StateRinging {
		t.Errorf("leg pending for another room: state = %s, want ringing", other.State())
	}
	if got := reasons(other.ID()); len(got) != 0 {
		t.Errorf("leg pending for another room disconnected: %v", got)
	}
}

func TestJoinPendingRoom_RoomGone(t *testing.T) {
	s := newTestServer(t)
	reasons := disconnectReasons(s)
	l := addPendingLeg(t, s, "gone")

	if s.joinPendingRoom(l, "gone") {
		t.Fatal("joinPendingRoom = true for a deleted room, want false")
	}
	if l.State() != leg.StateHungUp {
		t.Errorf("leg state = %s, want hung_up", l.State())
	}
	if got := reasons(l.ID()); len(got) != 1 || got[0] != "room_deleted" {
		t.Errorf("disconnect reasons = %v, want [room_deleted]", got)
	}
}

// A join that fails while the room still exists must not end the call.
func TestJoinPendingRoom_RoomPresent(t *testing.T) {
	s := newTestServer(t)
	reasons := disconnectReasons(s)
	if _, err := s.RoomMgr.Create("r1", "", 16000); err != nil {
		t.Fatalf("create room: %v", err)
	}
	l := addPendingLeg(t, s, "r1") // still ringing, so AddLeg refuses it

	if !s.joinPendingRoom(l, "r1") {
		t.Fatal("joinPendingRoom = false while the room exists, want true")
	}
	if l.State() != leg.StateRinging {
		t.Errorf("leg state = %s, want ringing", l.State())
	}
	if got := reasons(l.ID()); len(got) != 0 {
		t.Errorf("leg disconnected: %v", got)
	}
	if got := s.pendingRooms.legsFor("r1"); len(got) != 0 {
		t.Errorf("pending entry not cleared after the join attempt: %v", got)
	}
}

func TestRingTimeout(t *testing.T) {
	tests := []struct {
		name string
		body string
		want time.Duration
	}{
		{"omitted uses the 60s default", `{}`, 60 * time.Second},
		{"explicit value wins", `{"ring_timeout":5}`, 5 * time.Second},
		{"explicit zero disables", `{"ring_timeout":0}`, 0},
		{"negative disables", `{"ring_timeout":-1}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			var req CreateLegRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := s.ringTimeout(req); got != tt.want {
				t.Errorf("ringTimeout = %v, want %v", got, tt.want)
			}
		})
	}
}
