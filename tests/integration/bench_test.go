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
			benchScale(t, numRooms)
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
func benchScale(t *testing.T, numRooms int) {
	instA := newTestInstance(t, "bench-a")
	instB := newTestInstance(t, "bench-b")

	t.Logf("=== Concurrent rooms benchmark: %d rooms, %d calls ===", numRooms, numRooms*2)

	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Phase 1: Create calls and rooms concurrently.
	setupStart := time.Now()
	rooms, setupLatencies := setupRooms(t, instA, instB, numRooms)
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

	// Phase 2: Let audio mix for a sustained period.
	sustainDur := 3 * time.Second
	t.Logf("Phase 2 — Sustaining %d rooms for %v...", len(rooms), sustainDur)
	time.Sleep(sustainDur)

	var memAfterSustain runtime.MemStats
	runtime.ReadMemStats(&memAfterSustain)
	t.Logf("  Goroutines after sustain: %d", runtime.NumGoroutine())
	t.Logf("  Heap alloc after sustain: %.1f MB", float64(memAfterSustain.HeapAlloc)/1e6)

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

	// Final goroutine count (after cleanup settles).
	time.Sleep(500 * time.Millisecond)
	t.Logf("Final goroutines: %d", runtime.NumGoroutine())
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

// impulseDetector is an io.Writer that records the timestamp when it first
// receives audio samples above a threshold.
type impulseDetector struct {
	threshold int16
	detected  chan time.Time
	once      sync.Once
}

func newImpulseDetector(threshold int16) *impulseDetector {
	return &impulseDetector{
		threshold: threshold,
		detected:  make(chan time.Time, 1),
	}
}

func (d *impulseDetector) Write(p []byte) (int, error) {
	nSamples := len(p) / 2
	for i := 0; i < nSamples; i++ {
		sample := int16(binary.LittleEndian.Uint16(p[i*2:]))
		if sample > d.threshold || sample < -d.threshold {
			d.once.Do(func() {
				d.detected <- time.Now()
			})
			break
		}
	}
	return len(p), nil
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

// measureOneLatency injects an impulse through senderID's AudioWriter on
// instB, and detects it via the mixer's participant output tap on instA.
//
// Path: B.sender.writeLoop → RTP → A.sender.readLoop → mixer →
// mixed-minus-self for receiver → participantOutTap → detector.
func measureOneLatency(instA, instB *testInstance, rs roomSetup) (time.Duration, error) {
	// Get B-side sender leg to inject audio.
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

	// Get A-side room's mixer and install detector on receiver's output tap.
	// The participant out tap receives the mixed-minus-self audio for that
	// participant, which includes the sender's audio.
	rm, ok := instA.roomMgr.Get(rs.roomID)
	if !ok {
		return 0, fmt.Errorf("room %s not found on A", rs.roomID)
	}
	detector := newImpulseDetector(500)
	rm.Mixer().SetParticipantOutTap(rs.outboundID2, detector)
	defer rm.Mixer().ClearParticipantOutTap(rs.outboundID2)

	// Small delay to let any in-flight silence frames drain.
	time.Sleep(10 * time.Millisecond)

	impulse := generateImpulse(senderLeg.SampleRate())

	sendTime := time.Now()
	// Write several frames to survive any channel buffering.
	for i := 0; i < 5; i++ {
		w.Write(impulse)
	}

	// Wait for detection with timeout.
	select {
	case detectTime := <-detector.detected:
		return detectTime.Sub(sendTime), nil
	case <-time.After(1 * time.Second):
		return 0, fmt.Errorf("impulse not detected within 1s")
	}
}

// ---------------------------------------------------------------------------
// Room setup
// ---------------------------------------------------------------------------

// setupRooms creates numRooms rooms with 2 legs each. Uses concurrency
// bounded by GOMAXPROCS to avoid overwhelming SIP/UDP.
func setupRooms(t testing.TB, instA, instB *testInstance, numRooms int) ([]roomSetup, []time.Duration) {
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
				rs, err := setupOneRoom(instA, instB, idx)
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
func establishOneLeg(instA, instB *testInstance) (outboundID, inboundID string, err error) {
	correlationID := uuid.New().String()
	headers := map[string]string{"X-Correlation-ID": correlationID}

	outboundID, err = doCreateLeg(instA.baseURL(), instB.sipPort, headers)
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

func setupOneRoom(instA, instB *testInstance, idx int) (roomSetup, error) {
	// 1. Establish first leg (outbound on A → inbound on B, answered).
	out1, in1, err := establishOneLeg(instA, instB)
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
	out2, in2, err := establishOneLeg(instA, instB)
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

func doCreateLeg(baseURL string, targetSIPPort int, headers map[string]string) (string, error) {
	reqBody := map[string]interface{}{
		"type":   "sip",
		"uri":    fmt.Sprintf("sip:bench@127.0.0.1:%d", targetSIPPort),
		"codecs": []string{"PCMU"},
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
	if resp.StatusCode != http.StatusOK {
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
