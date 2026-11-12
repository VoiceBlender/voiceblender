//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

const recAppID = "rec-app"

// establishCallWithApp is establishCall with an app_id on the outbound leg.
func establishCallWithApp(t *testing.T, instA, instB *testInstance, appID string) string {
	t.Helper()

	createResp := httpPost(t, instA.baseURL()+"/v1/legs", map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:test@127.0.0.1:%d", instB.sipPort),
		"codecs": []string{"PCMU"},
		"app_id": appID,
	})
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create leg: unexpected status %d", createResp.StatusCode)
	}
	var outbound legView
	decodeJSON(t, createResp, &outbound)

	inbound := waitForInboundLeg(t, instB.baseURL(), 5*time.Second)
	answerResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", instB.baseURL(), inbound.ID), nil)
	if answerResp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer: unexpected status %d", answerResp.StatusCode)
	}
	answerResp.Body.Close()

	waitForLegState(t, instA.baseURL(), outbound.ID, "connected", 5*time.Second)
	return outbound.ID
}

// joinNewRoom creates a room and adds the leg to it.
func joinNewRoom(t *testing.T, inst *testInstance, legID, appID string) string {
	t.Helper()

	roomResp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{"app_id": appID})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": legID,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()

	joined := inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
	if got := joined.Data.GetAppID(); got != appID {
		t.Fatalf("leg.joined_room app_id = %q, want %q", got, appID)
	}
	return rm.ID
}

// recordThenDeleteLeg starts a recording on the leg, checks recording.started
// carries the app, then ends the recording by deleting the leg.
func recordThenDeleteLeg(t *testing.T, inst *testInstance, legID, appID string) {
	t.Helper()

	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", inst.baseURL(), legID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	recResp.Body.Close()

	started := inst.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
	if got := started.Data.GetAppID(); got != appID {
		t.Fatalf("recording.started app_id = %q, want %q", got, appID)
	}

	time.Sleep(200 * time.Millisecond)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()
}

func assertFinishedCarriesApp(t *testing.T, inst *testInstance, legID, appID string) {
	t.Helper()
	fin := inst.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 5*time.Second)
	if got := fin.Data.GetAppID(); got != appID {
		t.Errorf("recording.finished app_id = %q, want %q", got, appID)
	}
}

// DELETE /v1/legs/{id} takes the leg out of the manager before its recording
// stops, so recording.finished cannot rely on finding the leg by ID.
func TestRecording_FinishedCarriesAppIDOnLegDelete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inRoom bool
	}{
		{"standalone", false},
		{"in_room", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instA := newTestInstance(t, "instance-a")
			instB := newTestInstance(t, "instance-b")
			legID := establishCallWithApp(t, instA, instB, recAppID)

			if tc.inRoom {
				joinNewRoom(t, instA, legID, recAppID)
			}

			recordThenDeleteLeg(t, instA, legID, recAppID)
			assertFinishedCarriesApp(t, instA, legID, recAppID)
		})
	}
}

// The event stream drops events whose app_id does not match the client's
// filter, so an empty app_id hides recording.finished from the leg's own app.
func TestRecording_FinishedReachesAppFilteredStream(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	conn := dialVSIFiltered(t, instA, "^"+recAppID+"$")
	defer conn.Close()
	if f := readWSFrame(t, conn, 5*time.Second); f.Type != "connected" {
		t.Fatalf("first frame type = %q, want connected", f.Type)
	}

	legID := establishCallWithApp(t, instA, instB, recAppID)
	recordThenDeleteLeg(t, instA, legID, recAppID)

	var started, finished bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !finished {
		f, ok := tryReadWSFrame(conn, time.Until(deadline))
		if !ok {
			break
		}
		if f.LegID != legID {
			continue
		}
		switch f.Type {
		case string(events.RecordingStarted):
			started = true
		case string(events.RecordingFinished):
			finished = true
		case string(events.LegDisconnected):
			// recording.finished is published before leg.disconnected, so
			// allow only a short grace once the leg is reported gone.
			if d := time.Now().Add(time.Second); d.Before(deadline) {
				deadline = d
			}
		}
	}
	if !started {
		t.Fatal("filtered client did not receive recording.started, so this test proves nothing")
	}
	if !finished {
		t.Error("filtered client did not receive recording.finished after the leg was deleted")
	}
}

// A SIPREC session recording is stopped through the same leg teardown.
func TestSIPREC_RecordingFinishedCarriesAppIDOnLegDelete(t *testing.T) {
	src := newTestInstance(t, "src")
	srs := siprecInstance(t, "srs", nil)

	call, err := dialSIPREC(t, src, srs, twoPartyMetadata(t), sip.NewHeader("X-App-ID", recAppID))
	if err != nil {
		t.Fatalf("SIPREC INVITE failed: %v", err)
	}
	defer call.Dialog.Bye(context.Background())

	evt := srs.collector.waitForMatch(t, events.SIPRECSessionStarted, nil, 5*time.Second)
	legID := evt.Data.GetLegID()

	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", srs.baseURL(), legID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /record = %d, want 200", recResp.StatusCode)
	}
	recResp.Body.Close()
	started := srs.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 3*time.Second)
	if got := started.Data.GetAppID(); got != recAppID {
		t.Fatalf("recording.started app_id = %q, want %q", got, recAppID)
	}

	sendSIPRECAudio(t, call, 25)

	delResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", srs.baseURL(), legID))
	if delResp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg: unexpected status %d", delResp.StatusCode)
	}
	delResp.Body.Close()

	assertFinishedCarriesApp(t, srs, legID, recAppID)
}
