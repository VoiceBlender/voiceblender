package sip

import (
	"context"
	"sync"
	"testing"

	"github.com/emiago/sipgo/sip"
)

// fakeTrunk is a minimal Trunk implementation for manager tests.
type fakeTrunk struct {
	id        string
	typ       TrunkType
	aor       string
	host      string
	port      int
	transport string
	contact   string
	appID     string
	stops     int
	stopMu    sync.Mutex
}

func (f *fakeTrunk) ID() string                        { return f.id }
func (f *fakeTrunk) Type() TrunkType                   { return f.typ }
func (f *fakeTrunk) AOR() string                       { return f.aor }
func (f *fakeTrunk) PeerSocket() (string, int, string) { return f.host, f.port, f.transport }
func (f *fakeTrunk) ContactUser() string               { return f.contact }
func (f *fakeTrunk) AppID() string                     { return f.appID }
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

// TestTrunkManager_SharedPeerSocketIsAmbiguous pins that the socket index
// alone cannot tell apart trunks sharing a next hop; inbound attribution uses
// LookupInbound instead.
func TestTrunkManager_SharedPeerSocketIsAmbiguous(t *testing.T) {
	m := NewTrunkManager()
	a := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test", host: "10.0.0.9", port: 5060}
	b := &fakeTrunk{id: "t2", typ: TrunkTypeSIPRegister, aor: "sip:bob@vb.test", host: "10.0.0.9", port: 5060}
	m.Add(a)
	m.Add(b)

	got := m.LookupByPeerSocket("10.0.0.9", 5060)
	if got == nil {
		t.Fatal("LookupByPeerSocket returned nil for a socket two trunks share")
	}
	if got != a && got != b {
		t.Fatalf("LookupByPeerSocket returned an unknown trunk %v", got)
	}
	// Both remain reachable by their own identity, which is what callers that
	// need a definite answer must use.
	if m.Get("t1") != a || m.Get("t2") != b {
		t.Error("a shared peer socket must not affect lookups by id")
	}
	if m.LookupByFromAOR("sip:alice@vb.test") != a {
		t.Error("a shared peer socket must not affect lookups by AOR")
	}
}

func TestTrunkManager_LookupInbound(t *testing.T) {
	alice := &fakeTrunk{id: "t1", typ: TrunkTypeSIPRegister, aor: "sip:alice@vb.test", contact: "alice", host: "10.0.0.9", port: 5060, appID: "app-a"}
	bob := &fakeTrunk{id: "t2", typ: TrunkTypeSIPRegister, aor: "sip:bob@vb.test", contact: "bob-c", host: "10.0.0.9", port: 5060, appID: "app-b"}
	carol := &fakeTrunk{id: "t3", typ: TrunkTypeSIPRegister, aor: "sip:carol@other.test", contact: "carol", host: "10.0.0.20", port: 5060}

	m := NewTrunkManager()
	m.Add(alice)
	m.Add(bob)
	m.Add(carol)

	tests := []struct {
		name       string
		host       string
		port       int
		reqUser    string
		to         sip.Uri
		want       Trunk
		wantUnique bool
	}{
		{"single trunk on socket", "10.0.0.20", 5060, "", sip.Uri{}, carol, true},
		{"single trunk host-only fallback", "10.0.0.20", 40000, "", sip.Uri{}, carol, true},
		{"shared socket by request user", "10.0.0.9", 5060, "bob-c", sip.Uri{User: "someone", Host: "x.test"}, bob, true},
		{"shared socket by To AOR", "10.0.0.9", 5060, "unknown", sip.Uri{Scheme: "sip", User: "alice", Host: "VB.test"}, alice, true},
		{"shared socket by To user", "10.0.0.9", 5060, "", sip.Uri{User: "bob", Host: "carrier.test"}, bob, true},
		{"shared socket host-only fallback", "10.0.0.9", 41000, "alice", sip.Uri{}, alice, true},
		{"unknown host", "10.0.0.99", 5060, "alice", sip.Uri{}, nil, false},
		{"empty host", "", 5060, "alice", sip.Uri{}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, unique := m.LookupInbound(tc.host, tc.port, tc.reqUser, tc.to)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if unique != tc.wantUnique {
				t.Fatalf("unique = %v, want %v", unique, tc.wantUnique)
			}
		})
	}

	t.Run("shared socket without discriminator is not unique", func(t *testing.T) {
		got, unique := m.LookupInbound("10.0.0.9", 5060, "nobody", sip.Uri{User: "nobody", Host: "x.test"})
		if got != alice && got != bob {
			t.Fatalf("got %v, want one of the shared trunks", got)
		}
		if unique {
			t.Fatal("unique = true for an undecidable shared socket")
		}
	})

	t.Run("exact port beats host-only", func(t *testing.T) {
		m := NewTrunkManager()
		a := &fakeTrunk{id: "a", aor: "sip:a@vb.test", host: "10.0.0.30", port: 5060}
		b := &fakeTrunk{id: "b", aor: "sip:b@vb.test", host: "10.0.0.30", port: 5080}
		m.Add(a)
		m.Add(b)
		if got, unique := m.LookupInbound("10.0.0.30", 5080, "", sip.Uri{}); got != b || !unique {
			t.Fatalf("got %v unique=%v, want %v unique=true", got, unique, b)
		}
	})
}
