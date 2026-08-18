package sip

import (
	"log/slog"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func newTrustTestEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine(EngineConfig{
		BindIP: "127.0.0.1", BindPort: pickFreePort(t, "udp"),
		SIPHost: "test", Log: slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func newTrustTestTrunk(t *testing.T, e *Engine, registrar, proxy string, insecure bool) *OutboundRegistration {
	t.Helper()
	var registrarURI sip.Uri
	if err := sip.ParseUri(registrar, &registrarURI); err != nil {
		t.Fatalf("parse registrar %q: %v", registrar, err)
	}
	var aor sip.Uri
	if err := sip.ParseUri("sip:alice@vb.test", &aor); err != nil {
		t.Fatalf("parse aor: %v", err)
	}
	params := OutboundRegistrationParams{
		ID:                    "trunk-1",
		RegistrarURI:          registrarURI,
		AOR:                   aor,
		Password:              "secret",
		TLSInsecureSkipVerify: insecure,
	}
	if proxy != "" {
		u, err := ParseProxyURI(proxy)
		if err != nil {
			t.Fatalf("parse proxy %q: %v", proxy, err)
		}
		params.OutboundProxy = &u
	}
	return NewOutboundRegistration(e, nil, slog.Default(), OutboundRegistrationConfig{}, params)
}

func TestTrunkTLSTrust_ExemptsRegistrarHost(t *testing.T) {
	e := newTrustTestEngine(t)
	r := newTrustTestTrunk(t, e, "sips:sbc.example.net:5061", "", true)

	if !e.peerTrust.trusted("sbc.example.net") {
		t.Fatal("trunk did not exempt its registrar host")
	}
	r.clearTLSTrust()
	if e.peerTrust.trusted("sbc.example.net") {
		t.Error("exemption survived the trunk")
	}
	r.clearTLSTrust() // deleting a trunk twice must not underflow another's count
	if e.peerTrust.trusted("sbc.example.net") {
		t.Error("second clear resurrected the exemption")
	}
}

// TestTrunkTLSTrust_ExemptsProxyNotRegistrar — the TLS handshake is with the
// next hop, so a proxied trunk must exempt the proxy and nothing else.
func TestTrunkTLSTrust_ExemptsProxyNotRegistrar(t *testing.T) {
	e := newTrustTestEngine(t)
	newTrustTestTrunk(t, e, "sip:pbx.example.com:5060", "sips:edge.acme.net:5061", true)

	if !e.peerTrust.trusted("edge.acme.net") {
		t.Error("proxy host not exempted")
	}
	if e.peerTrust.trusted("pbx.example.com") {
		t.Error("registrar host exempted, but the TLS peer is the proxy")
	}
}

func TestTrunkTLSTrust_IgnoredWhenNotApplicable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		registrar string
		host      string
	}{
		{name: "plain UDP next hop", registrar: "sip:pbx.example.com:5060", host: "pbx.example.com"},
		{name: "IP-literal next hop sends no SNI", registrar: "sips:198.51.100.7:5061", host: "198.51.100.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTrustTestEngine(t)
			r := newTrustTestTrunk(t, e, tc.registrar, "", true)
			if e.peerTrust.trusted(tc.host) {
				t.Errorf("%q must not be exempted", tc.host)
			}
			if r.trustedTLSHost != "" {
				t.Errorf("trustedTLSHost = %q, want empty", r.trustedTLSHost)
			}
		})
	}
}

func TestTrunkTLSTrust_OffByDefault(t *testing.T) {
	e := newTrustTestEngine(t)
	r := newTrustTestTrunk(t, e, "sips:sbc.example.net:5061", "", false)

	if e.peerTrust.trusted("sbc.example.net") {
		t.Error("a trunk without the flag must not exempt anything")
	}
	if view := r.Snapshot().SIPRegister; view.TLSInsecureSkipVerify {
		t.Error("snapshot reports an exemption that was never configured")
	}
}

func TestTrunkTLSTrust_SnapshotReportsFlag(t *testing.T) {
	e := newTrustTestEngine(t)
	r := newTrustTestTrunk(t, e, "sips:sbc.example.net:5061", "", true)

	if view := r.Snapshot().SIPRegister; !view.TLSInsecureSkipVerify {
		t.Error("snapshot omits the configured exemption")
	}
}

// TestTrunkTLSTrust_SharedHostRefcount — two trunks on one carrier: deleting
// the first must not re-enable verification under the second.
func TestTrunkTLSTrust_SharedHostRefcount(t *testing.T) {
	e := newTrustTestEngine(t)
	first := newTrustTestTrunk(t, e, "sips:sbc.example.net:5061", "", true)
	newTrustTestTrunk(t, e, "sips:sbc.example.net:5061", "", true)

	first.clearTLSTrust()
	if !e.peerTrust.trusted("sbc.example.net") {
		t.Error("the surviving trunk lost its exemption")
	}
}
