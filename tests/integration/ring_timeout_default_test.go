//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestRingTimeout_Default verifies that an outbound leg created without
// ring_timeout is ended by the default ring timeout instead of ringing
// forever. The default is shortened so the test need not wait a minute.
func TestRingTimeout_Default(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instA.apiSrv.DefaultRingTimeout = 2 * time.Second
	instB := newTestInstance(t, "instance-b")

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

	// B never answers.
	disc := instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outbound.ID
	}, 6*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "ring_timeout" {
		t.Errorf("cdr.reason = %q, want ring_timeout", got)
	}
}

// TestRingTimeout_ExplicitZeroDisablesDefault verifies that ring_timeout: 0
// still means "no timeout" now that omitting it no longer does.
func TestRingTimeout_ExplicitZeroDisablesDefault(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instA.apiSrv.DefaultRingTimeout = time.Second
	instB := newTestInstance(t, "instance-b")

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":         "sip",
		"uri":          fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":       []string{"PCMU"},
		"ring_timeout": 0,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)
	waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	// Well past the shortened 1s default.
	time.Sleep(3 * time.Second)
	waitForLegState(t, instA.baseURL(), outbound.ID, "ringing", time.Second)
	if got := len(instA.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outbound.ID
	})); got != 0 {
		t.Errorf("leg.disconnected count = %d, want 0 (ring_timeout: 0 must disable the default)", got)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
}
