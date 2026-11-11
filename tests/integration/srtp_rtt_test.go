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

	"github.com/VoiceBlender/voiceblender/internal/codec/t140"
	"github.com/VoiceBlender/voiceblender/internal/events"
	sipmod "github.com/VoiceBlender/voiceblender/internal/sip"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
)

func sendRTT(t *testing.T, inst *testInstance, legID, text string) {
	t.Helper()
	r := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/rtt", inst.baseURL(), legID), map[string]string{"text": text})
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("send rtt on %s: status %d", legID, r.StatusCode)
	}
}

// TestSRTP_RTTInstanceToInstance carries real-time text on an SDES-SRTP call
// between two instances. The caller offers the text section on RTP/SAVP with
// its own keys and fails the call on an answer without one, so text arriving
// both ways, before and after a hold/unhold pair, is encrypted text.
func TestSRTP_RTTInstanceToInstance(t *testing.T) {
	cases := []struct {
		name   string
		modeA  string
		extraA map[string]interface{}
	}{
		{"leg opts in", "optional", map[string]interface{}{"srtp": true, "rtt": true}},
		{"server requires it", "required", map[string]interface{}{"rtt": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstanceWithOpts(t, "srtp-rtt-a", withSRTP(tc.modeA))
			instB := newTestInstance(t, "srtp-rtt-b")
			out, in := dialSRTP(t, instA, instB, tc.extraA)

			sendRTT(t, instA, out, "hello")
			waitForRTT(t, instB, in, "hello", 3*time.Second)
			sendRTT(t, instB, in, "world")
			waitForRTT(t, instA, out, "world", 3*time.Second)

			hold := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out), nil)
			hold.Body.Close()
			instA.collector.waitForMatch(t, events.LegHold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)
			unhold := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/hold", instA.baseURL(), out))
			unhold.Body.Close()
			instA.collector.waitForMatch(t, events.LegUnhold, func(e events.Event) bool { return e.Data.GetLegID() == out }, 5*time.Second)

			sendRTT(t, instA, out, "!")
			waitForRTT(t, instB, in, "hello!", 3*time.Second)
			hangup(t, instA, out)
		})
	}
}

// TestSRTP_RTTRawPeer offers SDES-SRTP audio and text from a bare peer holding
// its own text key. The answer keys the text section separately, the peer's
// encrypted T.140 is decoded, and the text VoiceBlender sends authenticates
// and decrypts under the key its answer advertised.
func TestSRTP_RTTRawPeer(t *testing.T) {
	inst := newTestInstance(t, "srtp-rtt-raw")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	audio, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	defer audio.Close()
	text, err := sipmod.NewRTPSession()
	if err != nil {
		t.Fatal(err)
	}
	defer text.Close()
	audioKey, _ := sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_80)
	textKey, _ := sipmod.NewSDESCrypto(1, sipmod.SuiteAES128CMSHA1_32)

	offer := strings.TrimSuffix(string(srtpOffer(audio.LocalPort(), "RTP/SAVP", audioKey)), "\r\n") + "\r\n" + strings.Join([]string{
		fmt.Sprintf("m=text %d RTP/SAVP 98 99", text.LocalPort()),
		"a=rtpmap:98 red/1000",
		"a=rtpmap:99 t140/1000",
		"a=fmtp:98 99/99/99",
		"a=sendrecv",
		"a=crypto:" + textKey.AttrValue(),
		"",
	}, "\r\n")

	dialog, inbound := answerInboundCall(t, ctx, inst,
		sip.Uri{Scheme: "sip", User: "500", Host: "127.0.0.1", Port: inst.sipPort}, []byte(offer))
	answer, err := sipmod.ParseSDP(dialog.InviteResponse.Body())
	if err != nil {
		t.Fatal(err)
	}
	if err := dialog.Ack(ctx); err != nil {
		t.Fatal(err)
	}
	at := answer.Text
	if at == nil || strings.Join(at.Proto, "/") != "RTP/SAVP" || len(at.Crypto) != 1 {
		t.Fatalf("text section is not answered with SDES-SRTP:\n%s", dialog.InviteResponse.Body())
	}
	vbTextKey := at.Crypto[0]
	if vbTextKey.Suite != sipmod.SuiteAES128CMSHA1_32 || vbTextKey.SameKey(&textKey) || vbTextKey.SameKey(&answer.PrimaryStream().Crypto[0]) {
		t.Fatalf("text answer crypto = %+v", vbTextKey)
	}
	waitForLegState(t, inst.baseURL(), inbound.ID, "connected", 5*time.Second)

	if err := text.SetSRTP(&textKey, &vbTextKey); err != nil {
		t.Fatal(err)
	}
	if err := text.SetRemote(at.RemoteIP, at.RemotePort); err != nil {
		t.Fatal(err)
	}

	// Peer → VoiceBlender.
	enc := t140.NewEncoder(0, 99)
	enc.Push("hi")
	payload, _ := enc.Flush(1000)
	if err := text.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 99, SequenceNumber: 1, Timestamp: 1000, SSRC: 0x7e47}, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	waitForRTT(t, inst, inbound.ID, "hi", 3*time.Second)

	// VoiceBlender → peer.
	sendRTT(t, inst, inbound.ID, "yo")
	dec := t140.NewDecoder()
	var got string
	var pkt rtp.Packet
	text.SetReadDeadline(time.Now().Add(3 * time.Second))
	for got != "yo" {
		if err := text.ReadRTPInto(&pkt); err != nil {
			if errors.Is(err, sipmod.ErrNotRTP) {
				continue
			}
			t.Fatalf("no text authenticated under the answered key (got %q): %v", got, err)
		}
		s, _, err := dec.DecodePacket(pkt.SequenceNumber, pkt.Timestamp, pkt.PayloadType, 99, 98, pkt.Payload)
		if err == nil {
			got += s
		}
	}
	hangup(t, inst, inbound.ID)
}
