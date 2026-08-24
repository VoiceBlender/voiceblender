//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/gobwas/ws/wsutil"
)

// stallingSender writes one 20ms PCM frame per tick, but every stallEvery
// frames it goes quiet for stallFor and then dumps the skipped frames at once.
// The long-run rate stays exactly real time, so the only thing a playout lead
// has to absorb is the transient.
func stallingSender(t *testing.T, conn interface{ Write([]byte) (int, error) }, stop <-chan struct{}) {
	t.Helper()
	const (
		stallEvery = 35                     // frames, i.e. 700ms
		stallFor   = 150 * time.Millisecond // 7 frames' worth, inside a 200ms lead
	)
	frame := make([]byte, 640) // 20ms @ 16kHz PCM16
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(frame[i*2:], uint16(int16(6000)))
	}

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for sent := 0; ; sent++ {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if err := wsutil.WriteClientBinary(conn, frame); err != nil {
			return
		}
		if sent > 0 && sent%stallEvery == 0 {
			select {
			case <-stop:
				return
			case <-time.After(stallFor):
			}
			for c := 0; c < int(stallFor/(20*time.Millisecond)); c++ {
				if err := wsutil.WriteClientBinary(conn, frame); err != nil {
					return
				}
			}
		}
	}
}

// starvedOverWindow returns how many mix ticks found no frame for legID during
// a steady-state window, skipping the ramp-up.
func starvedOverWindow(t *testing.T, inst *testInstance, roomID, legID string, window time.Duration) uint64 {
	t.Helper()
	rm, ok := inst.roomMgr.Get(roomID)
	if !ok {
		t.Fatalf("room %s not found", roomID)
	}
	_, before, _, ok := rm.Mixer().ParticipantFeed(legID)
	if !ok {
		t.Fatalf("leg %s is not a mixer participant", legID)
	}
	time.Sleep(window)
	_, after, _, ok := rm.Mixer().ParticipantFeed(legID)
	if !ok {
		t.Fatalf("leg %s left the mixer mid-window", legID)
	}
	return after - before
}

// runIngress connects a WS leg driven by send and reports how often the mixer
// starved on it in steady state.
func runIngress(t *testing.T, name, roomID string, jitterMs int, send func(*testing.T, interface{ Write([]byte) (int, error) }, <-chan struct{})) uint64 {
	t.Helper()
	inst := newTestInstanceWithOpts(t, name, func(c *config.Config) {
		c.WSJitterBufferMs = jitterMs
	})

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=" + roomID
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := wsDial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)

	stop := make(chan struct{})
	defer close(stop)
	go send(t, conn, stop)

	// Let the playout lead warm up and the stream settle before measuring.
	time.Sleep(700 * time.Millisecond)
	starved := starvedOverWindow(t, inst, roomID, legID, 3*time.Second)
	t.Logf("WS_JITTER_BUFFER_MS=%d: %d starved ticks over 3s (~150 ticks)", jitterMs, starved)
	return starved
}

func TestRoomWSJitterBufferAbsorbsIngressStalls(t *testing.T) {
	off := runIngress(t, "ws-jitter-off", "jitter-room", 0, stallingSender)
	on := runIngress(t, "ws-jitter-on", "jitter-room", 200, stallingSender)

	// The stall pattern has to be adversarial enough to matter, or the
	// comparison below proves nothing.
	if off < 5 {
		t.Fatalf("passthrough starved only %d times — the stall pattern is not adversarial", off)
	}
	// A stall needs a lead at least its own size — that is what the lead is
	// for. Allow a couple of ticks for scheduling noise on a loaded machine.
	if on > 2 {
		t.Fatalf("playout lead starved %d times vs %d without it — not absorbing the stalls", on, off)
	}
}

func TestRoomPlaybackMixerClockedNoStall(t *testing.T) {
	inst := newTestInstance(t, "playback-mixer-clocked")

	wsURL := "ws://" + inst.httpAddr +
		"/v1/legs/websocket?sample_rate=16000&wire_format=binary&room_id=pb-room"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := wsDial(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegJoinedRoom, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)

	playResp := httpPost(t, inst.baseURL()+"/v1/rooms/pb-room/play", map[string]any{
		"tone":   "us_dial",
		"repeat": -1,
	})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("play: status=%d", playResp.StatusCode)
	}
	var play struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, playResp, &play)
	if play.PlaybackID == "" {
		t.Fatal("play response carried no playback_id")
	}

	rm, ok := inst.roomMgr.Get("pb-room")
	if !ok {
		t.Fatal("room pb-room not found")
	}

	// The playback source is mixer-clocked, so its read loop blocks on a full
	// queue. Over a few seconds it must keep delivering a frame per tick and
	// never wedge the mix loop.
	time.Sleep(500 * time.Millisecond)
	first, starvedBefore, _, ok := rm.Mixer().ParticipantFeed(play.PlaybackID)
	if !ok {
		t.Fatalf("playback source %s is not a mixer participant", play.PlaybackID)
	}
	time.Sleep(2 * time.Second)
	last, starvedAfter, _, ok := rm.Mixer().ParticipantFeed(play.PlaybackID)
	if !ok {
		t.Fatalf("playback source %s vanished mid-test", play.PlaybackID)
	}

	const window = 2 * time.Second
	wantFrames := uint64(window/(20*time.Millisecond)) / 2 // half the nominal rate is plenty of margin
	gotFrames := last - first
	t.Logf("playback delivered %d frames in %v, starved %d ticks", gotFrames, window, starvedAfter-starvedBefore)
	if gotFrames < wantFrames {
		t.Fatalf("playback delivered %d frames in %v, want at least %d — the mixer-clocked read loop stalled",
			gotFrames, window, wantFrames)
	}
}

// driftingSender paces frames slightly slower than the mixer's 20ms tick, the
// way a naive `send(); sleep(20ms)` producer does. The long-run rate is short
// by a fixed fraction, so no amount of buffering can conserve every sample —
// something has to give a frame back. It alternates speech and pauses so the
// buffer has somewhere cheap to take it from.
func driftingSender(t *testing.T, conn interface{ Write([]byte) (int, error) }, stop <-chan struct{}) {
	t.Helper()
	const (
		framePeriod  = 20550 * time.Microsecond // 2.75% slow, as in the reported capture
		cycleFrames  = 70                       // 1s of speech then 0.4s of pause
		speechFrames = 50
	)
	voiced := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(voiced[i*2:], uint16(int16(6000)))
	}
	quiet := make([]byte, 640)

	next := time.Now()
	for i := 0; ; i++ {
		next = next.Add(framePeriod)
		select {
		case <-stop:
			return
		case <-time.After(time.Until(next)):
		}
		frame := voiced
		if i%cycleFrames >= speechFrames {
			frame = quiet
		}
		if err := wsutil.WriteClientBinary(conn, frame); err != nil {
			return
		}
	}
}

func TestRoomWSJitterBufferAbsorbsSlowProducerDrift(t *testing.T) {
	off := runIngress(t, "ws-drift-off", "drift-room", 0, driftingSender)
	on := runIngress(t, "ws-drift-on", "drift-room", 60, driftingSender)

	if off < 3 {
		t.Fatalf("passthrough starved only %d times — the producer is not drifting enough to test", off)
	}
	// The deficit is real: 20ms of audio is genuinely missing every ~730ms.
	// Correcting it inside a pause costs nothing and needs no meaningful lead —
	// 60ms is here to prove the point, not because drift requires it.
	if on > 1 {
		t.Fatalf("drift compensation left %d gaps vs %d without it", on, off)
	}
}
