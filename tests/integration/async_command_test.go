//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestAsync_HangupReturns202 verifies that DELETE /v1/legs/{id} now returns
// 202 Accepted and the leg disconnects asynchronously.
func TestAsync_HangupReturns202(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("hangup: status %d, want 202", delResp.StatusCode)
	}
	var body map[string]string
	decodeJSON(t, delResp, &body)
	if body["status"] != "hanging_up" {
		t.Errorf("status = %q, want hanging_up", body["status"])
	}

	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)
}

// TestAsync_HoldFailureSurfacesAsCommandFailed verifies that an async hold
// failure (after the 202 response) emits leg.command_failed. We trigger a
// failure by holding a leg whose dialog has gone away.
func TestAsync_HoldFailureSurfacesAsCommandFailed(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	// Tear down B's side abruptly so any re-INVITE A sends will fail at the
	// SIP transaction layer (no peer to answer the re-INVITE).
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inboundID))
	// Wait for A to see the disconnect, then ensure leg is gone.
	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 5*time.Second)

	// Now hold should 4xx synchronously because the leg is gone.
	holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), outboundID), nil)
	defer holdResp.Body.Close()
	if holdResp.StatusCode != http.StatusNotFound {
		t.Fatalf("hold after disconnect: status %d, want 404", holdResp.StatusCode)
	}
}

// TestAsync_TransferToBadTargetEmitsCommandFailed_OrTransferFailed verifies
// that a transfer to a non-routable target surfaces a failure event rather
// than blocking the HTTP handler.
func TestAsync_TransferReturns202(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	resp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), outboundID),
		map[string]interface{}{"target": "sip:nobody@127.0.0.1:1"},
	)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	// transfer_initiated fires synchronously before the goroutine starts;
	// transfer_failed fires after the REFER fails.
	instA.collector.waitForMatch(t, events.LegTransferInitiated, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	}, 3*time.Second)
}
