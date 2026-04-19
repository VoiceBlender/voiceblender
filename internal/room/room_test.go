package room

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// mockLeg implements leg.Leg for testing Room and Manager.
type mockLeg struct {
	id         string
	legType    leg.LegType
	state      leg.LegState
	roomID     string
	muted      bool
	deaf       bool
	acceptDTMF bool
	createdAt  time.Time
}

func newMockLeg(id string) *mockLeg {
	return &mockLeg{
		id:        id,
		legType:   leg.TypeSIPInbound,
		state:     leg.StateConnected,
		createdAt: time.Now(),
	}
}

func (m *mockLeg) ID() string                                   { return m.id }
func (m *mockLeg) Type() leg.LegType                            { return m.legType }
func (m *mockLeg) State() leg.LegState                          { return m.state }
func (m *mockLeg) SampleRate() int                              { return 16000 }
func (m *mockLeg) AudioReader() io.Reader                       { return nil }
func (m *mockLeg) AudioWriter() io.Writer                       { return nil }
func (m *mockLeg) OnDTMF(func(digit rune))                      {}
func (m *mockLeg) SendDTMF(ctx context.Context, d string) error { return nil }
func (m *mockLeg) Hangup(ctx context.Context) error             { m.state = leg.StateHungUp; return nil }
func (m *mockLeg) Answer(ctx context.Context) error             { return nil }
func (m *mockLeg) Context() context.Context                     { return context.Background() }
func (m *mockLeg) RoomID() string                               { return m.roomID }
func (m *mockLeg) SetRoomID(id string)                          { m.roomID = id }
func (m *mockLeg) AppID() string                                { return "" }
func (m *mockLeg) SetAppID(string)                              {}
func (m *mockLeg) IsMuted() bool                                { return m.muted }
func (m *mockLeg) SetMuted(v bool)                              { m.muted = v }
func (m *mockLeg) IsDeaf() bool                                 { return m.deaf }
func (m *mockLeg) SetDeaf(v bool)                               { m.deaf = v }
func (m *mockLeg) AcceptDTMF() bool                             { return m.acceptDTMF }
func (m *mockLeg) SetAcceptDTMF(v bool)                         { m.acceptDTMF = v }
func (m *mockLeg) SetSpeakingTap(w io.Writer)                   {}
func (m *mockLeg) ClearSpeakingTap()                            {}
func (m *mockLeg) IsHeld() bool                                 { return false }
func (m *mockLeg) CreatedAt() time.Time                         { return m.createdAt }
func (m *mockLeg) AnsweredAt() time.Time                        { return time.Time{} }
func (m *mockLeg) SIPHeaders() map[string]string                { return nil }
func (m *mockLeg) RTPStats() leg.RTPStats                       { return leg.RTPStats{} }

func newTestBus() *events.Bus  { return events.NewBus("test") }
func newTestLog() *slog.Logger { return slog.Default() }

// --- Room tests ---

func TestRoom_AddLeg(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	l := newMockLeg("leg-1")

	r.AddLeg(l)

	if r.ParticipantCount() != 1 {
		t.Errorf("count = %d, want 1", r.ParticipantCount())
	}
	if l.RoomID() != "r1" {
		t.Errorf("room_id = %q, want r1", l.RoomID())
	}
}

func TestRoom_AddMultipleLegs(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	r.AddLeg(newMockLeg("a"))
	r.AddLeg(newMockLeg("b"))
	r.AddLeg(newMockLeg("c"))

	if r.ParticipantCount() != 3 {
		t.Errorf("count = %d, want 3", r.ParticipantCount())
	}
}

func TestRoom_RemoveLeg(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	l := newMockLeg("leg-1")
	r.AddLeg(l)

	r.RemoveLeg("leg-1")

	if r.ParticipantCount() != 0 {
		t.Errorf("count = %d, want 0", r.ParticipantCount())
	}
	if l.RoomID() != "" {
		t.Errorf("room_id = %q, want empty", l.RoomID())
	}
}

func TestRoom_RemoveLeg_Nonexistent(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	r.RemoveLeg("nonexistent") // should not panic
}

func TestRoom_DetachLeg(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	l := newMockLeg("leg-1")
	r.AddLeg(l)

	detached, ok := r.DetachLeg("leg-1")
	if !ok {
		t.Fatal("DetachLeg returned false")
	}
	if detached.ID() != "leg-1" {
		t.Errorf("ID = %q", detached.ID())
	}
	if r.ParticipantCount() != 0 {
		t.Errorf("count = %d, want 0", r.ParticipantCount())
	}
	if l.RoomID() != "" {
		t.Errorf("room_id = %q, want empty", l.RoomID())
	}
}

func TestRoom_DetachLeg_NotFound(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	_, ok := r.DetachLeg("nonexistent")
	if ok {
		t.Error("expected false")
	}
}

func TestRoom_Participants(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	r.AddLeg(newMockLeg("a"))
	r.AddLeg(newMockLeg("b"))

	parts := r.Participants()
	if len(parts) != 2 {
		t.Fatalf("len = %d, want 2", len(parts))
	}
}

func TestRoom_Close(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	l := newMockLeg("leg-1")
	r.AddLeg(l)

	r.Close()

	if r.ParticipantCount() != 0 {
		t.Errorf("count = %d, want 0", r.ParticipantCount())
	}
	if l.RoomID() != "" {
		t.Errorf("room_id = %q, want empty", l.RoomID())
	}
}

func TestRoom_Mixer(t *testing.T) {
	r := NewRoom("r1", "", 0, newTestLog())
	if r.Mixer() == nil {
		t.Fatal("expected non-nil mixer")
	}
}

// --- Manager tests ---

func TestManager_Create(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	r, err := mgr.Create("", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ID == "" {
		t.Error("expected auto-generated ID")
	}
}

func TestManager_Create_CustomID(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	r, err := mgr.Create("my-room", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.ID != "my-room" {
		t.Errorf("ID = %q, want my-room", r.ID)
	}
}

func TestManager_Create_Duplicate(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	mgr.Create("r1", "", 0)
	_, err := mgr.Create("r1", "", 0)
	if err == nil {
		t.Error("expected error for duplicate room")
	}
}

func TestManager_Get(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	mgr.Create("r1", "", 0)

	r, ok := mgr.Get("r1")
	if !ok {
		t.Fatal("Get returned false")
	}
	if r.ID != "r1" {
		t.Errorf("ID = %q", r.ID)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	_, ok := mgr.Get("nonexistent")
	if ok {
		t.Error("expected false")
	}
}

func TestManager_List(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	mgr.Create("a", "", 0)
	mgr.Create("b", "", 0)

	rooms := mgr.List()
	if len(rooms) != 2 {
		t.Fatalf("len = %d, want 2", len(rooms))
	}
}

func TestManager_Delete(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	mgr.Create("r1", "", 0)

	if err := mgr.Delete("r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := mgr.Get("r1"); ok {
		t.Error("room should be deleted")
	}
}

func TestManager_Delete_NotFound(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	err := mgr.Delete("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestManager_AddLeg(t *testing.T) {
	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	mgr.Create("r1", "", 0)
	l := newMockLeg("leg-1")
	legMgr.Add(l)

	err := mgr.AddLeg("r1", "leg-1")
	if err != nil {
		t.Fatalf("AddLeg: %v", err)
	}

	r, _ := mgr.Get("r1")
	if r.ParticipantCount() != 1 {
		t.Errorf("count = %d, want 1", r.ParticipantCount())
	}
}

func TestManager_AddLeg_RoomNotFound(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	err := mgr.AddLeg("nonexistent", "leg-1")
	if err == nil {
		t.Error("expected error")
	}
}

func TestManager_AddLeg_LegNotFound(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	mgr.Create("r1", "", 0)

	err := mgr.AddLeg("r1", "nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestManager_AddLeg_NotConnected(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())
	mgr.Create("r1", "", 0)

	l := newMockLeg("leg-1")
	l.state = leg.StateRinging
	legMgr.Add(l)

	err := mgr.AddLeg("r1", "leg-1")
	if err == nil {
		t.Error("expected error for non-connected leg")
	}
}

func TestManager_RemoveLeg(t *testing.T) {
	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	mgr.Create("r1", "", 0)
	l := newMockLeg("leg-1")
	legMgr.Add(l)
	mgr.AddLeg("r1", "leg-1")

	err := mgr.RemoveLeg("r1", "leg-1")
	if err != nil {
		t.Fatalf("RemoveLeg: %v", err)
	}

	r, _ := mgr.Get("r1")
	if r.ParticipantCount() != 0 {
		t.Errorf("count = %d, want 0", r.ParticipantCount())
	}
}

func TestManager_FindLegRoom(t *testing.T) {
	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	mgr.Create("r1", "", 0)
	l := newMockLeg("leg-1")
	legMgr.Add(l)
	mgr.AddLeg("r1", "leg-1")

	roomID, found := mgr.FindLegRoom("leg-1")
	if !found {
		t.Fatal("expected found=true")
	}
	if roomID != "r1" {
		t.Errorf("roomID = %q, want r1", roomID)
	}
}

func TestManager_FindLegRoom_NotFound(t *testing.T) {
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, newTestBus(), newTestLog())

	_, found := mgr.FindLegRoom("nonexistent")
	if found {
		t.Error("expected found=false")
	}
}

func TestManager_MoveLeg(t *testing.T) {
	bus := newTestBus()
	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	mgr.Create("r1", "", 0)
	mgr.Create("r2", "", 0)
	l := newMockLeg("leg-1")
	legMgr.Add(l)
	mgr.AddLeg("r1", "leg-1")

	err := mgr.MoveLeg("r1", "r2", "leg-1")
	if err != nil {
		t.Fatalf("MoveLeg: %v", err)
	}

	r1, _ := mgr.Get("r1")
	r2, _ := mgr.Get("r2")
	if r1.ParticipantCount() != 0 {
		t.Errorf("r1 count = %d, want 0", r1.ParticipantCount())
	}
	if r2.ParticipantCount() != 1 {
		t.Errorf("r2 count = %d, want 1", r2.ParticipantCount())
	}
}

func TestManager_Events(t *testing.T) {
	bus := newTestBus()
	var eventTypes []events.EventType
	_ = bus.Subscribe(func(e events.Event) {
		eventTypes = append(eventTypes, e.Type)
	})

	legMgr := leg.NewManager()
	mgr := NewManager(legMgr, bus, newTestLog())

	mgr.Create("r1", "", 0)

	found := false
	for _, et := range eventTypes {
		if et == events.RoomCreated {
			found = true
		}
	}
	if !found {
		t.Error("expected room.created event")
	}
}
