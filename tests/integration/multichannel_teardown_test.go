//go:build integration

package integration

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// A multi-channel room recording must keep every leg however the call is torn
// down. The hang-up of a leg and the delete of its room are separate requests,
// and a controller sends them back to back, so the room's stop routinely runs
// while a leg's own capture is still being finalised.

// multiChannelCall is a two-leg room on instA with a multi-channel recording
// running. far holds the matching inbound legs on instB.
type multiChannelCall struct {
	roomID string
	legs   [2]string
	far    [2]string
}

func startMultiChannelCall(t *testing.T, instA, instB *testInstance) multiChannelCall {
	t.Helper()

	var c multiChannelCall
	c.legs[0], c.far[0] = establishCall(t, instA, instB)
	c.legs[1], c.far[1] = establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)
	c.roomID = rm.ID

	for _, id := range c.legs {
		addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID),
			map[string]interface{}{"leg_id": id})
		if addResp.StatusCode != http.StatusOK {
			t.Fatalf("add leg to room: status %d", addResp.StatusCode)
		}
		addResp.Body.Close()
		instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == id && e.Data.GetRoomID() == rm.ID
		}, 3*time.Second)
	}

	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID),
		map[string]interface{}{"multi_channel": true})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start multi-channel recording: status %d", recResp.StatusCode)
	}
	recResp.Body.Close()
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	time.Sleep(time.Second)
	return c
}

func deleteAccepted(t *testing.T, url string, want int) {
	t.Helper()
	resp := httpDelete(t, url)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("DELETE %s: status %d, want %d", url, resp.StatusCode, want)
	}
}

// assertMultiChannelComplete waits for the room's recording.finished and checks
// that the merged file holds one channel per leg and reports nothing omitted.
func assertMultiChannelComplete(t *testing.T, inst *testInstance, c multiChannelCall) {
	t.Helper()

	e := inst.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == c.roomID
	}, 5*time.Second)
	d, ok := e.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatalf("recording.finished data type = %T", e.Data)
	}

	if len(d.OmittedLegs) != 0 {
		t.Errorf("omitted_legs = %v, want none", d.OmittedLegs)
	}
	if d.MultiChannelFile == "" {
		t.Fatalf("recording.finished has no multi_channel_file (omitted_legs = %v)", d.OmittedLegs)
	}
	for _, id := range c.legs {
		if _, ok := d.Channels[id]; !ok {
			t.Errorf("leg %s has no channel; channels = %v", id, d.Channels)
		}
	}
	if t.Failed() {
		return
	}
	assertWAVAudio(t, d.MultiChannelFile, 2, 16000, 100)

	// A second recording.finished for the room would mean two stops both claimed it.
	time.Sleep(300 * time.Millisecond)
	if n := len(inst.collector.matchAll(events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == c.roomID
	})); n != 1 {
		t.Errorf("recording.finished published %d times for the room, want 1", n)
	}
}

// TestMultiChannel_HangupLegsThenDeleteRoom is the controller's teardown: both
// legs hung up and the room deleted with no wait between the requests.
func TestMultiChannel_HangupLegsThenDeleteRoom(t *testing.T) {
	instA := newTestInstance(t, "mc-teardown-a")
	instB := newTestInstance(t, "mc-teardown-b")
	c := startMultiChannelCall(t, instA, instB)

	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), c.legs[0]), http.StatusAccepted)
	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), c.legs[1]), http.StatusAccepted)
	deleteAccepted(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), c.roomID), http.StatusOK)

	assertMultiChannelComplete(t, instA, c)
}

// TestMultiChannel_HangupBothLegs has no room delete. The two legs leave at
// the same time, each can find the room empty, and both then stop the recording.
func TestMultiChannel_HangupBothLegs(t *testing.T) {
	instA := newTestInstance(t, "mc-hangup-a")
	instB := newTestInstance(t, "mc-hangup-b")
	c := startMultiChannelCall(t, instA, instB)

	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), c.legs[0]), http.StatusAccepted)
	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), c.legs[1]), http.StatusAccepted)

	assertMultiChannelComplete(t, instA, c)
}

// onLegDisconnected returns a channel that is closed as soon as instance
// publishes leg.disconnected for legID. The collector polls, which is too slow
// for a test that must act at once.
func onLegDisconnected(t *testing.T, inst *testInstance, legID string) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	unsub := inst.bus.Subscribe(func(e events.Event) {
		if e.Type == events.LegDisconnected && e.Data.GetLegID() == legID {
			once.Do(func() { close(ch) })
		}
	})
	t.Cleanup(unsub)
	return ch
}

// TestMultiChannel_RemoteByeThenDeleteRoom is a call ended by the far end: one
// leg receives BYE and the controller deletes the room as soon as it hears.
func TestMultiChannel_RemoteByeThenDeleteRoom(t *testing.T) {
	instA := newTestInstance(t, "mc-bye-a")
	instB := newTestInstance(t, "mc-bye-b")
	c := startMultiChannelCall(t, instA, instB)

	gone := onLegDisconnected(t, instA, c.legs[0])
	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), c.far[0]), http.StatusAccepted)
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("no leg.disconnected after the far end hung up")
	}
	deleteAccepted(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), c.roomID), http.StatusOK)

	assertMultiChannelComplete(t, instA, c)
}

// TestMultiChannel_RemoteByeThenHangupAndDeleteRoom is the same call end where
// the controller also hangs up the surviving leg before it deletes the room.
func TestMultiChannel_RemoteByeThenHangupAndDeleteRoom(t *testing.T) {
	instA := newTestInstance(t, "mc-bye-hangup-a")
	instB := newTestInstance(t, "mc-bye-hangup-b")
	c := startMultiChannelCall(t, instA, instB)

	gone := onLegDisconnected(t, instA, c.legs[0])
	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instB.baseURL(), c.far[0]), http.StatusAccepted)
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("no leg.disconnected after the far end hung up")
	}
	deleteAccepted(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), c.legs[1]), http.StatusAccepted)
	deleteAccepted(t, fmt.Sprintf("%s/v1/rooms/%s", instA.baseURL(), c.roomID), http.StatusOK)

	assertMultiChannelComplete(t, instA, c)
}
