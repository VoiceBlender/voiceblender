package api

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// newProxyTestServer builds a test server with a real SIP engine, which
// applyTrunkIdentity needs for the trunk registry, and an optional global
// outbound proxy.
func newProxyTestServer(t *testing.T, globalProxy string) *Server {
	t.Helper()
	s := newTestServer(t)
	engine, err := sipmod.NewEngine(sipmod.EngineConfig{
		BindIP:   "127.0.0.1",
		BindPort: freeUDPPort(t),
		SIPHost:  "test",
		Log:      slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	s.SIPEngine = engine
	s.Config.SIPOutboundProxy = globalProxy
	return s
}

// addTrunk registers a sip_register trunk with the given optional proxy and
// returns it. Nothing is started, so no REGISTER goes on the wire.
func addTrunk(t *testing.T, s *Server, aorUser, proxy string) *sipmod.OutboundRegistration {
	t.Helper()
	p := sipmod.OutboundRegistrationParams{
		ID:           "trunk-" + aorUser,
		RegistrarURI: sip.Uri{Scheme: "sip", Host: "pbx.example.com", Port: 5060},
		AOR:          sip.Uri{Scheme: "sip", User: aorUser, Host: "pbx.example.com"},
		Username:     aorUser,
		Password:     "secret",
	}
	if proxy != "" {
		u, err := sipmod.ParseProxyURI(proxy)
		if err != nil {
			t.Fatalf("ParseProxyURI(%q): %v", proxy, err)
		}
		p.OutboundProxy = &u
	}
	trunk := sipmod.NewOutboundRegistration(s.SIPEngine, nil, nil, sipmod.OutboundRegistrationConfig{}, p)
	s.SIPEngine.Trunks().Add(trunk)
	return trunk
}

func proxyHost(u *sip.Uri) string {
	if u == nil {
		return ""
	}
	return u.Host
}

// TestOutboundProxyPrecedence walks the resolution order that POST /v1/legs
// implements: trunk proxy beats the global default, and the global default only
// fills in when nothing more specific chose a next hop.
func TestOutboundProxyPrecedence(t *testing.T) {
	cases := []struct {
		name        string
		globalProxy string
		trunkProxy  string
		from        string
		wantProxy   string // expected ProxyURI host, "" for nil
		wantRoute   string // expected RouteURI host, "" for nil
	}{
		{
			name: "no trunk, no global", from: "", wantProxy: "", wantRoute: "",
		},
		{
			name: "no trunk, global set", globalProxy: "sip:global.acme.net", from: "",
			wantProxy: "global.acme.net", wantRoute: "",
		},
		{
			// Backwards-compat pin: an unconfigured trunk still routes at its
			// registrar via RouteURI, with no proxy.
			name: "trunk without proxy", from: "alice",
			wantProxy: "", wantRoute: "pbx.example.com",
		},
		{
			name: "trunk with proxy", trunkProxy: "sip:trunk.acme.net", from: "alice",
			wantProxy: "trunk.acme.net", wantRoute: "pbx.example.com",
		},
		{
			name:        "trunk proxy beats global",
			globalProxy: "sip:global.acme.net", trunkProxy: "sip:trunk.acme.net", from: "alice",
			wantProxy: "trunk.acme.net", wantRoute: "pbx.example.com",
		},
		{
			// The global default must not displace a matched trunk's registrar
			// route: setting the env var cannot silently redirect existing trunks.
			name:        "global does not override trunk registrar",
			globalProxy: "sip:global.acme.net", from: "alice",
			wantProxy: "", wantRoute: "pbx.example.com",
		},
		{
			name: "unmatched from with global", globalProxy: "sip:global.acme.net", from: "nobody",
			wantProxy: "global.acme.net", wantRoute: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newProxyTestServer(t, tc.globalProxy)
			if tc.from != "" {
				addTrunk(t, s, "alice", tc.trunkProxy)
			}
			var opts sipmod.InviteOptions
			s.applyFromIdentity(tc.from, &opts)

			if got := proxyHost(opts.ProxyURI); got != tc.wantProxy {
				t.Errorf("ProxyURI host = %q, want %q", got, tc.wantProxy)
			}
			if got := proxyHost(opts.RouteURI); got != tc.wantRoute {
				t.Errorf("RouteURI host = %q, want %q", got, tc.wantRoute)
			}
		})
	}
}

// TestApplyFromIdentity_InvalidGlobalProxyIgnored pins that a malformed env var
// degrades to no proxy rather than failing the call. Startup validation is the
// place that rejects it loudly.
func TestApplyFromIdentity_InvalidGlobalProxyIgnored(t *testing.T) {
	s := newProxyTestServer(t, "not-a-uri")
	var opts sipmod.InviteOptions
	s.applyFromIdentity("", &opts)
	if opts.ProxyURI != nil {
		t.Errorf("ProxyURI = %v, want nil for a malformed SIP_OUTBOUND_PROXY", opts.ProxyURI)
	}
}

func TestCreateLeg_InvalidOutboundProxy(t *testing.T) {
	s := newProxyTestServer(t, "")
	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"sip","to":"sip:bob@example.com","outbound_proxy":"not-a-uri"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid outbound_proxy") {
		t.Errorf("body = %s, want an outbound_proxy error", w.Body.String())
	}
}

func TestCreateLeg_OutboundProxyWrongScheme(t *testing.T) {
	s := newProxyTestServer(t, "")
	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"sip","to":"sip:bob@example.com","outbound_proxy":"http://p.example"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
