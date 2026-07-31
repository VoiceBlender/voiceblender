//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestTransfer_ReferInheritsTrunkIdentity pins that a REFER-originated INVITE
// goes out over the trunk the referrer leg is associated with, under that
// trunk's AOR.
//
// Topology: instA doubles as instB's registrar and as the far end of the call,
// so the trunk's Route points somewhere that actually answers — the fake
// registrar in sip_trunks_test.go rejects every INVITE, which is enough to
// inspect one request but never establishes the dialog a REFER needs.
//
//	instB --REGISTER/INVITE(from=alice, Route: instA)--> instA
//	instB <--------------- REFER(target=instC) --------- instA
//	instB --INVITE(From: sip:alice@vb.test, Route: instA)-> instA
//
// The AOR host "vb.test" is not instB's public host, so a From carrying it
// proves the identity came from the trunk. Asserting on instB's leg.ringing for
// the transfer leg (rather than on instA's inbound) is deliberate: the event
// carries the full From URI and the trunk_id, while an inbound leg's `from`
// only reports the user part.
func TestTransfer_ReferInheritsTrunkIdentity(t *testing.T) {
	// instA: registrar + call peer. instB: owns the trunk, auto-dials the
	// REFER target. instC: transfer target.
	instA := newTestInstance(t, "refer-trunk-a")
	instB := newTestInstanceWithOpts(t, "refer-trunk-b", func(c *config.Config) {
		c.SIPReferAutoDial = true
	})
	instC := newTestInstance(t, "refer-trunk-c")

	createResp, body := createTrunkRequest(t, instB.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   fmt.Sprintf("sip:127.0.0.1:%d", instA.sipPort),
			"aor":             "sip:alice@vb.test",
			"password":        "secret",
			"expires_seconds": 600,
		},
	})
	if createResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create trunk: status %d, body=%s", createResp.StatusCode, body)
	}
	var created map[string]interface{}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode trunk: %v", err)
	}
	trunkID, _ := created["id"].(string)
	if trunkID == "" {
		t.Fatal("missing trunk id")
	}
	waitForTrunkStatus(t, instB.baseURL(), trunkID, "active", 5*time.Second)

	// Call out over the trunk. `from: "alice"` matches the AOR user-part, so
	// the leg picks up the trunk and its Route toward instA.
	legResp := httpPost(t, instB.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     fmt.Sprintf("sip:bob@127.0.0.1:%d", instA.sipPort),
		"from":   "alice",
		"codecs": []string{"PCMU"},
	})
	if legResp.StatusCode != http.StatusCreated && legResp.StatusCode != http.StatusAccepted {
		t.Fatalf("create leg: status %d", legResp.StatusCode)
	}
	legResp.Body.Close()

	inboundOnA := waitForInboundLeg(t, instA.baseURL(), 5*time.Second)
	if r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instA.baseURL(), inboundOnA.ID), nil); r.StatusCode != http.StatusAccepted {
		t.Fatalf("answer on A: %d", r.StatusCode)
	}
	// The answer is async; POST /transfer is 409 until the leg is connected.
	waitForLegState(t, instA.baseURL(), inboundOnA.ID, "connected", 5*time.Second)

	// instA REFERs instB to instC; instB auto-dials.
	target := fmt.Sprintf("sip:test@127.0.0.1:%d", instC.sipPort)
	transferResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", instA.baseURL(), inboundOnA.ID), map[string]interface{}{
		"target": target,
	})
	if transferResp.StatusCode != http.StatusAccepted {
		t.Fatalf("transfer: status %d", transferResp.StatusCode)
	}

	// Match on the target URI, not on "the other leg": instB also rings for
	// the original outbound leg, and keying off that leg's ID makes the
	// assertion depend on the create-leg response body parsing correctly.
	ev := instB.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool {
		d, ok := e.Data.(*events.LegRingingData)
		return ok && d.URI == target
	}, 10*time.Second)

	d := ev.Data.(*events.LegRingingData)
	if d.From != "sip:alice@vb.test" {
		t.Errorf("transfer leg from = %q, want the trunk AOR %q", d.From, "sip:alice@vb.test")
	}
	if d.TrunkID != trunkID {
		t.Errorf("transfer leg trunk_id = %q, want the referrer's trunk %q", d.TrunkID, trunkID)
	}
}
