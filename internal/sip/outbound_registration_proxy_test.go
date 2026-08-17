package sip

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func proxyPtr(t *testing.T, raw string) *sip.Uri {
	t.Helper()
	u, err := ParseProxyURI(raw)
	if err != nil {
		t.Fatalf("ParseProxyURI(%q): %v", raw, err)
	}
	return &u
}

func newProxyTrunk(t *testing.T, engine *Engine, proxy string) *OutboundRegistration {
	t.Helper()
	p := OutboundRegistrationParams{
		ID:           "t1",
		RegistrarURI: sip.Uri{Scheme: "sip", Host: "pbx.example.com", Port: 5060},
		AOR:          sip.Uri{Scheme: "sip", User: "alice", Host: "pbx.example.com"},
		Username:     "alice",
		Password:     "secret",
	}
	if proxy != "" {
		p.OutboundProxy = proxyPtr(t, proxy)
	}
	return NewOutboundRegistration(engine, nil, nil, OutboundRegistrationConfig{}, p)
}

// TestOutboundRegistration_PeerSocketUsesProxy pins that the trunk indexes on
// its next hop. The registrar host and the proxy host are deliberately
// different so reaching for the wrong one is observable.
func TestOutboundRegistration_PeerSocketUsesProxy(t *testing.T) {
	r := newProxyTrunk(t, nil, "sips:edge.acme.net:5061")
	host, port, transport := r.PeerSocket()
	if host != "edge.acme.net" {
		t.Errorf("peer host = %q, want the proxy host %q", host, "edge.acme.net")
	}
	if port != 5061 {
		t.Errorf("peer port = %d, want 5061", port)
	}
	if transport != "tls" {
		t.Errorf("peer transport = %q, want tls", transport)
	}
}

// TestOutboundRegistration_PeerSocketWithoutProxyUnchanged is the
// backwards-compatibility pin for the no-proxy case.
func TestOutboundRegistration_PeerSocketWithoutProxyUnchanged(t *testing.T) {
	r := newProxyTrunk(t, nil, "")
	host, port, transport := r.PeerSocket()
	if host != "pbx.example.com" || port != 5060 || transport != "udp" {
		t.Errorf("PeerSocket() = (%q, %d, %q), want (pbx.example.com, 5060, udp)", host, port, transport)
	}
}

func TestOutboundRegistration_PeerSocketProxyDefaultPort(t *testing.T) {
	r := newProxyTrunk(t, nil, "sip:edge.acme.net")
	_, port, transport := r.PeerSocket()
	if port != 5060 {
		t.Errorf("peer port = %d, want the sip: default 5060", port)
	}
	if transport != "udp" {
		t.Errorf("peer transport = %q, want udp", transport)
	}
}

func TestOutboundRegistration_NextHopURI(t *testing.T) {
	withProxy := newProxyTrunk(t, nil, "sip:edge.acme.net:5080")
	if got := withProxy.nextHopURI().Host; got != "edge.acme.net" {
		t.Errorf("nextHopURI host = %q, want the proxy", got)
	}
	without := newProxyTrunk(t, nil, "")
	if got := without.nextHopURI().Host; got != "pbx.example.com" {
		t.Errorf("nextHopURI host = %q, want the registrar", got)
	}
	if withProxy.OutboundProxy() == nil {
		t.Error("OutboundProxy() = nil for a trunk configured with one")
	}
	if without.OutboundProxy() != nil {
		t.Error("OutboundProxy() non-nil for a trunk without one")
	}
}

// TestBuildRegister_ProxyRoute pins the core of loose routing: the REGISTER is
// steered at the proxy while the Request-URI still names the registrar, which
// is what the registrar matches on and what the digest `uri` must equal.
func TestBuildRegister_ProxyRoute(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test",
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	r := newProxyTrunk(t, engine, "sip:edge.acme.net:5080")
	req, err := r.buildRegister(3600)
	if err != nil {
		t.Fatalf("buildRegister: %v", err)
	}

	route := req.Route()
	if route == nil {
		t.Fatal("REGISTER has no Route header; the proxy was not applied")
	}
	if route.Address.Host != "edge.acme.net" || route.Address.Port != 5080 {
		t.Errorf("Route = %q, want the proxy edge.acme.net:5080", route.Address.String())
	}
	if !route.Address.UriParams.Has("lr") {
		t.Error("Route lacks lr; sipgo would strict-route and rewrite the Request-URI")
	}
	if got, want := req.Recipient.Host, "pbx.example.com"; got != want {
		t.Errorf("Request-URI host = %q, want the registrar %q", got, want)
	}
	if got := req.Destination(); got != "edge.acme.net:5080" {
		t.Errorf("Destination() = %q, want the proxy socket", got)
	}
}

// TestBuildRegister_NoProxyHasNoRoute is the backwards-compatibility pin: an
// unconfigured trunk must emit the exact bytes it always has.
func TestBuildRegister_NoProxyHasNoRoute(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test",
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	r := newProxyTrunk(t, engine, "")
	req, err := r.buildRegister(3600)
	if err != nil {
		t.Fatalf("buildRegister: %v", err)
	}
	if req.GetHeader("Route") != nil {
		t.Errorf("REGISTER gained a Route header with no proxy configured:\n%s", req.String())
	}
	if got := req.Destination(); !strings.HasPrefix(got, "pbx.example.com") {
		t.Errorf("Destination() = %q, want the registrar", got)
	}
}

// TestBuildDigestResponse_UsesRegistrarNotProxy pins RFC 3261 §22.4: the digest
// `uri` must equal the Request-URI, which stays the registrar even when a proxy
// carries the request.
func TestBuildDigestResponse_UsesRegistrarNotProxy(t *testing.T) {
	r := newProxyTrunk(t, nil, "sip:edge.acme.net:5080")
	cred, err := r.buildDigestResponse(`Digest realm="pbx.example.com", nonce="abc", algorithm=MD5`)
	if err != nil {
		t.Fatalf("buildDigestResponse: %v", err)
	}
	if !strings.Contains(cred, "pbx.example.com") {
		t.Errorf("digest credentials %q do not target the registrar", cred)
	}
	if strings.Contains(cred, "edge.acme.net") {
		t.Errorf("digest credentials %q target the proxy; must be the registrar", cred)
	}
}

func TestOutboundRegistration_SnapshotReportsProxy(t *testing.T) {
	engine, err := NewEngine(EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: pickFreePort(t, "udp"),
		SIPHost:  "test",
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	withProxy := newProxyTrunk(t, engine, "sip:edge.acme.net:5080").Snapshot()
	if got := withProxy.SIPRegister.OutboundProxy; got != "sip:edge.acme.net:5080" {
		t.Errorf("snapshot outbound_proxy = %q, want the configured proxy", got)
	}
	without := newProxyTrunk(t, engine, "").Snapshot()
	if got := without.SIPRegister.OutboundProxy; got != "" {
		t.Errorf("snapshot outbound_proxy = %q, want empty when unconfigured", got)
	}
}
