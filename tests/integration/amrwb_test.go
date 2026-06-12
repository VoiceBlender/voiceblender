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

// newAMRWBInstance builds a test instance that offers AMR-WB (octet-aligned,
// mode 8) in addition to the given fallback codecs.
func newAMRWBInstance(t *testing.T, name string, octetAligned bool) *testInstance {
	t.Helper()
	return newTestInstanceFull(t, name,
		func(c *config.Config) {
			c.AMRWBMode = 8
			c.AMRWBOctetAligned = octetAligned
		},
		[]codec.CodecType{codec.CodecAMRWB},
	)
}

// TestAMRWB_NegotiateAndConnect verifies that an AMR-WB-only offer is exposed
// in the ringing event with the correct 16 kHz clock and a dynamic PT, and
// that answering with AMR-WB connects both legs.
func TestAMRWB_NegotiateAndConnect(t *testing.T) {
	instA := newAMRWBInstance(t, "amrwb-a", true)
	instB := newAMRWBInstance(t, "amrwb-b", true)

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"AMR-WB"},
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	bRing := instB.collector.waitForMatch(t, events.LegRinging, nil, 3*time.Second)
	d := bRing.Data.(*events.LegRingingData)
	if len(d.OfferedCodecs) == 0 || d.OfferedCodecs[0].Name != "AMR-WB" {
		t.Fatalf("OfferedCodecs = %#v, want AMR-WB first", d.OfferedCodecs)
	}
	if d.OfferedCodecs[0].ClockRate != 16000 {
		t.Errorf("AMR-WB clock = %d, want 16000", d.OfferedCodecs[0].ClockRate)
	}
	if d.OfferedCodecs[0].PayloadType < 96 {
		t.Errorf("AMR-WB PT = %d, want a dynamic PT (>=96)", d.OfferedCodecs[0].PayloadType)
	}

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t,
		fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
		map[string]interface{}{"codec": "AMR-WB"},
	)
	if answerResp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(answerResp.Body)
		answerResp.Body.Close()
		t.Fatalf("answer with codec=AMR-WB: status %d, body=%s", answerResp.StatusCode, body)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)
}

// TestAMRWB_EndToEndAudio places an AMR-WB call, plays a tone on the caller,
// and asserts the far leg recovers non-silent audio through the AMR-WB
// encode → RTP → decode path. Runs for both RFC 4867 payload formats.
func TestAMRWB_EndToEndAudio(t *testing.T) {
	for _, tc := range []struct {
		name         string
		octetAligned bool
	}{
		{"octet_aligned", true},
		{"bandwidth_efficient", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := newAMRWBInstance(t, "amrwb-tx-"+tc.name, tc.octetAligned)
			instB := newAMRWBInstance(t, "amrwb-rx-"+tc.name, tc.octetAligned)

			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":   "sip",
				"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs": []string{"AMR-WB"},
			})
			if createResp.StatusCode != http.StatusCreated {
				t.Fatalf("create leg: status %d", createResp.StatusCode)
			}
			var outbound legView
			decodeJSON(t, createResp, &outbound)

			inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
			answerResp := httpPost(t,
				fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID),
				map[string]interface{}{"codec": "AMR-WB"},
			)
			if answerResp.StatusCode != http.StatusAccepted {
				body, _ := io.ReadAll(answerResp.Body)
				answerResp.Body.Close()
				t.Fatalf("answer: status %d, body=%s", answerResp.StatusCode, body)
			}
			answerResp.Body.Close()

			waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
			waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

			// Tap the decoded incoming PCM on the receiving (inbound) leg.
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

			// Play a looping tone on the caller; it is encoded as AMR-WB and
			// sent over RTP to the far leg.
			playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instA.baseURL(), outbound.ID),
				map[string]interface{}{"tone": "us_dial", "repeat": -1, "volume": 0})
			if playResp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(playResp.Body)
				playResp.Body.Close()
				t.Fatalf("play tone: status %d, body=%s", playResp.StatusCode, body)
			}
			playResp.Body.Close()

			if !waitNonSilence(t, tap, 5*time.Second) {
				t.Fatalf("no audio recovered through AMR-WB path (non-zero bytes=%d)", tap.count())
			}
		})
	}
}
