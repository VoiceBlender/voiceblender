//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/recording"
	goaudio "github.com/go-audio/audio"
	"github.com/go-audio/wav"
)

// multiChannelRecordingResponse extends recordingResponse with multi-channel fields.
type multiChannelRecordingResponse struct {
	Status           string                          `json:"status"`
	File             string                          `json:"file"`
	MultiChannelFile string                          `json:"multi_channel_file"`
	Channels         map[string]recording.ChannelInfo `json:"channels"`
}

// ---------------------------------------------------------------------------
// Multi-Channel Recording Tests
// ---------------------------------------------------------------------------

func TestMultiChannel_RoomRecording(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Establish two calls so we have two legs to put in the room.
	outboundID1, _ := establishCall(t, instA, instB)
	outboundID2, _ := establishCall(t, instA, instB)

	// Create room.
	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: unexpected status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	// Add both legs to the room.
	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID1,
	})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg 1 to room: unexpected status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID1
	}, 3*time.Second)

	addResp2 := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID2,
	})
	if addResp2.StatusCode != http.StatusOK {
		t.Fatalf("add leg 2 to room: unexpected status %d", addResp2.StatusCode)
	}
	addResp2.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID2
	}, 3*time.Second)

	// Start multi-channel room recording.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), map[string]interface{}{
		"multi_channel": true,
	})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start multi-channel recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("expected status 'recording', got %q", recStart.Status)
	}
	if !strings.HasSuffix(recStart.File, ".wav") {
		t.Fatalf("expected .wav file, got %q", recStart.File)
	}

	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// Let it record for a bit.
	time.Sleep(500 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	var recStop multiChannelRecordingResponse
	decodeJSON(t, stopResp, &recStop)

	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}

	// Verify the mix file exists and is valid mono 16kHz.
	assertWAVAudio(t, recStop.File, 1, 16000, 100)

	// Verify multi-channel file.
	if recStop.MultiChannelFile == "" {
		t.Fatal("expected multi_channel_file in response")
	}
	if !strings.HasSuffix(recStop.MultiChannelFile, ".wav") {
		t.Fatalf("expected .wav multi-channel file, got %q", recStop.MultiChannelFile)
	}

	// The multi-channel file should have 2 channels (one per leg).
	assertWAVAudio(t, recStop.MultiChannelFile, 2, 16000, 100)

	// Verify channel metadata.
	if len(recStop.Channels) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(recStop.Channels))
	}

	ch1, ok1 := recStop.Channels[outboundID1]
	ch2, ok2 := recStop.Channels[outboundID2]
	if !ok1 {
		t.Fatalf("channel metadata missing for leg %s", outboundID1)
	}
	if !ok2 {
		t.Fatalf("channel metadata missing for leg %s", outboundID2)
	}

	// Both legs were present from the start — start_ms should be 0 for both.
	if ch1.StartMs != 0 {
		t.Fatalf("expected leg1 start_ms=0, got %d", ch1.StartMs)
	}
	if ch2.StartMs != 0 {
		t.Fatalf("expected leg2 start_ms=0, got %d", ch2.StartMs)
	}
	// end_ms should be > 0 for both.
	if ch1.EndMs <= 0 {
		t.Fatalf("expected leg1 end_ms > 0, got %d", ch1.EndMs)
	}
	if ch2.EndMs <= 0 {
		t.Fatalf("expected leg2 end_ms > 0, got %d", ch2.EndMs)
	}
	// Channels should be 0 and 1 (in some order).
	if ch1.Channel == ch2.Channel {
		t.Fatalf("both legs got the same channel index %d", ch1.Channel)
	}

	// Verify recording.finished event includes multi-channel data.
	evt := instA.collector.waitForMatch(t, events.RecordingFinished, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)
	finData, ok := evt.Data.(*events.RecordingFinishedData)
	if !ok {
		t.Fatal("recording.finished event data is not RecordingFinishedData")
	}
	if finData.MultiChannelFile == "" {
		t.Fatal("recording.finished event missing multi_channel_file")
	}
	if len(finData.Channels) != 2 {
		t.Fatalf("recording.finished event: expected 2 channels, got %d", len(finData.Channels))
	}

	t.Logf("multi-channel file: %s", recStop.MultiChannelFile)
	t.Logf("channels: %+v", recStop.Channels)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID1))
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID2))
}

func TestMultiChannel_LateJoiner(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// Start with one leg in the room.
	outboundID1, _ := establishCall(t, instA, instB)
	outboundID2, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID1,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID1
	}, 3*time.Second)

	// Start multi-channel recording with only leg1.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), map[string]interface{}{
		"multi_channel": true,
	})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	// Wait a bit, then add leg2 (late joiner).
	time.Sleep(300 * time.Millisecond)

	addResp2 := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID2,
	})
	if addResp2.StatusCode != http.StatusOK {
		t.Fatalf("add leg 2: unexpected status %d", addResp2.StatusCode)
	}
	addResp2.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == outboundID2
	}, 3*time.Second)

	// Let both record for a bit.
	time.Sleep(300 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
	var recStop multiChannelRecordingResponse
	decodeJSON(t, stopResp, &recStop)

	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}

	// Multi-channel file should have 2 channels.
	if recStop.MultiChannelFile == "" {
		t.Fatal("expected multi_channel_file in response")
	}
	assertWAVAudio(t, recStop.MultiChannelFile, 2, 16000, 100)

	// Verify channel metadata — leg2 should have start_ms > 0 (late joiner).
	ch1 := recStop.Channels[outboundID1]
	ch2 := recStop.Channels[outboundID2]

	if ch1.StartMs != 0 {
		t.Fatalf("leg1 was present from start, expected start_ms=0, got %d", ch1.StartMs)
	}
	if ch2.StartMs == 0 {
		t.Fatal("leg2 joined late, expected start_ms > 0")
	}
	// The late joiner should have start_ms roughly around 300ms (±200ms tolerance).
	if ch2.StartMs < 100 || ch2.StartMs > 1000 {
		t.Fatalf("leg2 start_ms=%d seems out of expected range (100-1000)", ch2.StartMs)
	}

	t.Logf("leg1 channel: %+v", ch1)
	t.Logf("leg2 channel: %+v (late joiner)", ch2)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID1))
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID2))
}

func TestMultiChannel_EarlyLeaver(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	outboundID1, _ := establishCall(t, instA, instB)
	outboundID2, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	// Add both legs.
	for _, id := range []string{outboundID1, outboundID2} {
		addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
			"leg_id": id,
		})
		addResp.Body.Close()
		instA.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
			return e.Data.GetLegID() == id
		}, 3*time.Second)
	}

	// Start multi-channel recording.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), map[string]interface{}{
		"multi_channel": true,
	})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	recResp.Body.Close()
	instA.collector.waitForMatch(t, events.RecordingStarted, func(e events.Event) bool {
		return e.Data.GetRoomID() == rm.ID
	}, 3*time.Second)

	time.Sleep(300 * time.Millisecond)

	// Remove leg2 from the room (early leaver).
	removeResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/legs/%s", instA.baseURL(), rm.ID, outboundID2))
	if removeResp.StatusCode != http.StatusOK {
		t.Fatalf("remove leg 2: unexpected status %d", removeResp.StatusCode)
	}
	removeResp.Body.Close()

	// Let leg1 continue recording.
	time.Sleep(300 * time.Millisecond)

	// Stop recording.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
	var recStop multiChannelRecordingResponse
	decodeJSON(t, stopResp, &recStop)

	if recStop.Status != "stopped" {
		t.Fatalf("expected status 'stopped', got %q", recStop.Status)
	}
	if recStop.MultiChannelFile == "" {
		t.Fatal("expected multi_channel_file in response")
	}

	// File should have 2 channels.
	assertWAVAudio(t, recStop.MultiChannelFile, 2, 16000, 100)

	// Verify channel metadata — leg2 should have end_ms < leg1's end_ms.
	ch1 := recStop.Channels[outboundID1]
	ch2 := recStop.Channels[outboundID2]

	if ch2.EndMs >= ch1.EndMs {
		t.Fatalf("leg2 (early leaver) end_ms=%d should be less than leg1 end_ms=%d", ch2.EndMs, ch1.EndMs)
	}

	t.Logf("leg1 channel: %+v (stayed)", ch1)
	t.Logf("leg2 channel: %+v (left early)", ch2)

	// Verify the multi-channel file's second channel has silence in the tail.
	// Open the file and check that the last portion of channel 2 is silence.
	assertChannelHasSilentTail(t, recStop.MultiChannelFile, ch2.Channel, 2)

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID1))
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID2))
}

func TestMultiChannel_NoMultiChannelFlag(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")
	outboundID, _ := establishCall(t, instA, instB)

	roomResp := httpPost(t, instA.baseURL()+"/v1/rooms", map[string]interface{}{})
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", instA.baseURL(), rm.ID), map[string]interface{}{
		"leg_id": outboundID,
	})
	addResp.Body.Close()
	instA.collector.waitForMatch(t, events.LegJoinedRoom, nil, 3*time.Second)

	// Start recording WITHOUT multi_channel.
	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	recResp.Body.Close()

	time.Sleep(300 * time.Millisecond)

	// Stop recording — should NOT have multi-channel fields.
	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", instA.baseURL(), rm.ID))
	var raw map[string]json.RawMessage
	decodeJSON(t, stopResp, &raw)

	if _, ok := raw["multi_channel_file"]; ok {
		t.Fatal("unexpected multi_channel_file in non-multi-channel response")
	}
	if _, ok := raw["channels"]; ok {
		t.Fatal("unexpected channels in non-multi-channel response")
	}

	// Cleanup.
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", instA.baseURL(), outboundID))
}

// assertChannelHasSilentTail checks that the last 10% of samples in the given
// channel of a multi-channel WAV file are silence (zero).
func assertChannelHasSilentTail(t *testing.T, path string, channelIdx, numChannels int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open WAV: %v", err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatalf("invalid WAV: %s", path)
	}

	// Read all samples.
	buf := &goaudio.IntBuffer{
		Data:   make([]int, 4096),
		Format: &goaudio.Format{SampleRate: int(dec.SampleRate), NumChannels: int(dec.NumChans)},
	}
	var channelSamples []int
	for {
		n, err := dec.PCMBuffer(buf)
		if n > 0 {
			// Extract samples for the target channel from interleaved data.
			for i := channelIdx; i < n; i += numChannels {
				channelSamples = append(channelSamples, buf.Data[i])
			}
		}
		if err != nil || n == 0 {
			break
		}
	}

	if len(channelSamples) < 20 {
		t.Fatalf("too few samples (%d) to check silence tail", len(channelSamples))
	}

	// Check the last 10% of samples are silence.
	tailStart := len(channelSamples) - len(channelSamples)/10
	nonZero := 0
	for _, s := range channelSamples[tailStart:] {
		if s != 0 {
			nonZero++
		}
	}

	// Allow a small number of non-zero samples (noise/codec artifacts).
	maxNonZero := len(channelSamples[tailStart:]) / 10 // 10% tolerance
	if nonZero > maxNonZero {
		t.Fatalf("channel %d tail has %d non-zero samples out of %d (expected mostly silence)",
			channelIdx, nonZero, len(channelSamples[tailStart:]))
	}
	t.Logf("channel %d silent tail: %d/%d samples non-zero", channelIdx, nonZero, len(channelSamples[tailStart:]))
}
