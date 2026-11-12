//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

func reOffer(t *testing.T, ctx context.Context, dialog *sipgo.DialogClientSession, method sip.RequestMethod, offer []byte) *sip.Response {
	t.Helper()
	contact := dialog.InviteResponse.Contact()
	if contact == nil {
		t.Fatal("200 OK carries no Contact")
	}
	req := sip.NewRequest(method, contact.Address)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(offer)
	res, err := dialog.Do(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return res
}

func expectSRTPMedia(t *testing.T, peer *sipmod.RTPSession) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pkt rtp.Packet
	for {
		err := peer.ReadRTPInto(&pkt)
		if err == nil {
			return
		}
		if !errors.Is(err, sipmod.ErrNotRTP) {
			t.Fatalf("no media authenticated under the answered key: %v", err)
		}
	}
}

// TestSRTP_ReOfferRejected sends plain re-offers on an SRTP call under
// SIP_SRTP=required: each gets a 488 and the session stays as it was.
func TestSRTP_ReOfferRejected(t *testing.T) {
	inst := newTestInstanceWithOpts(t, "srtp-reoffer", withSRTP("required"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	if err := dialog.Ack(ctx); err != nil {
		t.Fatal(err)
	}
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatal(err)
	}
	ra := answer.PrimaryStream()
	if err := peer.SetSRTP(&peerKey, &ra.Crypto[0]); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
		t.Fatal(err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)

	// A plain re-offer moving media elsewhere and holding the call.
	plain := []byte(strings.Replace(string(srtpOffer(peer.LocalPort()+2, "RTP/AVP")), "a=sendrecv", "a=sendonly", 1))
	for _, method := range []sip.RequestMethod{sip.INVITE, sip.UPDATE} {
		if res := reOffer(t, ctx, dialog, method, plain); res.StatusCode != sip.StatusNotAcceptableHere {
			t.Fatalf("plain %s: got %d, want 488:\n%s", method, res.StatusCode, res.Body())
		}
	}
	expectSRTPMedia(t, peer)
	if legs := listLegs(t, inst); len(legs) != 1 || legs[0].State != "connected" {
		t.Fatalf("after the rejected re-offers: %+v", legs)
	}

	// The dialog still renegotiates an SRTP offer.
	res := reOffer(t, ctx, dialog, sip.UPDATE, srtpOffer(peer.LocalPort(), "RTP/SAVP", peerKey))
	if res.StatusCode != sip.StatusOK || !strings.Contains(string(res.Body()), " RTP/SAVP ") {
		t.Fatalf("SDES UPDATE: got %d:\n%s", res.StatusCode, res.Body())
	}
	hangup(t, inst, inbound.ID)
}

// answerSRTPInvite answers an SRTP INVITE from VoiceBlender with peerKey on
// the first offered tag and suite.
func answerSRTPInvite(t *testing.T, ua *rawSIPClient, e inviteEvent, peerKey *sipmod.SDESCrypto) *sip.Response {
	t.Helper()
	offer, err := sipmod.ParseSDP(e.req.Body())
	if err != nil {
		t.Fatal(err)
	}
	oc := offer.PrimaryStream().Crypto
	if len(oc) == 0 {
		t.Fatalf("INVITE offers no SDES key:\n%s", e.req.Body())
	}
	k, err := sipmod.NewSDESCrypto(oc[0].Tag, oc[0].Suite)
	if err != nil {
		t.Fatal(err)
	}
	*peerKey = k
	res := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", srtpOffer(40000, "RTP/SAVP", k))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: ua.host, Port: ua.port}})
	if err := e.tx.Respond(res); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)
	return res
}

// TestSRTP_LegSRTPHoldsForCall checks that a leg created with "srtp": true
// stays encrypted under SIP_SRTP=optional: a plain re-offer gets a 488.
func TestSRTP_LegSRTPHoldsForCall(t *testing.T) {
	inst := newTestInstance(t, "srtp-leg-holds")
	ua := newRawSIPClient(t, "srtp-uas")

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": ua.contactURI("bob"), "srtp": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)
	e := ua.waitInvite(t, 5*time.Second)
	var peerKey sipmod.SDESCrypto
	ok := answerSRTPInvite(t, ua, e, &peerKey)
	waitForLegState(t, inst.baseURL(), lv.ID, "connected", 5*time.Second)

	if res := ua.sendInDialogUpdate(t, e.req, ok, srtpOffer(40002, "RTP/AVP"), ""); res.StatusCode != sip.StatusNotAcceptableHere {
		t.Fatalf("plain UPDATE: got %d, want 488:\n%s", res.StatusCode, res.Body())
	}
	if res := ua.sendInDialogUpdate(t, e.req, ok, srtpOffer(40000, "RTP/SAVP", peerKey), ""); res.StatusCode != sip.StatusOK {
		t.Fatalf("SDES UPDATE: got %d", res.StatusCode)
	}
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), lv.ID)).Body.Close()
}

// TestSRTP_PlainAnswerReason answers an SRTP offer with plain RTP: the leg
// ends with the same reason as a 488.
func TestSRTP_PlainAnswerReason(t *testing.T) {
	inst := newTestInstance(t, "srtp-plain-answer")
	ua := newRawSIPClient(t, "plain-uas")

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type": "sip", "to": ua.contactURI("bob"), "srtp": true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)
	ua.answerInviteCaptured(t, ua.waitInvite(t, 5*time.Second))

	disc := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == lv.ID
	}, 5*time.Second)
	if got := disc.Data.(*events.LegDisconnectedData).CDR.Reason; got != "not_acceptable" {
		t.Errorf("reason = %q, want not_acceptable", got)
	}
}
