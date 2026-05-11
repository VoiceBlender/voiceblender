//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// rttBridge is the minimal RTT topology — two instances bridged by a single
// outbound INVITE. The caller passes "rtt": true on the originate request;
// the callee always accepts an offered m=text section.
type rttBridge struct {
	caller, callee *testInstance
	callerLegID    string
	calleeLegID    string
}

func setupRTTBridge(t *testing.T) *rttBridge {
	t.Helper()
	b := &rttBridge{
		caller: newTestInstance(t, "rtt-caller"),
		callee: newTestInstance(t, "rtt-callee"),
	}

	body := map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", b.callee.sipPort),
		"codecs": []string{"PCMU"},
		"rtt":    true,
	}
	resp := httpPost(t, b.caller.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("originate: status %d", resp.StatusCode)
	}
	var v legView
	decodeJSON(t, resp, &v)
	b.callerLegID = v.ID

	in := waitForInboundLeg(t, b.callee.baseURL(), 5*time.Second)
	b.calleeLegID = in.ID
	ans := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", b.callee.baseURL(), b.calleeLegID), nil)
	ans.Body.Close()
	waitForLegState(t, b.caller.baseURL(), b.callerLegID, "connected", 5*time.Second)
	waitForLegState(t, b.callee.baseURL(), b.calleeLegID, "connected", 5*time.Second)
	return b
}

func waitForRTT(t *testing.T, inst *testInstance, legID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches := inst.collector.matchAll(events.RTTReceived, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		})
		var got string
		for _, e := range matches {
			d := e.Data.(*events.RTTReceivedData)
			got += d.Text
		}
		if got == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	matches := inst.collector.matchAll(events.RTTReceived, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	})
	var got string
	for _, e := range matches {
		d := e.Data.(*events.RTTReceivedData)
		got += d.Text
	}
	t.Fatalf("instance %s did not observe RTT %q on leg %s within %v (got %q)", inst.name, want, legID, timeout, got)
}

func TestRTT_RoundTrip(t *testing.T) {
	b := setupRTTBridge(t)

	// Send "hello" from caller → callee should observe it.
	r := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/rtt", b.caller.baseURL(), b.callerLegID),
		map[string]string{"text": "hello"})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("send rtt: %d", r.StatusCode)
	}
	r.Body.Close()
	waitForRTT(t, b.callee, b.calleeLegID, "hello", 3*time.Second)

	// Send back from callee → caller should observe.
	r2 := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/rtt", b.callee.baseURL(), b.calleeLegID),
		map[string]string{"text": "world"})
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("send rtt back: %d", r2.StatusCode)
	}
	r2.Body.Close()
	waitForRTT(t, b.caller, b.callerLegID, "world", 3*time.Second)
}

func TestRTT_NotOfferedRejectsSendCleanly(t *testing.T) {
	// Caller does NOT pass "rtt": true, so no m=text appears in the offer.
	// The callee has nothing to accept — neither side negotiates RTT — and
	// POST .../rtt on either side must return 409.
	caller := newTestInstance(t, "rtt-no-offer-caller")
	callee := newTestInstance(t, "rtt-no-offer-callee")

	body := map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", callee.sipPort),
		"codecs": []string{"PCMU"},
	}
	resp := httpPost(t, caller.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("originate: %d", resp.StatusCode)
	}
	var v legView
	decodeJSON(t, resp, &v)

	in := waitForInboundLeg(t, callee.baseURL(), 5*time.Second)
	httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", callee.baseURL(), in.ID), nil).Body.Close()
	waitForLegState(t, caller.baseURL(), v.ID, "connected", 5*time.Second)

	r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/rtt", callee.baseURL(), in.ID),
		map[string]string{"text": "x"})
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on non-negotiated leg, got %d", r.StatusCode)
	}
	r.Body.Close()
}
