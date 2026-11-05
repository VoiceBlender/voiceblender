//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestAMD_FilteredLeg_KeepsReadingAfterVerdict covers a roomless outbound leg
// whose AMD runs through a corrective filter chain: the verdict must be
// published, and the leg must keep reading RTP afterwards, so DTMF still
// arrives.
func TestAMD_FilteredLeg_KeepsReadingAfterVerdict(t *testing.T) {
	for _, filter := range []string{"bandpass", "denoise"} {
		t.Run(filter, func(t *testing.T) {
			if filter == "denoise" {
				if err := denoise.Install(); err != nil {
					t.Skipf("denoise unavailable: %v", err)
				}
				t.Cleanup(func() { denoise.Shutdown() })
			}
			instA := newTestInstance(t, "amd-filter-a-"+filter)
			instB := newTestInstance(t, "amd-filter-b-"+filter)

			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":    "sip",
				"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs":  []string{"PCMU"},
				"filters": []map[string]interface{}{{"type": filter}},
				"amd": map[string]interface{}{
					"initial_silence_timeout": 4000,
					"greeting_duration":       1500,
					"after_greeting_silence":  800,
					"total_analysis_time":     8000,
				},
			})
			if createResp.StatusCode != http.StatusCreated {
				t.Fatalf("create leg: status %d", createResp.StatusCode)
			}
			var outbound legView
			decodeJSON(t, createResp, &outbound)

			inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
			answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
			if answerResp.StatusCode != http.StatusAccepted {
				t.Fatalf("answer: status %d", answerResp.StatusCode)
			}
			answerResp.Body.Close()
			waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
			waitForLegState(t, instB.baseURL(), inbound.ID, "connected", 5*time.Second)

			playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inbound.ID), map[string]interface{}{
				"tone": "us_dial", "repeat": -1,
			})
			if playResp.StatusCode != http.StatusOK {
				t.Fatalf("play: status %d", playResp.StatusCode)
			}
			var pb struct {
				PlaybackID string `json:"playback_id"`
			}
			decodeJSON(t, playResp, &pb)
			time.Sleep(500 * time.Millisecond)
			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", instB.baseURL(), inbound.ID, pb.PlaybackID)).Body.Close()

			instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
				return e.Data.GetLegID() == outbound.ID
			}, 15*time.Second)

			sendDTMFFrom(t, instB, inbound.ID, "1")
			waitForDTMF(t, instA, outbound.ID, "1")

			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
		})
	}
}
