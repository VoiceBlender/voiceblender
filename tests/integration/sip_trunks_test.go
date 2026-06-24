//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ---------------------------------------------------------------------------
// rawSIPRegistrar — minimal sipgo-backed UAS that accepts REGISTER requests
// ---------------------------------------------------------------------------

type rawSIPRegistrar struct {
	ua     *sipgo.UserAgent
	server *sipgo.Server
	host   string
	port   int
	cancel context.CancelFunc

	mu               sync.Mutex
	receivedRegs     []*sip.Request
	receivedInvites  []*sip.Request
	grantedExpires   int
	challengeOnce    bool
	expectedUsername string
	expectedPassword string

	// nonce used for the 401 challenge — fixed for deterministic verification.
	nonce string

	challengedCount atomic.Int32

	// successCount is the number of 200 OKs returned so far. rejectAfter,
	// when > 0, causes every REGISTER beyond that count to be rejected with
	// 503 — used to simulate the registrar going away after the initial
	// registration succeeded.
	successCount atomic.Int32
	rejectAfter  int32
}

type rawRegistrarOpts struct {
	grantExpires int    // 0 → echo what the client asked for
	digestUser   string // "" disables 401 challenge
	digestPass   string
	rejectAfter  int // > 0: reject (503) after this many successful REGISTERs
}

func newRawSIPRegistrar(t *testing.T, opts rawRegistrarOpts) *rawSIPRegistrar {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	u, err := sipgo.NewUA(
		sipgo.WithUserAgent("fake-registrar"),
		sipgo.WithUserAgentHostname("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	srv, err := sipgo.NewServer(u)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	r := &rawSIPRegistrar{
		ua:               u,
		server:           srv,
		host:             "127.0.0.1",
		port:             port,
		grantedExpires:   opts.grantExpires,
		challengeOnce:    opts.digestUser != "",
		expectedUsername: opts.digestUser,
		expectedPassword: opts.digestPass,
		nonce:            "abcdef1234567890",
		rejectAfter:      int32(opts.rejectAfter),
	}

	srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
		r.mu.Lock()
		r.receivedRegs = append(r.receivedRegs, req)
		r.mu.Unlock()

		// Digest challenge: first REGISTER without Authorization gets 401.
		if r.challengeOnce {
			if auth := req.GetHeader("Authorization"); auth == nil {
				r.challengedCount.Add(1)
				res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
				res.AppendHeader(sip.NewHeader("WWW-Authenticate",
					fmt.Sprintf(`Digest realm="vb-test", nonce="%s", algorithm=MD5`, r.nonce)))
				_ = tx.Respond(res)
				return
			}
			// On retry, just accept — we don't fully verify the digest in this
			// stub; OutboundRegistration's digest construction is exercised
			// end-to-end by the unit test for parseGrantedExpires and by the
			// fact that sipgo's library handles it.
		}

		// Simulate the registrar disappearing after N successful REGISTERs.
		if r.rejectAfter > 0 && r.successCount.Load() >= r.rejectAfter {
			res := sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil)
			_ = tx.Respond(res)
			return
		}

		expires := r.grantedExpires
		if expires == 0 {
			if e := req.GetHeader("Expires"); e != nil {
				_, _ = fmt.Sscanf(e.Value(), "%d", &expires)
			}
			if expires == 0 {
				expires = 3600
			}
		}
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		// Echo the Contact header with the granted expires=.
		if c := req.GetHeader("Contact"); c != nil {
			res.AppendHeader(sip.NewHeader("Contact",
				strings.TrimSuffix(c.Value(), ">")+fmt.Sprintf(">;expires=%d", expires)))
		}
		res.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", expires)))
		_ = tx.Respond(res)
		r.successCount.Add(1)
	})

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		r.mu.Lock()
		r.receivedInvites = append(r.receivedInvites, req)
		r.mu.Unlock()
		// Reject so the call setup terminates quickly.
		res := sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil)
		_ = tx.Respond(res)
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		_ = tx.Respond(res)
	})

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		_ = srv.ListenAndServe(ctx, "udp", fmt.Sprintf("127.0.0.1:%d", port))
	}()
	time.Sleep(150 * time.Millisecond)
	t.Cleanup(func() { cancel() })
	return r
}

func (r *rawSIPRegistrar) registerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.receivedRegs)
}

func (r *rawSIPRegistrar) lastRegister() *sip.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.receivedRegs) == 0 {
		return nil
	}
	return r.receivedRegs[len(r.receivedRegs)-1]
}

func (r *rawSIPRegistrar) registerAt(i int) *sip.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.receivedRegs) {
		return nil
	}
	return r.receivedRegs[i]
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func createTrunkRequest(t *testing.T, baseURL string, body interface{}) (*http.Response, []byte) {
	t.Helper()
	resp := httpPost(t, baseURL+"/v1/sip/trunks", body)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func trunkSnapshot(t *testing.T, baseURL, id string) map[string]interface{} {
	t.Helper()
	resp := httpGet(t, baseURL+"/v1/sip/trunks/"+id)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET trunk: %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func waitForTrunkStatus(t *testing.T, baseURL, id, want string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := trunkSnapshot(t, baseURL, id)
		if status, _ := snap["status"].(string); status == want {
			return snap
		}
		time.Sleep(75 * time.Millisecond)
	}
	t.Fatalf("trunk %s never reached status=%s", id, want)
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestTrunk_SIPRegister_HappyPath(t *testing.T) {
	inst := newTestInstance(t, "trunk-happy")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 120})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":             "sip:alice@vb.test",
			"password":        "secret",
			"expires_seconds": 600,
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, body=%s", createResp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing id in create response")
	}

	// Wait for the trunk to flip to active.
	snap := waitForTrunkStatus(t, inst.baseURL(), id, "active", 3*time.Second)
	if reg.registerCount() < 1 {
		t.Fatal("fake registrar did not receive REGISTER")
	}
	sub, _ := snap["sip_register"].(map[string]interface{})
	if granted, _ := sub["granted_expires_seconds"].(float64); int(granted) != 120 {
		t.Errorf("granted_expires_seconds = %v, want 120", granted)
	}

	// Event was published.
	ev := inst.collector.waitForMatch(t, events.SIPOutboundRegistrationActive, nil, 1*time.Second)
	d := ev.Data.(*events.SIPOutboundRegistrationActiveData)
	if d.TrunkID != id {
		t.Errorf("TrunkID = %q, want %q", d.TrunkID, id)
	}
	if d.GrantedExpiresSeconds != 120 {
		t.Errorf("GrantedExpiresSeconds = %d, want 120", d.GrantedExpiresSeconds)
	}

	// Response must never include the password.
	if strings.Contains(strings.ToLower(string(body)), "password") {
		t.Errorf("create response leaks password: %s", body)
	}
	if listResp := httpGet(t, inst.baseURL()+"/v1/sip/trunks"); listResp != nil {
		listBody, _ := io.ReadAll(listResp.Body)
		listResp.Body.Close()
		if strings.Contains(strings.ToLower(string(listBody)), "\"password\"") {
			t.Errorf("list response leaks password: %s", listBody)
		}
	}
}

func TestTrunk_SIPRegister_DigestAuth(t *testing.T) {
	inst := newTestInstance(t, "trunk-digest")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{
		grantExpires: 300,
		digestUser:   "alice",
		digestPass:   "secret",
	})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":             "sip:alice@vb.test",
			"username":        "alice",
			"password":        "secret",
			"expires_seconds": 300,
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, body=%s", createResp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := created["id"].(string)

	waitForTrunkStatus(t, inst.baseURL(), id, "active", 3*time.Second)

	// The fake registrar should have received at least 2 REGISTER requests:
	// the unauthenticated one and the digest-authenticated retry.
	if reg.registerCount() < 2 {
		t.Errorf("REGISTER count = %d, want ≥ 2 (challenge + retry)", reg.registerCount())
	}
	if reg.challengedCount.Load() == 0 {
		t.Error("expected at least one challenge")
	}
	// The retry must carry an Authorization header.
	authRetry := reg.registerAt(1)
	if authRetry == nil || authRetry.GetHeader("Authorization") == nil {
		t.Error("second REGISTER missing Authorization header")
	}
}

func TestTrunk_SIPRegister_Refresh(t *testing.T) {
	inst := newTestInstance(t, "trunk-refresh")
	// Short grant so the refresh fires quickly: 2 s with ratio 0.5 → refresh ~1 s.
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 2})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":             "sip:alice@vb.test",
			"password":        "x",
			"expires_seconds": 600,
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", createResp.StatusCode, body)
	}

	// Wait for at least 2 successful REGISTERs (initial + one refresh).
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if reg.registerCount() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reg.registerCount() < 2 {
		t.Fatalf("REGISTER count = %d, want ≥ 2 (refresh did not fire)", reg.registerCount())
	}
}

func TestTrunk_SIPRegister_Unregister(t *testing.T) {
	inst := newTestInstance(t, "trunk-unreg")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":           "sip:alice@vb.test",
			"password":      "x",
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d, body=%s", createResp.StatusCode, body)
	}
	var created map[string]interface{}
	_ = json.Unmarshal(body, &created)
	id, _ := created["id"].(string)

	waitForTrunkStatus(t, inst.baseURL(), id, "active", 3*time.Second)
	regsBefore := reg.registerCount()

	delResp := httpDelete(t, inst.baseURL()+"/v1/sip/trunks/"+id)
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE status = %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	// Wait for unregister REGISTER (Expires: 0) to land at the fake registrar.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reg.registerCount() > regsBefore {
			break
		}
		time.Sleep(75 * time.Millisecond)
	}
	last := reg.lastRegister()
	if last == nil {
		t.Fatal("no REGISTER seen after DELETE")
	}
	expHdr := last.GetHeader("Expires")
	if expHdr == nil || strings.TrimSpace(expHdr.Value()) != "0" {
		t.Errorf("final REGISTER Expires = %v, want 0", expHdr)
	}

	// expired event with reason=unregistered.
	exp := inst.collector.waitForMatch(t, events.SIPOutboundRegistrationExpired,
		func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationExpiredData).Reason == "unregistered"
		}, 2*time.Second)
	if exp.Data.(*events.SIPOutboundRegistrationExpiredData).TrunkID != id {
		t.Errorf("expired event TrunkID mismatch")
	}

	// Trunk is gone from the manager.
	getResp := httpGet(t, inst.baseURL()+"/v1/sip/trunks/"+id)
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", getResp.StatusCode)
		getResp.Body.Close()
	}
}

func TestTrunk_SIPRegister_OutboundCallUsesTrunk(t *testing.T) {
	inst := newTestInstance(t, "trunk-out")
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{grantExpires: 600})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":           "sip:alice@vb.test",
			"username":      "alice",
			"password":      "secret",
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d, body=%s", createResp.StatusCode, body)
	}
	var created map[string]interface{}
	_ = json.Unmarshal(body, &created)
	id, _ := created["id"].(string)
	waitForTrunkStatus(t, inst.baseURL(), id, "active", 3*time.Second)

	// Originate a leg with from=alice@vb.test — should auto-attach
	// the trunk's auth and Route header.
	createLegResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     fmt.Sprintf("sip:bob@127.0.0.1:%d", reg.port),
		"from":   "alice",
		"codecs": []string{"PCMU"},
	})
	if createLegResp.StatusCode != http.StatusCreated && createLegResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create leg: %d", createLegResp.StatusCode)
	}
	createLegResp.Body.Close()

	// The fake registrar should receive the INVITE; it returns 503 to keep
	// the test quick, but we just need to verify the request hit and carried
	// the Route header.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reg.mu.Lock()
		n := len(reg.receivedInvites)
		reg.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(75 * time.Millisecond)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.receivedInvites) == 0 {
		t.Fatal("fake registrar did not receive INVITE — trunk wiring not applied")
	}
	inv := reg.receivedInvites[0]
	if route := inv.GetHeader("Route"); route == nil {
		t.Error("INVITE missing Route header (trunk routing not wired)")
	}
}

func TestTrunk_SIPRegister_RefreshFailedEmitsExpired(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "trunk-refresh-fail", func(c *config.Config) {
		// Tight backoff cap so retries fire quickly inside the test window.
		c.SIPOutboundRegistrationFailureBackoffMaxMs = 500
	})
	// Grant 1 s; reject after the first 200 OK so refreshes all fail with 503.
	reg := newRawSIPRegistrar(t, rawRegistrarOpts{
		grantExpires: 1,
		rejectAfter:  1,
	})

	createResp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri": fmt.Sprintf("sip:127.0.0.1:%d", reg.port),
			"aor":           "sip:alice@vb.test",
			"password":      "x",
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: %d, body=%s", createResp.StatusCode, body)
	}

	// First REGISTER must succeed.
	inst.collector.waitForMatch(t, events.SIPOutboundRegistrationActive, nil, 2*time.Second)

	// Wait for the refresh_failed expired event. Granted=1 s, refresh at
	// 0.5 s fails, deadline (lastReg + 1 s) lapses ~1 s later — backoff
	// caps at 500 ms so a retry fires soon after.
	exp := inst.collector.waitForMatch(t, events.SIPOutboundRegistrationExpired,
		func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationExpiredData).Reason == "refresh_failed"
		}, 5*time.Second)
	if exp.Data.(*events.SIPOutboundRegistrationExpiredData).TrunkID == "" {
		t.Error("expired event missing trunk_id")
	}

	// Snapshot to also confirm at least one failed event was published.
	if !inst.collector.hasEvent(events.SIPOutboundRegistrationFailed, nil) {
		t.Error("expected at least one sip.outbound_registration_failed event")
	}

	// Wait briefly to make sure we don't get a second refresh_failed —
	// observers should see it once per outage.
	time.Sleep(1500 * time.Millisecond)
	all := inst.collector.matchAll(events.SIPOutboundRegistrationExpired,
		func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationExpiredData).Reason == "refresh_failed"
		})
	if len(all) != 1 {
		t.Errorf("got %d refresh_failed events, want exactly 1", len(all))
	}
}

func TestTrunk_TypeIPIP_NotImplemented(t *testing.T) {
	inst := newTestInstance(t, "trunk-ipip")
	resp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type":  "ip_ip",
		"ip_ip": map[string]interface{}{"peer_uri": "sip:pbx.example"},
	})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%s", resp.StatusCode, body)
	}
}

func TestTrunk_TypeUnknown_BadRequest(t *testing.T) {
	inst := newTestInstance(t, "trunk-bogus")
	resp, _ := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "bogus",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}
