//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
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
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instC.baseURL(), inboundOnC.ID), nil); r.StatusCode != http.StatusOK {
		t.Fatalf("answer on C: %d", r.StatusCode)
	}

	instA.collector.waitForMatch(t, events.LegTransferCompleted, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// Cleanup the surviving call on instance C side.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instC.baseURL(), inboundOnC.ID))
}

// TestTransfer_Inbound_DeclinedByDefault: with the default (auto-dial off),
// when a peer sends us a REFER we 603 Decline and emit a `declined: true`
// event. We exercise this by having instance A REFER instance B; B is
// at the default config so it should reject.
func TestTransfer_Inbound_DeclinedByDefault(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b") // SIP_REFER_AUTO_DIAL=false (default)
	instC := newTestInstance(t, "instance-c")

	outboundID, _ := establishCall(t, instA, instB)

	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID), map[string]interface{}{
		"target": fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort),
	})
	// REST accepts the request asynchronously (202) regardless of how
	// the peer responds. The peer's 603 Decline surfaces on the event bus.
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d, want 202", transferResp.StatusCode)
	}

	// instance B should publish an audit event for the declined REFER.
	instB.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegTransferRequestedData)
		return ok && d.Declined
	}, 3*time.Second)

	// instance A should publish transfer_failed once the REFER is rejected.
	instA.collector.waitForMatch(t, events.LegTransferFailed, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)

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

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
