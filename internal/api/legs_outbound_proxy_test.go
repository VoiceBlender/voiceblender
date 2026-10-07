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
// applyFromIdentity needs for the trunk registry, and an optional global
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

// addIPIPTrunk registers an ip_ip trunk peered with carrier.example.com and
// returns it. Nothing is started, so no OPTIONS goes on the wire.
func addIPIPTrunk(t *testing.T, s *Server, id, aorUser, proxy string) *sipmod.IPIPTrunk {
	t.Helper()
	p := sipmod.IPIPTrunkParams{
		ID:       id,
		PeerURI:  sip.Uri{Scheme: "sip", Host: "carrier.example.com", Port: 5060},
		Username: "acct",
		Password: "peersecret",
	}
	if aorUser != "" {
		p.AOR = &sip.Uri{Scheme: "sip", User: aorUser, Host: "carrier.example.com"}
	}
	if proxy != "" {
		u, err := sipmod.ParseProxyURI(proxy)
		if err != nil {
			t.Fatalf("ParseProxyURI(%q): %v", proxy, err)
		}
		p.OutboundProxy = &u
	}
	trunk := sipmod.NewIPIPTrunk(s.SIPEngine, nil, nil, p)
	s.SIPEngine.Trunks().Add(trunk)
	return trunk
}

var testRecipient = sip.Uri{Scheme: "sip", User: "bob", Host: "example.com"}

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
			s.applyFromIdentity(nil, tc.from, testRecipient, &opts)

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
	s.applyFromIdentity(nil, "", testRecipient, &opts)
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

// TestApplyFromIdentity_ExplicitTrunk covers a trunk named by trunk_id rather
// than found through the From.
func TestApplyFromIdentity_ExplicitTrunk(t *testing.T) {
	toPeer := sip.Uri{Scheme: "sip", User: "bob", Host: "carrier.example.com"}

	t.Run("explicit trunk beats from match", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		addTrunk(t, s, "alice", "")
		ipip := addIPIPTrunk(t, s, "peer", "", "")
		var opts sipmod.InviteOptions
		if got := s.applyFromIdentity(ipip, "alice", testRecipient, &opts); got != "peer" {
			t.Errorf("trunk id = %q, want peer", got)
		}
		if got := proxyHost(opts.RouteURI); got != "carrier.example.com" {
			t.Errorf("RouteURI host = %q, want the ip_ip peer", got)
		}
		if opts.AuthUsername != "acct" || opts.AuthPassword != "peersecret" {
			t.Errorf("auth = %q/%q, want the ip_ip trunk's", opts.AuthUsername, opts.AuthPassword)
		}
		if opts.FromUser != "alice" || opts.FromHost != "" {
			t.Errorf("from = %q@%q, want alice with no trunk realm", opts.FromUser, opts.FromHost)
		}
	})

	t.Run("empty from takes the trunk AOR", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		reg := addTrunk(t, s, "alice", "")
		var opts sipmod.InviteOptions
		if got := s.applyFromIdentity(reg, "", testRecipient, &opts); got != reg.ID() {
			t.Errorf("trunk id = %q, want %q", got, reg.ID())
		}
		if opts.FromUser != "alice" || opts.FromHost != "pbx.example.com" {
			t.Errorf("from = %q@%q, want alice@pbx.example.com", opts.FromUser, opts.FromHost)
		}
	})

	t.Run("empty from without an AOR stays empty", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		ipip := addIPIPTrunk(t, s, "peer", "", "")
		var opts sipmod.InviteOptions
		s.applyFromIdentity(ipip, "", testRecipient, &opts)
		if opts.FromUser != "" || opts.FromHost != "" {
			t.Errorf("from = %q@%q, want none", opts.FromUser, opts.FromHost)
		}
	})

	t.Run("caller auth wins", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		ipip := addIPIPTrunk(t, s, "peer", "", "")
		opts := sipmod.InviteOptions{AuthUsername: "caller", AuthPassword: "own"}
		s.applyFromIdentity(ipip, "", testRecipient, &opts)
		if opts.AuthUsername != "caller" || opts.AuthPassword != "own" {
			t.Errorf("auth = %q/%q, want the caller's", opts.AuthUsername, opts.AuthPassword)
		}
	})

	t.Run("no route when the recipient is the peer", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		ipip := addIPIPTrunk(t, s, "peer", "", "")
		var opts sipmod.InviteOptions
		s.applyFromIdentity(ipip, "", toPeer, &opts)
		if opts.RouteURI != nil || opts.ProxyURI != nil {
			t.Errorf("route = %v, proxy = %v, want neither", opts.RouteURI, opts.ProxyURI)
		}
	})

	t.Run("global proxy does not displace a routeless trunk", func(t *testing.T) {
		s := newProxyTestServer(t, "sip:global.acme.net")
		ipip := addIPIPTrunk(t, s, "peer", "", "")
		var opts sipmod.InviteOptions
		s.applyFromIdentity(ipip, "", toPeer, &opts)
		if opts.ProxyURI != nil {
			t.Errorf("ProxyURI = %v, want nil", opts.ProxyURI)
		}
	})

	t.Run("trunk proxy applies", func(t *testing.T) {
		s := newProxyTestServer(t, "")
		ipip := addIPIPTrunk(t, s, "peer", "", "sip:edge.acme.net")
		var opts sipmod.InviteOptions
		s.applyFromIdentity(ipip, "", testRecipient, &opts)
		if got := proxyHost(opts.ProxyURI); got != "edge.acme.net" {
			t.Errorf("ProxyURI host = %q, want edge.acme.net", got)
		}
	})

	t.Run("ip_ip matched by from AOR", func(t *testing.T) {
		for _, from := range []string{"acme", "sip:acme@carrier.example.com"} {
			s := newProxyTestServer(t, "")
			addIPIPTrunk(t, s, "peer", "acme", "")
			var opts sipmod.InviteOptions
			if got := s.applyFromIdentity(nil, from, testRecipient, &opts); got != "peer" {
				t.Errorf("from %q: trunk id = %q, want peer", from, got)
			}
			if opts.FromHost != "carrier.example.com" {
				t.Errorf("from %q: FromHost = %q, want the AOR realm", from, opts.FromHost)
			}
			if opts.AuthPassword != "peersecret" {
				t.Errorf("from %q: trunk credentials not attached", from)
			}
		}
	})
}

func TestCreateLeg_UnknownTrunkID(t *testing.T) {
	s := newProxyTestServer(t, "")
	w := doRequest(s, http.MethodPost, "/v1/legs",
		`{"type":"sip","to":"sip:bob@example.com","trunk_id":"missing","room_id":"r1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trunk not found") {
		t.Errorf("body = %s, want a trunk error", w.Body.String())
	}
	if n := len(s.LegMgr.List()); n != 0 {
		t.Errorf("legs = %d, want none created", n)
	}
	if _, ok := s.RoomMgr.Get("r1"); ok {
		t.Error("room r1 was created for a rejected request")
	}
}
