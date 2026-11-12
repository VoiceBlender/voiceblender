//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// TestRoomRecording_ContinuesAfterFirstLegHangsUp records a room that one leg
// leaves while another stays, as a transferred call does. The room mix must
// cover the whole recording, not stop at the first hang-up.
//
// The leg that hangs up is the only participant when the recording starts, so
// the test does not depend on the order the room lists its participants in.
func TestRoomRecording_ContinuesAfterFirstLegHangsUp(t *testing.T) {
	cases := []struct {
		name string
		// hangUp ends the first leg, given its ID on each instance.
		hangUp func(t *testing.T, instA, instB *testInstance, outboundID, inboundID string)
	}{
		{"api hang-up", func(t *testing.T, instA, _ *testInstance, outboundID, _ string) {
			resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
			resp.Body.Close()
		}},
		{"far end hangs up", func(t *testing.T, _, instB *testInstance, _, inboundID string) {
			resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), inboundID))
			resp.Body.Close()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstance(t, "recleave-a")
			instB := newTestInstance(t, "recleave-b")

			firstID, firstFarID := establishCall(t, instA, instB)
			secondID, _ := establishCall(t, instA, instB)

			roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{"sample_rate": 16000})
			if roomResp.StatusCode != http.StatusCreated {
				t.Fatalf("create room: status %d", roomResp.StatusCode)
			}
			var rm roomView
			decodeJSON(t, roomResp, &rm)

			addLeg := func(id string) {
				t.Helper()
				resp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
					map[string]interface{}{"leg_id": id})
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("add leg to room: status %d", resp.StatusCode)
				}
				instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
					return e.Data.GetLegID() == id && e.Data.GetRoomID() == rm.ID
				}, 3*time.Second)
			}

			addLeg(firstID)
			recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), map[string]interface{}{})
			if recResp.StatusCode != http.StatusOK {
				t.Fatalf("start room recording: status %d", recResp.StatusCode)
			}
			recResp.Body.Close()
			started := time.Now()
			addLeg(secondID)

			time.Sleep(time.Second)
			tc.hangUp(t, instA, instB, firstID, firstFarID)
			instA.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
				return e.Data.GetLegID() == firstID
			}, 5*time.Second)

			// The second leg is still in the room, so the recording must go on.
			time.Sleep(2 * time.Second)
			if instA.collector.hasEvent(events.RecordingFinished, func(e events.Event) bool {
				return e.Data.GetRoomID() == rm.ID
			}) {
				t.Fatal("recording.finished published while the room still had a participant")
			}

			stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
			if stopResp.StatusCode != http.StatusOK {
				t.Fatalf("stop room recording: status %d", stopResp.StatusCode)
			}
			recorded := time.Since(started)
			var stop recordingResponse
			decodeJSON(t, stopResp, &stop)
			if stop.File == "" {
				t.Fatal("stop returned no file")
			}

			samples := assertWAVAudio(t, stop.File, 1, 16000, 100)
			got := time.Duration(samples) * time.Second / 16000
			if want := recorded - 500*time.Millisecond; got < want {
				t.Errorf("room mix holds %v of audio, want at least %v: the recording ran for %v",
					got.Round(10*time.Millisecond), want.Round(10*time.Millisecond), recorded.Round(10*time.Millisecond))
			}

			httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), secondID)).Body.Close()
		})
	}
}
