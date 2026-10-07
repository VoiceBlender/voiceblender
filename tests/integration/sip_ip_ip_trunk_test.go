//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// createIPIPTrunk creates an ip_ip trunk from the given spec and returns its id.
func createIPIPTrunk(t *testing.T, baseURL, appID string, spec map[string]interface{}) string {
	t.Helper()
	body := map[string]interface{}{"type": "ip_ip", "ip_ip": spec}
	if appID != "" {
		body["app_id"] = appID
	}
	resp, data := createTrunkRequest(t, baseURL, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create ip_ip trunk: status %d, body=%s", resp.StatusCode, data)
	}
	var created struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatalf("decode trunk: %v", err)
	}
	if created.ID == "" || created.Type != "ip_ip" || created.Status != "active" {
		t.Fatalf("create response = %+v, want an active ip_ip trunk", created)
	}
	return created.ID
}

func peerURI(port int) string { return fmt.Sprintf("sip:127.0.0.1:%d", port) }

// waitForInvites blocks until the fake peer has recorded n INVITEs.
func waitForInvites(t *testing.T, r *rawSIPRegistrar, n int, timeout time.Duration) []*sip.Request {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := r.invites(); len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("peer received %d INVITEs within timeout, want %d", len(r.invites()), n)
	return nil
}

func trunkEventFor(id string) func(events.Event) bool {
	return func(e events.Event) bool {
		d, ok := e.Data.(*events.SIPTrunkStatusData)
		return ok && d.TrunkID == id
	}
}

// waitForTrunkEvents blocks until n events of typ have been published for the
// trunk, and returns the last one.
func waitForTrunkEvents(t *testing.T, inst *testInstance, typ events.EventType, id string, n int, timeout time.Duration) *events.SIPTrunkStatusData {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if all := inst.collector.matchAll(typ, trunkEventFor(id)); len(all) >= n {
			return all[n-1].Data.(*events.SIPTrunkStatusData)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s #%d on trunk %s", typ, n, id)
	return nil
}

func ringingTo(user string) func(events.Event) bool {
	return func(e events.Event) bool {
		d, ok := e.Data.(*events.LegRingingData)
		return ok && strings.Contains(d.To, user)
	}
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

func TestTrunk_IPIP_CreateGetDelete(t *testing.T) {
	inst := newTestInstance(t, "ipip-crud")
	peer := newRawSIPRegistrar(t, rawRegistrarOpts{})

	resp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type":   "ip_ip",
		"app_id": "acme",
		"ip_ip": map[string]interface{}{
			"peer_uri":        peerURI(peer.port),
			"aor":             "sip:acme@carrier.test",
			"password":        "hunter2",
			"inbound_sources": []string{"198.51.100.0/24"},
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "hunter2") {
		t.Errorf("create response leaks the password: %s", body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" || created["type"] != "ip_ip" || created["status"] != "active" {
		t.Fatalf("create response = %v, want an active ip_ip trunk", created)
	}

	snap := trunkSnapshot(t, inst.baseURL(), id)
	if snap["type"] != "ip_ip" || snap["status"] != "active" || snap["app_id"] != "acme" {
		t.Errorf("snapshot = %v", snap)
	}
	if _, has := snap["sip_register"]; has {
		t.Errorf("ip_ip snapshot carries a sip_register block: %v", snap)
	}
	sub, _ := snap["ip_ip"].(map[string]interface{})
	if sub["peer_uri"] != peerURI(peer.port) || sub["aor"] != "sip:acme@carrier.test" || sub["username"] != "acme" {
		t.Errorf("ip_ip block = %v", sub)
	}
	sources, _ := sub["inbound_sources"].([]interface{})
	if len(sources) != 2 || sources[0] != "198.51.100.0/24" || sources[1] != "127.0.0.1/32" {
		t.Errorf("inbound_sources = %v, want the listed range plus the peer", sources)
	}

	listResp := httpGet(t, inst.baseURL()+"/v1/sip/trunks")
	var list struct {
		Trunks []map[string]interface{} `json:"trunks"`
	}
	decodeJSON(t, listResp, &list)
	if len(list.Trunks) != 1 || list.Trunks[0]["id"] != id {
		t.Errorf("list = %v, want the one trunk", list.Trunks)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "password") {
		t.Errorf("list leaks the password: %s", raw)
	}

	// A static peer is never registered with, and is not pinged unless asked.
	time.Sleep(300 * time.Millisecond)
	if n := peer.registerCount(); n != 0 {
		t.Errorf("peer received %d REGISTERs, want none", n)
	}
	if n := len(peer.options()); n != 0 {
		t.Errorf("peer received %d OPTIONS with the health check off", n)
	}

	del := httpDelete(t, inst.baseURL()+"/v1/sip/trunks/"+id)
	del.Body.Close()
	if del.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE status = %d, want 202", del.StatusCode)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		get := httpGet(t, inst.baseURL()+"/v1/sip/trunks/"+id)
		get.Body.Close()
		if get.StatusCode == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trunk still present after delete: %d", get.StatusCode)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if inst.collector.hasEvent(events.SIPTrunkDown, nil) || inst.collector.hasEvent(events.SIPTrunkUp, nil) {
		t.Error("trunk health event published without a health check")
	}
}

func TestTrunk_IPIP_Validation(t *testing.T) {
	inst := newTestInstance(t, "ipip-validation")
	cases := []struct {
		name string
		spec map[string]interface{}
		want string
	}{
		{"missing peer_uri", map[string]interface{}{}, "peer_uri is required"},
		{"bad peer_uri", map[string]interface{}{"peer_uri": "http://pbx.example"}, "peer_uri is invalid"},
		{"hostname source", map[string]interface{}{"peer_uri": "sip:pbx.example", "inbound_sources": []string{"pbx.example"}}, "inbound_sources is invalid"},
		{"catch-all source", map[string]interface{}{"peer_uri": "sip:pbx.example", "inbound_sources": []string{"::/0"}}, "matches every address"},
		{"username without password", map[string]interface{}{"peer_uri": "sip:pbx.example", "username": "u"}, "password is required"},
		{"interval out of range", map[string]interface{}{"peer_uri": "sip:pbx.example", "options_ping_interval_seconds": 7200}, "options_ping_interval_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{"type": "ip_ip", "ip_ip": tc.spec})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %s, want %q", body, tc.want)
			}
		})
	}
	resp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{"type": "ip_ip"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing block: status = %d, want 400; body=%s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

// TestTrunk_IPIP_OutboundByTrunkID is the core proof of outbound routing: a
// call to a host that is not listening reaches the peer through a loose Route,
// while a call that already targets the peer carries none.
func TestTrunk_IPIP_OutboundByTrunkID(t *testing.T) {
	t.Run("routed at the peer", func(t *testing.T) {
		inst := newTestInstance(t, "ipip-out-route")
		peer := newRawSIPRegistrar(t, rawRegistrarOpts{})
		id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{"peer_uri": peerURI(peer.port)})

		to := fmt.Sprintf("sip:bob@127.0.0.1:%d", blackholePort(t))
		originateLeg(t, inst.baseURL(), map[string]interface{}{
			"type": "sip", "to": to, "from": "+15551234567", "trunk_id": id, "codecs": []string{"PCMU"},
		})

		inv := waitForInvite(t, peer, 3*time.Second)
		assertLooseRoute(t, inv, "127.0.0.1", peer.port, to)
		// The trunk has no AOR, so the From realm is the engine's own host.
		assertInviteFromIdentity(t, inv, "+15551234567", "127.0.0.1")

		ev := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
		if d := ev.Data.(*events.LegRingingData); d.TrunkID != id {
			t.Errorf("leg.ringing trunk_id = %q, want %q", d.TrunkID, id)
		}
	})

	t.Run("no route when the call targets the peer", func(t *testing.T) {
		inst := newTestInstance(t, "ipip-out-direct")
		peer := newRawSIPRegistrar(t, rawRegistrarOpts{})
		id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{"peer_uri": peerURI(peer.port)})

		originateLeg(t, inst.baseURL(), map[string]interface{}{
			"type": "sip", "to": fmt.Sprintf("sip:bob@127.0.0.1:%d", peer.port), "trunk_id": id, "codecs": []string{"PCMU"},
		})

		inv := waitForInvite(t, peer, 3*time.Second)
		if inv.GetHeader("Route") != nil {
			t.Errorf("INVITE to the peer itself carries a Route:\n%s", inv.String())
		}
	})
}

func TestTrunk_IPIP_OutboundByFromAOR(t *testing.T) {
	for _, from := range []string{"acme", "sip:acme@carrier.test"} {
		t.Run(from, func(t *testing.T) {
			inst := newTestInstance(t, "ipip-out-aor")
			peer := newRawSIPRegistrar(t, rawRegistrarOpts{})
			createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
				"peer_uri": peerURI(peer.port),
				"aor":      "sip:acme@carrier.test",
			})

			to := fmt.Sprintf("sip:bob@127.0.0.1:%d", blackholePort(t))
			originateLeg(t, inst.baseURL(), map[string]interface{}{
				"type": "sip", "to": to, "from": from, "codecs": []string{"PCMU"},
			})

			inv := waitForInvite(t, peer, 3*time.Second)
			assertLooseRoute(t, inv, "127.0.0.1", peer.port, to)
			assertInviteFromIdentity(t, inv, "acme", "carrier.test")
		})
	}
}

// TestTrunk_IPIP_TrunkIDBeatsFrom pins the precedence: a `from` matching one
// trunk's AOR does not pull the call away from the trunk named by trunk_id.
func TestTrunk_IPIP_TrunkIDBeatsFrom(t *testing.T) {
	inst := newTestInstance(t, "ipip-out-precedence")
	byAOR := newRawSIPRegistrar(t, rawRegistrarOpts{})
	byID := newRawSIPRegistrar(t, rawRegistrarOpts{})
	createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
		"peer_uri": peerURI(byAOR.port),
		"aor":      "sip:acme@carrier.test",
	})
	id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{"peer_uri": peerURI(byID.port)})

	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip", "to": fmt.Sprintf("sip:bob@127.0.0.1:%d", blackholePort(t)),
		"from": "acme", "trunk_id": id, "codecs": []string{"PCMU"},
	})

	inv := waitForInvite(t, byID, 3*time.Second)
	// The named trunk has no realm of its own.
	assertInviteFromIdentity(t, inv, "acme", "127.0.0.1")
	time.Sleep(300 * time.Millisecond)
	if n := len(byAOR.invites()); n != 0 {
		t.Errorf("the AOR-matched trunk's peer received %d INVITEs, want none", n)
	}
}

func TestTrunk_IPIP_DigestAuth(t *testing.T) {
	inst := newTestInstance(t, "ipip-out-digest")
	peer := newRawSIPRegistrar(t, rawRegistrarOpts{challengeInvite: true})
	id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
		"peer_uri": peerURI(peer.port),
		"username": "acct",
		"password": "secret",
	})

	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip", "to": fmt.Sprintf("sip:bob@127.0.0.1:%d", peer.port), "trunk_id": id, "codecs": []string{"PCMU"},
	})

	invites := waitForInvites(t, peer, 2, 5*time.Second)
	if invites[0].GetHeader("Proxy-Authorization") != nil {
		t.Error("first INVITE already carries credentials")
	}
	auth := invites[1].GetHeader("Proxy-Authorization")
	if auth == nil {
		t.Fatalf("challenged INVITE was not retried with credentials:\n%s", invites[1].String())
	}
	if !strings.Contains(auth.Value(), `username="acct"`) {
		t.Errorf("Proxy-Authorization = %q, want the trunk's username", auth.Value())
	}
}

// TestLeg_TrunkID_SIPRegister pins that trunk_id selects a sip_register trunk
// too, and that an omitted `from` falls back to the trunk's AOR.
func TestLeg_TrunkID_SIPRegister(t *testing.T) {
	inst := newTestInstance(t, "leg-trunk-id-reg")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})
	id := createProxyTrunk(t, inst.baseURL(), reg.port, 0)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 3*time.Second)

	to := fmt.Sprintf("sip:bob@127.0.0.1:%d", blackholePort(t))
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip", "to": to, "trunk_id": id, "codecs": []string{"PCMU"},
	})

	inv := waitForInvite(t, reg, 3*time.Second)
	assertLooseRoute(t, inv, "127.0.0.1", reg.port, to)
	assertInviteFromIdentity(t, inv, "alice", "vb.test")
}

func TestLeg_TrunkID_Unknown(t *testing.T) {
	inst := newTestInstance(t, "leg-trunk-id-unknown")
	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": "sip:bob@127.0.0.1:5999", "trunk_id": "no-such-trunk",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	legs := httpGet(t, inst.baseURL()+"/v1/legs")
	var listed []legView
	decodeJSON(t, legs, &listed)
	if len(listed) != 0 {
		t.Errorf("%d legs exist after a rejected create", len(listed))
	}
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// TestTrunk_IPIP_InboundByPeerSocket pins that an inbound call is attributed to
// the trunk whose peer socket it came from, that the trunk's app_id beats a
// caller-supplied X-App-ID, and that two peers on one address are told apart by
// port.
func TestTrunk_IPIP_InboundByPeerSocket(t *testing.T) {
	inst := newTestInstance(t, "ipip-in-socket")
	cliA := newRawSIPClient(t, "ipip-peer-a")
	cliB := newRawSIPClient(t, "ipip-peer-b")
	idA := createIPIPTrunk(t, inst.baseURL(), "app-a", map[string]interface{}{"peer_uri": peerURI(cliA.port)})
	idB := createIPIPTrunk(t, inst.baseURL(), "app-b", map[string]interface{}{"peer_uri": peerURI(cliB.port)})

	cases := []struct {
		user      string
		cli       *rawSIPClient
		wantTrunk string
		wantApp   string
	}{
		{"from-a", cliA, idA, "app-a"},
		{"from-b", cliB, idB, "app-b"},
	}
	for _, tc := range cases {
		t.Run(tc.user, func(t *testing.T) {
			sendRawInvite(t, tc.cli, inst.sipPort, tc.user, sip.NewHeader("X-App-ID", "spoofed"))

			ev := inst.collector.waitForMatch(t, events.LegRinging, ringingTo(tc.user), 5*time.Second)
			d := ev.Data.(*events.LegRingingData)
			if d.TrunkID != tc.wantTrunk {
				t.Errorf("trunk_id = %q, want %q", d.TrunkID, tc.wantTrunk)
			}
			if d.AppID != tc.wantApp {
				t.Errorf("app_id = %q, want %q", d.AppID, tc.wantApp)
			}
			resp := httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID)
			resp.Body.Close()
		})
	}
}

// TestTrunk_IPIP_InboundByCIDR covers a peer named by hostname, recognised only
// through inbound_sources, and the longest-prefix rule between two trunks.
func TestTrunk_IPIP_InboundByCIDR(t *testing.T) {
	inst := newTestInstance(t, "ipip-in-cidr")
	cli := newRawSIPClient(t, "ipip-cidr-ua")

	wide := createIPIPTrunk(t, inst.baseURL(), "app-wide", map[string]interface{}{
		"peer_uri":        "sip:carrier.invalid",
		"inbound_sources": []string{"127.0.0.0/8"},
	})
	createIPIPTrunk(t, inst.baseURL(), "app-elsewhere", map[string]interface{}{
		"peer_uri":        "sip:other.invalid",
		"inbound_sources": []string{"10.0.0.0/8"},
	})

	sendRawInvite(t, cli, inst.sipPort, "wide-call")
	d := inst.collector.waitForMatch(t, events.LegRinging, ringingTo("wide-call"), 5*time.Second).Data.(*events.LegRingingData)
	if d.TrunkID != wide || d.AppID != "app-wide" {
		t.Errorf("trunk_id = %q app_id = %q, want %q / app-wide", d.TrunkID, d.AppID, wide)
	}
	httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID).Body.Close()

	narrow := createIPIPTrunk(t, inst.baseURL(), "app-narrow", map[string]interface{}{
		"peer_uri":        "sip:carrier2.invalid",
		"inbound_sources": []string{"127.0.0.0/24"},
	})
	sendRawInvite(t, cli, inst.sipPort, "narrow-call")
	d = inst.collector.waitForMatch(t, events.LegRinging, ringingTo("narrow-call"), 5*time.Second).Data.(*events.LegRingingData)
	if d.TrunkID != narrow || d.AppID != "app-narrow" {
		t.Errorf("trunk_id = %q app_id = %q, want the longer prefix %q / app-narrow", d.TrunkID, d.AppID, narrow)
	}
	httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID).Body.Close()
}

// TestTrunk_IPIP_InboundUnmatchedStillRings pins that inbound_sources is a
// matching rule and not an access list.
func TestTrunk_IPIP_InboundUnmatchedStillRings(t *testing.T) {
	inst := newTestInstance(t, "ipip-in-unmatched")
	cli := newRawSIPClient(t, "ipip-unmatched-ua")
	createIPIPTrunk(t, inst.baseURL(), "app-elsewhere", map[string]interface{}{
		"peer_uri":        "sip:192.0.2.10",
		"inbound_sources": []string{"10.0.0.0/8"},
	})

	sendRawInvite(t, cli, inst.sipPort, "stranger")
	d := inst.collector.waitForMatch(t, events.LegRinging, ringingTo("stranger"), 5*time.Second).Data.(*events.LegRingingData)
	if d.TrunkID != "" || d.AppID != "" {
		t.Errorf("trunk_id = %q app_id = %q, want an untagged call", d.TrunkID, d.AppID)
	}
	httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID).Body.Close()
}

// ---------------------------------------------------------------------------
// OPTIONS health check
// ---------------------------------------------------------------------------

func TestTrunk_IPIP_OptionsPing_UpDownUp(t *testing.T) {
	inst := newTestInstance(t, "ipip-ping")
	peer := newRawSIPRegistrar(t, rawRegistrarOpts{})
	id := createIPIPTrunk(t, inst.baseURL(), "acme", map[string]interface{}{
		"peer_uri":                      peerURI(peer.port),
		"options_ping_interval_seconds": 1,
	})

	up := waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 1, 5*time.Second)
	if up.TrunkType != "ip_ip" || up.AppID != "acme" || up.StatusCode != 200 || up.PeerURI != peerURI(peer.port) {
		t.Errorf("sip.trunk_up = %+v", up)
	}
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "active", 2*time.Second)
	sub, _ := snap["ip_ip"].(map[string]interface{})
	if sub["last_ping_at"] == nil || sub["last_ping_status_code"] != float64(200) {
		t.Errorf("ip_ip block after up = %v", sub)
	}

	peer.optionsStatus.Store(503)
	down := waitForTrunkEvents(t, inst, events.SIPTrunkDown, id, 1, 5*time.Second)
	if down.StatusCode != 503 || down.Reason != "Options Reply" {
		t.Errorf("sip.trunk_down = %+v", down)
	}
	snap = waitForTrunkStatus(t, inst.baseURL(), id, "failed", 2*time.Second)
	if snap["last_error"] != "503 Options Reply" {
		t.Errorf("last_error = %v", snap["last_error"])
	}

	// Status is informational: a call is still attempted on a failed trunk.
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip", "to": fmt.Sprintf("sip:bob@127.0.0.1:%d", peer.port), "trunk_id": id, "codecs": []string{"PCMU"},
	})
	waitForInvite(t, peer, 3*time.Second)

	// A refusal still shows a serving peer.
	peer.optionsStatus.Store(404)
	waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 2, 5*time.Second)
	snap = waitForTrunkStatus(t, inst.baseURL(), id, "active", 2*time.Second)
	if _, has := snap["last_error"]; has {
		t.Errorf("last_error = %v after recovery, want it cleared", snap["last_error"])
	}

	// Let a few more pings land: one event per change, none per ping.
	seen := len(peer.options())
	deadline := time.Now().Add(5 * time.Second)
	for len(peer.options()) < seen+2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	ups := len(inst.collector.matchAll(events.SIPTrunkUp, trunkEventFor(id)))
	downs := len(inst.collector.matchAll(events.SIPTrunkDown, trunkEventFor(id)))
	if ups != 2 || downs != 1 {
		t.Errorf("events = %d up / %d down, want 2 / 1", ups, downs)
	}
}

func TestTrunk_IPIP_OptionsPing_Unreachable(t *testing.T) {
	inst := newTestInstance(t, "ipip-ping-dead")
	id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
		"peer_uri":                      peerURI(blackholePort(t)),
		"options_ping_interval_seconds": 1,
	})

	down := waitForTrunkEvents(t, inst, events.SIPTrunkDown, id, 1, 8*time.Second)
	if down.StatusCode != 0 || down.Reason == "" {
		t.Errorf("sip.trunk_down = %+v, want a reason and no status code", down)
	}
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "failed", 2*time.Second)
	if snap["last_error"] == nil {
		t.Error("failed trunk has no last_error")
	}
	if inst.collector.hasEvent(events.SIPTrunkUp, trunkEventFor(id)) {
		t.Error("sip.trunk_up published for an unreachable peer")
	}
}

// TestTrunk_IPIP_OptionsPing_SilentDelete pins that deleting a trunk whose peer
// is not answering does not publish a sip.trunk_down for the aborted ping.
func TestTrunk_IPIP_OptionsPing_SilentDelete(t *testing.T) {
	inst := newTestInstance(t, "ipip-ping-delete")
	peer := newRawSIPRegistrar(t, rawRegistrarOpts{})
	peer.optionsStatus.Store(0)
	id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
		"peer_uri":                      peerURI(peer.port),
		"options_ping_interval_seconds": 60,
	})
	deadline := time.Now().Add(3 * time.Second)
	for len(peer.options()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(peer.options()) == 0 {
		t.Fatal("peer never received the first OPTIONS")
	}

	httpDelete(t, inst.baseURL()+"/v1/sip/trunks/"+id).Body.Close()
	time.Sleep(500 * time.Millisecond)
	if inst.collector.hasEvent(events.SIPTrunkDown, nil) || inst.collector.hasEvent(events.SIPTrunkUp, nil) {
		t.Error("trunk health event published for a ping cut short by delete")
	}
}

// ---------------------------------------------------------------------------
// Global outbound proxy
// ---------------------------------------------------------------------------

// TestTrunk_IPIP_GlobalProxyAdopted pins that an ip_ip trunk created while
// SIP_OUTBOUND_PROXY is set sends its INVITEs and OPTIONS through it, and that
// the proxy does not become an inbound source for the trunk.
func TestTrunk_IPIP_GlobalProxyAdopted(t *testing.T) {
	proxy := newRawSIPRegistrar(t, rawRegistrarOpts{})
	inst := newTestInstanceWithOpts(t, "ipip-global-proxy", func(c *config.Config) {
		c.SIPOutboundProxy = peerURI(proxy.port)
	})
	// 192.0.2.10 is unroutable: anything that reaches the proxy got there
	// because of the proxy setting.
	id := createIPIPTrunk(t, inst.baseURL(), "acme", map[string]interface{}{
		"peer_uri":                      "sip:192.0.2.10",
		"options_ping_interval_seconds": 1,
	})

	sub, _ := trunkSnapshot(t, inst.baseURL(), id)["ip_ip"].(map[string]interface{})
	if sub["outbound_proxy"] != peerURI(proxy.port) {
		t.Errorf("outbound_proxy = %v, want the global default", sub["outbound_proxy"])
	}

	waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 1, 5*time.Second)
	ping := proxy.options()[0]
	if ping.Recipient.Host != "192.0.2.10" {
		t.Errorf("OPTIONS Request-URI host = %q, want the peer", ping.Recipient.Host)
	}
	if ping.GetHeader("Route") != nil {
		t.Errorf("OPTIONS carries a Route:\n%s", ping.String())
	}

	to := "sip:bob@192.0.2.10"
	originateLeg(t, inst.baseURL(), map[string]interface{}{
		"type": "sip", "to": to, "trunk_id": id, "codecs": []string{"PCMU"},
	})
	assertLooseRoute(t, waitForInvite(t, proxy, 3*time.Second), "127.0.0.1", proxy.port, to)

	cli := newRawSIPClient(t, "ipip-proxy-ua")
	sendRawInvite(t, cli, inst.sipPort, "via-proxy")
	d := inst.collector.waitForMatch(t, events.LegRinging, ringingTo("via-proxy"), 5*time.Second).Data.(*events.LegRingingData)
	if d.TrunkID != "" {
		t.Errorf("trunk_id = %q for a call from the proxy's address, want none", d.TrunkID)
	}
	httpDelete(t, inst.baseURL()+"/v1/legs/"+d.LegID).Body.Close()
}

// ---------------------------------------------------------------------------
// REFER
// ---------------------------------------------------------------------------

// TestTransfer_ReferInheritsIPIPTrunk pins that a REFER-originated INVITE goes
// out over the referrer leg's ip_ip trunk even though the trunk has no AOR to
// be re-found by. Topology as in TestTransfer_ReferInheritsTrunkIdentity, with
// instA as instB's static peer.
func TestTransfer_ReferInheritsIPIPTrunk(t *testing.T) {
	instA := newTestInstance(t, "refer-ipip-a")
	instB := newTestInstanceWithOpts(t, "refer-ipip-b", func(c *config.Config) {
		c.SIPReferAutoDial = true
	})
	instC := newTestInstance(t, "refer-ipip-c")

	trunkID := createIPIPTrunk(t, instB.baseURL(), "", map[string]interface{}{"peer_uri": peerURI(instA.sipPort)})

	originateLeg(t, instB.baseURL(), map[string]interface{}{
		"type":     "sip",
		"to":       fmt.Sprintf("sip:bob@127.0.0.1:%d", instA.sipPort),
		"from":     "+15550001",
		"trunk_id": trunkID,
		"codecs":   []string{"PCMU"},
	})

	inboundOnA := waitForInboundLeg(t, instA.baseURL(), 5*time.Second)
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instA.baseURL(), inboundOnA.ID), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("answer on A: %d", r.StatusCode)
	}
	waitForLegState(t, instA.baseURL(), inboundOnA.ID, "connected", 5*time.Second)

	target := fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort)
	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), inboundOnA.ID), map[string]interface{}{
		"target": target,
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", transferResp.StatusCode)
	}

	ev := instB.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegRingingData)
		return ok && d.URI == target
	}, 10*time.Second)
	d := ev.Data.(*events.LegRingingData)
	if d.TrunkID != trunkID {
		t.Errorf("transfer leg trunk_id = %q, want the referrer's trunk %q", d.TrunkID, trunkID)
	}
	if d.From != "+15550001" {
		t.Errorf("transfer leg from = %q, want the referrer's caller ID", d.From)
	}
}

// ---------------------------------------------------------------------------
// VSI
// ---------------------------------------------------------------------------

func TestVSI_Trunk_IPIP_Lifecycle(t *testing.T) {
	inst := newTestInstance(t, "vsi-ipip")
	peer := newRawSIPRegistrar(t, rawRegistrarOpts{})

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	created := vsiSend(t, conn, "create_sip_trunk", "create-1", map[string]interface{}{
		"type": "ip_ip",
		"ip_ip": map[string]interface{}{
			"peer_uri":                      peerURI(peer.port),
			"password":                      "hunter2",
			"username":                      "acct",
			"options_ping_interval_seconds": 1,
		},
	})
	if created.Type != "create_sip_trunk.result" {
		t.Fatalf("create type = %q, want create_sip_trunk.result; data=%s", created.Type, created.Data)
	}
	var createData struct {
		ID     string `json:"id"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(created.Data, &createData); err != nil {
		t.Fatalf("decode create data: %v", err)
	}
	if createData.ID == "" || createData.Type != "ip_ip" || createData.Status != "active" {
		t.Fatalf("create data = %+v, want an active ip_ip trunk", createData)
	}
	id := createData.ID

	waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 1, 5*time.Second)

	got := vsiSend(t, conn, "get_sip_trunk", "get-1", map[string]string{"id": id})
	if got.Type != "get_sip_trunk.result" {
		t.Fatalf("get type = %q, want get_sip_trunk.result", got.Type)
	}
	if !strings.Contains(string(got.Data), `"ip_ip"`) || !strings.Contains(string(got.Data), id) {
		t.Errorf("get result = %s", got.Data)
	}
	if strings.Contains(string(got.Data), "hunter2") {
		t.Errorf("get result leaks the password: %s", got.Data)
	}

	deleted := vsiSend(t, conn, "delete_sip_trunk", "del-1", map[string]string{"id": id})
	if deleted.Type != "delete_sip_trunk.result" {
		t.Fatalf("delete type = %q, want delete_sip_trunk.result", deleted.Type)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		missing := vsiSend(t, conn, "get_sip_trunk", "get-2", map[string]string{"id": id})
		if missing.Type == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("get after delete still succeeds: %s", missing.Type)
		}
		time.Sleep(75 * time.Millisecond)
	}
}

func TestVSI_CreateLeg_UnknownTrunk(t *testing.T) {
	inst := newTestInstance(t, "vsi-leg-trunk")

	conn := dialVSI(t, inst)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "create_leg", "leg-1", map[string]interface{}{
		"type": "sip", "to": "sip:bob@127.0.0.1:5999", "trunk_id": "no-such-trunk",
	})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error; data=%s", f.Type, f.Data)
	}
	var e struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(f.Data, &e); err != nil {
		t.Fatalf("decode error data: %v", err)
	}
	if e.Code != 404 {
		t.Errorf("code = %d, want 404", e.Code)
	}
}
