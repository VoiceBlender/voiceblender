//go:build integration && interop

package integration

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

func TestInterop_CodecSelect_AnswerWithExplicitCodec(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		offer := interopCodecs(p)
		for _, want := range offer {
			t.Run(want, func(t *testing.T) {
				call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: offer}, map[string]string{"codec": want})
				assertNegotiated(t, inst, legID, call, want)
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

func TestInterop_CodecSelect_AnswerRejectsCodecNotInOffer(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		call, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}})
		resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), lv.ID), map[string]string{"codec": "G722"})
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("answer with a codec the peer did not offer: status %d, want 400", resp.StatusCode)
		}
		answerOnVB(t, inst, lv.ID, nil)
		waitForLegState(t, inst.baseURL(), lv.ID, "connected", 5*time.Second)
		assertNegotiated(t, inst, lv.ID, call, "PCMU")
	})
}

// The peer re-INVITEs onto each codec in turn. VoiceBlender must switch the
// codec it decodes and the rate it runs at, not just agree in the SDP.
func TestInterop_CodecRenegotiation(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Reneg, "mid-call codec renegotiation")
		inst := newInteropVB(t, "vb", nil)
		codecs := interopCodecs(p)
		for _, setup := range []string{"vb-dials", "peer-dials"} {
			t.Run(setup, func(t *testing.T) {
				var legID string
				var call peers.Call
				if setup == "vb-dials" {
					legID, call = connectVBToPeer(t, inst, p, interopDial{Codecs: codecs}, peers.AnswerOpts{Codec: "PCMU"})
				} else {
					call, legID = connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: codecs}, map[string]string{"codec": "PCMU"})
				}
				assertNegotiated(t, inst, legID, call, "PCMU")
				for _, next := range []string{"G722", "OPUS", "PCMA", "PCMU"} {
					if !p.Caps().HasCodec(next) {
						continue
					}
					if err := call.Renegotiate(next, 0); err != nil {
						t.Fatalf("re-INVITE to %s: %v", next, err)
					}
					waitStreamCodec(t, inst, legID, next, 5*time.Second)
					waitPeerCodec(t, call, next, 5*time.Second)
					assertNegotiated(t, inst, legID, call, next)
					assertMediaBothWays(t, inst, legID, call)
				}
			})
		}
	})
}

func waitStreamCodec(t *testing.T, inst *testInstance, legID, codec string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got renegStreamView
	for time.Now().Before(deadline) {
		got = primaryStream(t, inst.baseURL(), legID)
		if strings.EqualFold(got.Codec, codec) && got.SampleRate == codecRate(codec) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("leg runs %s at %d Hz, want %s at %d Hz", got.Codec, got.SampleRate, codec, codecRate(codec))
}

// waitPeerCodec waits for the peer to finish switching codec, which lands a
// few frames after VoiceBlender has answered the re-offer.
func waitPeerCodec(t *testing.T, call peers.Call, codec string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.EqualFold(call.Codec(), codec) {
			time.Sleep(200 * time.Millisecond)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("peer runs %s, want %s", call.Codec(), codec)
}

// sdpOrigin returns the session id and version of an SDP's o= line.
func sdpOrigin(t *testing.T, sdp string) (id string, version uint64) {
	t.Helper()
	for _, line := range strings.Split(sdp, "\n") {
		if f := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "o="))); strings.HasPrefix(line, "o=") && len(f) >= 3 {
			v, err := strconv.ParseUint(f[2], 10, 64)
			if err != nil {
				t.Fatalf("bad o= version in %q", line)
			}
			return f[1], v
		}
	}
	t.Fatalf("no o= line in SDP:\n%s", sdp)
	return "", 0
}

// RFC 3264 section 8 / RFC 4566 section 5.2: every SDP VoiceBlender sends in
// a session keeps the o= session id, and the version goes up when the SDP
// changes. Peers that diff the version to decide whether media changed
// otherwise ignore or misapply a re-negotiation.
func TestInterop_SDPOriginStableAcrossReOffers(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Reneg, "mid-call codec renegotiation")
		inst := newInteropVB(t, "vb", nil)
		for _, setup := range []string{"vb-dials", "peer-dials"} {
			t.Run(setup, func(t *testing.T) {
				var legID string
				var call peers.Call
				if setup == "vb-dials" {
					legID, call = connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU", "G722"}}, peers.AnswerOpts{Codec: "PCMU"})
				} else {
					call, legID = connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU", "G722"}}, map[string]string{"codec": "PCMU"})
				}
				first := call.RemoteSDP()
				if first == "" {
					t.Skip("peer does not expose the SDP it received")
				}
				id0, v0 := sdpOrigin(t, first)
				if err := call.Renegotiate("G722", 0); err != nil {
					t.Fatal(err)
				}
				waitStreamCodec(t, inst, legID, "G722", 5*time.Second)
				waitPeerCodec(t, call, "G722", 5*time.Second)
				id1, v1 := sdpOrigin(t, call.RemoteSDP())
				t.Logf("o= before: %s %d, after re-offer: %s %d", id0, v0, id1, v1)
				if id1 != id0 {
					t.Errorf("o= session id changed within the session: %s -> %s", id0, id1)
				}
				if v1 <= v0 {
					t.Errorf("o= version did not increase for a changed SDP: %d -> %d", v0, v1)
				}
			})
		}
	})
}

func TestInterop_CustomHeaders(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		want := map[string]string{"X-Interop-Case": "MiXeD-Value", "X-Correlation-ID": "abc-123"}

		t.Run("vb-dials", func(t *testing.T) {
			_, call := vbDialsPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}, Headers: want})
			for k, v := range want {
				if got := call.Header(k); got != v {
					t.Errorf("peer got %s = %q, want %q", k, got, v)
				}
			}
		})
		t.Run("peer-dials", func(t *testing.T) {
			_, lv := peerDialsVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}, Headers: want})
			for k, v := range want {
				var got string
				for hk, hv := range lv.SIPHeaders {
					if strings.EqualFold(hk, k) {
						got = hv
					}
				}
				if got != v {
					t.Errorf("VoiceBlender got %s = %q, want %q (all: %v)", k, got, v, lv.SIPHeaders)
				}
			}
		})
	})
}

func TestInterop_Hold_PeerHolds(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Hold, "hold")
		inst := newInteropVB(t, "vb", nil)
		for _, setup := range interopCallSetups {
			t.Run(setup.name, func(t *testing.T) {
				legID, call := setup.connect(t, inst, p, []string{"PCMU"}, 0)
				if err := call.Hold(); err != nil {
					t.Fatal(err)
				}
				inst.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool { return e.Data.GetLegID() == legID }, 5*time.Second)
				if !getLeg(t, inst, legID).Held {
					t.Error("leg not marked held after the peer's hold re-INVITE")
				}
				if err := call.Unhold(); err != nil {
					t.Fatal(err)
				}
				inst.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool { return e.Data.GetLegID() == legID }, 5*time.Second)
				if getLeg(t, inst, legID).Held {
					t.Error("leg still held after the peer's unhold re-INVITE")
				}
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

func vbHold(t *testing.T, inst *testInstance, legID string, on bool) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), legID)
	resp := httpPost(t, url, nil)
	if !on {
		resp.Body.Close()
		resp = httpDelete(t, url)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("hold=%v: status %d", on, resp.StatusCode)
	}
}

func TestInterop_Hold_VBHolds(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Hold, "hold")
		inst := newInteropVB(t, "vb", nil)
		for _, setup := range interopCallSetups {
			t.Run(setup.name, func(t *testing.T) {
				legID, call := setup.connect(t, inst, p, []string{"PCMU"}, 0)
				isLeg := func(e events.Event) bool { return e.Data.GetLegID() == legID }

				vbHold(t, inst, legID, true)
				if err := call.WaitEvent(interopCtx(t, 5*time.Second), peers.EventHeld); err != nil {
					t.Fatalf("peer never saw the hold: %v", err)
				}
				inst.collector.waitForMatch(t, events.LegHold, isLeg, 5*time.Second)

				resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), legID))
				resp.Body.Close()
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("unhold: status %d", resp.StatusCode)
				}
				if err := call.WaitEvent(interopCtx(t, 5*time.Second), peers.EventUnheld); err != nil {
					t.Fatalf("peer never saw the unhold: %v", err)
				}
				inst.collector.waitForMatch(t, events.LegUnhold, isLeg, 5*time.Second)
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

// An unhold requested while the hold re-INVITE is still waiting for the
// peer's answer must still take effect: the API has already answered 202.
// A real endpoint takes long enough to answer for the two to overlap.
func TestInterop_Hold_VBUnholdWhileHoldPending(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().Hold, "hold")
		inst := newInteropVB(t, "vb", nil)
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})

		vbHold(t, inst, legID, true)
		if err := call.WaitEvent(interopCtx(t, 5*time.Second), peers.EventHeld); err != nil {
			t.Fatalf("peer never saw the hold: %v", err)
		}
		resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", inst.baseURL(), legID))
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("unhold: status %d", resp.StatusCode)
		}
		if err := call.WaitEvent(interopCtx(t, 5*time.Second), peers.EventUnheld); err != nil {
			t.Errorf("unhold accepted with 202 during the pending hold, but never sent: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		if getLeg(t, inst, legID).Held {
			t.Error("leg still held after an accepted unhold")
		}
	})
}

func getLeg(t *testing.T, inst *testInstance, legID string) legView {
	t.Helper()
	var lv legView
	decodeJSON(t, httpGet(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID)), &lv)
	return lv
}

func TestInterop_RTCP(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().RTCPStats, "RTCP statistics")
		inst := newInteropVB(t, "vb", nil)
		for _, setup := range interopCallSetups {
			t.Run(setup.name, func(t *testing.T) {
				legID, call := setup.connect(t, inst, p, []string{"PCMU"}, 0)
				playToneOnVB(t, inst, legID, 1100)
				call.PlayTone(700)
				if err := call.WaitEvent(interopCtx(t, 12*time.Second), peers.EventRTCP); err != nil {
					t.Errorf("peer received no RTCP from VoiceBlender: %v", err)
				}
				time.Sleep(6 * time.Second)
				hangupOnVB(t, inst, legID)
				o := outboundQuality(t, inst, legID)
				t.Logf("quality.outbound = %+v", o)
				if o == nil || o.PacketsSent == 0 {
					t.Errorf("VoiceBlender took no RTCP report from the peer: quality.outbound = %+v", o)
				}
			})
		}
	})
}

func TestInterop_RTCP_Disabled(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().RTCPStats, "RTCP statistics")
		inst := newInteropVB(t, "vb", func(c *config.Config) { c.SIPRTCP = false })
		legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{"PCMU"}}, peers.AnswerOpts{})
		playToneOnVB(t, inst, legID, 1100)
		time.Sleep(12 * time.Second)
		if st, _ := call.RTCPStats(); st.Reports > 0 {
			t.Errorf("SIP_RTCP off, yet the peer received %d RTCP reports", st.Reports)
		}
	})
}

// A session timer at the peer's minimum interval must be refreshed by
// whichever side is the refresher, or the call drops at expiry.
func TestInterop_SessionTimer_Survives(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past a 120 s session interval")
	}
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SessionTimers, "session timers")
		inst := newInteropVB(t, "vb", nil)
		cases := []struct {
			name    string
			connect func(t *testing.T) (string, peers.Call)
		}{
			{"vb-dials", func(t *testing.T) (string, peers.Call) {
				return connectVBToPeer(t, inst, p, interopDial{
					Codecs:  []string{"PCMU"},
					Headers: map[string]string{"Session-Expires": "120", "Min-SE": "120", "Supported": "timer"},
				}, peers.AnswerOpts{})
			}},
			{"peer-dials", func(t *testing.T) (string, peers.Call) {
				call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: []string{"PCMU"}, SessionTimer: 120}, nil)
				return legID, call
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				legID, call := tc.connect(t)
				start := time.Now()
				for time.Since(start) < 155*time.Second {
					if lv := getLeg(t, inst, legID); lv.State != "connected" {
						t.Fatalf("leg is %s %v into a 120 s session interval", lv.State, time.Since(start).Round(time.Second))
					}
					time.Sleep(time.Second)
				}
				if info, err := call.WaitHangup(interopCtx(t, 10*time.Millisecond)); err == nil {
					t.Fatalf("peer ended the call: %+v", info)
				}
				waitForLegState(t, inst.baseURL(), legID, "connected", time.Second)
				assertMediaBothWays(t, inst, legID, call)
			})
		}
	})
}

// RFC 4028 section 6: a 422 carries the peer's Min-SE, and the UAC retries
// with a Session-Expires at least that large instead of failing the call.
func TestInterop_SessionTimer_422Retry(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		requireCap(t, p.Caps().SessionTimers, "session timers")
		inst := newInteropVB(t, "vb", nil)
		legID, tag := vbDialsPeerNoWait(t, inst, p, interopDial{
			Codecs:  []string{"PCMU"},
			Headers: map[string]string{"Session-Expires": "90", "Min-SE": "90", "Supported": "timer"},
		})
		call, err := p.WaitInbound(interopCtx(t, 5*time.Second), tag)
		if err != nil {
			if d := waitDisconnect(t, inst, legID, time.Second); d != nil {
				t.Fatalf("call failed with reason %q instead of retrying after 422", d.CDR.Reason)
			}
			t.Fatal(err)
		}
		answerOnPeer(t, call, peers.AnswerOpts{})
		waitForLegState(t, inst.baseURL(), legID, "connected", 5*time.Second)
	})
}
