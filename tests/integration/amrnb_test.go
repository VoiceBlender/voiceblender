//go:build integration

package integration

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// newAMRNBInstance builds a test instance that offers AMR-NB only, at the
// requested octet-aligned framing.
func newAMRNBInstance(t *testing.T, name string, octetAligned bool) *testInstance {
	t.Helper()
	return newTestInstanceFull(t, name,
		func(c *config.Config) {
			c.AMRNBMode = 7
			c.AMRNBOctetAligned = octetAligned
		},
		[]codec.CodecType{codec.CodecAMRNB},
	)
}

// TestAMRNB_NegotiateAndConnect verifies that an AMR-NB-only offer is exposed
// in the ringing event with the correct 8 kHz clock and a dynamic PT, and that
// answering with AMR-NB connects both legs.
func TestAMRNB_NegotiateAndConnect(t *testing.T) {
	instA := newAMRNBInstance(t, "amrnb-a", true)
	instB := newAMRNBInstance(t, "amrnb-b", true)

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"AMR-NB"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	bRing := instB.collector.waitForMatch(t, events.LegRinging, nil, 3*time.Second)
	d := bRing.Data.(*events.LegRingingData)
	if len(d.OfferedCodecs) == 0 || d.OfferedCodecs[0].Name != "AMR-NB" {
		t.Fatalf("OfferedCodecs = %#v, want AMR-NB first", d.OfferedCodecs)
	}
	if d.OfferedCodecs[0].ClockRate != 8000 {
		t.Errorf("AMR-NB clock = %d, want 8000", d.OfferedCodecs[0].ClockRate)
	}
	if d.OfferedCodecs[0].PayloadType < 96 {
		t.Errorf("AMR-NB PT = %d, want a dynamic PT (>=96)", d.OfferedCodecs[0].PayloadType)
	}

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "AMR-NB"},
	)
	if answerResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(answerResp.Body)
		answerResp.Body.Close()
		t.Fatalf("answer with codec=AMR-NB: status %d, body=%s", answerResp.StatusCode, body)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)
}

// TestAMRNB_EndToEndAudio places an AMR-NB call, plays a tone on the caller,
// and asserts the far leg recovers non-silent audio through the AMR-NB
// encode → RTP → decode path. Runs for both RFC 4867 payload formats.
func TestAMRNB_EndToEndAudio(t *testing.T) {
	for _, tc := range []struct {
		name         string
		octetAligned bool
	}{
		{"octet_aligned", true},
		{"bandwidth_efficient", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := newAMRNBInstance(t, "amrnb-tx-"+tc.name, tc.octetAligned)
			instB := newAMRNBInstance(t, "amrnb-rx-"+tc.name, tc.octetAligned)

			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":   "sip",
				"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs": []string{"AMR-NB"},
			})
			if createResp.StatusCode != http.StatusCreated {
				t.Fatalf("create leg: status %d", createResp.StatusCode)
			}
			var outbound legView
			decodeJSON(t, createResp, &outbound)

			inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
			answerResp := httpPost(t,
				fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
				map[string]interface{}{"codec": "AMR-NB"},
			)
			if answerResp.StatusCode != http.StatusAccepted {
				body, _ := io.ReadAll(answerResp.Body)
				answerResp.Body.Close()
				t.Fatalf("answer: status %d, body=%s", answerResp.StatusCode, body)
			}
			answerResp.Body.Close()

			waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
			waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

			rawLeg, ok := instB.legMgr.Get(inbound.ID)
			if !ok {
				t.Fatalf("inbound leg %s not found", inbound.ID)
			}
			sipLeg, ok := rawLeg.(*leg.SIPLeg)
			if !ok {
				t.Fatalf("inbound leg is %T, want *leg.SIPLeg", rawLeg)
			}
			tap := &countingTap{}
			sipLeg.SetInTap(tap)
			t.Cleanup(sipLeg.ClearInTap)

			playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outbound.ID),
				map[string]interface{}{"tone": "us_dial", "repeat": -1, "volume": 0})
			if playResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(playResp.Body)
				playResp.Body.Close()
				t.Fatalf("play tone: status %d, body=%s", playResp.StatusCode, body)
			}
			playResp.Body.Close()

			if !waitNonSilence(t, tap, 5*time.Second) {
				t.Fatalf("no audio recovered through AMR-NB path (non-zero bytes=%d)", tap.count())
			}
		})
	}
}

// TestAMRNB_DTMF verifies out-of-band DTMF (RFC 4733) flows over the 8 kHz
// telephone-event clock paired with AMR-NB. This is the standard 8 kHz path
// (unlike AMR-WB which required a 16 kHz telephone-event rate).
func TestAMRNB_DTMF(t *testing.T) {
	instA := newAMRNBInstance(t, "amrnb-dtmf-a", true)
	instB := newAMRNBInstance(t, "amrnb-dtmf-b", true)

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"AMR-NB"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "AMR-NB"},
	)
	if answerResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(answerResp.Body)
		answerResp.Body.Close()
		t.Fatalf("answer with codec=AMR-NB: status %d, body=%s", answerResp.StatusCode, body)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

	sendDTMFFrom(t, instA, outbound.ID, "5")
	waitForDTMF(t, instB, inbound.ID, "5")

	sendDTMFFrom(t, instB, inbound.ID, "7")
	waitForDTMF(t, instA, outbound.ID, "7")
}
