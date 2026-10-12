//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
)

// TestTransfer_Blind_Outbound: A↔B established. POST /transfer on B's leg
// asks B to dial C. We assert the transfer initiated/completed events
// fire on instance A and that A's leg ends up hung up.
func TestTransfer_Blind_Outbound(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	// instance B receives the REFER and must have auto-dial enabled.
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferAutoDial = true
	})
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	// Initiate transfer on the outbound leg (this is the A-side leg whose
	// peer is instance B). REST call on instance A.
	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", transferResp.StatusCode)
	}

	instA.collector.waitForMatch(t, events.LegTransferInitiated, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	// B should auto-dial C; answer the inbound on C as soon as it arrives.
	inboundOnC := waitForInboundLeg(t, instC.baseURL(), 5*time.Second)
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instC.baseURL(), inboundOnC.ID), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("answer on C: %d", r.StatusCode)
	}

	instA.collector.waitForMatch(t, events.LegTransferCompleted, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// Cleanup the surviving call on instance C side.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instC.baseURL(), inboundOnC.ID))
}

// TestTransfer_Blind_RemoteByeEndsTransferredLeg: once a blind transfer
// completes, the leg B originated toward C is observed by nothing but its own
// dialog monitor — the referrer that would otherwise have watched it is already
// torn down. When C hangs up, B must publish leg.disconnected with reason
// remote_bye and drop the leg from its manager. Without the monitor the BYE is
// swallowed: no event, no CDR, and the leg is stuck on B for the process
// lifetime.
func TestTransfer_Blind_RemoteByeEndsTransferredLeg(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferAutoDial = true
	})
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", transferResp.StatusCode)
	}

	inboundOnC := waitForInboundLeg(t, instC.baseURL(), 5*time.Second)
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instC.baseURL(), inboundOnC.ID), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("answer on C: %d", r.StatusCode)
	}

	instA.collector.waitForMatch(t, events.LegTransferCompleted, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// B's surviving leg is the one it originated toward C.
	transferred := waitForConnectedOutboundLeg(t, instB.baseURL(), 5*time.Second)

	// C hangs up. B learns of it only through the transferred leg's dialog.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instC.baseURL(), inboundOnC.ID))

	disc := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == transferred.ID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "remote_bye" {
		t.Errorf("B cdr.reason = %q, want remote_bye", got)
	}

	// The event is not enough: the leg must also be gone from B's manager.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp := httpGet(t, instB.baseURL()+"/v1/legs")
		var legs []legView
		decodeJSON(t, resp, &legs)
		found := false
		for _, l := range legs {
			if l.ID == transferred.ID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("transferred leg still registered on B after the remote BYE — cleanupLeg never ran")
}

// waitForConnectedOutboundLeg polls instance's leg list until a connected
// sip_outbound leg appears.
func waitForConnectedOutboundLeg(t *testing.T, baseURL string, timeout time.Duration) legView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := httpGet(t, baseURL+"/v1/legs")
		var legs []legView
		decodeJSON(t, resp, &legs)
		for _, l := range legs {
			if l.Type == "sip_outbound" && l.State == "connected" {
				return l
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no connected outbound leg found within %v", timeout)
	return legView{}
}

// TestTransfer_Inbound_AutoDeclineOnTimeout: with the default (auto-dial off),
// an inbound REFER is parked for an app decision and surfaced as
// leg.transfer_requested. When no app accepts/declines within
// SIP_REFER_CONSULT_TIMEOUT_MS, VoiceBlender auto-declines (603, fail-closed)
// and the referrer (instance A) sees transfer_failed.
func TestTransfer_Inbound_AutoDeclineOnTimeout(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferConsultTimeoutMs = 200 // decline fast; no app responds
	})
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d, want 202", transferResp.StatusCode)
	}

	// instance B surfaces the parked REFER as a decision request (declined is
	// vestigial and always false now).
	instB.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegTransferRequestedData)
		return ok && !d.Declined
	}, 3*time.Second)

	// instance A should publish transfer_failed once the auto-decline 603 lands.
	instA.collector.waitForMatch(t, events.LegTransferFailed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestTransfer_Inbound_AppAcceptsAndCompletes: the app-driven happy path. B
// parks the REFER; the test (acting as B's controller) accepts it, then reports
// completion. The referrer (instance A) observes the 202 → NOTIFY 100 → NOTIFY
// 200 lifecycle as transfer_completed.
func TestTransfer_Inbound_AppAcceptsAndCompletes(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferConsultTimeoutMs = 4000 // ample time for the test to accept
	})
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d, want 202", transferResp.StatusCode)
	}

	// B's controller: on the parked REFER, accept then complete on B's leg.
	ev := instB.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegTransferRequestedData)
		return ok && !d.Declined
	}, 3*time.Second)
	referrerLeg := ev.Data.GetLegID()

	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer/accept", instB.baseURL(), referrerLeg), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("accept_transfer: status %d, want 202", r.StatusCode)
	}
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer/complete", instB.baseURL(), referrerLeg), map[string]interface{}{
		"success": true,
	}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("complete_transfer: status %d, want 202", r.StatusCode)
	}

	// A sees the transfer complete (terminal 2xx NOTIFY from B).
	instA.collector.waitForMatch(t, events.LegTransferCompleted, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestTransfer_Inbound_AppDeclines: the app explicitly rejects a parked REFER;
// the referrer (instance A) sees transfer_failed.
func TestTransfer_Inbound_AppDeclines(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferConsultTimeoutMs = 4000
	})
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	}); r.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d, want 202", r.StatusCode)
	}

	ev := instB.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegTransferRequestedData)
		return ok && !d.Declined
	}, 3*time.Second)
	referrerLeg := ev.Data.GetLegID()

	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer/decline", instB.baseURL(), referrerLeg), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("decline_transfer: status %d, want 202", r.StatusCode)
	}

	instA.collector.waitForMatch(t, events.LegTransferFailed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestTransfer_Blind_CustomHeaders: `refer_to_headers` end up percent-encoded
// in the Refer-To URI and `headers` on the REFER request itself, where a
// caller-supplied Referred-By replaces the default one. Asserted on the wire,
// as instance B receives it.
func TestTransfer_Blind_CustomHeaders(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferConsultTimeoutMs = 200 // nothing answers the parked REFER
	})

	type wireRefer struct {
		referTo, correlation, uui string
		referredBy                []string
	}
	got := make(chan wireRefer, 1)
	instB.engine.OnRefer(func(callID, target string, replaces *sipmod.ReplacesParams, req *sip.Request, tx sip.ServerTransaction) {
		w := wireRefer{}
		if h := req.GetHeader("Refer-To"); h != nil {
			w.referTo = h.Value()
		}
		if h := req.GetHeader("X-Correlation-ID"); h != nil {
			w.correlation = h.Value()
		}
		if h := req.GetHeader("User-to-User"); h != nil {
			w.uui = h.Value()
		}
		for _, h := range req.GetHeaders("Referred-By") {
			w.referredBy = append(w.referredBy, h.Value())
		}
		select {
		case got <- w:
		default:
		}
		instB.apiSrv.HandleIncomingRefer(callID, target, replaces, req, tx)
	})

	outboundID, _ := establishCall(t, instA, instB)

	target := "sip:test@127.0.0.1:5999"
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": target,
		"refer_to_headers": map[string]string{
			"x-inin-cnv":   "b2de3fe4-f097",
			"User-to-User": "00FA1B2C;encoding=hex",
		},
		"headers": map[string]string{
			"X-Correlation-ID": "abc 123",
			"User-to-User":     "00C0FFEE;encoding=hex",
			"Referred-By":      "<sip:+15551230000@example.com>",
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", resp.StatusCode)
	}

	var w wireRefer
	select {
	case w = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("instance B never received the REFER")
	}
	if want := "<" + target + "?User-to-User=00FA1B2C%3Bencoding%3Dhex&x-inin-cnv=b2de3fe4-f097>"; w.referTo != want {
		t.Errorf("Refer-To = %q, want %q", w.referTo, want)
	}
	if w.correlation != "abc 123" {
		t.Errorf("X-Correlation-ID = %q, want %q", w.correlation, "abc 123")
	}
	if w.uui != "00C0FFEE;encoding=hex" {
		t.Errorf("User-to-User = %q, want the REFER's own value", w.uui)
	}
	if len(w.referredBy) != 1 || w.referredBy[0] != "<sip:+15551230000@example.com>" {
		t.Errorf("Referred-By = %q, want only the caller's", w.referredBy)
	}

	// B strips the URI headers from the target it reports.
	instB.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegTransferRequestedData)
		return ok && d.Target == target
	}, 3*time.Second)

	instA.collector.waitForMatch(t, events.LegTransferFailed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestTransfer_Blind_DefaultReferredBy: without `headers` the REFER still
// carries exactly one Referred-By, and Refer-To is the bare target.
func TestTransfer_Blind_DefaultReferredBy(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPReferConsultTimeoutMs = 200
	})

	type wireRefer struct {
		referTo    string
		referredBy []string
	}
	got := make(chan wireRefer, 1)
	instB.engine.OnRefer(func(callID, target string, replaces *sipmod.ReplacesParams, req *sip.Request, tx sip.ServerTransaction) {
		w := wireRefer{}
		if h := req.GetHeader("Refer-To"); h != nil {
			w.referTo = h.Value()
		}
		for _, h := range req.GetHeaders("Referred-By") {
			w.referredBy = append(w.referredBy, h.Value())
		}
		select {
		case got <- w:
		default:
		}
		instB.apiSrv.HandleIncomingRefer(callID, target, replaces, req, tx)
	})

	outboundID, _ := establishCall(t, instA, instB)

	target := "sip:test@127.0.0.1:5999"
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": target,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", resp.StatusCode)
	}

	select {
	case w := <-got:
		if w.referTo != "<"+target+">" {
			t.Errorf("Refer-To = %q, want %q", w.referTo, "<"+target+">")
		}
		if len(w.referredBy) != 1 || w.referredBy[0] != "<sip:127.0.0.1>" {
			t.Errorf("Referred-By = %q, want the default", w.referredBy)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("instance B never received the REFER")
	}

	instA.collector.waitForMatch(t, events.LegTransferFailed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// TestTransfer_NotConnected: POST /transfer against a leg that hasn't
// connected → 409.
func TestTransfer_NotConnected(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: %d", createResp.StatusCode)
	}
	var lv legView
	decodeJSON(t, createResp, &lv)

	// Leg is in `ringing`, not `connected`.
	tr := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), lv.ID), map[string]interface{}{
		"target": "sip:bob@127.0.0.1:5060",
	})
	if tr.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on non-connected leg, got %d", tr.StatusCode)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), lv.ID))
}

// TestTransfer_BadRequest: invalid target URI → 400.
func TestTransfer_BadRequest(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing target: expected 400, got %d", r.StatusCode)
	}
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{"target": "not a uri"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad uri: expected 400, got %d", r.StatusCode)
	}
	// "sip:" parses under sipgo but has no host — must be rejected.
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{"target": "sip:"}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("sip: bare scheme: expected 400, got %d", r.StatusCode)
	}

	// Header validation: every one of these must stop at 400 without a REFER.
	target := "sip:bob@127.0.0.1:5060"
	for name, body := range map[string]map[string]interface{}{
		"refer_to_headers bad name":       {"target": target, "refer_to_headers": map[string]string{"X Foo": "1"}},
		"refer_to_headers Replaces":       {"target": target, "refer_to_headers": map[string]string{"Replaces": "abc"}},
		"refer_to_headers dialog header":  {"target": target, "refer_to_headers": map[string]string{"From": "<sip:ceo@example.com>"}},
		"refer_to_headers body":           {"target": target, "refer_to_headers": map[string]string{"body": "v=0"}},
		"refer_to_headers CRLF value":     {"target": target, "refer_to_headers": map[string]string{"X-A": "1\r\nVia: x"}},
		"refer_to_headers duplicate":      {"target": target, "refer_to_headers": map[string]string{"X-A": "1", "x-a": "2"}},
		"refer_to_headers already on URI": {"target": target + "?X-A=0", "refer_to_headers": map[string]string{"x-a": "1"}},
		"headers Refer-To":                {"target": target, "headers": map[string]string{"Refer-To": "<sip:evil@example.com>"}},
		"headers Contact":                 {"target": target, "headers": map[string]string{"Contact": "<sip:evil@example.com>"}},
		"headers CRLF value":              {"target": target, "headers": map[string]string{"X-A": "1\r\nRefer-To: <sip:evil@example.com>"}},
	} {
		if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, r.StatusCode)
		}
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
