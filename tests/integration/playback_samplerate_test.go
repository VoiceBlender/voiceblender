//go:build integration

package integration

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
)

// TestPlaybackCrossSampleRate exercises every supported leg-rate × room-rate
// combination the platform can produce in real deployments: a low-rate
// telephony leg (8 kHz), the mixer default (16 kHz), and high-rate WebRTC /
// MoQ peers (48 kHz). Each combination runs the same scenario that caused
// the high-pitched-TTS bug: a leg-level tone playback is started while the
// leg has no room, then the leg is added to a room with a different mixer
// rate mid-stream. The captured WS egress must hold the original tone
// frequency through both halves of the test.
//
// Without the fix in legPlaybackWriter, the inject channel receives raw PCM
// at the producer's rate, the room mixer reinterprets the bytes as its own
// rate, and the captured tone shifts pitch (e.g. 425 Hz → ~1275 Hz when a
// 16 kHz playback is routed through a 48 kHz mixer).
func TestPlaybackCrossSampleRate(t *testing.T) {
	cases := []struct {
		name     string
		roomRate int
		legRate  int
	}{
		{"room16k_leg16k_baseline", 16000, 16000},
		{"room48k_leg16k_upsample_inject", 48000, 16000},
		{"room8k_leg16k_downsample_inject", 8000, 16000},
		{"room48k_leg8k_extreme_cross", 48000, 8000},
		{"room8k_leg48k_legHigh", 8000, 48000},
		{"room16k_leg48k_legHigh", 16000, 48000},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runPlaybackRateCase(t, tc.roomRate, tc.legRate)
		})
	}
}

func runPlaybackRateCase(t *testing.T, roomRate, legRate int) {
	t.Helper()

	inst := newTestInstanceWithOpts(t, fmt.Sprintf("rate-r%d-l%d", roomRate, legRate),
		func(c *config.Config) { c.DefaultSampleRate = roomRate })

	// Open a WS leg at legRate, NOT in any room. This mirrors the IVR's
	// "play menu before the caller has been placed in any room" state.
	wsURL := fmt.Sprintf("ws://%s/v1/legs/websocket?sample_rate=%d&wire_format=binary",
		inst.httpAddr, legRate)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	conn, _, _, err := ws.Dial(dialCtx, wsURL)
	if err != nil {
		t.Fatalf("dial ws leg: %v", err)
	}
	defer conn.Close()

	ringing := inst.collector.waitForMatch(t, events.LegRinging, nil, 2*time.Second)
	legID := ringing.Data.GetLegID()
	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, 2*time.Second)
	t.Cleanup(func() {
		httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	})

	// Pure single-frequency tone makes the FFT/zc check unambiguous.
	const toneName = "de_dial"
	const toneFreq = 425.0

	// Frame: 20 ms × legRate × 2 bytes/sample.
	legFrameBytes := legRate * 20 / 1000 * 2

	// Audio capture starts before playback so we see whether the WS
	// pipeline ever delivers a frame before the leg-room move.
	type capture struct {
		frames       []byte
		preRoomCount int
		postRoomMark int // index in `frames` where the leg moved into the room
	}
	capCh := make(chan capture, 1)
	roomMoved := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		var c capture
		moved := false
		readDeadline := time.Now().Add(10 * time.Second)
		for {
			select {
			case <-stop:
				capCh <- c
				return
			default:
			}
			select {
			case <-roomMoved:
				if !moved {
					c.postRoomMark = len(c.frames)
					moved = true
				}
			default:
			}
			if time.Now().After(readDeadline) {
				capCh <- c
				return
			}
			wsutilx.SetReadDeadline(conn, 200*time.Millisecond)
			hdr, err := ws.ReadHeader(conn)
			if err != nil {
				// Likely deadline; loop to recheck stop/moved.
				continue
			}
			payload := make([]byte, hdr.Length)
			if _, rerr := io.ReadFull(conn, payload); rerr != nil {
				continue
			}
			if hdr.Masked {
				ws.Cipher(payload, hdr.Mask, 0)
			}
			if hdr.OpCode != ws.OpBinary {
				continue
			}
			if len(payload) != legFrameBytes {
				t.Errorf("unexpected ws frame size %d, want %d", len(payload), legFrameBytes)
				continue
			}
			c.frames = append(c.frames, payload...)
			if !moved {
				c.preRoomCount++
			}
		}
	}()

	// Phase 1: leg is not in any room → direct path through legPlaybackWriter.
	playResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", inst.baseURL(), legID),
		map[string]any{"tone": toneName, "repeat": -1})
	if playResp.StatusCode != http.StatusOK {
		t.Fatalf("start leg play: status %d", playResp.StatusCode)
	}
	playResp.Body.Close()

	// Capture ~800 ms of audio with leg out of room.
	time.Sleep(800 * time.Millisecond)

	// Phase 2: add the leg to a room with `roomRate`. The mixer auto-creates
	// at the instance's DefaultSampleRate. Playback continues to flow but
	// now hits the inject path inside legPlaybackWriter.
	roomID := "rate-room"
	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), roomID),
		map[string]any{"leg_id": legID})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", addResp.StatusCode)
	}
	addResp.Body.Close()
	close(roomMoved)

	// Capture another ~1500 ms while the leg is in the (potentially
	// different-rate) room.
	time.Sleep(1500 * time.Millisecond)

	close(stop)
	cap := <-capCh

	if cap.preRoomCount < 10 {
		t.Fatalf("only %d pre-room frames captured, expected >=10", cap.preRoomCount)
	}
	if cap.postRoomMark >= len(cap.frames) {
		t.Fatalf("no post-room audio captured (mark=%d, len=%d)", cap.postRoomMark, len(cap.frames))
	}
	preBytes := cap.frames[:cap.postRoomMark]
	postBytes := cap.frames[cap.postRoomMark:]

	// Drop the first ~3 frames post-move; they're a transitional region
	// (RTP/jitter buffer settle for SIP wouldn't apply here, but the
	// mixer needs a tick or two to pick up the leg as a participant).
	dropBytes := 3 * legFrameBytes
	if len(postBytes) <= dropBytes {
		t.Fatalf("post-room audio too short: %d bytes (drop=%d)", len(postBytes), dropBytes)
	}
	postBytes = postBytes[dropBytes:]

	// The producer's source rate when playback starts outside any room
	// is the hardcoded mixer default (16 kHz). Without the fix, the
	// inject path would reinterpret these bytes at roomRate, shifting
	// pitch by roomRate / srcRate. Track that explicitly so the
	// downsample cases (0.5×) get a tight check, not just the upsample
	// cases (3×).
	const srcRate = 16000
	shift := float64(roomRate) / float64(srcRate)
	decoys := []float64{
		toneFreq * 3,
		toneFreq / 3,
		toneFreq * shift,
		toneFreq / shift,
	}

	// Pre-room: leg was not in a room, so legPlaybackWriter routed through
	// the direct path. This path always worked, so the tone should be
	// present at the right frequency.
	preEnergy := assertDominantFrequency(t, "pre-room", preBytes, legRate, toneFreq, decoys)
	// Post-room: this is the path that broke the user's IVR. Verify the
	// tone is *not* pitch-shifted by the room's sample-rate mismatch.
	postEnergy := assertDominantFrequency(t, "post-room", postBytes, legRate, toneFreq, decoys)

	// Even if no single decoy bin dominates, a buggy resample shreds the
	// signal across many bins and drops the target-frequency energy. Hold
	// the post-room target energy to within 10× of the pre-room baseline
	// so silent-but-glitchy outputs still fail.
	if postEnergy < preEnergy/10 {
		t.Errorf("post-room %g Hz energy %.3g is >10× lower than pre-room %.3g — signal degraded by sample-rate mismatch",
			toneFreq, postEnergy, preEnergy)
	}
}

// assertDominantFrequency runs a Goertzel filter at `wantHz` and at
// each frequency in `decoys` on `pcm` mono 16-bit LE at `sampleRate`.
// The energy at `wantHz` must exceed every decoy by at least 6 dB.
// Decoys should be the pitch shifts the bug would produce given the
// leg/room rate ratio (e.g. 3×, 0.5×). Returns the measured target
// energy so callers can also assert it stays stable across segments.
func assertDominantFrequency(t *testing.T, label string, pcm []byte, sampleRate int, wantHz float64, decoys []float64) float64 {
	t.Helper()
	const minMs = 300
	minBytes := sampleRate * minMs / 1000 * 2
	if len(pcm) < minBytes {
		t.Fatalf("[%s] only %d bytes of audio captured (need >= %d)", label, len(pcm), minBytes)
	}

	samples := bytesToInt16(pcm)
	start := 0
	for start < len(samples) {
		if absInt16(samples[start]) > 200 {
			break
		}
		start++
	}
	if start >= len(samples)-sampleRate/4 {
		t.Fatalf("[%s] audio appears silent (%d/%d below threshold)", label, start, len(samples))
	}
	samples = samples[start:]

	target := goertzelPower(samples, sampleRate, wantHz)
	t.Logf("[%s] sr=%d target=%gHz energy=%.3g", label, sampleRate, wantHz, target)

	const minRatio = 4.0 // ~6 dB
	for _, d := range decoys {
		if d <= 0 || math.Abs(d-wantHz) < 5 {
			continue
		}
		if d >= float64(sampleRate)/2 {
			continue // above Nyquist; Goertzel reflects, not meaningful
		}
		dp := goertzelPower(samples, sampleRate, d)
		ratio := target / math.Max(dp, 1)
		t.Logf("[%s]   decoy %.0fHz energy=%.3g ratio target/decoy=%.2f", label, d, dp, ratio)
		if ratio < minRatio {
			t.Errorf("[%s] %g Hz energy %.3g not dominant over %g Hz energy %.3g (ratio %.2f) — pitch-shifted",
				label, wantHz, target, d, dp, ratio)
		}
	}
	return target
}

func bytesToInt16(p []byte) []int16 {
	n := len(p) / 2
	out := make([]int16, n)
	for i := 0; i < n; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	return out
}

func absInt16(s int16) int16 {
	if s < 0 {
		return -s
	}
	return s
}

// goertzelPower computes the discrete Goertzel-filter power at `targetHz`
// for the given mono samples. Returns the squared magnitude of the bin
// (units arbitrary; only ratios between bins are meaningful).
func goertzelPower(samples []int16, sampleRate int, targetHz float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	k := math.Round(float64(len(samples)) * targetHz / float64(sampleRate))
	w := 2 * math.Pi * k / float64(len(samples))
	cw := math.Cos(w)
	coeff := 2 * cw
	var s1, s2 float64
	for _, s := range samples {
		v := float64(s) + coeff*s1 - s2
		s2 = s1
		s1 = v
	}
	return s1*s1 + s2*s2 - coeff*s1*s2
}
