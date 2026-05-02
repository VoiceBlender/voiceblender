//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestDeleteReason_BusyOnUnanswered verifies that DELETE with reason "busy"
// on a ringing inbound leg sends SIP 486 Busy Here, surfaces "busy" in the
// inbound leg.disconnected event, and that the outbound caller observes
// "busy" via its own SIP-failure-to-reason mapping.
func TestDeleteReason_BusyOnUnanswered(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	delResp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID),
		map[string]interface{}{"reason": "busy"})
	if delResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(delResp.Body)
		delResp.Body.Close()
		t.Fatalf("delete with reason=busy: status %d, body=%s", delResp.StatusCode, body)
	}
	delResp.Body.Close()

	// Inbound side: cdr.reason should be the user-supplied "busy".
	bDisc := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)
	if got := bDisc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "busy" {
		t.Errorf("B cdr.reason = %q, want busy", got)
	}

	// Outbound side: sipgo maps the 486 to "busy" via inviteFailureReason.
	aDisc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outbound.ID
	}, 5*time.Second)
	if got := aDisc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "busy" {
		t.Errorf("A cdr.reason = %q, want busy (caller observed 486)", got)
	}
}

// TestDeleteReason_DeclinedOnUnanswered verifies that DELETE with reason
// "declined" maps to SIP 603 Decline.
func TestDeleteReason_DeclinedOnUnanswered(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	delResp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID),
		map[string]interface{}{"reason": "declined"})
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	bDisc := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)
	if got := bDisc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "declined" {
		t.Errorf("B cdr.reason = %q, want declined", got)
	}
	aDisc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outbound.ID
	}, 5*time.Second)
	if got := aDisc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "declined" {
		t.Errorf("A cdr.reason = %q, want declined (caller observed 603)", got)
	}
}

// TestDeleteReason_UnknownReturns400 verifies the synchronous validation
// error for an unknown reason value.
func TestDeleteReason_UnknownReturns400(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"uri":  fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	createResp.Body.Close()

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	resp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID),
		map[string]interface{}{"reason": "no_such_reason"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete with bad reason: status %d, body=%s, want 400", resp.StatusCode, body)
	}
}

// TestDeleteReason_IgnoredOnConnected verifies that a reason on a connected
// leg is silently ignored — the leg is BYE'd as today and cdr.reason is the
// legacy "api_hangup".
func TestDeleteReason_IgnoredOnConnected(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	delResp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID),
		map[string]interface{}{"reason": "busy"})
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete: status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	disc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "api_hangup" {
		t.Errorf("connected DELETE cdr.reason = %q, want api_hangup (reason ignored)", got)
	}
}
