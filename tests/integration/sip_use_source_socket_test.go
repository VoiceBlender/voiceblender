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

// TestUseSourceSocket_RoundTripCall verifies that a normal SIP call between
// two VoiceBlender instances completes when SIP_USE_SOURCE_SOCKET is enabled
// on both sides. Both instances bind to localhost so the source-socket and
// Contact addresses agree and routing must still succeed end-to-end.
//
// Unit tests in internal/sip/engine_test.go cover the destination-pinning
// logic itself (TestEngine_PinDestinationToSource); this is a regression
// guard that the flag doesn't break ordinary call setup, in-dialog requests
// (BYE), or response delivery.
func TestUseSourceSocket_RoundTripCall(t *testing.T) {
	enable := func(c *config.Config) { c.SIPUseSourceSocket = true }
	instA := newTestInstanceWithOpts(t, "use-source-a", enable)
	instB := newTestInstanceWithOpts(t, "use-source-b", enable)

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	// Hang up from A — this exercises the BYE path that, when peers advertise
	// unroutable Contact, would benefit from RewriteContact-style routing.
	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID))
	if delResp.StatusCode >= 300 {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	instA.collector.waitForMatch(t, events.LegDisconnected, nil, 5*time.Second)
	instB.collector.waitForMatch(t, events.LegDisconnected, nil, 5*time.Second)

	// Assert the flag actually pinned a destination on at least one side.
	// Without this guard a silent no-op (e.g. due to a future refactor that
	// drops the SetDestination call) would still let the round trip succeed
	// because both peers happen to share 127.0.0.1.
	if instA.engine.DestinationsPinned() == 0 && instB.engine.DestinationsPinned() == 0 {
		t.Errorf("SIP_USE_SOURCE_SOCKET=true but no destination pinning fired on either side")
	}
}
