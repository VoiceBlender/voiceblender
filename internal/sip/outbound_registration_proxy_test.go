package sip

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// TestBuildRegister_ProxyDestination pins how an outbound proxy is applied to a
// REGISTER: the request is steered at the proxy socket while the Request-URI
// still names the registrar (what it matches on, and what the digest `uri` must
// equal), and no Route header is pre-loaded. A proxy that does not recognise
// the Route URI as one of its own forwards the REGISTER back at itself rather
// than popping the header, which registers as silence rather than an error.
func TestBuildRegister_ProxyDestination(t *testing.T) {
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

	if req.GetHeader("Route") != nil {
		t.Errorf("REGISTER carries a pre-loaded Route:\n%s", req.String())
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

// TestBuildRegister_TLSProxyTransport pins that both spellings of a TLS proxy
// reach the wire over TLS. The "sips:" form is the one that needs the explicit
// SetTransport: sipgo derives transport from a Route's ";transport=" param but
// not from its scheme.
func TestBuildRegister_TLSProxyTransport(t *testing.T) {
	for _, proxy := range []string{"sips:edge.acme.net:5061", "sip:edge.acme.net:5061;transport=tls"} {
		t.Run(proxy, func(t *testing.T) {
			engine, err := NewEngine(EngineConfig{
				BindIP:   "127.0.0.1",
				BindPort: pickFreePort(t, "udp"),
				SIPHost:  "test",
				Log:      slog.Default(),
			})
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			r := newProxyTrunk(t, engine, proxy)
			if _, _, tp := r.PeerSocket(); tp != "tls" {
				t.Errorf("peer transport = %q, want tls", tp)
			}
			req, err := r.buildRegister(3600)
			if err != nil {
				t.Fatalf("buildRegister: %v", err)
			}
			if got := req.Transport(); got != "TLS" {
				t.Errorf("REGISTER transport = %q, want TLS", got)
			}
			if got := req.Destination(); got != "edge.acme.net:5061" {
				t.Errorf("Destination() = %q, want the proxy socket", got)
			}
		})
	}
}

// TestContactURI_TLSProxyNeedsTLSListener documents the one sharp edge of a TLS
// outbound proxy: the trunk registers fine, but with no SIP_TLS_PORT the
// Contact can only name the UDP socket, so the upstream sends calls back in the
// clear. POST /v1/sip/trunks logs a warning for exactly this case.
func TestContactURI_TLSProxyNeedsTLSListener(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())

	withoutTLS, err := NewEngine(EngineConfig{
		BindIP: "127.0.0.1", BindPort: pickFreePort(t, "udp"),
		SIPHost: "test", Log: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got := newProxyTrunk(t, withoutTLS, "sips:edge.acme.net:5061").contactString()
	if !strings.HasPrefix(got, "sip:") || strings.HasPrefix(got, "sips:") {
		t.Errorf("contact = %q, want a plain sip: URI when no TLS listener exists", got)
	}

	tlsPort := pickFreePort(t, "tcp")
	withTLS, err := NewEngine(EngineConfig{
		BindIP: "127.0.0.1", BindPort: pickFreePort(t, "udp"),
		TLSBindPort: tlsPort, TLSCertPath: certPath, TLSKeyPath: keyPath,
		SIPHost: "test", Log: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got = newProxyTrunk(t, withTLS, "sips:edge.acme.net:5061").contactString()
	if !strings.HasPrefix(got, "sips:") {
		t.Errorf("contact = %q, want a sips: URI when a TLS listener exists", got)
	}
	if !strings.Contains(got, "transport=tls") {
		t.Errorf("contact = %q, want transport=tls", got)
	}
}

// TestSnapshot_ContactMatchesWire pins that the reported contact_uri is the
// header the REGISTER actually carries. These were two hand-maintained copies
// of the same logic and drifted: with a TLS proxy but no SIP_TLS_PORT, the
// snapshot claimed "sips:" while the wire sent "sip:", so an operator reading
// the API saw an encrypted return path that did not exist.
func TestSnapshot_ContactMatchesWire(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t, t.TempDir())

	for _, tc := range []struct {
		name       string
		tls        bool
		proxy      string
		wantScheme string
	}{
		{name: "tls proxy without listener", proxy: "sip:edge.acme.net:5184;transport=tls", wantScheme: "sip:"},
		{name: "tls proxy with listener", tls: true, proxy: "sip:edge.acme.net:5184;transport=tls", wantScheme: "sips:"},
		{name: "plain proxy", proxy: "sip:edge.acme.net:5080", wantScheme: "sip:"},
		{name: "no proxy", proxy: "", wantScheme: "sip:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := EngineConfig{
				BindIP: "127.0.0.1", BindPort: pickFreePort(t, "udp"),
				SIPHost: "test", Log: slog.Default(),
			}
			if tc.tls {
				cfg.TLSBindPort = pickFreePort(t, "tcp")
				cfg.TLSCertPath, cfg.TLSKeyPath = certPath, keyPath
			}
			engine, err := NewEngine(cfg)
			if err != nil {
				t.Fatalf("NewEngine: %v", err)
			}
			r := newProxyTrunk(t, engine, tc.proxy)

			snapshot := r.Snapshot().SIPRegister.ContactURI
			wire := r.contactString()
			if snapshot != wire {
				t.Errorf("snapshot contact_uri = %q but the wire sends %q", snapshot, wire)
			}
			if !strings.HasPrefix(wire, tc.wantScheme) {
				t.Errorf("contact = %q, want the %q scheme", wire, tc.wantScheme)
			}
		})
	}
}

func TestNextHopSocket_DefaultPorts(t *testing.T) {
	cases := []struct{ proxy, want string }{
		{"sip:edge.acme.net:5080", "edge.acme.net:5080"},
		{"sip:edge.acme.net", "edge.acme.net:5060"},
		{"sips:edge.acme.net", "edge.acme.net:5061"},
		{"", ""},
	}
	for _, tt := range cases {
		if got := newProxyTrunk(t, nil, tt.proxy).nextHopSocket(); got != tt.want {
			t.Errorf("nextHopSocket() for proxy %q = %q, want %q", tt.proxy, got, tt.want)
		}
	}
}

// TestSendRegister_ProxyDeliversToProxySocket proves the routing end to end:
// the registrar name resolves nowhere, so the REGISTER can only arrive if the
// proxy socket is the transport destination. What lands there still names the
// registrar in its Request-URI and carries no Route.
func TestSendRegister_ProxyDeliversToProxySocket(t *testing.T) {
	reg := startFakeRegistrar(t, func(_ int, req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	})
	engine := newNATTestEngine(t)
	r := NewOutboundRegistration(engine, nil, nil, OutboundRegistrationConfig{}, OutboundRegistrationParams{
		ID:            "t-proxy-e2e",
		RegistrarURI:  sip.Uri{Scheme: "sip", Host: "registrar.invalid"},
		AOR:           sip.Uri{Scheme: "sip", User: "alice", Host: "registrar.invalid"},
		Username:      "alice",
		Password:      "secret",
		OutboundProxy: &sip.Uri{Scheme: "sip", Host: "127.0.0.1", Port: reg.port},
	})

	var lastErr error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, lastErr = r.sendRegister(ctx, 60, "", "")
		cancel()
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("REGISTER never reached the proxy socket: %v", lastErr)
	}

	seen := reg.received()
	if len(seen) == 0 {
		t.Fatal("proxy socket received no REGISTER")
	}
	got := seen[len(seen)-1]
	if got.GetHeader("Route") != nil {
		t.Errorf("REGISTER arrived with a pre-loaded Route:\n%s", got.String())
	}
	if got.Recipient.Host != "registrar.invalid" {
		t.Errorf("Request-URI host = %q, want the registrar", got.Recipient.Host)
	}
}
