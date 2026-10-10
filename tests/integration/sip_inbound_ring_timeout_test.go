//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// parkedInboundHandlers counts the instance's goroutines still inside an
// inbound INVITE handler, i.e. INVITE transactions it has not let go of. The
// receiver pointer scopes the count to this instance, so handlers still
// unwinding from an earlier test's instance do not leak into it.
func parkedInboundHandlers(inst *testInstance) int {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), fmt.Sprintf("(*Server).HandleInboundCall(%p", inst.apiSrv))
}

func waitForParkedInboundHandlers(t *testing.T, inst *testInstance, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if parkedInboundHandlers(inst) == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("parked inbound INVITE handlers = %d, want %d", parkedInboundHandlers(inst), want)
}

// dialUnanswered has A ring B with no outbound ring timeout, so only B can end
// the attempt. It returns A's outbound leg and B's inbound leg.
func dialUnanswered(t *testing.T, instA, instB *testInstance) (outbound, inbound legView) {
	t.Helper()
	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":         "sip",
		"uri":          fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs":       []string{"PCMU"},
		"ring_timeout": 0,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	decodeJSON(t, createResp, &outbound)
	inbound = waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	return outbound, inbound
}

// assertCallerStillRinging verifies the caller saw no final response: its leg
// is still ringing and was never disconnected.
func assertCallerStillRinging(t *testing.T, instA *testInstance, outboundID string) {
	t.Helper()
	time.Sleep(time.Second)
	waitForLegState(t, instA.baseURL(), outboundID, "ringing", time.Second)
	if got := len(instA.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID
	})); got != 0 {
		t.Errorf("caller leg.disconnected count = %d, want 0 (no final response may be sent)", got)
	}
}

// TestInboundRingTimeout_DropsSilently verifies that an inbound leg nobody
// answers, and whose caller never CANCELs, is released after
// SIP_INBOUND_RING_TIMEOUT_SECONDS without a final response to the caller.
func TestInboundRingTimeout_DropsSilently(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPInboundRingTimeoutSeconds = 1
	})

	outbound, inbound := dialUnanswered(t, instA, instB)
	waitForParkedInboundHandlers(t, instB, 1, 3*time.Second)

	disc := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)
	d := disc.Data.(*events.LegDisconnectedData)
	if d.CDR.Reason != "ring_timeout" {
		t.Errorf("cdr.reason = %q, want ring_timeout", d.CDR.Reason)
	}
	if d.CDR.DurationAnswered != 0 {
		t.Errorf("duration_answered = %f, want 0", d.CDR.DurationAnswered)
	}

	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET timed-out leg: status %d, want 404", resp.StatusCode)
	}

	waitForParkedInboundHandlers(t, instB, 0, 3*time.Second)
	assertCallerStillRinging(t, instA, outbound.ID)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
}

// TestInboundRingTimeout_ZeroIsUnbounded verifies that 0 keeps an unanswered
// inbound leg ringing indefinitely.
func TestInboundRingTimeout_ZeroIsUnbounded(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPInboundRingTimeoutSeconds = 0
	})

	outbound, inbound := dialUnanswered(t, instA, instB)

	time.Sleep(2 * time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "ringing", time.Second)
	if got := len(instB.collector.matchAll(events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	})); got != 0 {
		t.Errorf("leg.disconnected count = %d, want 0 with the timeout disabled", got)
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
}

// TestInboundRingTimeout_AnswerBeatsTimeout verifies that the timeout only
// bounds the unanswered phase: an answered call outlives it.
func TestInboundRingTimeout_AnswerBeatsTimeout(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstanceWithOpts(t, "instance-b", func(c *config.Config) {
		c.SIPInboundRingTimeoutSeconds = 1
	})

	outboundID, inboundID := establishCall(t, instA, instB)

	time.Sleep(2 * time.Second)
	waitForLegState(t, instB.baseURL(), inboundID, "connected", time.Second)
	waitForLegState(t, instA.baseURL(), outboundID, "connected", time.Second)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID)).Body.Close()
}

// TestDeleteUnansweredInbound_NoReasonDropsSilently verifies that DELETE
// without a reason releases an unanswered inbound leg's INVITE transaction
// without sending the caller a final response.
func TestDeleteUnansweredInbound_NoReasonDropsSilently(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outbound, inbound := dialUnanswered(t, instA, instB)
	waitForParkedInboundHandlers(t, instB, 1, 3*time.Second)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inbound.ID))
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}

	disc := instB.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == inbound.ID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "api_hangup" {
		t.Errorf("cdr.reason = %q, want api_hangup", got)
	}

	waitForParkedInboundHandlers(t, instB, 0, 3*time.Second)
	assertCallerStillRinging(t, instA, outbound.ID)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
}
