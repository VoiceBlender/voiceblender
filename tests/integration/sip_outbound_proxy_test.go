//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/emiago/sipgo/sip"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// blackholePort returns a port nothing is listening on. Used as a registrar or
// callee address that must never be contacted directly once a proxy is
// configured — if the Route is dropped, the request lands nowhere and the test
// fails on the missing arrival rather than passing by accident.
func blackholePort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	return port
}

// assertLooseRoute checks that a received request was steered by a loose Route
// header naming the given socket, and that its Request-URI survived untouched.
// The ";lr" is a hard assert: without it sipgo strict-routes and rewrites the
// Request-URI, which would silently break every in-dialog request.
func assertLooseRoute(t *testing.T, req *sip.Request, wantHost string, wantPort int, wantRequestURI string) {
	t.Helper()
	route := req.Route()
	if route == nil {
		t.Fatalf("request has no Route header; proxy not applied:\n%s", req.String())
	}
	if route.Address.Host != wantHost || route.Address.Port != wantPort {
		t.Errorf("Route = %q, want %s:%d", route.Address.String(), wantHost, wantPort)
	}
	if !route.Address.UriParams.Has("lr") {
		t.Errorf("Route %q lacks the lr param — sipgo would strict-route", route.Address.String())
	}
	if got := req.Recipient.String(); got != wantRequestURI {
		t.Errorf("Request-URI = %q, want it unchanged at %q", got, wantRequestURI)
	}
}

// createProxyTrunk brings up a sip_register trunk with AOR sip:alice@vb.test
// and waits for it to register. registrarPort names the registrar in the
// Request-URI; proxyPort, when non-zero, is the configured next hop.
func createProxyTrunk(t *testing.T, baseURL string, registrarPort, proxyPort int) string {
	t.Helper()
	spec := map[string]interface{}{
		"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", registrarPort),
		"aor":           "sip:alice@vb.test",
		"username":      "alice",
		"password":      "secret",
	}
	if proxyPort != 0 {
		spec["outbound_proxy"] = fmt.Sprintf("sip:127.0.0.1:%d", proxyPort)
	}
	resp, body := createTrunkRequest(t, baseURL, map[string]interface{}{
		"type":         "sip_register",
		"sip_register": spec,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create trunk: status %d, body=%s", resp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode trunk: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing trunk id")
	}
	return id
}

// waitForInvite blocks until the fake endpoint records an INVITE.
func waitForInvite(t *testing.T, r *rawSIPRegistrar, timeout time.Duration) *sip.Request {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		n := len(r.receivedInvites)
		r.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.receivedInvites) == 0 {
		t.Fatal("no INVITE received within timeout")
	}
	return r.receivedInvites[0]
}

func originateLeg(t *testing.T, baseURL string, body map[string]interface{}) {
	t.Helper()
	resp := httpPost(t, baseURL+"/v1/legs", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Trunk REGISTER via a proxy
// ---------------------------------------------------------------------------

// TestTrunk_OutboundProxy_RegisterRoutedViaProxy is the core proof of loose
// routing: the REGISTER reaches the proxy while its Request-URI still names a
// registrar that is not listening at all.
func TestTrunk_OutboundProxy_RegisterRoutedViaProxy(t *testing.T) {
	inst := newTestInstance(t, "proxy-register")
	proxy := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})
	registrarPort := blackholePort(t)

	id := createProxyTrunk(t, inst.baseURL(), registrarPort, proxy.port)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	req := proxy.lastRegister()
	if req == nil {
		t.Fatal("proxy received no REGISTER")
	}
	assertLooseRoute(t, req, "127.0.0.1", proxy.port,
		fmt.Sprintf("sip:127.0.0.1:%d", registrarPort))

	// The snapshot must report the next hop actually in effect.
	snap := trunkSnapshot(t, inst.baseURL(), id)
	reg, _ := snap["sip_register"].(map[string]interface{})
	if got, _ := reg["outbound_proxy"].(string); got != fmt.Sprintf("sip:127.0.0.1:%d", proxy.port) {
		t.Errorf("snapshot outbound_proxy = %q, want the configured proxy", got)
	}
}

// TestTrunk_NoProxy_RegisterHasNoRoute is the backwards-compatibility pin.
func TestTrunk_NoProxy_RegisterHasNoRoute(t *testing.T) {
	inst := newTestInstance(t, "proxy-none")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})

	id := createProxyTrunk(t, inst.baseURL(), reg.port, 0)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	req := reg.lastRegister()
	if req == nil {
		t.Fatal("registrar received no REGISTER")
	}
	if req.GetHeader("Route") != nil {
		t.Errorf("REGISTER gained a Route header with no proxy configured:\n%s", req.String())
	}

	snap := trunkSnapshot(t, inst.baseURL(), id)
	sr, _ := snap["sip_register"].(map[string]interface{})
	if _, present := sr["outbound_proxy"]; present {
		t.Error("snapshot exposes outbound_proxy when none is configured")
	}
}

// ---------------------------------------------------------------------------
// INVITE via a proxy
// ---------------------------------------------------------------------------

func TestTrunk_OutboundProxy_InviteRoutedViaProxy(t *testing.T) {
	inst := newTestInstance(t, "proxy-invite")
	proxy := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})
	registrarPort := blackholePort(t)
	calleePort := blackholePort(t)

	id := createProxyTrunk(t, inst.baseURL(), registrarPort, proxy.port)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	callee := fmt.Sprintf("sip:bob@127.0.0.1:%d", calleePort)
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type":   "sip",
		"to":     callee,
		"from":   "alice",
		"codecs": []string{"PCMU"},
	})

	inv := waitForInvite(t, proxy, 5*time.Second)
	assertLooseRoute(t, inv, "127.0.0.1", proxy.port, callee)
}

// TestLeg_OutboundProxy_Explicit covers a per-call proxy with no trunk at all.
func TestLeg_OutboundProxy_Explicit(t *testing.T) {
	inst := newTestInstance(t, "proxy-leg")
	proxy := newRawSIPRegistrar(t, rawRegistrarOpts{})
	calleePort := blackholePort(t)

	callee := fmt.Sprintf("sip:bob@127.0.0.1:%d", calleePort)
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type":           "sip",
		"to":             callee,
		"outbound_proxy": fmt.Sprintf("sip:127.0.0.1:%d", proxy.port),
		"codecs":         []string{"PCMU"},
	})

	inv := waitForInvite(t, proxy, 5*time.Second)
	assertLooseRoute(t, inv, "127.0.0.1", proxy.port, callee)
}

// TestLeg_OutboundProxy_OverridesTrunk pins leg > trunk: the trunk's proxy is a
// blackhole, so the INVITE can only arrive if the per-leg value won.
func TestLeg_OutboundProxy_OverridesTrunk(t *testing.T) {
	inst := newTestInstance(t, "proxy-leg-wins")
	legProxy := newRawSIPRegistrar(t, rawRegistrarOpts{})
	registrar := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})
	trunkProxyPort := registrar.port // trunk registers fine through here
	calleePort := blackholePort(t)

	id := createProxyTrunk(t, inst.baseURL(), registrar.port, trunkProxyPort)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 5*time.Second)

	callee := fmt.Sprintf("sip:bob@127.0.0.1:%d", calleePort)
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type":           "sip",
		"to":             callee,
		"from":           "alice",
		"outbound_proxy": fmt.Sprintf("sip:127.0.0.1:%d", legProxy.port),
		"codecs":         []string{"PCMU"},
	})

	inv := waitForInvite(t, legProxy, 5*time.Second)
	assertLooseRoute(t, inv, "127.0.0.1", legProxy.port, callee)
}

// TestLeg_GlobalOutboundProxy_Default covers SIP_OUTBOUND_PROXY with no trunk
// and no per-leg value.
func TestLeg_GlobalOutboundProxy_Default(t *testing.T) {
	proxy := newRawSIPRegistrar(t, rawRegistrarOpts{})
	inst := newTestInstanceWithOpts(t, "proxy-global", func(c *config.Config) {
		c.SIPOutboundProxy = fmt.Sprintf("sip:127.0.0.1:%d", proxy.port)
	})
	calleePort := blackholePort(t)

	callee := fmt.Sprintf("sip:bob@127.0.0.1:%d", calleePort)
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type":   "sip",
		"to":     callee,
		"codecs": []string{"PCMU"},
	})

	inv := waitForInvite(t, proxy, 5*time.Second)
	assertLooseRoute(t, inv, "127.0.0.1", proxy.port, callee)
}

// TestLeg_OutboundProxy_Invalid pins the 400.
func TestLeg_OutboundProxy_Invalid(t *testing.T) {
	inst := newTestInstance(t, "proxy-bad")
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":           "sip",
		"to":             "sip:bob@127.0.0.1:5060",
		"outbound_proxy": "not-a-uri",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
