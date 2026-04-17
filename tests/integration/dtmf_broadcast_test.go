//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// dtmfBridge holds a 3-instance topology used to exercise DTMF broadcast.
//
//   - inst1 is the VoiceBlender under test. It originates two legs (A and B)
//     into a single room "broadcast", so DTMF received on one should be
//     forwarded to the other.
//   - inst2 plays the role of leg A's far end (DTMF source).
//   - inst3 plays the role of leg B's far end (DTMF target).
type dtmfBridge struct {
	inst1, inst2, inst3                                *testInstance
	legAOnInst1, legAOnInst2, legBOnInst1, legBOnInst3 string
}

// setupDTMFBridge dials A then B from inst1, with optional extra body fields
// applied to each originate request (e.g. {"accept_dtmf": false}).
func setupDTMFBridge(t *testing.T, legABody, legBBody map[string]interface{}) *dtmfBridge {
	t.Helper()

	b := &dtmfBridge{
		inst1: newTestInstance(t, "inst1-vb"),
		inst2: newTestInstance(t, "inst2-A-far-end"),
		inst3: newTestInstance(t, "inst3-B-far-end"),
	}

	b.legAOnInst1, b.legAOnInst2 = b.dial(t, b.inst2, legABody)
	b.legBOnInst1, b.legBOnInst3 = b.dial(t, b.inst3, legBBody)
	return b
}

func (b *dtmfBridge) dial(t *testing.T, farEnd *testInstance, extra map[string]interface{}) (legOnInst1, legOnFar string) {
	t.Helper()
	body := map[string]interface{}{
		"type":    "sip",
		"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", farEnd.sipPort),
		"codecs":  []string{"PCMU"},
		"room_id": "broadcast",
	}
	for k, v := range extra {
		body[k] = v
	}
	resp := httpPost(t, b.inst1.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("originate to %s: status %d", farEnd.name, resp.StatusCode)
	}
	var v legView
	decodeJSON(t, resp, &v)
	legOnInst1 = v.ID

	in := waitForInboundLeg(t, farEnd.baseURL(), 5*time.Second)
	legOnFar = in.ID
	ans := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", farEnd.baseURL(), legOnFar), nil)
	ans.Body.Close()

	waitForLegState(t, b.inst1.baseURL(), legOnInst1, "connected", 5*time.Second)
	return
}

// sendDTMFFrom asks inst to emit DTMF digits on legID toward its far end.
func sendDTMFFrom(t *testing.T, inst *testInstance, legID, digits string) {
	t.Helper()
	r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/dtmf", inst.baseURL(), legID),
		map[string]string{"digits": digits})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("send dtmf %q on leg %s: status %d", digits, legID, r.StatusCode)
	}
	r.Body.Close()
}

// dtmfHasArrived reports whether a dtmf.received event for the given digit
// has been recorded against legID on inst's event bus.
func dtmfHasArrived(inst *testInstance, legID, digit string) bool {
	return inst.collector.hasEvent(events.DTMFReceived, func(e events.Event) bool {
		if e.Data.GetLegID() != legID {
			return false
		}
		d, ok := e.Data.(*events.DTMFReceivedData)
		return ok && d.Digit == digit
	})
}

// waitForDTMF polls the bus for up to 5s.
func waitForDTMF(t *testing.T, inst *testInstance, legID, digit string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dtmfHasArrived(inst, legID, digit) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("instance %s did not observe DTMF %q on leg %s within 5s", inst.name, digit, legID)
}

// assertNoDTMF waits a short window and asserts the digit never arrived.
func assertNoDTMF(t *testing.T, inst *testInstance, legID, digit string) {
	t.Helper()
	time.Sleep(2 * time.Second)
	if dtmfHasArrived(inst, legID, digit) {
		t.Fatalf("instance %s unexpectedly received DTMF %q on leg %s", inst.name, digit, legID)
	}
}

func TestDTMFBroadcast_Default(t *testing.T) {
	b := setupDTMFBridge(t, nil, nil)

	// Leg A's far end (inst2) sends digit "5" toward inst1's leg A.
	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "5")

	// inst1 should fire dtmf.received for leg A.
	waitForDTMF(t, b.inst1, b.legAOnInst1, "5")
	// And forward to leg B's far end (inst3).
	waitForDTMF(t, b.inst3, b.legBOnInst3, "5")
}

func TestDTMFBroadcast_RejectAtRuntime(t *testing.T) {
	b := setupDTMFBridge(t, nil, nil)

	// Disable DTMF reception on leg B (the recipient).
	r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/dtmf/reject", b.inst1.baseURL(), b.legBOnInst1), nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("dtmf/reject: status %d", r.StatusCode)
	}
	r.Body.Close()

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "7")

	// Originating event still fires.
	waitForDTMF(t, b.inst1, b.legAOnInst1, "7")
	// But the forward must not reach leg B's far end.
	assertNoDTMF(t, b.inst3, b.legBOnInst3, "7")

	// Re-enable and confirm forwarding resumes.
	r2 := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/dtmf/accept", b.inst1.baseURL(), b.legBOnInst1), nil)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("dtmf/accept: status %d", r2.StatusCode)
	}
	r2.Body.Close()

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "8")
	waitForDTMF(t, b.inst3, b.legBOnInst3, "8")
}

func TestDTMFBroadcast_RejectAtOriginate(t *testing.T) {
	b := setupDTMFBridge(t, nil, map[string]interface{}{"accept_dtmf": false})

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "9")
	waitForDTMF(t, b.inst1, b.legAOnInst1, "9")
	assertNoDTMF(t, b.inst3, b.legBOnInst3, "9")
}

func TestDTMFBroadcast_SequenceNumbers(t *testing.T) {
	b := setupDTMFBridge(t, nil, nil)

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "1")
	waitForDTMF(t, b.inst1, b.legAOnInst1, "1")

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "2")
	waitForDTMF(t, b.inst1, b.legAOnInst1, "2")

	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "3")
	waitForDTMF(t, b.inst1, b.legAOnInst1, "3")

	dtmfEvents := b.inst1.collector.matchAll(events.DTMFReceived, func(e events.Event) bool {
		return e.Data.GetLegID() == b.legAOnInst1
	})

	if len(dtmfEvents) != 3 {
		t.Fatalf("got %d DTMF events for leg A, want 3", len(dtmfEvents))
	}

	for i, e := range dtmfEvents {
		d := e.Data.(*events.DTMFReceivedData)
		wantSeq := uint64(i + 1)
		if d.Seq != wantSeq {
			t.Errorf("event[%d] seq = %d, want %d", i, d.Seq, wantSeq)
		}
	}
}

func TestDTMFBroadcast_SenderExcluded(t *testing.T) {
	b := setupDTMFBridge(t, nil, nil)

	// A's far end sends "3"; leg A's far end (sender side) must NOT receive
	// a forwarded copy back to itself.
	sendDTMFFrom(t, b.inst2, b.legAOnInst2, "3")
	waitForDTMF(t, b.inst3, b.legBOnInst3, "3")
	assertNoDTMF(t, b.inst2, b.legAOnInst2, "3")
}
