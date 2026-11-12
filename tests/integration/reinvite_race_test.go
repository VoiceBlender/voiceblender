//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// waitForRTPBeyond blocks until the leg has received more than n RTP packets.
func waitForRTPBeyond(t *testing.T, sl *leg.SIPLeg, n uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sl.RTPStats().PacketsReceived > n+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("leg %s: no RTP received beyond packet %d", sl.ID(), n)
}

// TestReInvite_HoldResumeWithAudioFlowing holds and resumes a call twice from
// the answering side while both legs send audio, so every re-INVITE lands on a
// stream whose media loops are busy. It asserts little by itself: its job is to
// fail under -race if applying an offer touches state the loops read.
func TestReInvite_HoldResumeWithAudioFlowing(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, inboundID := establishCall(t, instA, instB)

	for _, l := range []struct{ base, id string }{
		{instA.baseURL(), outboundID},
		{instB.baseURL(), inboundID},
	} {
		resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", l.base, l.id),
			map[string]interface{}{"tone": "us_dial", "repeat": -1, "volume": 0})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("play on %s: unexpected status %d", l.id, resp.StatusCode)
		}
		resp.Body.Close()
	}
	legA, legB := sipLegOf(t, instA, outboundID), sipLegOf(t, instB, inboundID)
	waitForRTPBeyond(t, legA, 0)
	waitForRTPBeyond(t, legB, 0)

	countOf := func(inst *testInstance, typ events.EventType, legID string) int {
		return len(inst.collector.matchAll(typ, func(e events.Event) bool {
			return e.Data.GetLegID() == legID
		}))
	}
	waitForCount := func(inst *testInstance, typ events.EventType, legID string, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if countOf(inst, typ, legID) >= want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s number %d on leg %s", typ, want, legID)
	}

	for round := 1; round <= 2; round++ {
		holdResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), inboundID), nil)
		if holdResp.StatusCode != http.StatusAccepted {
			t.Fatalf("round %d hold: unexpected status %d", round, holdResp.StatusCode)
		}
		holdResp.Body.Close()
		waitForCount(instB, events.LegHold, inboundID, round)
		waitForCount(instA, events.LegHold, outboundID, round)

		beforeA := legA.RTPStats().PacketsReceived
		beforeB := legB.RTPStats().PacketsReceived

		unholdResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), inboundID))
		if unholdResp.StatusCode != http.StatusAccepted {
			t.Fatalf("round %d unhold: unexpected status %d", round, unholdResp.StatusCode)
		}
		unholdResp.Body.Close()
		waitForCount(instB, events.LegUnhold, inboundID, round)
		waitForCount(instA, events.LegUnhold, outboundID, round)

		// Audio must flow again both ways, which also puts both read loops back
		// on the state the re-INVITE just rewrote.
		waitForRTPBeyond(t, legA, beforeA)
		waitForRTPBeyond(t, legB, beforeB)
	}

	for _, l := range []struct{ base, id string }{
		{instA.baseURL(), outboundID},
		{instB.baseURL(), inboundID},
	} {
		resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s", l.base, l.id))
		var lv legView
		decodeJSON(t, resp, &lv)
		if lv.State != "connected" || lv.Held {
			t.Fatalf("leg %s after two hold/resume rounds: state %q, held %v", l.id, lv.State, lv.Held)
		}
	}

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}
