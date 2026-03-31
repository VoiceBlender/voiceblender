package leg

import (
	"context"
	"io"
	"testing"
	"time"
)

// mockLeg implements the Leg interface for testing the Manager.
type mockLeg struct {
	id        string
	legType   LegType
	state     LegState
	roomID    string
	muted     bool
	deaf      bool
	held      bool
	createdAt time.Time
}

func newMockLeg(id string) *mockLeg {
	return &mockLeg{
		id:        id,
		legType:   TypeSIPInbound,
		state:     StateConnected,
		createdAt: time.Now(),
	}
}

func (m *mockLeg) ID() string                                  { return m.id }
func (m *mockLeg) Type() LegType                               { return m.legType }
func (m *mockLeg) State() LegState                             { return m.state }
func (m *mockLeg) SampleRate() int                             { return 8000 }
func (m *mockLeg) AudioReader() io.Reader                      { return nil }
func (m *mockLeg) AudioWriter() io.Writer                      { return nil }
func (m *mockLeg) OnDTMF(func(digit rune))                     {}
func (m *mockLeg) SendDTMF(ctx context.Context, d string) error { return nil }
func (m *mockLeg) Hangup(ctx context.Context) error            { return nil }
func (m *mockLeg) Answer(ctx context.Context) error            { return nil }
func (m *mockLeg) Context() context.Context                    { return context.Background() }
func (m *mockLeg) RoomID() string                              { return m.roomID }
func (m *mockLeg) SetRoomID(id string)                         { m.roomID = id }
func (m *mockLeg) IsMuted() bool                               { return m.muted }
func (m *mockLeg) SetMuted(v bool)                             { m.muted = v }
func (m *mockLeg) IsDeaf() bool                                { return m.deaf }
func (m *mockLeg) SetDeaf(v bool)                              { m.deaf = v }
func (m *mockLeg) IsHeld() bool                                { return m.held }
func (m *mockLeg) CreatedAt() time.Time                        { return m.createdAt }
func (m *mockLeg) AnsweredAt() time.Time                       { return time.Time{} }
func (m *mockLeg) SIPHeaders() map[string]string               { return nil }
func (m *mockLeg) RTPStats() RTPStats                          { return RTPStats{} }

func TestNewManager(t *testing.T) {
	mgr := NewManager()
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
	if len(mgr.List()) != 0 {
		t.Error("new manager should have no legs")
	}
}

func TestManager_Add_Get(t *testing.T) {
	mgr := NewManager()
	l := newMockLeg("leg-1")

	mgr.Add(l)

	got, ok := mgr.Get("leg-1")
	if !ok {
		t.Fatal("Get returned false")
	}
	if got.ID() != "leg-1" {
		t.Errorf("ID = %q, want leg-1", got.ID())
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	mgr := NewManager()
	_, ok := mgr.Get("nonexistent")
	if ok {
		t.Error("expected false for nonexistent leg")
	}
}

func TestManager_Remove(t *testing.T) {
	mgr := NewManager()
	mgr.Add(newMockLeg("leg-1"))
	mgr.Remove("leg-1")

	_, ok := mgr.Get("leg-1")
	if ok {
		t.Error("leg should be removed")
	}
}

func TestManager_Remove_Nonexistent(t *testing.T) {
	mgr := NewManager()
	mgr.Remove("nonexistent") // should not panic
}

func TestManager_List(t *testing.T) {
	mgr := NewManager()
	mgr.Add(newMockLeg("a"))
	mgr.Add(newMockLeg("b"))
	mgr.Add(newMockLeg("c"))

	legs := mgr.List()
	if len(legs) != 3 {
		t.Fatalf("len = %d, want 3", len(legs))
	}
}

func TestManager_All(t *testing.T) {
	mgr := NewManager()
	mgr.Add(newMockLeg("a"))
	mgr.Add(newMockLeg("b"))

	all := mgr.All()
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if _, ok := all["a"]; !ok {
		t.Error("missing leg a")
	}
	if _, ok := all["b"]; !ok {
		t.Error("missing leg b")
	}

	// Verify it's a copy — modifying returned map doesn't affect manager.
	delete(all, "a")
	if _, ok := mgr.Get("a"); !ok {
		t.Error("deleting from All() map should not affect manager")
	}
}

func TestManager_Add_Overwrites(t *testing.T) {
	mgr := NewManager()
	l1 := newMockLeg("leg-1")
	l1.state = StateRinging
	mgr.Add(l1)

	l2 := newMockLeg("leg-1")
	l2.state = StateConnected
	mgr.Add(l2)

	got, _ := mgr.Get("leg-1")
	if got.State() != StateConnected {
		t.Errorf("state = %q, want connected", got.State())
	}
}

// --- calculateMOS tests ---

func TestCalculateMOS_Perfect(t *testing.T) {
	mos := calculateMOS(0, 0)
	if mos < 4.0 {
		t.Errorf("MOS = %.2f, expected > 4.0 for perfect conditions", mos)
	}
}

func TestCalculateMOS_HighLoss(t *testing.T) {
	mos := calculateMOS(0.5, 0)
	if mos > 2.0 {
		t.Errorf("MOS = %.2f, expected < 2.0 for 50%% loss", mos)
	}
}

func TestCalculateMOS_HighJitter(t *testing.T) {
	mos := calculateMOS(0, 200)
	if mos > 4.0 {
		t.Errorf("MOS = %.2f, expected < 4.0 for high jitter", mos)
	}
	// With 200ms jitter, MOS should be notably below perfect (4.41)
	perfect := calculateMOS(0, 0)
	if mos >= perfect {
		t.Errorf("MOS with jitter (%.2f) should be less than perfect (%.2f)", mos, perfect)
	}
}

func TestCalculateMOS_Bounds(t *testing.T) {
	mos := calculateMOS(1.0, 1000)
	if mos < 1.0 || mos > 5.0 {
		t.Errorf("MOS = %.2f, expected between 1.0 and 5.0", mos)
	}
}
