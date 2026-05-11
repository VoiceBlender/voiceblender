//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws/wsutil"
)

// vsiResultFrame is the subset of a VSI command response we care about for
// these tests — type, request_id, and the data payload (as raw JSON).
type vsiResultFrame struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// vsiSend writes a command frame with the given type, request_id, and
// payload, then returns the matching result frame. Drains intervening
// event frames silently.
func vsiSend(t *testing.T, conn net.Conn, typ, reqID string, payload interface{}) vsiResultFrame {
	t.Helper()
	envelope := map[string]interface{}{
		"type":       typ,
		"request_id": reqID,
	}
	if payload != nil {
		p, _ := json.Marshal(payload)
		envelope["payload"] = json.RawMessage(p)
	}
	msg, _ := json.Marshal(envelope)
	if err := wsutil.WriteClientText(conn, msg); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		data, err := wsutil.ReadServerText(conn)
		if err != nil {
			t.Fatalf("read %s: %v", typ, err)
		}
		var f vsiResultFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.RequestID == reqID {
			return f
		}
	}
	t.Fatalf("no result for %s/%s within 5s", typ, reqID)
	return vsiResultFrame{}
}

func TestVSI_RTT_SendDelivers(t *testing.T) {
	b := setupRTTBridge(t)

	conn := dialVSI(t, b.caller)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second) // consume "connected"

	f := vsiSend(t, conn, "send_leg_rtt", "send-1", map[string]string{
		"id":   b.callerLegID,
		"text": "vsi-hello",
	})
	if f.Type != "send_leg_rtt.result" {
		t.Fatalf("type = %q, want send_leg_rtt.result", f.Type)
	}

	waitForRTT(t, b.callee, b.calleeLegID, "vsi-hello", 3*time.Second)
}

func TestVSI_RTT_AcceptRejectFlags(t *testing.T) {
	b := setupRTTBridge(t)

	conn := dialVSI(t, b.callee)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	// reject — flag goes false. accept_text gates inbound rtt.received events
	// on the receiving leg, so a subsequent send from caller to callee should
	// produce no event on the callee.
	f := vsiSend(t, conn, "reject_leg_rtt", "rej-1", map[string]string{"id": b.calleeLegID})
	if f.Type != "reject_leg_rtt.result" {
		t.Fatalf("reject type = %q, want reject_leg_rtt.result", f.Type)
	}

	r := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/rtt", b.caller.baseURL(), b.callerLegID),
		map[string]string{"text": "ignored"})
	r.Body.Close()

	// Wait briefly and assert callee did NOT see the chunk.
	time.Sleep(400 * time.Millisecond)
	if got := b.callee.collector.matchAll(events.RTTReceived, func(e events.Event) bool {
		return e.Data.GetLegID() == b.calleeLegID
	}); len(got) != 0 {
		t.Fatalf("expected no rtt.received with reject, got %d events", len(got))
	}

	// accept — flag goes true again. Now callee should observe the next send.
	f = vsiSend(t, conn, "accept_leg_rtt", "acc-1", map[string]string{"id": b.calleeLegID})
	if f.Type != "accept_leg_rtt.result" {
		t.Fatalf("accept type = %q, want accept_leg_rtt.result", f.Type)
	}
	r2 := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/rtt", b.caller.baseURL(), b.callerLegID),
		map[string]string{"text": "after-accept"})
	r2.Body.Close()
	waitForRTT(t, b.callee, b.calleeLegID, "after-accept", 3*time.Second)
}

func TestVSI_RTT_SendOnNonNegotiatedLegReturns409(t *testing.T) {
	caller := newTestInstance(t, "vsi-rtt-no-negot-caller")
	callee := newTestInstance(t, "vsi-rtt-no-negot-callee")

	// Bridge without rtt:true so no m=text is offered.
	resp := httpPost(t, caller.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", callee.sipPort),
		"codecs": []string{"PCMU"},
	})
	var v legView
	decodeJSON(t, resp, &v)
	in := waitForInboundLeg(t, callee.baseURL(), 5*time.Second)
	httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", callee.baseURL(), in.ID), nil).Body.Close()
	waitForLegState(t, caller.baseURL(), v.ID, "connected", 5*time.Second)

	conn := dialVSI(t, caller)
	defer conn.Close()
	readWSFrame(t, conn, 5*time.Second)

	f := vsiSend(t, conn, "send_leg_rtt", "send-bad", map[string]string{
		"id":   v.ID,
		"text": "no-negot",
	})
	if f.Type != "error" {
		t.Fatalf("type = %q, want error", f.Type)
	}
}
