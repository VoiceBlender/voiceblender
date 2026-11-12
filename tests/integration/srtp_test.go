//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

func withSRTP(mode string) func(*config.Config) {
	return func(c *config.Config) { c.SIPSRTP = mode }
}

func srtpOffer(port int, proto string, crypto ...sipmod.SDESCrypto) []byte {
	lines := []string{
		"v=0",
		"o=raw 1 1 IN IP4 127.0.0.1",
		"s=-",
		"c=IN IP4 127.0.0.1",
		"t=0 0",
		"m=audio " + strconv.Itoa(port) + " " + proto + " 0 101",
		"a=rtpmap:0 PCMU/8000",
		"a=rtpmap:101 telephone-event/8000",
		"a=fmtp:101 0-15",
		"a=sendrecv",
	}
	for _, c := range crypto {
		lines = append(lines, "a=crypto:"+c.AttrValue())
	}
	return []byte(strings.Join(append(lines, ""), "\r\n"))
}

// rawInvite sends offer to inst from a bare SIP client and returns the dialog
// with the INVITE outstanding.
func rawInvite(t *testing.T, ctx context.Context, inst *testInstance, offer []byte) *sipgo.DialogClientSession {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgentHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new UA: %v", err)
	}
	t.Cleanup(func() { ua.Close() })
	client, err := sipgo.NewClient(ua, sipgo.WithClientHostname("127.0.0.1"))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	dua := sipgo.DialogUA{
		Client:     client,
		ContactHDR: sip.ContactHeader{Address: sip.Uri{Scheme: "sip", User: "caller", Host: "127.0.0.1", Port: 5060}},
	}
	recipient := sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}
	dialog, err := dua.Invite(ctx, recipient, offer, sip.NewHeader("Content-Type", "application/sdp"))
	if err != nil {
		t.Fatalf("INVITE: %v", err)
	}
	return dialog
}

// TestSRTP_RawPeerEncryptedMedia answers an SDES offer from a bare SIP peer
// that holds its own keys, then proves both media directions: the peer can
// authenticate and decrypt what VoiceBlender sends under the key it answered,
// and DTMF the peer sends encrypted under its own key is decoded.
func TestSRTP_RawPeerEncryptedMedia(t *testing.T) {
	inst := newTestInstance(t, "srtp-raw")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	peer, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerKey, err := sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_80)
	if err != nil {
		t.Fatal(err)
	}

	dialog, inbound := answerInboundCall(t, ctx, inst,
		sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort},
		srtpOffer(peer.LocalPort(), "RTP/SAVP", peerKey))
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse answer: %v", err)
	}
	ra := answer.PrimaryStream()
	if strings.Join(ra.Proto, "/") != "RTP/SAVP" || len(ra.Crypto) != 1 {
		t.Fatalf("answer is not SDES-SRTP:\n%s", dialog.InviteResponse.Body())
	}
	vbKey := ra.Crypto[0]
	if vbKey.Tag != 1 || vbKey.Suite != sipmod.SuiteAES128CMSHA1_80 || vbKey.SameKey(&peerKey) {
		t.Fatalf("answer crypto = %+v", vbKey)
	}
	if err := dialog.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)

	if err := peer.SetSRTP(&peerKey, &vbKey); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
		t.Fatal(err)
	}

	// VoiceBlender → peer: its writeLoop sends continuously; at least one
	// packet must authenticate under the answered key.
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pkt rtp.Packet
	for {
		err := peer.ReadRTPInto(&pkt)
		if err == nil {
			break
		}
		if !errors.Is(err, sipmod.ErrNotRTP) {
			t.Fatalf("no media from VoiceBlender authenticated under the answered key: %v", err)
		}
	}

	// Peer → VoiceBlender: DTMF encrypted under the peer's key.
	for _, p := range sipmod.GenerateDTMFPackets('7', 101, 0x1234, 100, 8000, 160) {
		if err := peer.WriteRTP(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForDTMF(t, inst, inbound.ID, "7")

	hangup(t, inst, inbound.ID)
}

// TestSRTP_RejectedOffers covers the up-front 488 for offers no audio section
// of which the SRTP policy allows.
func TestSRTP_RejectedOffers(t *testing.T) {
	key, err := sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_80)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		mode  string
		offer []byte
	}{
		{"plain offer under required", "required", srtpOffer(40002, "RTP/AVP")},
		{"SDES offer under disabled", "disabled", srtpOffer(40002, "RTP/SAVP", key)},
		{"SAVP without crypto", "optional", srtpOffer(40002, "RTP/SAVP")},
		{"DTLS-SRTP on a classic leg", "optional", srtpOffer(40002, "UDP/TLS/RTP/SAVPF")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newTestInstanceWithOpts(t, "srtp-reject", withSRTP(tc.mode))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			dialog := rawInvite(t, ctx, inst, tc.offer)
			err := dialog.WaitAnswer(ctx, sipgo.AnswerOptions{})
			if code := responseCode(err); code != sip.StatusNotAcceptableHere {
				t.Fatalf("WaitAnswer err = %v, want 488", err)
			}
			if legs := listLegs(t, inst); len(legs) != 0 {
				t.Errorf("a rejected INVITE surfaced %d legs", len(legs))
			}
		})
	}
}

// responseCode extracts the final status from a failed INVITE, which sipgo
// returns as either an ErrDialogResponse value or a pointer to one.
func responseCode(err error) int {
	var p *sipgo.ErrDialogResponse
	if errors.As(err, &p) {
		return int(p.Res.StatusCode)
	}
	var v sipgo.ErrDialogResponse
	if errors.As(err, &v) {
		return int(v.Res.StatusCode)
	}
	return 0
}

func listLegs(t *testing.T, inst *testInstance) []legView {
	t.Helper()
	resp := httpGet(t, inst.baseURL()+"/v1/legs")
	var legs []legView
	decodeJSON(t, resp, &legs)
	return legs
}

// dialSRTP places a call from a to b with the given create-leg extras, answers
// it on b, and returns both leg IDs once connected.
func dialSRTP(t *testing.T, a, b *testInstance, extra map[string]interface{}) (out, in string) {
	t.Helper()
	body := map[string]interface{}{
		"type":   "sip",
		"to":     fmt.Sprintf("sip:test@127.0.0.1:%d", b.sipPort),
		"codecs": []string{"PCMU"},
	}
	for k, v := range extra {
		body[k] = v
	}
	resp := httpPost(t, a.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("create leg: status %d: %s", resp.StatusCode, msg)
	}
	var outbound legView
	decodeJSON(t, resp, &outbound)

	inbound := waitForInboundLeg(t, b.baseURL(), 5*time.Second)
	ans := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", b.baseURL(), inbound.ID), nil)
	ans.Body.Close()
	waitForLegState(t, a.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, b.baseURL(), inbound.ID, "connected", 5*time.Second)
	return outbound.ID, inbound.ID
}

// assertDTMFBothWays proves media flows in each direction of the call.
func assertDTMFBothWays(t *testing.T, a *testInstance, aLeg string, b *testInstance, bLeg string, digits string) {
	t.Helper()
	sendDTMFFrom(t, a, aLeg, digits[:1])
	waitForDTMF(t, b, bLeg, digits[:1])
	sendDTMFFrom(t, b, bLeg, digits[1:2])
	waitForDTMF(t, a, aLeg, digits[1:2])
}

// TestSRTP_InstanceToInstance runs a whole SDES-SRTP call between two
// instances: the INVITE, a hold/unhold re-INVITE pair and an added stream all
// keep SRTP, and media flows both ways throughout. An answer without an
// accepted key would have failed the call, so a connected leg carrying DTMF
// is an encrypted one.
func TestSRTP_InstanceToInstance(t *testing.T) {
	cases := []struct {
		name   string
		modeA  string
		extraA map[string]interface{}
	}{
		{"leg opts in", "optional", map[string]interface{}{"srtp": true}},
		{"server requires it", "required", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstanceWithOpts(t, "srtp-a", withSRTP(tc.modeA))
			instB := newTestInstance(t, "srtp-b")

			out, in := dialSRTP(t, instA, instB, tc.extraA)
			assertDTMFBothWays(t, instA, out, instB, in, "12")

			hold := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out), nil)
			hold.Body.Close()
			instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)
			unhold := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out))
			unhold.Body.Close()
			instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)
			assertDTMFBothWays(t, instA, out, instB, in, "34")

			add := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), out), map[string]interface{}{"direction": "sendonly"})
			if add.StatusCode != http.StatusCreated {
				msg, _ := io.ReadAll(add.Body)
				t.Fatalf("add stream on an SRTP call: status %d: %s", add.StatusCode, msg)
			}
			add.Body.Close()
			assertDTMFBothWays(t, instA, out, instB, in, "56")

			hangup(t, instA, out)
		})
	}
}

// TestSRTP_OutboundOfferRejected dials an SRTP-only offer at a peer that has
// SRTP disabled: the call must fail rather than fall back to plain RTP.
func TestSRTP_OutboundOfferRejected(t *testing.T) {
	instA := newTestInstance(t, "srtp-offer")
	instB := newTestInstanceWithOpts(t, "srtp-off", withSRTP("disabled"))

	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip",
		"to":   fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"srtp": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, resp, &outbound)

	instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == outbound.ID
	}, 5*time.Second)
	if legs := listLegs(t, instB); len(legs) != 0 {
		t.Errorf("the SRTP-disabled peer surfaced %d legs", len(legs))
	}
}

func TestSRTP_CreateLegValidation(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "srtp-validate", withSRTP("required"))
	for name, body := range map[string]map[string]interface{}{
		"opt out under required": {"type": "sip", "to": "sip:bob@127.0.0.1:5999", "srtp": false},
	} {
		resp := httpPost(t, inst.baseURL()+"/v1/legs", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
}
