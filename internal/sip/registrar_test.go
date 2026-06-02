package sip

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

func newTestRegistrar(t *testing.T, cfg RegistrarConfig) (*Registrar, *capturingBus) {
	t.Helper()
	bus := events.NewBus("test")
	cap := newCapturingBus()
	bus.Subscribe(cap.handle)
	return NewRegistrar(bus, nil, cfg), cap
}

type capturingBus struct {
	mu     sync.Mutex
	events []events.Event
}

func newCapturingBus() *capturingBus { return &capturingBus{} }

func (c *capturingBus) handle(e events.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *capturingBus) byType(typ events.EventType) []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []events.Event
	for _, e := range c.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func makeBinding(aor, contact, socket string, expiresIn time.Duration) Binding {
	return Binding{
		AOR:       aor,
		Contact:   contact,
		Socket:    socket,
		Transport: "udp",
		ExpiresAt: time.Now().Add(expiresIn),
	}
}

func TestRegistrar_BindLookup(t *testing.T) {
	r, bus := newTestRegistrar(t, RegistrarConfig{AllowMultipleContacts: true})

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", time.Hour))

	got, ok := r.Lookup("sip:alice@vb.test")
	if !ok {
		t.Fatalf("Lookup: missing")
	}
	if got.Socket != "10.0.0.5:5060" {
		t.Errorf("Socket = %q", got.Socket)
	}
	if len(bus.byType(events.SIPRegistrationActive)) != 1 {
		t.Errorf("active events = %d, want 1", len(bus.byType(events.SIPRegistrationActive)))
	}
}

func TestRegistrar_RefreshUpdatesSocket(t *testing.T) {
	r, bus := newTestRegistrar(t, RegistrarConfig{AllowMultipleContacts: true})

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", time.Hour))
	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:6000", time.Hour))

	got, _ := r.Lookup("sip:alice@vb.test")
	if got.Socket != "10.0.0.5:6000" {
		t.Errorf("refreshed Socket = %q, want 10.0.0.5:6000", got.Socket)
	}
	all := r.LookupAll("sip:alice@vb.test")
	if len(all) != 1 {
		t.Errorf("LookupAll len = %d, want 1", len(all))
	}
	if got := len(bus.byType(events.SIPRegistrationActive)); got != 2 {
		t.Errorf("active events = %d, want 2", got)
	}
}

func TestRegistrar_MultipleContactsPerAOR(t *testing.T) {
	r, _ := newTestRegistrar(t, RegistrarConfig{AllowMultipleContacts: true})

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", time.Hour))
	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.6:5060", "10.0.0.6:5060", time.Hour))

	all := r.LookupAll("sip:alice@vb.test")
	if len(all) != 2 {
		t.Fatalf("LookupAll len = %d, want 2", len(all))
	}
	// Most recent first.
	if all[0].Socket != "10.0.0.6:5060" {
		t.Errorf("first = %q, want 10.0.0.6:5060", all[0].Socket)
	}
}

func TestRegistrar_SingleBindingMode(t *testing.T) {
	r, bus := newTestRegistrar(t, RegistrarConfig{AllowMultipleContacts: false})

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", time.Hour))
	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.6:5060", "10.0.0.6:5060", time.Hour))

	all := r.LookupAll("sip:alice@vb.test")
	if len(all) != 1 {
		t.Errorf("LookupAll len = %d, want 1", len(all))
	}
	if all[0].Socket != "10.0.0.6:5060" {
		t.Errorf("Socket = %q, want 10.0.0.6:5060", all[0].Socket)
	}
	replaced := bus.byType(events.SIPRegistrationExpired)
	if len(replaced) != 1 || replaced[0].Data.(*events.SIPRegistrationExpiredData).Reason != "replaced" {
		t.Errorf("expected one 'replaced' expired event, got %+v", replaced)
	}
}

func TestRegistrar_UnbindContactAndAll(t *testing.T) {
	r, bus := newTestRegistrar(t, RegistrarConfig{AllowMultipleContacts: true})

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", time.Hour))
	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.6:5060", "10.0.0.6:5060", time.Hour))

	if !r.UnbindContact("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "unregistered") {
		t.Fatal("UnbindContact returned false")
	}
	if got := len(r.LookupAll("sip:alice@vb.test")); got != 1 {
		t.Errorf("after unbind one: count = %d", got)
	}
	if n := r.UnbindAll("sip:alice@vb.test", "forced"); n != 1 {
		t.Errorf("UnbindAll removed = %d", n)
	}
	if _, ok := r.Lookup("sip:alice@vb.test"); ok {
		t.Error("expected no binding after UnbindAll")
	}
	expired := bus.byType(events.SIPRegistrationExpired)
	if len(expired) != 2 {
		t.Errorf("expired events = %d, want 2", len(expired))
	}
}

func TestRegistrar_SweepRemovesExpired(t *testing.T) {
	r, bus := newTestRegistrar(t, RegistrarConfig{
		AllowMultipleContacts: true,
		SweepInterval:         50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)

	r.Bind(makeBinding("sip:alice@vb.test", "sip:alice@10.0.0.5:5060", "10.0.0.5:5060", 100*time.Millisecond))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bus.byType(events.SIPRegistrationExpired)) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if _, ok := r.Lookup("sip:alice@vb.test"); ok {
		t.Fatal("binding still present after expiry")
	}
	expired := bus.byType(events.SIPRegistrationExpired)
	if len(expired) != 1 || expired[0].Data.(*events.SIPRegistrationExpiredData).Reason != "ttl" {
		t.Errorf("expected one 'ttl' expired event, got %+v", expired)
	}
}

func TestRegistrar_ClampExpires(t *testing.T) {
	r := NewRegistrar(nil, nil, RegistrarConfig{
		DefaultExpiresSeconds: 1800,
		MaxExpiresSeconds:     3600,
	})
	cases := []struct {
		in, want int
	}{
		{0, 1800},
		{10, 60},
		{1000, 1000},
		{9999, 3600},
	}
	for _, c := range cases {
		if got := r.ClampExpires(c.in); got != c.want {
			t.Errorf("ClampExpires(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeAOR(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sip:Alice@VB.example", "sip:Alice@vb.example"},
		{"sip:bob@host.example:5060", "sip:bob@host.example:5060"},
		{"SIPS:carol@host.example", "sips:carol@host.example"},
	}
	for _, c := range cases {
		var u sip.Uri
		if err := sip.ParseUri(c.in, &u); err != nil {
			t.Fatalf("parse %q: %v", c.in, err)
		}
		if got := CanonicalizeAOR(u); got != c.want {
			t.Errorf("CanonicalizeAOR(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
