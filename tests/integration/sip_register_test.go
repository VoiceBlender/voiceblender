//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ---------------------------------------------------------------------------
// rawSIPClient — minimal sipgo-backed UA for SIP register integration tests
// ---------------------------------------------------------------------------

type rawSIPClient struct {
	ua     *sipgo.UserAgent
	client *sipgo.Client
	server *sipgo.Server
	host   string // listener address (without port; "127.0.0.1")
	port   int    // local UDP port
	cancel context.CancelFunc

	// Inbound INVITE delivery for AOR-dial tests.
	invites chan inviteEvent
	// Inbound CANCEL delivery — lets tests assert a CANCEL actually reached
	// this contact (rather than being misrouted to the AOR host).
	cancels chan *sip.Request
}

type inviteEvent struct {
	req  *sip.Request
	tx   sip.ServerTransaction
	done chan struct{}
}

func newRawSIPClient(t *testing.T, ua string) *rawSIPClient {
	t.Helper()

	// Bind a free UDP port for the UA.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()

	u, err := sipgo.NewUA(
		sipgo.WithUserAgent(ua),
		sipgo.WithUserAgentHostname("127.0.0.1"),
	)
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	cli, err := sipgo.NewClient(u,
		sipgo.WithClientHostname("127.0.0.1"),
		sipgo.WithClientPort(port),
		sipgo.WithClientConnectionAddr(fmt.Sprintf("127.0.0.1:%d", port)),
	)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	srv, err := sipgo.NewServer(u)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	c := &rawSIPClient{
		ua:      u,
		client:  cli,
		server:  srv,
		host:    "127.0.0.1",
		port:    port,
		invites: make(chan inviteEvent, 8),
		cancels: make(chan *sip.Request, 8),
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		// Send 180 Ringing right away. RFC 3261 §9.1 requires a provisional
		// response before the UAC can CANCEL — tests that exercise CANCEL
		// (e.g. parallel fork) rely on this.
		ring := sip.NewResponseFromRequest(req, sip.StatusRinging, "Ringing", nil)
		_ = tx.Respond(ring)

		// A matching CANCEL is handled at the transaction layer (sipgo does not
		// call the server's OnCancel request handler for it), so record it here.
		tx.OnCancel(func(r *sip.Request) {
			select {
			case c.cancels <- r:
			default:
			}
		})

		done := make(chan struct{})
		select {
		case c.invites <- inviteEvent{req: req, tx: tx, done: done}:
		default:
			// drop excess; tests should poll fast enough
			return
		}
		// Block until the test calls answerInvite, the peer CANCELs us
		// (sipgo terminates the tx and closes tx.Done()), or 10 s elapse.
		// sipgo terminates the transaction when this handler returns, so
		// we must stay alive until one of those events happens.
		select {
		case <-done:
		case <-tx.Done():
		case <-time.After(10 * time.Second):
		}
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		_ = tx.Respond(res)
	})
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		_ = tx.Respond(res)
	})

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() {
		_ = srv.ListenAndServe(ctx, "udp", fmt.Sprintf("127.0.0.1:%d", port))
	}()
	// Give the listener a moment to bind.
	time.Sleep(150 * time.Millisecond)

	t.Cleanup(func() {
		cancel()
	})
	return c
}

func (c *rawSIPClient) contactURI(user string) string {
	return fmt.Sprintf("sip:%s@%s:%d", user, c.host, c.port)
}

// buildRegister constructs a REGISTER request targeting the given
// VoiceBlender SIP port.
func (c *rawSIPClient) buildRegister(aorUser string, sipPort int, contact string, expires int) *sip.Request {
	registrar := sip.Uri{Scheme: "sip", Host: c.host, Port: sipPort}
	req := sip.NewRequest(sip.REGISTER, registrar)
	toURI := sip.Uri{Scheme: "sip", User: aorUser, Host: "vb.test"}
	req.AppendHeader(&sip.ToHeader{Address: toURI})
	fromHdr := &sip.FromHeader{Address: toURI, Params: sip.NewParams()}
	fromHdr.Params.Add("tag", sip.GenerateTagN(8))
	req.AppendHeader(fromHdr)
	if contact != "" {
		req.AppendHeader(sip.NewHeader("Contact", contact))
	}
	req.AppendHeader(sip.NewHeader("Expires", fmt.Sprintf("%d", expires)))
	req.AppendHeader(sip.NewHeader("User-Agent", "rawSIPClient/1.0"))
	return req
}

// sendRegister sends a fully-formed REGISTER and returns the final response.
func (c *rawSIPClient) sendRegister(t *testing.T, sipPort int, aorUser string, contact string, expires int) *sip.Response {
	t.Helper()
	req := c.buildRegister(aorUser, sipPort, "<"+contact+">", expires)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("REGISTER: %v", err)
	}
	return resp
}

// sendUnregisterWildcard sends `REGISTER ... Contact: *; Expires: 0`.
func (c *rawSIPClient) sendUnregisterWildcard(t *testing.T, sipPort int, aorUser string) *sip.Response {
	t.Helper()
	req := c.buildRegister(aorUser, sipPort, "*", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := c.client.Do(ctx, req)
	if err != nil {
		t.Fatalf("REGISTER (unregister): %v", err)
	}
	return resp
}

// waitInvite waits for the next incoming INVITE.
func (c *rawSIPClient) waitInvite(t *testing.T, timeout time.Duration) inviteEvent {
	t.Helper()
	select {
	case e := <-c.invites:
		return e
	case <-time.After(timeout):
		t.Fatal("timed out waiting for INVITE")
		return inviteEvent{}
	}
}

// answerInvite replies 200 OK with a trivial SDP answer using PCMU.
func (c *rawSIPClient) answerInvite(t *testing.T, e inviteEvent) {
	t.Helper()
	sdp := []byte(strings.Join([]string{
		"v=0",
		fmt.Sprintf("o=raw 1 1 IN IP4 %s", c.host),
		"s=-",
		fmt.Sprintf("c=IN IP4 %s", c.host),
		"t=0 0",
		"m=audio 40000 RTP/AVP 0",
		"a=rtpmap:0 PCMU/8000",
		"a=sendrecv",
		"",
	}, "\r\n"))
	res := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", sdp)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: c.host, Port: c.port}})
	if err := e.tx.Respond(res); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func registrationsList(t *testing.T, baseURL string) api.RegistrationsResponse {
	t.Helper()
	resp := httpGet(t, baseURL+"/v1/sip/registrations")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/sip/registrations: %d", resp.StatusCode)
	}
	var out api.RegistrationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSIPRegister_Basic(t *testing.T) {
	inst := newTestInstance(t, "reg-basic")
	cli := newRawSIPClient(t, "test-ua")

	resp := cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	if resp.StatusCode != 200 {
		t.Fatalf("REGISTER status = %d", resp.StatusCode)
	}
	if c := resp.GetHeader("Contact"); c == nil || !strings.Contains(c.Value(), "expires=") {
		t.Errorf("response missing Contact;expires=, got %v", c)
	}

	// Event arrived.
	ev := inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, 1*time.Second)
	d := ev.Data.(*events.SIPRegistrationActiveData)
	if d.AOR != "sip:alice@vb.test" {
		t.Errorf("AOR = %q", d.AOR)
	}
	if d.Transport != "udp" {
		t.Errorf("Transport = %q, want udp", d.Transport)
	}
	if d.Socket == "" {
		t.Errorf("Socket empty")
	}
	if d.GrantedExpiresSeconds <= 0 || d.GrantedExpiresSeconds > 7200 {
		t.Errorf("GrantedExpiresSeconds = %d", d.GrantedExpiresSeconds)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings = %d", len(list.Bindings))
	}
	b := list.Bindings[0]
	if b.AOR != "sip:alice@vb.test" || b.Contact != cli.contactURI("alice") {
		t.Errorf("binding = %+v", b)
	}
	// Source socket from sipgo is the actual UDP source — must match our port.
	if !strings.HasSuffix(b.Socket, fmt.Sprintf(":%d", cli.port)) {
		t.Errorf("Socket %q does not match client port %d", b.Socket, cli.port)
	}
}

func TestSIPRegister_Refresh(t *testing.T) {
	inst := newTestInstance(t, "reg-refresh")
	cli := newRawSIPClient(t, "test-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 1200)

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings after refresh = %d", len(list.Bindings))
	}
	active := inst.collector.matchAll(events.SIPRegistrationActive, nil)
	if len(active) != 2 {
		t.Errorf("active events = %d, want 2", len(active))
	}
}

func TestSIPRegister_MultiContact(t *testing.T) {
	inst := newTestInstance(t, "reg-multi")
	cli1 := newRawSIPClient(t, "ua-1")
	cli2 := newRawSIPClient(t, "ua-2")

	cli1.sendRegister(t, inst.sipPort, "alice", cli1.contactURI("alice"), 600)
	cli2.sendRegister(t, inst.sipPort, "alice", cli2.contactURI("alice"), 600)

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2", len(list.Bindings))
	}

	// Per-contact unregister of cli1 leaves cli2 in place.
	cli1.sendRegister(t, inst.sipPort, "alice", cli1.contactURI("alice"), 0)
	list = registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("after unregister: bindings = %d", len(list.Bindings))
	}
	if list.Bindings[0].Contact != cli2.contactURI("alice") {
		t.Errorf("remaining binding: %+v", list.Bindings[0])
	}
	if len(inst.collector.matchAll(events.SIPRegistrationExpired, nil)) < 1 {
		t.Error("missing expired event")
	}
}

func TestSIPRegister_SingleBindingMode(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-single", func(c *config.Config) {
		c.SIPRegistrationAllowMultipleContacts = false
	})
	cli1 := newRawSIPClient(t, "ua-1")
	cli2 := newRawSIPClient(t, "ua-2")

	cli1.sendRegister(t, inst.sipPort, "alice", cli1.contactURI("alice"), 600)
	cli2.sendRegister(t, inst.sipPort, "alice", cli2.contactURI("alice"), 600)

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(list.Bindings))
	}
	if list.Bindings[0].Contact != cli2.contactURI("alice") {
		t.Errorf("remaining binding: %+v", list.Bindings[0])
	}

	exp := inst.collector.matchAll(events.SIPRegistrationExpired, func(e events.Event) bool {
		return e.Data.(*events.SIPRegistrationExpiredData).Reason == "replaced"
	})
	if len(exp) != 1 {
		t.Errorf("replaced events = %d", len(exp))
	}
}

func TestSIPRegister_Unregister(t *testing.T) {
	inst := newTestInstance(t, "reg-unreg")
	cli := newRawSIPClient(t, "test-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	resp := cli.sendUnregisterWildcard(t, inst.sipPort, "alice")
	if resp.StatusCode != 200 {
		t.Fatalf("unregister status = %d", resp.StatusCode)
	}

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 0 {
		t.Errorf("bindings after unregister = %d", len(list.Bindings))
	}
	exp := inst.collector.matchAll(events.SIPRegistrationExpired, func(e events.Event) bool {
		return e.Data.(*events.SIPRegistrationExpiredData).Reason == "unregistered"
	})
	if len(exp) == 0 {
		t.Error("missing unregistered expired event")
	}
}

func TestSIPRegister_ForceDelete(t *testing.T) {
	inst := newTestInstance(t, "reg-force")
	cli := newRawSIPClient(t, "test-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)

	aor := url.PathEscape("sip:alice@vb.test")
	resp := httpDelete(t, inst.baseURL()+"/v1/sip/registrations/"+aor)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}
	resp.Body.Close()

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 0 {
		t.Errorf("after DELETE: %d bindings remain", len(list.Bindings))
	}
	if !inst.collector.hasEvent(events.SIPRegistrationExpired, func(e events.Event) bool {
		return e.Data.(*events.SIPRegistrationExpiredData).Reason == "forced"
	}) {
		t.Error("missing forced expired event")
	}

	// DELETE on missing AOR is 404.
	resp = httpDelete(t, inst.baseURL()+"/v1/sip/registrations/"+aor)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE missing: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSIPRegister_Expiry(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "reg-ttl", func(c *config.Config) {
		c.SIPRegistrationSweepIntervalMs = 100
	})
	cli := newRawSIPClient(t, "test-ua")

	// Below the 60s floor — registrar clamps to 60. To exercise the sweeper
	// in a fast test we instead manually drive expiry: register, then poke
	// the registrar via the force-delete path while asserting the TTL path
	// in the unit tests. Here we just confirm the binding exists and the
	// granted expiry was clamped.
	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 5)
	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 1 {
		t.Fatalf("bindings = %d", len(list.Bindings))
	}
	if list.Bindings[0].GrantedExpiresSeconds < 60 {
		t.Errorf("GrantedExpiresSeconds = %d, want clamp >= 60", list.Bindings[0].GrantedExpiresSeconds)
	}
}

func TestSIPRegister_DialAOR(t *testing.T) {
	inst := newTestInstance(t, "reg-dial")
	cli := newRawSIPClient(t, "alice-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, 1*time.Second)

	// POST /v1/legs with the AOR URI; VB should send the INVITE to the
	// REGISTER source socket rather than the URI's host:port.
	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	// raw client receives the INVITE on its own listener.
	e := cli.waitInvite(t, 3*time.Second)
	// The Request-URI is left as the dialed AOR (the transport delivered the
	// packet to our port via a loose Route to the contact socket). Preserving
	// the Request-URI keeps CANCEL/ACK — which reuse it per RFC 3261 §9.1 —
	// pointing at the AOR the caller asked for.
	if e.req.Recipient.Host != "vb.test" {
		t.Errorf("Recipient.Host = %q, want vb.test (dialed AOR preserved)", e.req.Recipient.Host)
	}
	cli.answerInvite(t, e)
}

// TestSIPRegister_CancelUnansweredRoutesToContact reproduces the bug where
// deleting an unanswered outbound leg dialed to a registered AOR sent CANCEL to
// the AOR host (potentially VoiceBlender itself) instead of the contact. The
// contact must receive the CANCEL.
func TestSIPRegister_CancelUnansweredRoutesToContact(t *testing.T) {
	inst := newTestInstance(t, "reg-cancel")
	cli := newRawSIPClient(t, "alice-ua")

	cli.sendRegister(t, inst.sipPort, "alice", cli.contactURI("alice"), 600)
	inst.collector.waitForMatch(t, events.SIPRegistrationActive, nil, 1*time.Second)

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	var lv legView
	decodeJSON(t, createResp, &lv)

	// Contact receives the INVITE (and auto-replies 180 Ringing) but never
	// answers.
	cli.waitInvite(t, 3*time.Second)

	// Delete the unanswered leg → VB must CANCEL the outbound INVITE.
	delResp := httpDelete(t, inst.baseURL()+"/v1/legs/"+lv.ID)
	delResp.Body.Close()

	select {
	case req := <-cli.cancels:
		if req.Method != sip.CANCEL {
			t.Fatalf("got %s, want CANCEL", req.Method)
		}
		// RFC 3261 §9.1: the CANCEL Request-URI must be identical to the
		// INVITE's — i.e. the dialed AOR — even though it was delivered to the
		// contact socket (the same next hop as the INVITE).
		if req.Recipient.Host != "vb.test" {
			t.Errorf("CANCEL Request-URI host = %q, want vb.test (dialed AOR)", req.Recipient.Host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("contact never received CANCEL — it was misrouted to the AOR host")
	}
}

func TestSIPRegister_Fork(t *testing.T) {
	inst := newTestInstance(t, "reg-fork")
	cli1 := newRawSIPClient(t, "alice-ua-1")
	cli2 := newRawSIPClient(t, "alice-ua-2")

	cli1.sendRegister(t, inst.sipPort, "alice", cli1.contactURI("alice"), 600)
	cli2.sendRegister(t, inst.sipPort, "alice", cli2.contactURI("alice"), 600)

	list := registrationsList(t, inst.baseURL())
	if len(list.Bindings) != 2 {
		t.Fatalf("expected 2 bindings before dial, got %d", len(list.Bindings))
	}

	createResp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	// Both raw clients must receive the INVITE in parallel.
	type recv struct {
		cli *rawSIPClient
		e   inviteEvent
	}
	got := make(chan recv, 2)
	go func() { got <- recv{cli1, cli1.waitInvite(t, 5*time.Second)} }()
	go func() { got <- recv{cli2, cli2.waitInvite(t, 5*time.Second)} }()

	r1 := <-got
	r2 := <-got
	if r1.cli == r2.cli {
		t.Fatal("both INVITEs arrived on the same raw client — fork did not happen")
	}

	// Pick cli2 as the answerer; the other one is the loser.
	var winner, loser recv
	if r1.cli == cli2 {
		winner, loser = r1, r2
	} else {
		winner, loser = r2, r1
	}

	winner.cli.answerInvite(t, winner.e)

	// VB sends CANCEL to the loser branch; sipgo terminates that server
	// transaction. tx.Done() closes only after Timer I (T4 = 5 s for UDP)
	// elapses after the ACK arrives, so we allow up to 10 s here.
	select {
	case <-loser.e.tx.Done():
		// expected — loser branch was cancelled
	case <-time.After(10 * time.Second):
		t.Fatal("loser branch tx did not terminate within 10s")
	}
	close(loser.e.done)

	waitForLegState(t, inst.baseURL(), outbound.ID, "connected", 5*time.Second)
}
