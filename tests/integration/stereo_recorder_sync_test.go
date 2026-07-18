//go:build integration

package integration

import (
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-audio/wav"
)

const (
	stallCallRate  = 8000 // PCMU
	stallFrameLen  = stallCallRate / 50 * 2
	stallFrameTime = 20 * time.Millisecond
	stallBurstAmp  = int16(20000)
	// Comfortably above mu-law's quantization of digital silence, and well
	// below the burst amplitude.
	stallBurstFloor = 8000
)

// sipLegOf fetches a leg by ID from an instance as a concrete *leg.SIPLeg.
func sipLegOf(t *testing.T, inst *testInstance, legID string) *leg.SIPLeg {
	t.Helper()
	l, ok := inst.legMgr.Get(legID)
	if !ok {
		t.Fatalf("leg %s not found on %s", legID, inst.name)
	}
	sl, ok := l.(*leg.SIPLeg)
	if !ok {
		t.Fatalf("leg %s is %T, want *leg.SIPLeg", legID, l)
	}
	return sl
}

// readStereoChannels splits a stereo WAV into its two channels.
func readStereoChannels(t *testing.T, path string) (left, right []int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if int(dec.NumChans) != 2 {
		t.Fatalf("recording has %d channels, want 2", dec.NumChans)
	}
	for i := 0; i+1 < len(buf.Data); i += 2 {
		left = append(left, buf.Data[i])
		right = append(right, buf.Data[i+1])
	}
	return left, right
}

// burstOnsets returns the sample index at which each loud burst begins. Samples
// louder than threshold that are within debounce of the previous loud sample
// belong to the burst already found.
func burstOnsets(samples []int, threshold, debounce int) []int {
	var onsets []int
	last := -debounce - 1
	for i, s := range samples {
		if s < 0 {
			s = -s
		}
		if s < threshold {
			continue
		}
		if i-last > debounce {
			onsets = append(onsets, i)
		}
		last = i
	}
	return onsets
}

// TestStereoRecording_StaysAlignedAcrossIncomingStall drives a real loopback
// SIP call, stalls the incoming RTP mid-recording while outgoing keeps flowing,
// resumes it, and proves the stall introduced no lasting skew between the
// recording's two channels.
//
// A stereo leg recording taps incoming audio to the left channel (written only
// when a packet decodes) and outgoing audio to the right (written every 20 ms
// tick, silence included). A marker burst is injected on both sides at the same
// moment before and after the stall. The recorded gap between the two channels'
// copies of a marker is the call's transport latency, which is not zero and not
// the thing under test; what must hold is that this gap does not *change* across
// the stall. It changing means the channels drifted apart permanently and the
// recording is no longer usable for review.
func TestStereoRecording_StaysAlignedAcrossIncomingStall(t *testing.T) {
	instA := newTestInstance(t, "instance-a")
	instB := newTestInstance(t, "instance-b")

	// establishCall leaves the legs standalone (not in a room), which is the
	// stereo SIP tap path.
	outboundID, inboundID := establishCall(t, instA, instB)

	legA := sipLegOf(t, instA, outboundID)
	legB := sipLegOf(t, instB, inboundID)

	recResp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID), nil)
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: unexpected status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)

	wA := legA.AudioWriter()
	wB := legB.AudioWriter()
	if wA == nil || wB == nil {
		t.Fatal("leg has no audio writer")
	}

	silence := make([]byte, stallFrameLen)
	burst := make([]byte, stallFrameLen)
	for i := 0; i+1 < len(burst); i += 2 {
		binary.LittleEndian.PutUint16(burst[i:], uint16(stallBurstAmp))
	}

	// feed writes n frames to both ends at the leg's own frame cadence. A's
	// frames become the recording's right channel directly; B's travel over RTP
	// and become the recording's left channel.
	feed := func(n int, loud bool) {
		frame := silence
		if loud {
			frame = burst
		}
		for i := 0; i < n; i++ {
			wA.Write(frame)
			wB.Write(frame)
			time.Sleep(stallFrameTime)
		}
	}

	start := time.Now()
	feed(25, false) // settle
	feed(3, true)   // marker 1, before the stall
	feed(25, false)

	// Stall the incoming RTP without signalling, so A never learns and its
	// outgoing side keeps ticking. This is the failure the recorder must ride
	// out: hold, packet loss, or a DTMF-only stretch all look like this.
	legB.SetHeld(true)
	feed(30, false) // ~600 ms of incoming silence
	legB.SetHeld(false)

	feed(25, false) // settle after resume
	feed(3, true)   // marker 2, after the stall
	feed(25, false)
	elapsed := time.Since(start)

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", instA.baseURL(), outboundID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: unexpected status %d", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	assertWAVAudio(t, recStart.File, 2, stallCallRate, 100)
	left, right := readStereoChannels(t, recStart.File)

	// The master clock keeps the file advancing through the stall, so the
	// recording covers the call rather than stopping where incoming did.
	wantSamples := int(elapsed.Seconds() * stallCallRate)
	if len(right) < wantSamples*3/4 {
		t.Errorf("recorded %d samples/channel, want ~%d (%.0f%% of the %v call): the stall shortened the recording",
			len(right), wantSamples, 100*float64(len(right))/float64(wantSamples), elapsed.Round(time.Millisecond))
	}
	if len(left) != len(right) {
		t.Fatalf("channel lengths differ: left=%d right=%d", len(left), len(right))
	}

	const debounce = 10 * (stallFrameLen / 2) // 200 ms: far below the marker spacing
	onsetsL := burstOnsets(left, stallBurstFloor, debounce)
	onsetsR := burstOnsets(right, stallBurstFloor, debounce)

	if len(onsetsL) != 2 {
		t.Fatalf("left channel has %d marker bursts at %v, want 2 (incoming audio lost)", len(onsetsL), onsetsL)
	}
	if len(onsetsR) != 2 {
		t.Fatalf("right channel has %d marker bursts at %v, want 2 (outgoing audio lost)", len(onsetsR), onsetsR)
	}

	// Each marker went onto both sides at the same instant, so the offset
	// between the channels is the call's transport latency. It must not change
	// across the stall.
	offsetBefore := onsetsL[0] - onsetsR[0]
	offsetAfter := onsetsL[1] - onsetsR[1]
	drift := offsetAfter - offsetBefore
	if drift < 0 {
		drift = -drift
	}

	// One slot is 20 ms; allow a few for RTP jitter and the resync after the
	// gap. The regression this guards against skews by the whole stall (~600 ms
	// / 30 slots), so this stays far away from a false pass.
	const tolerance = 5 * (stallFrameLen / 2) // 100 ms
	t.Logf("marker offsets: before=%d samples, after=%d samples, drift=%d samples (%.1f ms), tolerance=%d",
		offsetBefore, offsetAfter, drift, 1000*float64(drift)/stallCallRate, tolerance)
	if drift > tolerance {
		t.Errorf("channels drifted %d samples (%.0f ms) across the incoming stall, want <= %d: "+
			"left and right are permanently misaligned for the rest of the call",
			drift, 1000*float64(drift)/stallCallRate, tolerance)
	}
}
