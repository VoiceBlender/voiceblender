//go:build integration && interop

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

func TestInterop_VBToPeer_VBHangsUp(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})

		hangupOnVB(t, inst, legID)
		if got := waitDisconnect(t, inst, legID, 5*time.Second).CDR.Reason; got != "api_hangup" {
			t.Errorf("VoiceBlender reason = %q, want api_hangup", got)
		}
		info := waitPeerHangup(t, call, 5*time.Second)
		if info.Cause != "NORMAL_CLEARING" {
			t.Errorf("peer cause = %s, want NORMAL_CLEARING (%+v)", info.Cause, info)
		}
	})
}

func TestInterop_VBToPeer_PeerHangsUp(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})

		if err := call.Hangup(); err != nil {
			t.Fatal(err)
		}
		if got := waitDisconnect(t, inst, legID, 5*time.Second).CDR.Reason; got != "remote_bye" {
			t.Errorf("VoiceBlender reason = %q, want remote_bye", got)
		}
		waitPeerHangup(t, call, 5*time.Second)
	})
}

func TestInterop_VBToPeer_CancelWhileRinging(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		legID, call := vbDialsPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}})
		inst.collector.waitForMatch(t, events.LegRinging, func(e events.Event) bool { return e.Data.GetLegID() == legID }, 5*time.Second)

		hangupOnVB(t, inst, legID)
		waitDisconnect(t, inst, legID, 5*time.Second)
		info := waitPeerHangup(t, call, 5*time.Second)
		if info.Cause != "ORIGINATOR_CANCEL" {
			t.Errorf("peer cause = %s, want ORIGINATOR_CANCEL (%+v)", info.Cause, info)
		}
	})
}

func TestInterop_VBToPeer_PeerRejects(t *testing.T) {
	cases := []struct {
		code   int
		reason string
	}{
		{486, "busy"},
		{603, "declined"},
		{404, "not_found"},
	}
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, tc := range cases {
			t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
				legID, call := vbDialsPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}})
				if err := call.Reject(tc.code); err != nil {
					t.Fatal(err)
				}
				if got := waitDisconnect(t, inst, legID, 5*time.Second).CDR.Reason; got != tc.reason {
					t.Errorf("VoiceBlender reason = %q, want %q", got, tc.reason)
				}
				if info := waitPeerHangup(t, call, 5*time.Second); info.SIPCode != tc.code {
					t.Errorf("peer sent SIP %d, want %d (%+v)", info.SIPCode, tc.code, info)
				}
			})
		}
	})
}

func TestInterop_VBToPeer_RingTimeout(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		legID, call := vbDialsPeer(t, inst, p, interopDial{
			Codecs: []string{"PCMU"},
			Extra:  map[string]interface{}{"ring_timeout": 2},
		})
		if got := waitDisconnect(t, inst, legID, 8*time.Second).CDR.Reason; got != "ring_timeout" {
			t.Errorf("VoiceBlender reason = %q, want ring_timeout", got)
		}
		if info := waitPeerHangup(t, call, 5*time.Second); info.Cause != "ORIGINATOR_CANCEL" {
			t.Errorf("peer cause = %s, want ORIGINATOR_CANCEL (%+v)", info.Cause, info)
		}
	})
}

func TestInterop_VBToPeer_EarlyMedia(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().EarlyMedia, "early media")
		inst := newInteropVB(t, "vb", nil)
		legID, call := vbDialsPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}})

		if err := call.EarlyMedia(700); err != nil {
			t.Fatal(err)
		}
		waitForLegState(t, inst.baseURL(), legID, "early_media", 5*time.Second)
		inst.collector.waitForMatch(t, events.LegEarlyMedia, func(e events.Event) bool { return e.Data.GetLegID() == legID }, 2*time.Second)
		assertVBHearsTone(t, inst, legID, 700)

		answerOnPeer(t, call, peers.AnswerOpts{})
		waitForLegState(t, inst.baseURL(), legID, "connected", 5*time.Second)
		call.StopMedia()
		assertMediaBothWays(t, inst, legID, call)
	})
}

func TestInterop_PeerToVB_AnswerAndVBHangsUp(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}}, nil)
		assertMediaBothWays(t, inst, legID, call)

		hangupOnVB(t, inst, legID)
		waitDisconnect(t, inst, legID, 5*time.Second)
		if info := waitPeerHangup(t, call, 5*time.Second); info.Cause != "NORMAL_CLEARING" {
			t.Errorf("peer cause = %s, want NORMAL_CLEARING (%+v)", info.Cause, info)
		}
	})
}

func TestInterop_PeerToVB_PeerHangsUp(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}}, nil)
		if err := call.Hangup(); err != nil {
			t.Fatal(err)
		}
		if got := waitDisconnect(t, inst, legID, 5*time.Second).CDR.Reason; got != "remote_bye" {
			t.Errorf("VoiceBlender reason = %q, want remote_bye", got)
		}
	})
}

func TestInterop_PeerToVB_VBRejects(t *testing.T) {
	cases := []struct {
		reason string
		code   int
	}{
		{"busy", 486},
		{"declined", 603},
		{"unavailable", 480},
	}
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, tc := range cases {
			t.Run(tc.reason, func(t *testing.T) {
				call, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}})
				resp := httpDeleteWithBody(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), lv.ID), map[string]string{"reason": tc.reason})
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("reject: status %d", resp.StatusCode)
				}
				if info := waitPeerHangup(t, call, 5*time.Second); info.SIPCode != tc.code {
					t.Errorf("peer got SIP %d, want %d (%+v)", info.SIPCode, tc.code, info)
				}
			})
		}
	})
}

func TestInterop_PeerToVB_PeerCancels(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		call, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}})
		if err := call.Hangup(); err != nil {
			t.Fatal(err)
		}
		if got := waitDisconnect(t, inst, lv.ID, 5*time.Second).CDR.Reason; got != "caller_cancel" {
			t.Errorf("VoiceBlender reason = %q, want caller_cancel", got)
		}
	})
}
