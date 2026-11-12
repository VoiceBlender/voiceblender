//go:build integration && interop

package integration

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

func boolPtr(b bool) *bool { return &b }

func connectSRTP(t *testing.T, inst *testInstance, p peers.Peer, dir string, tls bool) (string, peers.Call) {
	t.Helper()
	if dir == "vb-dials" {
		return connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}, SRTP: boolPtr(true), TLS: tls}, peers.AnswerOpts{})
	}
	call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}, SRTP: true, TLS: tls}, nil)
	return legID, call
}

func assertPeerSRTP(t *testing.T, call peers.Call) {
	t.Helper()
	if !call.SRTP() {
		t.Errorf("peer is not running SRTP (rtp_has_crypto=%q)", call.Var("rtp_has_crypto"))
	}
}

func TestInterop_SRTP(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SRTP, "SDES-SRTP")
		inst := newInteropVB(t, "vb", nil)
		for _, dir := range []string{"vb-dials", "peer-dials"} {
			t.Run(dir, func(t *testing.T) {
				legID, call := connectSRTP(t, inst, p, dir, false)
				assertPeerSRTP(t, call)
				assertMediaBothWays(t, inst, legID, call)
				assertDTMFBothWaysInterop(t, inst, legID, call)
			})
		}
	})
}

func TestInterop_SRTP_KeptAcrossHold(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SRTP && p.Caps().Hold, "SDES-SRTP and hold")
		inst := newInteropVB(t, "vb", nil)
		legID, call := connectSRTP(t, inst, p, "vb-dials", false)
		isLeg := func(e events.Event) bool { return e.Data.GetLegID() == legID }

		vbHold(t, inst, legID, true)
		inst.collector.waitForMatch(t, events.LegHold, isLeg, 5*time.Second)
		vbHold(t, inst, legID, false)
		inst.collector.waitForMatch(t, events.LegUnhold, isLeg, 5*time.Second)
		if err := call.WaitEvent(interopCtx(t, 5*time.Second), peers.EventUnheld); err != nil {
			t.Fatalf("peer never saw the unhold: %v", err)
		}
		assertPeerSRTP(t, call)
		assertMediaBothWays(t, inst, legID, call)
	})
}

// An SRTP call stays SRTP for its lifetime: a plain RTP re-offer is refused
// and the call carries on encrypted.
func TestInterop_SRTP_PlainReOfferRefused(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SRTP && p.Caps().Reneg, "SDES-SRTP and re-offers")
		inst := newInteropVB(t, "vb", nil)
		for _, dir := range []string{"vb-dials", "peer-dials"} {
			t.Run(dir, func(t *testing.T) {
				legID, call := connectSRTP(t, inst, p, dir, false)
				if err := call.DropSRTP(); errors.Is(err, peers.ErrUnsupported) {
					t.Skip("peer cannot re-offer plain RTP on an SRTP call")
				} else if err != nil {
					t.Logf("peer reports the re-offer failed: %v", err)
				}
				time.Sleep(time.Second)
				waitForLegState(t, inst.baseURL(), legID, "connected", time.Second)
				assertPeerSRTP(t, call)
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

// VoiceBlender offers SRTP only; a peer that will not do SRTP must not end
// up in a plain call.
func TestInterop_SRTP_PeerRefusesSRTP(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SRTP, "SDES-SRTP")
		inst := newInteropVB(t, "vb", nil)
		legID, call := vbDialsPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}, SRTP: boolPtr(true)})
		if err := call.Answer(peers.AnswerOpts{NoSRTP: true}); err != nil {
			t.Logf("peer answer: %v", err)
		}
		d := waitDisconnect(t, inst, legID, 5*time.Second)
		t.Logf("VoiceBlender reason %q, peer %+v", d.CDR.Reason, waitPeerHangup(t, call, 5*time.Second))
		if d.CDR.Reason == "api_hangup" || d.CDR.Reason == "remote_bye" {
			t.Errorf("reason %q: the call should have been refused, not connected", d.CDR.Reason)
		}
	})
}

func TestInterop_TLS_VBToPeer(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().TLS, "SIP over TLS")
		inst, _ := newInteropVBTLS(t, "vb")
		for _, srtp := range []bool{false, true} {
			name := "rtp"
			if srtp {
				name = "srtp"
			}
			t.Run(name, func(t *testing.T) {
				legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}, TLS: true, SRTP: boolPtr(srtp)}, peers.AnswerOpts{})
				if got := call.Transport(); got != "tls" {
					t.Errorf("call arrived over %q, want tls", got)
				}
				if srtp {
					assertPeerSRTP(t, call)
				}
				assertMediaBothWays(t, inst, legID, call)
				hangupOnVB(t, inst, legID)
				if info := waitPeerHangup(t, call, 5*time.Second); info.Cause != "NORMAL_CLEARING" {
					t.Errorf("BYE over TLS: peer cause %s", info.Cause)
				}
			})
		}
	})
}

func TestInterop_TLS_PeerToVB(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().TLS, "SIP over TLS")
		inst, tlsPort := newInteropVBTLS(t, "vb")
		for _, srtp := range []bool{false, true} {
			name := "rtp"
			if srtp {
				name = "srtp"
			}
			t.Run(name, func(t *testing.T) {
				call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{
					To:     fmt.Sprintf("sips:interop@127.0.0.1:%d", tlsPort),
					TLS:    true,
					SRTP:   srtp,
					Codecs: []string{"PCMU"},
				}, nil)
				if got := call.Transport(); got != "tls" {
					t.Errorf("call placed over %q, want tls", got)
				}
				if srtp {
					assertPeerSRTP(t, call)
				}
				assertMediaBothWays(t, inst, legID, call)
				if err := call.Hangup(); err != nil {
					t.Fatal(err)
				}
				if got := waitDisconnect(t, inst, legID, 5*time.Second).CDR.Reason; got != "remote_bye" {
					t.Errorf("BYE over TLS: reason %q, want remote_bye", got)
				}
			})
		}
	})
}

// A TLS INVITE answered with a Contact that names another transport: the
// dialog's later requests (ACK, BYE) follow the Contact (RFC 3261 §12.2.1.1).
// Without its own TLS listener VoiceBlender advertises a UDP Contact, and the
// peer answers in kind.
func TestInterop_TLS_RemoteTargetTransport(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().TLS, "SIP over TLS")
		inst := newInteropVB(t, "vb", nil)
		to := strings.Replace(p.URI("interop", peers.URIOpts{TLS: true}), "sips:", "sip:", 1)
		if !strings.Contains(to, "transport=") {
			to += ";transport=tls"
		}
		legID, call := connectVBToPeer(t, inst, p, interopDial{To: to, Codecs: []string{"PCMU"}}, peers.AnswerOpts{})
		assertMediaBothWays(t, inst, legID, call)
		hangupOnVB(t, inst, legID)
		if info := waitPeerHangup(t, call, 5*time.Second); info.Cause != "NORMAL_CLEARING" {
			t.Errorf("peer cause %s after VoiceBlender's BYE", info.Cause)
		}
	})
}
