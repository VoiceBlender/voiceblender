package sip

import (
	"context"
	"sync"
	"testing"
)

// fakeTrunk is a minimal Trunk implementation for manager tests.
type fakeTrunk struct {
	id        string
	typ       TrunkType
	aor       string
	host      string
	port      int
	transport string
	stops     int
	stopMu    sync.Mutex
}

func (f *fakeTrunk) ID() string                        { return f.id }
func (f *fakeTrunk) Type() TrunkType                   { return f.typ }
func (f *fakeTrunk) AOR() string                       { return f.aor }
func (f *fakeTrunk) PeerSocket() (string, int, string) { return f.host, f.port, f.transport }
func (f *fakeTrunk) AppID() string                     { return "" }
func (f *fakeTrunk) Snapshot() TrunkView               { return TrunkView{ID: f.id, Type: f.typ} }
func (f *fakeTrunk) Start(context.Context)             {}
func (f *fakeTrunk) Stop(context.Context) error {
	f.stopMu.Lock()
	defer f.stopMu.Unlock()
	f.stops++
	return nil
}
func (f *fakeTrunk) stopCount() int {
	f.stopMu.Lock()
	defer f.stopMu.Unlock()
	return f.stops
}

func TestTrunkManager_AddGetRemove(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test", host: "10.0.0.1", port: 5060}
	m.Add(a)

	if got := m.Get("t1"); got != a {
		t.Errorf("Get returned %v, want %v", got, a)
	}
	if got := m.Get("missing"); got != nil {
		t.Errorf("Get missing returned %v, want nil", got)
	}
	if list := m.List(); len(list) != 1 {
		t.Errorf("List len = %d, want 1", len(list))
	}
	if removed := m.Remove("t1"); removed != a {
		t.Errorf("Remove returned %v, want %v", removed, a)
	}
	if list := m.List(); len(list) != 0 {
		t.Errorf("after Remove len = %d, want 0", len(list))
	}
}

func TestTrunkManager_LookupByFromAOR(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test"}
	m.Add(a)

	if got := m.LookupByFromAOR("sip:alice@vb.test"); got != a {
		t.Errorf("hit returned %v", got)
	}
	if got := m.LookupByFromAOR("sip:bob@vb.test"); got != nil {
		t.Errorf("miss returned %v, want nil", got)
	}
	if got := m.LookupByFromAOR(""); got != nil {
		t.Errorf("empty returned %v, want nil", got)
	}
}

func TestTrunkManager_LookupByPeerSocket(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test", host: "10.0.0.5", port: 5060}
	m.Add(a)

	if got := m.LookupByPeerSocket("10.0.0.5", 5060); got != a {
		t.Errorf("exact match returned %v, want %v", got, a)
	}
	// Ephemeral source port — host-only fallback should still find it.
	if got := m.LookupByPeerSocket("10.0.0.5", 56789); got != a {
		t.Errorf("host-only fallback returned %v, want %v", got, a)
	}
	if got := m.LookupByPeerSocket("10.0.0.99", 5060); got != nil {
		t.Errorf("unknown host returned %v, want nil", got)
	}
	if got := m.LookupByPeerSocket("", 5060); got != nil {
		t.Errorf("empty host returned %v, want nil", got)
	}
}

func TestTrunkManager_ShutdownStopsAll(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1"}
	b := &fakeTrunk{id: "t2"}
	m.Add(a)
	m.Add(b)

	m.Shutdown(context.Background())
	if a.stopCount() != 1 {
		t.Errorf("a stops = %d, want 1", a.stopCount())
	}
	if b.stopCount() != 1 {
		t.Errorf("b stops = %d, want 1", b.stopCount())
	}
	if len(m.List()) != 0 {
		t.Errorf("after Shutdown len = %d, want 0", len(m.List()))
	}
}

func TestTrunkManager_RefreshIndexAfterPeerSocketChange(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, host: "10.0.0.5", port: 5060}
	m.Add(a)

	// Simulate the registrar's source port becoming known after first REGISTER.
	a.host = "203.0.113.10"
	a.port = 5070
	m.RefreshIndex("t1")

	if got := m.LookupByPeerSocket("203.0.113.10", 5070); got != a {
		t.Errorf("after RefreshIndex new socket lookup returned %v, want %v", got, a)
	}
	if got := m.LookupByPeerSocket("10.0.0.5", 5060); got != nil {
		t.Errorf("stale socket should no longer resolve; got %v", got)
	}
}

func TestTrunkManager_Deindex(t *testing.T) {
	m := NewTrunkManager()
	ft := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test", host: "10.0.0.1", port: 5060}
	other := &fakeTrunk{id: "t2", typ: TrunkTypeSIPRegister, aor: "sip:bob@vb.test", host: "10.0.0.2", port: 5060}
	m.Add(ft)
	m.Add(other)

	if got := m.LookupByFromAOR("sip:alice@vb.test"); got != Trunk(ft) {
		t.Fatalf("pre-Deindex LookupByFromAOR = %v, want t1", got)
	}
	if got := m.LookupByAORUser("alice"); got != Trunk(ft) {
		t.Fatalf("pre-Deindex LookupByAORUser = %v, want t1", got)
	}
	if got := m.LookupByPeerSocket("10.0.0.1", 5060); got != Trunk(ft) {
		t.Fatalf("pre-Deindex LookupByPeerSocket = %v, want t1", got)
	}

	m.Deindex("t1")

	if got := m.LookupByFromAOR("sip:alice@vb.test"); got != nil {
		t.Errorf("LookupByFromAOR after Deindex = %v, want nil", got)
	}
	if got := m.LookupByAORUser("alice"); got != nil {
		t.Errorf("LookupByAORUser after Deindex = %v, want nil", got)
	}
	if got := m.LookupByPeerSocket("10.0.0.1", 5060); got != nil {
		t.Errorf("LookupByPeerSocket after Deindex = %v, want nil", got)
	}
	// Retained by id so an operator can still inspect and delete it.
	if got := m.Get("t1"); got != Trunk(ft) {
		t.Errorf("Get(t1) after Deindex = %v, want t1", got)
	}
	// Neighbouring trunks are untouched.
	if got := m.LookupByFromAOR("sip:bob@vb.test"); got != Trunk(other) {
		t.Errorf("t2 AOR lookup after Deindex(t1) = %v, want t2", got)
	}
	if got := m.LookupByPeerSocket("10.0.0.2", 5060); got != Trunk(other) {
		t.Errorf("t2 socket lookup after Deindex(t1) = %v, want t2", got)
	}

	// Unknown ids are a no-op, not a panic.
	m.Deindex("nope")
	if got := m.Get("t1"); got != Trunk(ft) {
		t.Errorf("Deindex(unknown) disturbed t1: %v", got)
	}
}
