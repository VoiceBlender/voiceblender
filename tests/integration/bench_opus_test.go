//go:build integration

package integration

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
)

// TestConcurrentRoomsScaleOpus mirrors TestConcurrentRoomsScale but
// negotiates the Opus codec end-to-end. Use it to characterize the
// codec-path cost (48 kHz native, gopus encode/decode, 6× resample
// to/from the 16 kHz mixer) under concurrent room load.
//
// Invocation examples:
//
//	go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScaleOpus ./tests/integration/
//	go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScaleOpus ./tests/integration/ -bench-rooms=200
//	BENCH_ROOMS=50,100 go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScaleOpus ./tests/integration/
func TestConcurrentRoomsScaleOpus(t *testing.T) {
	engineCodecs := []codec.CodecType{codec.CodecOpus}
	for _, numRooms := range parseBenchRooms() {
		t.Run(fmt.Sprintf("rooms_%d", numRooms), func(t *testing.T) {
			benchScale(t, numRooms, "opus", engineCodecs)
		})
	}
}

// TestOpusAudioLatency measures end-to-end audio latency for Opus over
// many fresh 2-leg rooms (one trial per room), isolating the codec path
// (encode + RTP + decode + 6× upsample + mixer) from the noise of a
// concurrent-room scale run.
//
// Path measured (per trial):
//
//	B.leg1.AudioWriter → Opus encode → RTP → A.leg1.readLoop
//	→ Opus decode → upsample 48k→16k → mixer (mix-minus-self)
//	→ A.leg2 participantOutTap → cross-correlation detector
//
// Configure trial count via OPUS_LATENCY_TRIALS env var (default: 100).
func TestOpusAudioLatency(t *testing.T) {
	trials := getOpusLatencyTrials()

	engineCodecs := []codec.CodecType{codec.CodecOpus}
	instA := newTestInstanceWithCodecs(t, "opus-lat-a", engineCodecs)
	instB := newTestInstanceWithCodecs(t, "opus-lat-b", engineCodecs)

	t.Logf("=== Opus audio latency: %d rooms (1 trial per fresh room) ===", trials)

	rooms, _ := setupRooms(t, instA, instB, trials, "opus")
	if len(rooms) == 0 {
		t.Fatalf("setup: no rooms created")
	}
	if len(rooms) < trials {
		t.Logf("  only %d/%d rooms set up successfully", len(rooms), trials)
	}

	// Allow all RTP streams to start flowing before the first measurement.
	time.Sleep(200 * time.Millisecond)

	latencies := make([]time.Duration, 0, len(rooms))
	var failures int
	for i, rs := range rooms {
		d, err := measureOneLatency(instA, instB, rs)
		if err != nil {
			failures++
			if failures <= 5 {
				t.Logf("  room %d: %v", i, err)
			}
			continue
		}
		latencies = append(latencies, d)
	}

	if len(latencies) == 0 {
		t.Fatalf("no successful latency samples (failures=%d)", failures)
	}
	if failures > 0 {
		t.Logf("  %d/%d measurements failed", failures, len(rooms))
	}

	logLatencyStatsDetailed(t, "opus codec round-trip", latencies)
}

func getOpusLatencyTrials() int {
	if s := os.Getenv("OPUS_LATENCY_TRIALS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 100
}

// logLatencyStatsDetailed extends logLatencyStats with min, p90, and
// stddev — useful for tight-loop latency characterization where the
// shape of the distribution matters more than throughput.
func logLatencyStatsDetailed(t testing.TB, label string, latencies []time.Duration) {
	t.Helper()
	if len(latencies) == 0 {
		return
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	avg := total / time.Duration(len(sorted))

	var sumSq float64
	for _, d := range sorted {
		diff := float64(d - avg)
		sumSq += diff * diff
	}
	stddev := time.Duration(math.Sqrt(sumSq / float64(len(sorted))))

	pct := func(p int) time.Duration {
		idx := len(sorted) * p / 100
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return sorted[idx]
	}

	t.Logf("%s: n=%d min=%v avg=%v p50=%v p90=%v p95=%v p99=%v max=%v stddev=%v",
		label, len(sorted), sorted[0], avg, pct(50), pct(90), pct(95), pct(99), sorted[len(sorted)-1], stddev)
}
