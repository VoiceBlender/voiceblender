//go:build integration

package integration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/google/uuid"
)

// benchRooms can be set via -bench-rooms flag or BENCH_ROOMS env var to run
// a custom number of rooms. Examples:
//
//	go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/ -bench-rooms=200
//	BENCH_ROOMS=50,100,200 go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/
//	BENCH_ROOMS=500 go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScale ./tests/integration/
var benchRooms = flag.String("bench-rooms", "", "comma-separated room counts for benchmark (e.g. \"50,100,200\")")
var benchLatencyRooms = flag.Int("bench-latency-rooms", 0, "max rooms to sample for audio latency (default: 10, or BENCH_LATENCY_ROOMS env)")
var benchLatencyTrials = flag.Int("bench-latency-trials", 0, "trials per room for audio latency (default: 3, or BENCH_LATENCY_TRIALS env)")

// parseBenchRooms returns room counts from the -bench-rooms flag, BENCH_ROOMS
// env var, or the default set.
func parseBenchRooms() []int {
	raw := ""
	if benchRooms != nil && *benchRooms != "" {
		raw = *benchRooms
	}
	if raw == "" {
		raw = os.Getenv("BENCH_ROOMS")
	}
	if raw == "" {
		return []int{5, 10, 25, 50, 100}
	}

	var counts []int
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			continue
		}
		counts = append(counts, n)
	}
	if len(counts) == 0 {
		return []int{5, 10, 25, 50, 100}
	}
	return counts
}

func getLatencyRooms() int {
	if benchLatencyRooms != nil && *benchLatencyRooms > 0 {
		return *benchLatencyRooms
	}
	if s := os.Getenv("BENCH_LATENCY_ROOMS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 10
}

func getLatencyTrials() int {
	if benchLatencyTrials != nil && *benchLatencyTrials > 0 {
		return *benchLatencyTrials
	}
	if s := os.Getenv("BENCH_LATENCY_TRIALS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

// TestConcurrentRoomsScale creates N rooms, each with 2 SIP legs (two
// outbound calls from instance A to instance B, both added to a room on A).
// It measures setup throughput, sustained audio mixing, audio latency between
// legs, and teardown.
//
// Default scales: 5, 10, 25, 50, 100 rooms.
// Custom scales via flag or env var:
//
//	go test -tags integration -v -timeout 300s -run TestConcurrentRoomsScale ./tests/integration/ -bench-rooms=200
//	BENCH_ROOMS=50,100,500 go test -tags integration -v -timeout 600s -run TestConcurrentRoomsScale ./tests/integration/
func TestConcurrentRoomsScale(t *testing.T) {
	for _, numRooms := range parseBenchRooms() {
		t.Run(fmt.Sprintf("rooms_%d", numRooms), func(t *testing.T) {
			benchScale(t, numRooms, "PCMU", nil)
		})
	}
}

type roomSetup struct {
	roomID string

	// A-side outbound leg IDs (in the room's mixer).
	outboundID1 string
	outboundID2 string

	// B-side inbound leg IDs (SIP peers of the outbound legs).
	// inboundID1 is the peer of outboundID1, etc.
	inboundID1 string
	inboundID2 string
}

// benchScale runs the concurrent room test reporting results via t.Logf.
// codecName is the wire codec advertised in leg-creation requests (e.g.
// "PCMU", "opus"). engineCodecs overrides the SIP engine's advertised
// codec list; nil defaults to PCMU.
func benchScale(t *testing.T, numRooms int, codecName string, engineCodecs []codec.CodecType) {
	instA := newTestInstanceWithCodecs(t, "bench-a", engineCodecs)
	instB := newTestInstanceWithCodecs(t, "bench-b", engineCodecs)

	t.Logf("=== Concurrent rooms benchmark [%s]: %d rooms, %d calls ===", codecName, numRooms, numRooms*2)

	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Phase 1: Create calls and rooms concurrently.
	setupStart := time.Now()
	rooms, setupLatencies := setupRooms(t, instA, instB, numRooms, codecName)
	setupDur := time.Since(setupStart)

	var memAfterSetup runtime.MemStats
	runtime.ReadMemStats(&memAfterSetup)

	t.Logf("Phase 1 — Setup: %d rooms in %v (%.1f rooms/sec)",
		len(rooms), setupDur, float64(len(rooms))/setupDur.Seconds())
	logLatencyStats(t, "  call+room setup", setupLatencies)
	t.Logf("  Goroutines: %d", runtime.NumGoroutine())
	t.Logf("  Heap alloc: %.1f MB (delta: %.1f MB)",
		float64(memAfterSetup.HeapAlloc)/1e6,
		float64(memAfterSetup.HeapAlloc-memBefore.HeapAlloc)/1e6)

	// Phase 2: Let audio mix for a sustained period. CPU usage during
	// this window is the cleanest signal of steady-state per-room cost
	// — no setup work, no teardown, just N rooms mixing audio.
	sustainDur := 3 * time.Second
	t.Logf("Phase 2 — Sustaining %d rooms for %v...", len(rooms), sustainDur)
	cpuBeforeSustain := snapCPU()
	sustainStart := time.Now()
	time.Sleep(sustainDur)
	sustainWall := time.Since(sustainStart)
	cpuSustain := snapCPU().sub(cpuBeforeSustain)

	var memAfterSustain runtime.MemStats
	runtime.ReadMemStats(&memAfterSustain)
	t.Logf("  Goroutines after sustain: %d", runtime.NumGoroutine())
	t.Logf("  Heap alloc after sustain: %.1f MB", float64(memAfterSustain.HeapAlloc)/1e6)
	logCPU(t, "  Sustain CPU", cpuSustain, sustainWall, len(rooms))

	// Verify all legs are still connected.
	var disconnected int
	for _, rs := range rooms {
		if !isLegConnected(instA.baseURL(), rs.outboundID1) {
			disconnected++
		}
	}
	if disconnected > 0 {
		t.Errorf("%d/%d outbound legs disconnected during sustain", disconnected, len(rooms))
	} else {
		t.Logf("  All %d calls still connected", len(rooms)*2)
	}

	// Phase 3: Measure audio latency across a sample of rooms.
	t.Logf("Phase 3 — Measuring audio latency...")
	audioLatencies := measureLatencySample(t, instA, instB, rooms)
	if len(audioLatencies) > 0 {
		logLatencyStats(t, "  audio leg-to-leg", audioLatencies)
	} else {
		t.Logf("  No latency samples collected")
	}

	// Phase 4: Teardown — delete all rooms (hangs up legs).
	teardownStart := time.Now()
	teardownLatencies := teardownRooms(t, instA, rooms)
	teardownDur := time.Since(teardownStart)

	t.Logf("Phase 4 — Teardown: %d rooms in %v (%.1f rooms/sec)",
		len(rooms), teardownDur, float64(len(rooms))/teardownDur.Seconds())
	logLatencyStats(t, "  room teardown", teardownLatencies)

	// Final goroutine count (after cleanup settles). Sleep also gives
	// any in-flight leg.disconnected events time to land in the
	// per-instance event collectors before we read them.
	time.Sleep(500 * time.Millisecond)
	t.Logf("Final goroutines: %d", runtime.NumGoroutine())

	// Per-leg call-quality stats (computed by sip_leg from RTP
	// statistics, attached to leg.disconnected events). Higher MOS is
	// better; expected range ~3.5–4.5 for clean loopback.
	logCallQuality(t, "  call quality", []*testInstance{instA, instB})
}

// ---------------------------------------------------------------------------
// Audio latency measurement
// ---------------------------------------------------------------------------

// measureLatencySample picks up to maxSamples rooms and measures the audio
// latency in each by injecting an impulse through one leg and detecting it
// on the other.
//
// Path measured:
//
//	B.leg1.AudioWriter → RTP → A.leg1.readLoop → mixer (mix-minus-self)
//	→ A.leg2.writeLoop → RTP → B.leg2.readLoop → B.leg2.InTap
func measureLatencySample(t *testing.T, instA, instB *testInstance, rooms []roomSetup) []time.Duration {
	maxSamples := getLatencyRooms()
	trialsPerRoom := getLatencyTrials()

	n := len(rooms)
	if n > maxSamples {
		n = maxSamples
	}

	var latencies []time.Duration
	for i := 0; i < n; i++ {
		rs := rooms[i]
		for trial := 0; trial < trialsPerRoom; trial++ {
			d, err := measureOneLatency(instA, instB, rs)
			if err != nil {
				t.Logf("  room %d trial %d: %v", i, trial, err)
				continue
			}
			latencies = append(latencies, d)
		}
	}
	return latencies
}

// generateImpulse creates a 20ms frame of a 1kHz sine wave at near-max
// amplitude. At 8kHz sample rate: 160 samples = 320 bytes.
func generateImpulse(sampleRate int) []byte {
	samplesPerFrame := sampleRate / 50 // 20ms
	buf := make([]byte, samplesPerFrame*2)
	amplitude := float64(math.MaxInt16) * 0.9
	for i := 0; i < samplesPerFrame; i++ {
		sample := int16(amplitude * math.Sin(2*math.Pi*1000*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(sample))
	}
	return buf
}

// ---------------------------------------------------------------------------
// Cross-correlation latency measurement
// ---------------------------------------------------------------------------
//
// We can't use a simple amplitude-threshold detector: with Opus (and any
// perceptual codec), quantization transients can exceed a naive threshold
// before the reconstructed impulse arrives, biasing latency low. Instead,
// the detector buffers all tap audio for a fixed window, then locates the
// impulse by cross-correlating against the known reference waveform.
//
// Works equally well for PCMU (no pre-ringing) and Opus (heavy pre-ringing)
// because we lock onto the correlation RISING EDGE, not its global max —
// the plateau of a long sine reference against a long sine impulse
// otherwise introduces ~20ms measurement jitter from FP/int noise picking
// different plateau edges.

// Mixer tap rate — see internal/mixer/mixer.go (SampleRate=16000,
// SamplesPerFrame=320, FrameSizeBytes=640; one Write per 20ms tick).
const mixerTapSampleRate = 16000

// correlationDetector buffers per-tick audio frames with their wall-clock
// arrival time so an offline correlation pass can locate the impulse.
type correlationDetector struct {
	mu     sync.Mutex
	frames []capturedFrame
}

type capturedFrame struct {
	t       time.Time
	samples []int16
}

func newCorrelationDetector() *correlationDetector {
	return &correlationDetector{}
}

func (d *correlationDetector) Write(p []byte) (int, error) {
	now := time.Now()
	n := len(p) / 2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(p[i*2:]))
	}
	d.mu.Lock()
	d.frames = append(d.frames, capturedFrame{t: now, samples: samples})
	d.mu.Unlock()
	return len(p), nil
}

// findImpulseTime cross-correlates the captured audio against ref and
// returns the wall-clock time of the impulse rising edge. Returns false
// if the capture is shorter than the reference, or the peak score is
// below minPeakFraction × the reference self-correlation (impulse
// arrived too attenuated, or never arrived).
func (d *correlationDetector) findImpulseTime(ref []int16, sampleRate int, minPeakFraction float64) (time.Time, bool) {
	d.mu.Lock()
	frames := make([]capturedFrame, len(d.frames))
	copy(frames, d.frames)
	d.mu.Unlock()

	if len(frames) == 0 {
		return time.Time{}, false
	}

	var all []int16
	frameStartIdx := make([]int, len(frames))
	for i, f := range frames {
		frameStartIdx[i] = len(all)
		all = append(all, f.samples...)
	}
	if len(all) < len(ref) {
		return time.Time{}, false
	}

	var refSelf int64
	for _, s := range ref {
		refSelf += int64(s) * int64(s)
	}

	maxOff := len(all) - len(ref)
	scores := make([]int64, maxOff+1)
	var bestScore int64
	for i := 0; i <= maxOff; i++ {
		var score int64
		for j := 0; j < len(ref); j++ {
			score += int64(all[i+j]) * int64(ref[j])
		}
		scores[i] = score
		if score > bestScore {
			bestScore = score
		}
	}
	if bestScore <= 0 {
		return time.Time{}, false
	}

	peakFrac := float64(bestScore) / float64(refSelf)
	if peakFrac < minPeakFraction {
		return time.Time{}, false
	}

	// Lock onto the correlation rising edge: the first offset where
	// the score crosses a high fraction of the peak. Immune to the
	// ~20ms plateau-edge jitter that picking the global max would
	// introduce when the reference and impulse are both long sine
	// waves.
	const onsetFrac = 0.95
	threshold := int64(onsetFrac * float64(bestScore))
	bestIdx := -1
	for i, s := range scores {
		if s >= threshold {
			bestIdx = i
			break
		}
	}
	if bestIdx < 0 {
		return time.Time{}, false
	}

	// The mixer writes the tap frame after computing 20ms of audio,
	// so frame.t marks the END of that 20ms window — sample k of a
	// frame of length L corresponds to frame.t - L/sr + k/sr.
	frameIdx := sort.Search(len(frameStartIdx), func(i int) bool {
		return frameStartIdx[i] > bestIdx
	}) - 1
	if frameIdx < 0 {
		frameIdx = 0
	}
	offsetInFrame := bestIdx - frameStartIdx[frameIdx]
	samplePeriod := time.Second / time.Duration(sampleRate)
	frameLen := len(frames[frameIdx].samples)
	return frames[frameIdx].t.Add(time.Duration(offsetInFrame-frameLen) * samplePeriod), true
}

// generateSineSamples produces a sineHz sine of the given duration at
// the given sample rate and amplitude (0–1). Used as the
// cross-correlation reference at the mixer's tap rate.
func generateSineSamples(sampleRate, sineHz int, dur time.Duration, amplitude float64) []int16 {
	n := int(int64(sampleRate) * int64(dur) / int64(time.Second))
	out := make([]int16, n)
	a := float64(math.MaxInt16) * amplitude
	for i := 0; i < n; i++ {
		out[i] = int16(a * math.Sin(2*math.Pi*float64(sineHz)*float64(i)/float64(sampleRate)))
	}
	return out
}

// measureOneLatency injects an impulse through inboundID1's AudioWriter
// on instB, captures the resulting audio via the mixer's participant
// output tap for outboundID2 on instA, and cross-correlates to find the
// impulse arrival time.
//
// Path: B.sender.writeLoop → Opus/PCMU encode → RTP → A.sender.readLoop
//
//	→ decode → mixer (mix-minus-self) → A.outboundID2 participantOutTap
//	→ correlationDetector.
func measureOneLatency(instA, instB *testInstance, rs roomSetup) (time.Duration, error) {
	senderLeg, ok := instB.legMgr.Get(rs.inboundID1)
	if !ok {
		return 0, fmt.Errorf("sender leg %s not found on B", rs.inboundID1)
	}
	senderSIP, ok := senderLeg.(*leg.SIPLeg)
	if !ok {
		return 0, fmt.Errorf("sender leg is not SIP")
	}
	w := senderSIP.AudioWriter()
	if w == nil {
		return 0, fmt.Errorf("sender has no audio writer")
	}

	rm, ok := instA.roomMgr.Get(rs.roomID)
	if !ok {
		return 0, fmt.Errorf("room %s not found on A", rs.roomID)
	}

	const (
		impulseFrames   = 5
		frameDur        = 20 * time.Millisecond
		captureWindow   = 300 * time.Millisecond
		minPeakFraction = 0.70
	)
	refDur := time.Duration(impulseFrames) * frameDur
	ref := generateSineSamples(mixerTapSampleRate, 1000, refDur, 0.9)

	detector := newCorrelationDetector()
	rm.Mixer().SetParticipantOutTap(rs.outboundID2, detector)
	defer rm.Mixer().ClearParticipantOutTap(rs.outboundID2)

	time.Sleep(20 * time.Millisecond)

	impulse := generateImpulse(senderLeg.SampleRate())

	sendTime := time.Now()
	for i := 0; i < impulseFrames; i++ {
		if _, err := w.Write(impulse); err != nil {
			return 0, fmt.Errorf("write impulse: %w", err)
		}
	}

	time.Sleep(captureWindow)

	arrivalTime, ok := detector.findImpulseTime(ref, mixerTapSampleRate, minPeakFraction)
	if !ok {
		return 0, fmt.Errorf("impulse not located (peak too weak — likely never arrived)")
	}
	return arrivalTime.Sub(sendTime), nil
}

// ---------------------------------------------------------------------------
// Room setup
// ---------------------------------------------------------------------------

// setupRooms creates numRooms rooms with 2 legs each. Uses concurrency
// bounded by GOMAXPROCS to avoid overwhelming SIP/UDP.
func setupRooms(t testing.TB, instA, instB *testInstance, numRooms int, codecName string) ([]roomSetup, []time.Duration) {
	t.Helper()

	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	if workers > numRooms {
		workers = numRooms
	}

	results := make([]roomSetup, numRooms)
	latencies := make([]time.Duration, numRooms)
	var setupErrors atomic.Int64

	work := make(chan int, numRooms)
	for i := 0; i < numRooms; i++ {
		work <- i
	}
	close(work)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				start := time.Now()
				rs, err := setupOneRoom(instA, instB, idx, codecName)
				latencies[idx] = time.Since(start)
				if err != nil {
					setupErrors.Add(1)
					logTB(t, "room %d setup failed: %v", idx, err)
					continue
				}
				results[idx] = rs
			}
		}()
	}
	wg.Wait()

	if errs := setupErrors.Load(); errs > 0 {
		logTB(t, "WARNING: %d/%d room setups failed", errs, numRooms)
	}

	// Filter out failed setups.
	var good []roomSetup
	var goodLatencies []time.Duration
	for i, rs := range results {
		if rs.roomID != "" {
			good = append(good, rs)
			goodLatencies = append(goodLatencies, latencies[i])
		}
	}
	return good, goodLatencies
}

// establishOneLeg creates an outbound leg on instA to instB, waits for
// the inbound leg on instB (matched by X-Correlation-ID header), answers it,
// and waits for both to connect.
// Returns (outbound leg ID on A, inbound leg ID on B).
func establishOneLeg(instA, instB *testInstance, codecName string) (outboundID, inboundID string, err error) {
	correlationID := uuid.New().String()
	headers := map[string]string{"X-Correlation-ID": correlationID}

	outboundID, err = doCreateLeg(instA.baseURL(), instB.sipPort, headers, codecName)
	if err != nil {
		return "", "", fmt.Errorf("create outbound leg: %w", err)
	}

	inboundID, err = waitForCorrelatedLeg(instB.baseURL(), correlationID, 10*time.Second)
	if err != nil {
		return "", "", fmt.Errorf("wait inbound leg: %w", err)
	}

	if err := doAnswer(instB.baseURL(), inboundID); err != nil {
		return "", "", fmt.Errorf("answer: %w", err)
	}

	if err := waitLegConnected(instA.baseURL(), outboundID, 10*time.Second); err != nil {
		return "", "", fmt.Errorf("wait outbound connected: %w", err)
	}

	return outboundID, inboundID, nil
}

// waitForCorrelatedLeg polls GET /v1/legs until an inbound ringing leg with
// a matching X-Correlation-ID sip_header appears.
func waitForCorrelatedLeg(baseURL, correlationID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/v1/legs")
		if err != nil {
			time.Sleep(30 * time.Millisecond)
			continue
		}
		var legs []struct {
			ID         string            `json:"id"`
			Type       string            `json:"type"`
			State      string            `json:"state"`
			SIPHeaders map[string]string `json:"sip_headers"`
		}
		json.NewDecoder(resp.Body).Decode(&legs)
		resp.Body.Close()

		for _, l := range legs {
			if l.Type == "sip_inbound" && l.State == "ringing" && l.SIPHeaders["X-Correlation-ID"] == correlationID {
				return l.ID, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("timeout waiting for correlated inbound leg")
}

func setupOneRoom(instA, instB *testInstance, idx int, codecName string) (roomSetup, error) {
	// 1. Establish first leg (outbound on A → inbound on B, answered).
	out1, in1, err := establishOneLeg(instA, instB, codecName)
	if err != nil {
		return roomSetup{}, fmt.Errorf("leg 1: %w", err)
	}

	// 2. Create room on A.
	roomID, err := doCreateRoom(instA.baseURL())
	if err != nil {
		return roomSetup{}, fmt.Errorf("create room: %w", err)
	}

	// 3. Add first leg to room.
	if err := doAddLegToRoom(instA.baseURL(), roomID, out1); err != nil {
		return roomSetup{}, fmt.Errorf("add leg 1 to room: %w", err)
	}

	// 4. Establish second leg.
	out2, in2, err := establishOneLeg(instA, instB, codecName)
	if err != nil {
		return roomSetup{}, fmt.Errorf("leg 2: %w", err)
	}

	// 5. Add second leg to room.
	if err := doAddLegToRoom(instA.baseURL(), roomID, out2); err != nil {
		return roomSetup{}, fmt.Errorf("add leg 2 to room: %w", err)
	}

	return roomSetup{
		roomID:      roomID,
		outboundID1: out1,
		outboundID2: out2,
		inboundID1:  in1,
		inboundID2:  in2,
	}, nil
}

func teardownRooms(t testing.TB, instA *testInstance, rooms []roomSetup) []time.Duration {
	t.Helper()

	latencies := make([]time.Duration, len(rooms))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)

	for i, rs := range rooms {
		wg.Add(1)
		go func(idx int, rs roomSetup) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			doDeleteRoom(instA.baseURL(), rs.roomID)
			latencies[idx] = time.Since(start)
		}(i, rs)
	}
	wg.Wait()
	return latencies
}

// ---------------------------------------------------------------------------
// HTTP helpers (non-fatal — return errors for concurrent use)
// ---------------------------------------------------------------------------

func doCreateLeg(baseURL string, targetSIPPort int, headers map[string]string, codecName string) (string, error) {
	if codecName == "" {
		codecName = "PCMU"
	}
	reqBody := map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:bench@127.0.0.1:%d", targetSIPPort),
		"codecs": []string{codecName},
	}
	if len(headers) > 0 {
		reqBody["headers"] = headers
	}
	body, _ := json.Marshal(reqBody)
	resp, err := http.Post(baseURL+"/v1/legs", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	var v struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	return v.ID, nil
}

func doAnswer(baseURL, legID string) error {
	body, _ := json.Marshal(nil)
	resp, err := http.Post(fmt.Sprintf("%s/v1/legs/%s/answer", baseURL, legID), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("answer status %d", resp.StatusCode)
	}
	return nil
}

func waitLegConnected(baseURL, legID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/v1/legs/%s", baseURL, legID))
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var v struct {
			State string `json:"state"`
		}
		json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if v.State == "connected" {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("leg %s did not reach connected state", legID)
}

func doCreateRoom(baseURL string) (string, error) {
	body, _ := json.Marshal(map[string]interface{}{})
	resp, err := http.Post(baseURL+"/v1/rooms", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	var v struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	return v.ID, nil
}

func doAddLegToRoom(baseURL, roomID, legID string) error {
	body, _ := json.Marshal(map[string]interface{}{"leg_id": legID})
	resp, err := http.Post(fmt.Sprintf("%s/v1/rooms/%s/legs", baseURL, roomID), "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("add leg to room status %d", resp.StatusCode)
	}
	return nil
}

func doDeleteRoom(baseURL, roomID string) {
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/v1/rooms/%s", baseURL, roomID), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func isLegConnected(baseURL, legID string) bool {
	resp, err := http.Get(fmt.Sprintf("%s/v1/legs/%s", baseURL, legID))
	if err != nil {
		return false
	}
	var v struct {
		State string `json:"state"`
	}
	json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	return v.State == "connected"
}

// ---------------------------------------------------------------------------
// Stats helpers
// ---------------------------------------------------------------------------

func logLatencyStats(t testing.TB, label string, latencies []time.Duration) {
	if len(latencies) == 0 {
		return
	}
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	p50 := sorted[len(sorted)*50/100]
	p95 := sorted[len(sorted)*95/100]
	p99 := sorted[len(sorted)*99/100]
	max := sorted[len(sorted)-1]

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	avg := total / time.Duration(len(sorted))

	logTB(t, "%s latency: avg=%v p50=%v p95=%v p99=%v max=%v (n=%d)",
		label, avg, p50, p95, p99, max, len(sorted))
}

// logTB works with both *testing.T and *testing.B.
func logTB(t testing.TB, format string, args ...interface{}) {
	t.Helper()
	t.Logf(format, args...)
}

// logCallQuality walks each instance's event collector for
// leg.disconnected events and reports per-leg RTP-derived call quality
// (MOS, packets received/lost, jitter). Empty if no quality data was
// captured (e.g. legs torn down before any RTP arrived).
func logCallQuality(t testing.TB, label string, instances []*testInstance) {
	t.Helper()

	var mos, jitter []float64
	var totalRecv, totalLost uint64
	for _, inst := range instances {
		inst.collector.mu.Lock()
		evs := make([]events.Event, len(inst.collector.events))
		copy(evs, inst.collector.events)
		inst.collector.mu.Unlock()
		for _, e := range evs {
			if e.Type != events.LegDisconnected {
				continue
			}
			d, ok := e.Data.(*events.LegDisconnectedData)
			if !ok || d.Quality == nil {
				continue
			}
			mos = append(mos, d.Quality.MOSScore)
			jitter = append(jitter, d.Quality.JitterMs)
			totalRecv += uint64(d.Quality.PacketsReceived)
			totalLost += uint64(d.Quality.PacketsLost)
		}
	}

	if len(mos) == 0 {
		t.Logf("%s: no RTP quality samples captured", label)
		return
	}

	mosMin, mosAvg, mosP50, mosP95 := floatStats(mos)
	jMin, jAvg, jP50, jP95 := floatStats(jitter)
	lossPct := 0.0
	if totalRecv+totalLost > 0 {
		lossPct = 100 * float64(totalLost) / float64(totalRecv+totalLost)
	}
	t.Logf("%s (n=%d legs): MOS min=%.2f avg=%.2f p50=%.2f p95=%.2f | jitter min=%.1fms avg=%.1fms p50=%.1fms p95=%.1fms | packets recv=%d lost=%d (%.3f%%)",
		label, len(mos), mosMin, mosAvg, mosP50, mosP95, jMin, jAvg, jP50, jP95, totalRecv, totalLost, lossPct)
}

func floatStats(vals []float64) (min, avg, p50, p95 float64) {
	if len(vals) == 0 {
		return
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	min = sorted[0]
	var sum float64
	for _, v := range sorted {
		sum += v
	}
	avg = sum / float64(len(sorted))
	p50 = sorted[len(sorted)*50/100]
	p95idx := len(sorted) * 95 / 100
	if p95idx >= len(sorted) {
		p95idx = len(sorted) - 1
	}
	p95 = sorted[p95idx]
	return
}
