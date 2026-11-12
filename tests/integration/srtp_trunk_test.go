//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func createSRTPTrunk(t *testing.T, inst *testInstance, srtp string, spec map[string]interface{}) string {
	t.Helper()
	resp, data := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{"type": "ip_ip", "srtp": srtp, "ip_ip": spec})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create trunk: status %d, body=%s", resp.StatusCode, data)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		t.Fatal(err)
	}
	if got := trunkSnapshot(t, inst.baseURL(), created.ID)["srtp"]; got != srtp {
		t.Fatalf("GET trunk srtp = %v, want %q", got, srtp)
	}
	return created.ID
}

func TestSRTP_TrunkValidation(t *testing.T) {
	inst := newTestInstance(t, "srtp-trunk-validate")
	resp, data := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "ip_ip", "srtp": "sometimes", "ip_ip": map[string]interface{}{"peer_uri": "sip:127.0.0.1:5999"},
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "srtp") {
		t.Errorf("invalid srtp: status %d body %s", resp.StatusCode, data)
	}

	// Without srtp the trunk follows SIP_SRTP and its view leaves the field out.
	resp, data = createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "ip_ip", "ip_ip": map[string]interface{}{"peer_uri": "sip:127.0.0.1:5998"},
	})
	var plain struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(data, &plain) != nil {
		t.Fatalf("trunk without srtp: status %d body %s", resp.StatusCode, data)
	}
	if v, ok := trunkSnapshot(t, inst.baseURL(), plain.ID)["srtp"]; ok {
		t.Errorf("a trunk following SIP_SRTP reports srtp = %v", v)
	}

	// A leg's srtp is checked against the trunk it goes through, whether
	// named by trunk_id or selected by from.
	id := createSRTPTrunk(t, inst, "disabled", map[string]interface{}{"peer_uri": "sip:127.0.0.1:5997", "aor": "sip:acme@127.0.0.1"})
	for name, body := range map[string]map[string]interface{}{
		"by trunk_id": {"type": "sip", "to": "sip:bob@127.0.0.1:5997", "trunk_id": id, "srtp": true},
		"by from":     {"type": "sip", "to": "sip:bob@127.0.0.1:5997", "from": "sip:acme@127.0.0.1", "srtp": true},
	} {
		resp := httpPost(t, inst.baseURL()+"/v1/legs", body)
		msg, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(msg), "trunk "+id) {
			t.Errorf("%s: status %d body %s, want 400 naming the trunk", name, resp.StatusCode, msg)
		}
	}
}

// TestSRTP_TrunkOutbound places calls through a trunk whose srtp differs from
// SIP_SRTP, at a peer that accepts plain RTP only.
func TestSRTP_TrunkOutbound(t *testing.T) {
	t.Run("trunk requires SRTP", func(t *testing.T) {
		instA := newTestInstance(t, "srtp-trunk-req")
		instB := newTestInstanceWithOpts(t, "srtp-trunk-plain", withSRTP("disabled"))
		id := createSRTPTrunk(t, instA, "required", map[string]interface{}{"peer_uri": peerURI(instB.sipPort)})

		resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
			"type": "sip", "to": fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort), "trunk_id": id,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create leg: status %d", resp.StatusCode)
		}
		var out legView
		decodeJSON(t, resp, &out)
		instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool { return e.Data.GetLegID() == out.ID }, 5*time.Second)
		if legs := listLegs(t, instB); len(legs) != 0 {
			t.Errorf("the SRTP offer reached the plain-only peer as %d legs", len(legs))
		}
	})

	t.Run("trunk relaxes SIP_SRTP=required", func(t *testing.T) {
		instA := newTestInstanceWithOpts(t, "srtp-trunk-loose", withSRTP("required"))
		instB := newTestInstanceWithOpts(t, "srtp-trunk-plain", withSRTP("disabled"))
		id := createSRTPTrunk(t, instA, "disabled", map[string]interface{}{"peer_uri": peerURI(instB.sipPort)})

		out, in := dialSRTP(t, instA, instB, map[string]interface{}{"trunk_id": id})
		assertDTMFBothWays(t, instA, out, instB, in, "12")
		// Re-INVITEs on the call keep the trunk's policy.
		hold := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out), nil)
		hold.Body.Close()
		instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)
		unhold := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out))
		unhold.Body.Close()
		instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)
		assertDTMFBothWays(t, instA, out, instB, in, "34")
		hangup(t, instA, out)
	})
}

// TestSRTP_TrunkInbound sends INVITEs from a source a trunk claims, so the
// trunk's srtp decides instead of SIP_SRTP.
func TestSRTP_TrunkInbound(t *testing.T) {
	key, err := sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_80)
	if err != nil {
		t.Fatal(err)
	}
	expect488 := func(t *testing.T, inst *testInstance, offer []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dialog := rawInvite(t, ctx, inst, offer)
		if code := responseCode(dialog.WaitAnswer(ctx, sipgo.AnswerOptions{})); code != sip.StatusNotAcceptableHere {
			t.Fatalf("got %d, want 488", code)
		}
	}
	claim := func(t *testing.T, inst *testInstance, srtp string) {
		createSRTPTrunk(t, inst, srtp, map[string]interface{}{"peer_uri": "sip:carrier.invalid", "inbound_sources": []string{"127.0.0.0/8"}})
	}

	t.Run("trunk requires SRTP", func(t *testing.T) {
		inst := newTestInstance(t, "srtp-trunk-in-req")
		claim(t, inst, "required")
		expect488(t, inst, srtpOffer(40002, "RTP/AVP"))
	})

	t.Run("trunk relaxes SIP_SRTP=required", func(t *testing.T) {
		inst := newTestInstanceWithOpts(t, "srtp-trunk-in-loose", withSRTP("required"))
		claim(t, inst, "disabled")
		expect488(t, inst, srtpOffer(40002, "RTP/SAVP", key))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dialog, inbound := answerInboundCall(t, ctx, inst,
			sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, srtpOffer(40004, "RTP/AVP"))
		body := string(dialog.InviteResponse.Body())
		if !strings.Contains(body, " RTP/AVP ") || strings.Contains(body, "a=crypto") {
			t.Errorf("plain offer through a disabled trunk was not answered plain:\n%s", body)
		}
		if err := dialog.Ack(ctx); err != nil {
			t.Fatal(err)
		}
		hangup(t, inst, inbound.ID)
	})

	t.Run("unclaimed calls keep SIP_SRTP", func(t *testing.T) {
		inst := newTestInstanceWithOpts(t, "srtp-trunk-in-other", withSRTP("required"))
		createSRTPTrunk(t, inst, "disabled", map[string]interface{}{"peer_uri": "sip:carrier.invalid", "inbound_sources": []string{"10.0.0.0/8"}})
		expect488(t, inst, srtpOffer(40002, "RTP/AVP"))
	})
}
