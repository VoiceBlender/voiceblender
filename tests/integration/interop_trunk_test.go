//go:build integration && interop

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

// peerHostURI is the peer's plain listener without a user part.
func peerHostURI(p peers.Peer) string {
	return strings.Replace(p.URI("x", peers.URIOpts{}), "x@", "", 1)
}

func peerUser(p peers.Peer, n int) (string, string) {
	user := fmt.Sprint(1000 + n)
	return user, p.Registrar().Users[user]
}

func createRegisterTrunk(t *testing.T, inst *testInstance, p peers.Peer, user, password string) string {
	t.Helper()
	ri := p.Registrar()
	resp, body := createTrunkRequest(t, inst.baseURL(), map[string]interface{}{
		"type": "sip_register",
		"sip_register": map[string]interface{}{
			"registrar_uri":   "sip:" + ri.HostPort,
			"aor":             fmt.Sprintf("sip:%s@%s", user, ri.Domain),
			"password":        password,
			"expires_seconds": 120,
		},
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create sip_register trunk: status %d, body=%s", resp.StatusCode, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { httpDelete(t, inst.baseURL()+"/v1/sip/trunks/"+created.ID).Body.Close() })
	return created.ID
}

func TestInterop_Trunk_RegisterToPeer(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Registrar, "acting as registrar")
		inst := newInteropVB(t, "vb", nil)
		user, pass := peerUser(p, 1)
		id := createRegisterTrunk(t, inst, p, user, pass)
		inst.collector.waitForMatch(t, events.SIPOutboundRegistrationActive, func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationActiveData).TrunkID == id
		}, 10*time.Second)
		contact, err := p.WaitRegistered(interopCtx(t, 5*time.Second), user)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("peer holds contact %s", contact)

		t.Run("outbound-through-trunk", func(t *testing.T) {
			legID, call := connectVBToPeer(t, inst, p, interopDial{
				Auth:   true,
				Codecs: []string{"PCMU"},
				Extra:  map[string]interface{}{"trunk_id": id},
			}, peers.AnswerOpts{})
			if got := call.Var("sip_from_user"); got != user {
				t.Errorf("From user %q, want the trunk's %q", got, user)
			}
			assertMediaBothWays(t, inst, legID, call)
		})

		t.Run("unregister", func(t *testing.T) {
			resp := httpDelete(t, inst.baseURL()+"/v1/sip/trunks/"+id)
			resp.Body.Close()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := p.WaitRegistered(interopCtx(t, 100*time.Millisecond), user); err != nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
			t.Error("peer still holds a binding after the trunk was deleted")
		})
	})
}

func TestInterop_Trunk_RegisterWrongPassword(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Registrar, "acting as registrar")
		inst := newInteropVB(t, "vb", nil)
		user, _ := peerUser(p, 2)
		id := createRegisterTrunk(t, inst, p, user, "wrong")
		ev := inst.collector.waitForMatch(t, events.SIPOutboundRegistrationFailed, func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationFailedData).TrunkID == id
		}, 15*time.Second)
		t.Logf("failure: %+v", ev.Data)
		if _, err := p.WaitRegistered(interopCtx(t, 500*time.Millisecond), user); err == nil {
			t.Error("peer registered a binding for a wrong password")
		}
	})
}

// A REFER on a call placed through a registered trunk sends the transfer
// INVITE through that trunk too, under its credentials: the target challenges
// and only the trunk's account gets through.
func TestInterop_Trunk_ReferInheritsTrunk(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Registrar && p.Caps().Refer, "acting as registrar and REFER")
		inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPReferAutoDial = true })
		user, pass := peerUser(p, 3)
		id := createRegisterTrunk(t, inst, p, user, pass)
		inst.collector.waitForMatch(t, events.SIPOutboundRegistrationActive, func(e events.Event) bool {
			return e.Data.(*events.SIPOutboundRegistrationActiveData).TrunkID == id
		}, 10*time.Second)

		legID, call := connectVBToPeer(t, inst, p, interopDial{Auth: true, Codecs: []string{"PCMU"}, Extra: map[string]interface{}{"trunk_id": id}}, peers.AnswerOpts{})
		target := p.ToneURI(900)
		authTarget := fmt.Sprintf("sip:tone-900@%s", p.Registrar().HostPort)
		if authTarget == target {
			t.Skip("peer has no challenging listener distinct from its plain one")
		}
		if err := call.Refer(authTarget); err != nil {
			t.Fatal(err)
		}
		newLeg := waitNewConnectedLeg(t, inst, legID, 10*time.Second)
		assertVBHearsTone(t, inst, newLeg, 900)
	})
}

func TestInterop_Trunk_IPIP(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Registrar, "challenging INVITEs")
		ri := p.Registrar()

		t.Run("outbound-digest", func(t *testing.T) {
			inst := newInteropVB(t, "vb", nil)
			user, pass := peerUser(p, 4)
			id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
				"peer_uri": "sip:" + ri.HostPort,
				"username": user,
				"password": pass,
			})
			legID, call := connectVBToPeer(t, inst, p, interopDial{Auth: true, Codecs: []string{"PCMU"}, Extra: map[string]interface{}{"trunk_id": id}}, peers.AnswerOpts{})
			assertMediaBothWays(t, inst, legID, call)
		})

		for _, tc := range []struct {
			name string
			spec map[string]interface{}
		}{
			{"inbound-by-peer-socket", map[string]interface{}{"peer_uri": peerHostURI(p)}},
			{"inbound-by-cidr", map[string]interface{}{"peer_uri": "sip:carrier.invalid", "inbound_sources": []string{"127.0.0.0/8"}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inst := newInteropVB(t, "vb", nil)
				id := createIPIPTrunk(t, inst.baseURL(), "acme", tc.spec)
				call, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}})
				ev := inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool { return e.Data.GetLegID() == lv.ID }, 5*time.Second)
				d := ev.Data.(*events.LegRingingData)
				if d.TrunkID != id || d.AppID != "acme" {
					t.Errorf("inbound call claimed by trunk %q app %q, want %q / acme", d.TrunkID, d.AppID, id)
				}
				answerOnVB(t, inst, lv.ID, nil)
				if err := call.WaitAnswered(interopCtx(t, 5*time.Second)); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}

func TestInterop_Trunk_OptionsPing(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Flap, "a listener that can be stopped")
		inst := newInteropVB(t, "vb", nil)
		id := createIPIPTrunk(t, inst.baseURL(), "", map[string]interface{}{
			"peer_uri":                      "sip:" + p.Registrar().FlapHostPort,
			"options_ping_interval_seconds": 1,
		})
		t.Cleanup(func() { p.StartFlap() })

		up := waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 1, 10*time.Second)
		t.Logf("up: %+v", up)
		if err := p.StopFlap(); err != nil {
			t.Fatal(err)
		}
		down := waitForTrunkEvents(t, inst, events.SIPTrunkDown, id, 1, 40*time.Second)
		t.Logf("down: %+v", down)
		if err := p.StartFlap(); err != nil {
			t.Fatal(err)
		}
		waitForTrunkEvents(t, inst, events.SIPTrunkUp, id, 2, 40*time.Second)
	})
}

// A trunk that requires SRTP carries SRTP both ways with the peer and refuses
// a plain call from it.
func TestInterop_Trunk_SRTPRequired(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SRTP, "SDES-SRTP")
		inst := newInteropVB(t, "vb", nil)
		id := createSRTPTrunk(t, inst, "required", map[string]interface{}{"peer_uri": peerHostURI(p)})

		t.Run("outbound", func(t *testing.T) {
			legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}, Extra: map[string]interface{}{"trunk_id": id}}, peers.AnswerOpts{})
			assertPeerSRTP(t, call)
			assertMediaBothWays(t, inst, legID, call)
		})
		t.Run("inbound-srtp", func(t *testing.T) {
			call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}, SRTP: true}, nil)
			assertPeerSRTP(t, call)
			assertMediaBothWays(t, inst, legID, call)
		})
		t.Run("inbound-plain-refused", func(t *testing.T) {
			tag := newInteropTag()
			t.Cleanup(func() { p.HangupAll(tag) })
			call, err := p.Originate(interopCtx(t, 5*time.Second), peers.OriginateOpts{
				To:     fmt.Sprintf("sip:interop@127.0.0.1:%d", inst.sipPort),
				Tag:    tag,
				Codecs: []string{"PCMU"},
			})
			if err != nil {
				t.Fatal(err)
			}
			info := waitPeerHangup(t, call, 10*time.Second)
			if info.SIPCode != 488 {
				t.Errorf("plain offer on an SRTP-required trunk: peer got %+v, want 488", info)
			}
		})
	})
}
