//go:build integration

package integration

import (
	"context"
	"errors"
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
	"github.com/pion/rtp"
)

// assertStreamsSRTP checks the srtp flag of each of a leg's audio streams, in
// m-line order.
func assertStreamsSRTP(t *testing.T, inst *testInstance, legID string, want ...bool) {
	t.Helper()
	resp := httpGet(t, fmt.Sprintf("%s/v1/legs/%s/streams", inst.baseURL(), legID))
	var streams []legStreamView
	decodeJSON(t, resp, &streams)
	if len(streams) != len(want) {
		t.Fatalf("[%s] leg %s has %d streams, want %d", inst.name, legID, len(streams), len(want))
	}
	for i, s := range streams {
		if s.SRTP != want[i] {
			t.Errorf("[%s] leg %s stream %s: srtp = %v, want %v", inst.name, legID, s.ID, s.SRTP, want[i])
		}
	}
}

// reInvite sends a re-INVITE carrying offer and ACKs a 2xx.
func reInvite(t *testing.T, ctx context.Context, dialog *sipgo.DialogClientSession, offer []byte) *sip.Response {
	t.Helper()
	res := reOffer(t, ctx, dialog, sip.INVITE, offer)
	if res.IsSuccess() {
		if err := dialog.WriteRequest(sip.NewRequest(sip.ACK, dialog.InviteResponse.Contact().Address)); err != nil {
			t.Fatalf("ACK re-INVITE: %v", err)
		}
	}
	return res
}

// answerSRTPOn answers an SRTP INVITE from VoiceBlender with media on peer,
// which it keys with a fresh key on the first offered tag and suite.
func answerSRTPOn(t *testing.T, ua *rawSIPClient, e inviteEvent, peer *sipmod.RTPSession) {
	t.Helper()
	offer, err := sipmod.ParseSDP(e.req.Body())
	if err != nil {
		t.Fatal(err)
	}
	ra := offer.PrimaryStream()
	if len(ra.Crypto) == 0 {
		t.Fatalf("INVITE offers no SDES key:\n%s", e.req.Body())
	}
	key, err := sipmod.NewSDESCrypto(ra.Crypto[0].Tag, ra.Crypto[0].Suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetSRTP(&key, &ra.Crypto[0]); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
		t.Fatal(err)
	}
	res := sip.NewResponseFromRequest(e.req, sip.StatusOK, "OK", srtpOffer(peer.LocalPort(), "RTP/SAVP", key))
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{Scheme: "sip", Host: ua.host, Port: ua.port}})
	if err := e.tx.Respond(res); err != nil {
		t.Fatalf("respond 200: %v", err)
	}
	close(e.done)
}

// TestSRTP_StreamFlag checks that GET /v1/legs/{id}/streams tells an encrypted
// stream from a plain one, on both ends of a call and for a stream added to it.
func TestSRTP_StreamFlag(t *testing.T) {
	t.Run("SDES call", func(t *testing.T) {
		instA := newTestInstance(t, "srtp-flag-a")
		instB := newTestInstance(t, "srtp-flag-b")
		out, in := dialSRTP(t, instA, instB, map[string]interface{}{"srtp": true})
		assertStreamsSRTP(t, instA, out, true)
		assertStreamsSRTP(t, instB, in, true)

		add := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/streams", instA.baseURL(), out), map[string]interface{}{"direction": "sendonly"})
		if add.StatusCode != http.StatusCreated {
			msg, _ := io.ReadAll(add.Body)
			t.Fatalf("add stream: status %d: %s", add.StatusCode, msg)
		}
		var added legStreamView
		decodeJSON(t, add, &added)
		if !added.SRTP {
			t.Error("the added stream's own view does not report srtp")
		}
		assertStreamsSRTP(t, instA, out, true, true)
		assertStreamsSRTP(t, instB, in, true, true)
		hangup(t, instA, out)
	})

	t.Run("plain call", func(t *testing.T) {
		instA := newTestInstance(t, "plain-flag-a")
		instB := newTestInstance(t, "plain-flag-b")
		out, in := dialSRTP(t, instA, instB, nil)
		assertStreamsSRTP(t, instA, out, false)
		assertStreamsSRTP(t, instB, in, false)
		hangup(t, instA, out)
	})
}

// TestSRTP_HoldFromAnsweringSide holds and resumes an SDES-SRTP call from the
// called end, so the caller is the one answering the re-INVITEs: both ends stay
// encrypted and media flows both ways afterwards.
func TestSRTP_HoldFromAnsweringSide(t *testing.T) {
	instA := newTestInstance(t, "srtp-hold-a")
	instB := newTestInstance(t, "srtp-hold-b")
	out, in := dialSRTP(t, instA, instB, map[string]interface{}{"srtp": true})

	hold := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), in), nil)
	hold.Body.Close()
	instB.collector.waitForMatch(t, events.LegHold, forLeg(in), 5*time.Second)
	assertStreamsSRTP(t, instA, out, true)
	unhold := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instB.baseURL(), in))
	unhold.Body.Close()
	instB.collector.waitForMatch(t, events.LegUnhold, forLeg(in), 5*time.Second)

	assertStreamsSRTP(t, instA, out, true)
	assertStreamsSRTP(t, instB, in, true)
	assertDTMFBothWays(t, instA, out, instB, in, "12")
	hangup(t, instA, out)
}

// TestSRTP_EarlyMedia keys an SDES-SRTP call from the 183: the caller's leg is
// encrypted and hears the callee while still in early_media, and stays
// encrypted once the 200 OK repeats the key.
func TestSRTP_EarlyMedia(t *testing.T) {
	instA := newTestInstance(t, "srtp-early-a")
	instB := newTestInstance(t, "srtp-early-b")

	resp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"srtp":   true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, resp, &outbound)
	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)

	em := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/early-media", instB.baseURL(), inbound.ID), nil)
	if em.StatusCode != http.StatusAccepted {
		t.Fatalf("early-media: status %d", em.StatusCode)
	}
	em.Body.Close()
	waitForLegState(t, instA.baseURL(), outbound.ID, "early_media", 5*time.Second)
	assertStreamsSRTP(t, instA, outbound.ID, true)

	play := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inbound.ID),
		map[string]interface{}{"tone": "us_dial", "repeat": -1, "volume": 0})
	play.Body.Close()
	tap := &countingTap{}
	sl := sipLegOf(t, instA, outbound.ID)
	sl.SetInTap(tap)
	heard := waitNonSilence(t, tap, 5*time.Second)
	sl.ClearInTap()
	if !heard {
		t.Fatalf("the caller decoded no early media over SRTP (non-zero bytes=%d)", tap.count())
	}

	ans := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	ans.Body.Close()
	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)
	assertStreamsSRTP(t, instA, outbound.ID, true)
	assertStreamsSRTP(t, instB, inbound.ID, true)
	assertDTMFBothWays(t, instA, outbound.ID, instB, inbound.ID, "12")
	hangup(t, instA, outbound.ID)
}

// TestSRTP_ForkedInvite forks an SDES-SRTP INVITE to two registered contacts.
// One RTP session serves whichever branch answers, so every branch must carry
// the same keys, and the winner's answer is what keys the media.
func TestSRTP_ForkedInvite(t *testing.T) {
	inst := newTestInstance(t, "srtp-fork")
	cli1 := newRawSIPClient(t, "alice-ua-1")
	cli2 := newRawSIPClient(t, "alice-ua-2")
	cli1.sendRegister(t, inst.sipPort, "alice", cli1.contactURI("alice"), 600)
	cli2.sendRegister(t, inst.sipPort, "alice", cli2.contactURI("alice"), 600)

	resp := httpPost(t, inst.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"to":     "sip:alice@vb.test",
		"from":   "support",
		"codecs": []string{"PCMU"},
		"srtp":   true,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", resp.StatusCode)
	}
	var lv legView
	decodeJSON(t, resp, &lv)

	e1 := cli1.waitInvite(t, 5*time.Second)
	e2 := cli2.waitInvite(t, 5*time.Second)
	defer close(e1.done)
	var offered [2][]sipmod.SDESCrypto
	for i, e := range []inviteEvent{e1, e2} {
		offer, err := sipmod.ParseSDP(e.req.Body())
		if err != nil {
			t.Fatalf("branch %d offer: %v", i+1, err)
		}
		offered[i] = offer.PrimaryStream().Crypto
	}
	if len(offered[0]) == 0 || len(offered[0]) != len(offered[1]) {
		t.Fatalf("branches offer %d and %d crypto lines", len(offered[0]), len(offered[1]))
	}
	for i := range offered[0] {
		a, b := offered[0][i], offered[1][i]
		if a.Tag != b.Tag || !a.SameKey(&b) {
			t.Fatalf("crypto line %d differs between the branches", i+1)
		}
	}

	peer, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	answerSRTPOn(t, cli2, e2, peer)
	waitForLegState(t, inst.baseURL(), lv.ID, "connected", 5*time.Second)
	assertStreamsSRTP(t, inst, lv.ID, true)
	expectSRTPMedia(t, peer)
	hangup(t, inst, lv.ID)
}

// TestSRTP_PeerReOffersKeepKey re-offers an SDES-SRTP call from the far end
// with its key unchanged: a refresh, a hold and a resume. Every answer must
// stay on RTP/SAVP and repeat our key, and media must authenticate both ways
// afterwards on the keys from the first exchange.
func TestSRTP_PeerReOffersKeepKey(t *testing.T) {
	inst := newTestInstance(t, "srtp-peer-hold")
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
	offer := srtpOffer(peer.LocalPort(), "RTP/SAVP", peerKey)
	dialog, inbound := answerInboundCall(t, ctx, inst,
		sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, offer)
	if err := dialog.Ack(ctx); err != nil {
		t.Fatal(err)
	}
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatal(err)
	}
	ra := answer.PrimaryStream()
	if len(ra.Crypto) != 1 {
		t.Fatalf("answer is not SDES-SRTP:\n%s", dialog.InviteResponse.Body())
	}
	vbKey := ra.Crypto[0]
	if err := peer.SetSRTP(&peerKey, &vbKey); err != nil {
		t.Fatal(err)
	}
	if err := peer.SetRemote(ra.RemoteIP, ra.RemotePort); err != nil {
		t.Fatal(err)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)
	expectSRTPMedia(t, peer)

	keepsKey := func(what string, res *sip.Response, direction string) {
		t.Helper()
		if res.StatusCode != sip.StatusOK {
			t.Fatalf("%s: got %d", what, res.StatusCode)
		}
		got, err := sipmod.ParseSDP(res.Body())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		s := got.PrimaryStream()
		if strings.Join(s.Proto, "/") != "RTP/SAVP" || len(s.Crypto) != 1 || !s.Crypto[0].SameKey(&vbKey) {
			t.Fatalf("%s: the answer does not repeat our key on RTP/SAVP:\n%s", what, res.Body())
		}
		if !strings.Contains(string(res.Body()), "a="+direction+"\r\n") {
			t.Fatalf("%s: the answer is not %s:\n%s", what, direction, res.Body())
		}
	}

	keepsKey("refresh", reInvite(t, ctx, dialog, offer), "sendrecv")

	held := []byte(strings.Replace(string(offer), "a=sendrecv", "a=sendonly", 1))
	keepsKey("hold", reInvite(t, ctx, dialog, held), "recvonly")
	inst.collector.waitForMatch(t, events.LegHold, forLeg(inbound.ID), 3*time.Second)
	assertStreamsSRTP(t, inst, inbound.ID, true)

	// Nothing is sent to a holding peer, so once the socket is empty any packet
	// that follows was sent after the resume.
	var pkt rtp.Packet
	peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		if err := peer.ReadRTPInto(&pkt); err != nil && !errors.Is(err, sipmod.ErrNotRTP) {
			break
		}
	}

	keepsKey("resume", reInvite(t, ctx, dialog, offer), "sendrecv")
	inst.collector.waitForMatch(t, events.LegUnhold, forLeg(inbound.ID), 3*time.Second)
	expectSRTPMedia(t, peer)

	for _, p := range sipmod.GenerateDTMFPackets('7', 101, 0x1234, 100, 8000, 160) {
		if err := peer.WriteRTP(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitForDTMF(t, inst, inbound.ID, "7")
	assertStreamsSRTP(t, inst, inbound.ID, true)

	hangup(t, inst, inbound.ID)
}
