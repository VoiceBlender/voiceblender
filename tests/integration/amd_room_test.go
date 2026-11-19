//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestAMD_RoomLeg covers an outbound leg created with both amd and room_id:
// the verdict must be published, a DELETE of the leg must publish its
// leg.disconnected, and the room API must keep answering.
func TestAMD_RoomLeg(t *testing.T) {
	for _, tc := range []struct {
		name string
		tone string // "" = silent answer
		want string
	}{
		{"silent", "", "no_speech"},
		{"tone", "us_dial", "machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstance(t, "amd-room-a-"+tc.name)
			instB := newTestInstance(t, "amd-room-b-"+tc.name)

			createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
				"type":    "sip",
				"uri":     fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
				"codecs":  []string{"PCMU"},
				"room_id": "room-amd-" + tc.name,
				"amd":     map[string]interface{}{"total_analysis_time": 5000},
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

			if tc.tone != "" {
				playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", instB.baseURL(), inbound.ID), map[string]interface{}{
					"tone": tc.tone, "repeat": -1,
				})
				if playResp.StatusCode != http.StatusOK {
					t.Fatalf("play: status %d", playResp.StatusCode)
				}
				playResp.Body.Close()
			}

			e := instA.collector.waitForMatch(t, events.AMDResult, func(e events.Event) bool {
				return e.Data.GetLegID() == outbound.ID
			}, 10*time.Second)
			if got := e.Data.(*events.AMDResultData).Result; got != tc.want {
				t.Errorf("amd.result = %s, want %s", got, tc.want)
			}

			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outbound.ID)).Body.Close()
			instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
				return e.Data.GetLegID() == outbound.ID
			}, 5*time.Second)

			c := http.Client{Timeout: 3 * time.Second}
			resp, err := c.Get(instA.baseURL() + "/v1/rooms")
			if err != nil {
				t.Fatalf("GET /v1/rooms: %v", err)
			}
			resp.Body.Close()
		})
	}
}
