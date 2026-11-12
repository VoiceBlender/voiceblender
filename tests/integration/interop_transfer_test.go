//go:build integration && interop

package integration

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

// assertReferSucceeded checks the peer saw the transfer succeed, where it can
// tell.
func assertReferSucceeded(t *testing.T, call peers.Call) {
	t.Helper()
	code, err := call.ReferStatus(interopCtx(t, 5*time.Second))
	switch {
	case errors.Is(err, peers.ErrUnsupported):
		t.Log("peer cannot see the NOTIFY outcome; relying on VoiceBlender's side")
	case err != nil:
		t.Errorf("peer got no outcome for its REFER: %v", err)
	case code != 200:
		t.Errorf("REFER outcome %d, want 200", code)
	}
}

// bridgedUser is the user part of p.BridgedToneURI, for dialling it with the
// interop tag header.
func bridgedUser(p peers.Peer) string {
	u := strings.TrimPrefix(p.BridgedToneURI(500), "sip:")
	user, _, _ := strings.Cut(u, "@")
	return user
}

// waitNewConnectedLeg returns the first leg other than exclude to connect.
func waitNewConnectedLeg(t *testing.T, inst *testInstance, exclude string, timeout time.Duration) string {
	t.Helper()
	seen := map[string]bool{exclude: true}
	for _, e := range inst.collector.matchAll(events.LegConnected, nil) {
		seen[e.Data.GetLegID()] = true
	}
	e := inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return !seen[e.Data.GetLegID()]
	}, timeout)
	return e.Data.GetLegID()
}

// The peer blind-transfers VoiceBlender to a target that plays a tone;
// VoiceBlender dials it, reports success in NOTIFY and the new leg carries
// the target's audio.
func TestInterop_Transfer_PeerRefersVB_AutoDial(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Refer, "REFER")
		inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPReferAutoDial = true })
		for _, setup := range interopCallSetups {
			t.Run(setup.name, func(t *testing.T) {
				legID, call := setup.connect(t, inst, p, []string{"PCMU"}, 0)
				if err := call.Refer(p.ToneURI(900)); err != nil {
					t.Fatal(err)
				}
				newLeg := waitNewConnectedLeg(t, inst, legID, 10*time.Second)
				assertVBHearsTone(t, inst, newLeg, 900)

				assertReferSucceeded(t, call)
				waitDisconnect(t, inst, legID, 5*time.Second)
				hangupOnVB(t, inst, newLeg)
			})
		}
	})
}

// Headers embedded in the Refer-To URI reach the INVITE VoiceBlender sends
// to the target, and a BYE from the target ends the transferred leg.
func TestInterop_Transfer_PeerRefersVB_ReferToHeadersAndRemoteBye(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Refer, "REFER")
		inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPReferAutoDial = true })
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})

		tag := newInteropTag()
		t.Cleanup(func() { p.HangupAll(tag) })
		target := p.URI("transfer-target", peers.URIOpts{}) + "?" + peers.TagHeader + "=" + tag
		if err := call.Refer(target); err != nil {
			t.Fatal(err)
		}
		target2, err := p.WaitInbound(interopCtx(t, 10*time.Second), tag)
		if err != nil {
			t.Fatalf("VoiceBlender's INVITE to the target did not carry the Refer-To header: %v", err)
		}
		answerOnPeer(t, target2, peers.AnswerOpts{})
		newLeg := waitNewConnectedLeg(t, inst, legID, 10*time.Second)
		assertMediaBothWays(t, inst, newLeg, target2)

		if err := target2.Hangup(); err != nil {
			t.Fatal(err)
		}
		if got := waitDisconnect(t, inst, newLeg, 5*time.Second).CDR.Reason; got != "remote_bye" {
			t.Errorf("transferred leg reason %q, want remote_bye", got)
		}
	})
}

func transferAppVB(t *testing.T) *testInstance {
	return newInteropVB(t, "vb", func(c *config.Config) { c.SIPReferConsultTimeoutMs = 4000 })
}

func waitTransferRequested(t *testing.T, inst *testInstance, legID string) {
	t.Helper()
	inst.collector.waitForMatch(t, events.LegTransferRequested, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
}

func postTransferAction(t *testing.T, inst *testInstance, legID, action string, body interface{}) {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer/%s", inst.baseURL(), legID, action), body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("transfer/%s: status %d: %s", action, resp.StatusCode, b)
	}
}

func TestInterop_Transfer_PeerRefersVB_AppAccepts(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Refer, "REFER")
		inst := transferAppVB(t)
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})
		if err := call.Refer(p.ToneURI(900)); err != nil {
			t.Fatal(err)
		}
		waitTransferRequested(t, inst, legID)
		postTransferAction(t, inst, legID, "accept", nil)
		postTransferAction(t, inst, legID, "complete", map[string]interface{}{"success": true})
		assertReferSucceeded(t, call)
	})
}

func TestInterop_Transfer_PeerRefersVB_Declined(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Refer, "REFER")
		cases := []struct {
			name    string
			decline func(t *testing.T, inst *testInstance, legID string)
		}{
			{"app-declines", func(t *testing.T, inst *testInstance, legID string) {
				time.Sleep(time.Second)
				postTransferAction(t, inst, legID, "decline", nil)
			}},
			{"consult-timeout", func(t *testing.T, inst *testInstance, legID string) {}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Long enough for a UDP peer to retransmit the unanswered REFER.
				inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPReferConsultTimeoutMs = 3000 })
				legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})
				if err := call.Refer(p.ToneURI(900)); err != nil {
					t.Logf("peer reports the REFER failed: %v", err)
				}
				waitTransferRequested(t, inst, legID)
				tc.decline(t, inst, legID)
				time.Sleep(4 * time.Second)
				if n := len(inst.collector.matchAll(events.LegTransferRequested, func(e events.Event) bool { return e.Data.GetLegID() == legID })); n != 1 {
					t.Errorf("one REFER produced %d leg.transfer_requested events; its retransmissions were treated as new REFERs", n)
				}
				code, _ := call.ReferStatus(interopCtx(t, time.Second))
				t.Logf("peer refer status %d", code)
				if code >= 200 && code < 300 {
					t.Errorf("declined transfer reported %d to the peer", code)
				}
				waitForLegState(t, inst.baseURL(), legID, "connected", time.Second)
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

// VoiceBlender refers the peer; the peer dials the target and reports the
// outcome in NOTIFY, which VoiceBlender turns into transfer events.
func TestInterop_Transfer_VBRefersPeer(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Refer, "REFER")
		inst := newInteropVB(t, "vb", nil)
		for _, setup := range interopCallSetups {
			t.Run(setup.name, func(t *testing.T) {
				var legID string
				if setup.name == "vb-dials" {
					legID, _ = vbDialsPeer(t, inst, p, interopDial{User: bridgedUser(p), Codecs: []string{"PCMU"}})
					waitForLegState(t, inst.baseURL(), legID, "connected", 10*time.Second)
				} else {
					t.Skip("the peer needs a bridged call to act on a REFER, which it only sets up for calls it receives")
				}
				resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/transfer", inst.baseURL(), legID), map[string]interface{}{
					"target":  p.ToneURI(900),
					"headers": map[string]string{"X-Interop-Case": "refer"},
				})
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("transfer: status %d", resp.StatusCode)
				}
				isLeg := func(e events.Event) bool { return e.Data.GetLegID() == legID }
				inst.collector.waitForMatch(t, events.LegTransferInitiated, isLeg, 5*time.Second)
				done := inst.collector.waitForMatch(t, events.LegTransferCompleted, isLeg, 10*time.Second)
				if code := done.Data.(*events.LegTransferCompletedData).StatusCode; code != 200 {
					t.Errorf("transfer completed with %d, want 200", code)
				}

			})
		}
	})
}
