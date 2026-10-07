package sip

import (
	"context"
	"log/slog"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func TestParseLocalNets(t *testing.T) {
	got, err := ParseLocalNets(" 10.1.2.3/8, 192.168.1.20 ,,2001:db8::/32")
	if err != nil {
		t.Fatalf("ParseLocalNets: %v", err)
	}
	want := []string{"10.0.0.0/8", "192.168.1.20/32", "2001:db8::/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("prefix %d = %s, want %s", i, p, want[i])
		}
	}

	if got, err := ParseLocalNets(""); err != nil || len(got) != 0 {
		t.Errorf("empty input = %v, %v; want no prefixes", got, err)
	}
	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0.0/8,lan"} {
		if _, err := ParseLocalNets(bad); err == nil {
			t.Errorf("ParseLocalNets(%q) = nil error, want failure", bad)
		}
	}
}

func localNetEngine(t *testing.T, nets string) *Engine {
	t.Helper()
	prefixes, err := ParseLocalNets(nets)
	if err != nil {
		t.Fatalf("ParseLocalNets: %v", err)
	}
	e, err := NewEngine(EngineConfig{
		BindIP:     "192.168.1.10",
		ExternalIP: "203.0.113.5",
		LocalNets:  prefixes,
		BindPort:   15099,
		SIPHost:    "test",
		Codecs:     []codec.CodecType{codec.CodecPCMU},
		Log:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestLocalAdvertisedIP(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8,192.168.0.0/16")
	tests := []struct {
		peer string
		want string
	}{
		{"10.1.2.3", "192.168.1.10"},
		{"10.1.2.3:5060", "192.168.1.10"},
		{"192.168.44.7:50248", "192.168.1.10"},
		{"::ffff:10.1.2.3", "192.168.1.10"},
		{"[::ffff:10.1.2.3]:5060", "192.168.1.10"},
		{"172.20.220.28:5060", ""},
		{"8.8.8.8", ""},
		{"2001:db8::1", ""},
		{"pbx.example.com", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := e.localAdvertisedIP(tt.peer); got != tt.want {
			t.Errorf("localAdvertisedIP(%q) = %q, want %q", tt.peer, got, tt.want)
		}
	}

	if got := localNetEngine(t, "").localAdvertisedIP("10.1.2.3"); got != "" {
		t.Errorf("without local nets: localAdvertisedIP = %q, want empty", got)
	}
}

func TestAdvertisedIPForPeer(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8")
	if got := e.AdvertisedIPForPeer("IP4", "10.1.2.3:5060"); got != "192.168.1.10" {
		t.Errorf("local peer = %q, want the local address", got)
	}
	if got := e.AdvertisedIPForPeer("", "10.1.2.3:5060"); got != "192.168.1.10" {
		t.Errorf("local peer, no offer family = %q, want the local address", got)
	}
	if got := e.AdvertisedIPForPeer("IP4", "198.51.100.7:5060"); got != "203.0.113.5" {
		t.Errorf("external peer = %q, want the external address", got)
	}
}

func TestAdvertisedIPForRecipient_LocalNets(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8")
	if ip, local := e.advertisedIPForRecipient(context.Background(), "10.1.2.3"); ip != "192.168.1.10" || !local {
		t.Errorf("local recipient = %q, %v; want 192.168.1.10, true", ip, local)
	}
	if ip, local := e.advertisedIPForRecipient(context.Background(), "198.51.100.7"); ip != "203.0.113.5" || local {
		t.Errorf("external recipient = %q, %v; want 203.0.113.5, false", ip, local)
	}
}

func TestLocalContact(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8")
	c := e.localContact("10.1.2.3:5060")
	if c == nil {
		t.Fatal("localContact for a local peer = nil")
	}
	if c.Address.Host != "192.168.1.10" || c.Address.Port != 15099 {
		t.Errorf("local Contact = %s, want sip:192.168.1.10:15099", c.Address.String())
	}
	if c := e.localContact("198.51.100.7:5060"); c != nil {
		t.Errorf("localContact for an external peer = %s, want nil", c.Address.String())
	}
}

func TestInviteOptions_NextHopHost(t *testing.T) {
	recipient := sip.Uri{Scheme: "sip", User: "102", Host: "pbx.example.com"}
	proxy := sip.Uri{Scheme: "sip", Host: "10.9.9.9"}
	route := sip.Uri{Scheme: "sip", Host: "10.8.8.8"}
	one := []ForkTarget{{Socket: "10.1.2.3:50248"}}

	tests := []struct {
		name string
		opts InviteOptions
		want string
	}{
		{"plain", InviteOptions{}, "pbx.example.com"},
		{"registered contact", InviteOptions{ForkTargets: one}, "10.1.2.3"},
		{"proxy", InviteOptions{ProxyURI: &proxy}, "10.9.9.9"},
		{"proxy ignored for a registered contact", InviteOptions{ProxyURI: &proxy, ForkTargets: one}, "10.1.2.3"},
		{"trunk route", InviteOptions{RouteURI: &route}, "10.8.8.8"},
		{"proxy outranks trunk route", InviteOptions{ProxyURI: &proxy, RouteURI: &route}, "10.9.9.9"},
	}
	for _, tt := range tests {
		if got := tt.opts.nextHopHost(recipient); got != tt.want {
			t.Errorf("%s: nextHopHost = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestAppendDialogContact(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8")
	peer := sip.Uri{Scheme: "sip", User: "102", Host: "10.1.2.3", Port: 5060}

	inbound := func(source string) *sipgo.DialogServerSession {
		invite := sip.NewRequest(sip.INVITE, peer)
		invite.SetSource(source)
		return &sipgo.DialogServerSession{Dialog: sipgo.Dialog{InviteRequest: invite}}
	}

	req := sip.NewRequest(sip.INVITE, peer)
	e.appendDialogContact(req, inbound("10.1.2.3:5060"))
	if c := req.Contact(); c == nil || c.Address.Host != "192.168.1.10" {
		t.Errorf("re-INVITE to a local caller: Contact = %v, want host 192.168.1.10", c)
	}

	// An external caller keeps the default Contact, which sipgo adds on send.
	req = sip.NewRequest(sip.INVITE, peer)
	e.appendDialogContact(req, inbound("198.51.100.7:5060"))
	if c := req.Contact(); c != nil {
		t.Errorf("re-INVITE to an external caller: Contact = %s, want none set here", c.Address.String())
	}

	// Outbound dialogs repeat whatever the INVITE advertised.
	invite := sip.NewRequest(sip.INVITE, peer)
	invite.AppendHeader(e.contactWithHost("192.168.1.10"))
	req = sip.NewRequest(sip.REFER, peer)
	e.appendDialogContact(req, &sipgo.DialogClientSession{Dialog: sipgo.Dialog{InviteRequest: invite}})
	if c := req.Contact(); c == nil || c.Address.Host != "192.168.1.10" {
		t.Errorf("REFER on an outbound dialog: Contact = %v, want host 192.168.1.10", c)
	}
}

func TestContactHostForRequest_LocalNets(t *testing.T) {
	e := localNetEngine(t, "10.0.0.0/8")
	req := sip.NewRequest(sip.INVITE, sip.Uri{Scheme: "sip", Host: "192.168.1.10"})
	req.SetSource("10.1.2.3:5060")
	if got := e.contactHostForRequest(req); got != "192.168.1.10" {
		t.Errorf("local caller: Contact host = %q, want 192.168.1.10", got)
	}
	req.SetSource("198.51.100.7:5060")
	if got := e.contactHostForRequest(req); got != "203.0.113.5" {
		t.Errorf("external caller: Contact host = %q, want 203.0.113.5", got)
	}
}
