//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	goaudio "github.com/go-audio/audio"
	"github.com/go-audio/wav"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/thesyncim/gopus"
)

// TestWebRTC_AudioFlowToRoomRecording proves the WebRTC inbound audio path
// end-to-end: a pion test client encodes a 1 kHz sine wave with Opus, the
// server WebRTC leg decodes it, the room mixer forwards it to a recording,
// and the resulting WAV is asserted to contain the original tone. Catches
// both a totally silent path and a wrong-codec path (μ-law decoding of Opus
// payloads → broadband noise).
func TestWebRTC_AudioFlowToRoomRecording(t *testing.T) {
	inst := newTestInstance(t, "webrtc-audio")

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client NewPeerConnection: %v", err)
	}
	defer clientPC.Close()

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypeOpus,
		ClockRate: 48000,
		Channels:  2,
	}, "audio", "test-tone")
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	if _, err := clientPC.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	candCh := make(chan webrtc.ICECandidateInit, 16)
	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			close(candCh)
			return
		}
		select {
		case candCh <- c.ToJSON():
		default:
		}
	})

	connected := make(chan struct{}, 1)
	clientPC.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local desc: %v", err)
	}

	resp := httpPost(t, inst.baseURL()+"/v1/webrtc/offer",
		map[string]string{"sdp": clientPC.LocalDescription().SDP})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("webrtc_offer: status %d", resp.StatusCode)
	}
	var offerResult struct {
		LegID string `json:"leg_id"`
		SDP   string `json:"sdp"`
	}
	decodeJSON(t, resp, &offerResult)
	if offerResult.LegID == "" {
		t.Fatal("empty leg_id")
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: offerResult.SDP,
	}); err != nil {
		t.Fatalf("client SetRemoteDescription: %v", err)
	}

	trickleICEUntilConnected(t, inst, clientPC, offerResult.LegID, candCh, connected, 10*time.Second)

	inst.collector.waitForMatch(t, events.LegConnected, func(e events.Event) bool {
		return e.Data.GetLegID() == offerResult.LegID
	}, 5*time.Second)

	roomResp := httpPost(t, inst.baseURL()+"/v1/rooms", map[string]interface{}{})
	if roomResp.StatusCode != http.StatusCreated {
		t.Fatalf("create room: status %d", roomResp.StatusCode)
	}
	var rm roomView
	decodeJSON(t, roomResp, &rm)

	addResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/legs", inst.baseURL(), rm.ID),
		map[string]string{"leg_id": offerResult.LegID})
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("add leg to room: status %d", addResp.StatusCode)
	}
	addResp.Body.Close()

	stopTone := make(chan struct{})
	toneDone := make(chan struct{})
	toneErr := make(chan error, 1)
	go pumpToneRTP(track, 1000.0, stopTone, toneDone, toneErr)
	defer func() {
		close(stopTone)
		<-toneDone
		select {
		case err := <-toneErr:
			t.Errorf("tone pump: %v", err)
		default:
		}
	}()

	// Let SRTP/decoder settle before opening the recording tap.
	time.Sleep(300 * time.Millisecond)

	recResp := httpPost(t, fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), rm.ID),
		map[string]interface{}{})
	if recResp.StatusCode != http.StatusOK {
		t.Fatalf("start recording: status %d", recResp.StatusCode)
	}
	var recStart recordingResponse
	decodeJSON(t, recResp, &recStart)
	if recStart.Status != "recording" {
		t.Fatalf("recording start status = %q, want recording", recStart.Status)
	}

	time.Sleep(1 * time.Second)

	stopResp := httpDelete(t, fmt.Sprintf("%s/v1/rooms/%s/record", inst.baseURL(), rm.ID))
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("stop recording: status %d", stopResp.StatusCode)
	}
	var recStop recordingResponse
	decodeJSON(t, stopResp, &recStop)
	if recStop.Status != "stopped" {
		t.Fatalf("recording stop status = %q, want stopped", recStop.Status)
	}

	assertToneInWAV(t, recStop.File, 16000, 1000.0)

	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), offerResult.LegID))
}

// trickleICEUntilConnected pushes client-gathered candidates up to the server
// and pulls server-gathered candidates down to the client until the client
// peer connection reaches the connected state, or the timeout expires.
func trickleICEUntilConnected(
	t *testing.T,
	inst *testInstance,
	pc *webrtc.PeerConnection,
	legID string,
	candCh chan webrtc.ICECandidateInit,
	connected <-chan struct{},
	timeout time.Duration,
) {
	t.Helper()
	candURL := fmt.Sprintf("%s/v1/legs/%s/ice-candidates", inst.baseURL(), legID)
	deadline := time.Now().Add(timeout)

	for {
		select {
		case <-connected:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("client peer connection did not reach connected within %v", timeout)
		}

	drainClient:
		for {
			select {
			case c, ok := <-candCh:
				if !ok {
					candCh = nil
					break drainClient
				}
				body, _ := json.Marshal(c)
				r := httpPost(t, candURL, json.RawMessage(body))
				r.Body.Close()
			default:
				break drainClient
			}
		}

		r := httpGet(t, candURL)
		var got struct {
			Candidates []webrtc.ICECandidateInit `json:"candidates"`
			Done       bool                      `json:"done"`
		}
		decodeJSON(t, r, &got)
		for _, c := range got.Candidates {
			if err := pc.AddICECandidate(c); err != nil {
				t.Logf("client AddICECandidate: %v", err)
			}
		}

		time.Sleep(30 * time.Millisecond)
	}
}

// pumpToneRTP encodes a continuous sine wave at `freq` Hz with gopus and
// writes 20 ms Opus RTP frames to `track` until stop is closed. Reports any
// fatal error on errCh; the caller decides whether to fail the test.
func pumpToneRTP(
	track *webrtc.TrackLocalStaticRTP,
	freq float64,
	stop <-chan struct{},
	done chan<- struct{},
	errCh chan<- error,
) {
	defer close(done)

	enc, err := gopus.NewEncoder(gopus.EncoderConfig{
		SampleRate:  48000,
		Channels:    1,
		Application: gopus.ApplicationVoIP,
	})
	if err != nil {
		errCh <- fmt.Errorf("gopus.NewEncoder: %w", err)
		return
	}

	const sampleRate = 48000
	const frameMs = 20
	const samplesPerFrame = sampleRate * frameMs / 1000
	pcm := make([]int16, samplesPerFrame)
	buf := make([]byte, 4000)

	ssrc := rand.Uint32()
	var seq uint16
	var ts uint32
	var phase float64
	inc := 2 * math.Pi * freq / float64(sampleRate)

	ticker := time.NewTicker(frameMs * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		for i := 0; i < samplesPerFrame; i++ {
			pcm[i] = int16(math.Sin(phase) * 12000)
			phase += inc
			if phase > 2*math.Pi {
				phase -= 2 * math.Pi
			}
		}
		n, err := enc.EncodeInt16(pcm, buf)
		if err != nil {
			errCh <- fmt.Errorf("opus encode: %w", err)
			return
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: seq,
				Timestamp:      ts,
				SSRC:           ssrc,
			},
			Payload: append([]byte(nil), buf[:n]...),
		}
		if err := track.WriteRTP(pkt); err != nil {
			return
		}
		seq++
		ts += samplesPerFrame
	}
}

// assertToneInWAV verifies that the recording at `path` contains a tone
// around `expectedFreq` Hz. Uses two cheap, complementary checks:
//   - RMS well above silence (rejects empty / muted recordings)
//   - zero-crossings per second ≈ 2·freq (rejects garbage / white noise)
//
// A μ-law decoder fed Opus payload bytes produces a wildly different
// zero-crossing rate, so the ±30% gate easily catches the codec-mismatch bug.
func assertToneInWAV(t *testing.T, path string, expectedSR int, expectedFreq float64) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open WAV %s: %v", path, err)
	}
	defer f.Close()

	dec := wav.NewDecoder(f)
	if !dec.IsValidFile() {
		t.Fatalf("%s: invalid WAV", path)
	}
	if int(dec.SampleRate) != expectedSR {
		t.Fatalf("sample rate = %d, want %d", dec.SampleRate, expectedSR)
	}

	buf := &goaudio.IntBuffer{
		Data:   make([]int, 4096),
		Format: &goaudio.Format{SampleRate: expectedSR, NumChannels: int(dec.NumChans)},
	}
	samples := make([]int, 0, expectedSR)
	for {
		n, err := dec.PCMBuffer(buf)
		if n > 0 {
			samples = append(samples, buf.Data[:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}
	if len(samples) < expectedSR/2 {
		t.Fatalf("only %d samples captured, need >= %d (0.5s)", len(samples), expectedSR/2)
	}

	// Skip leading silence — the first mixer tick after recording starts may
	// land before any audio frame has been pushed.
	start := 0
	for start < len(samples) && absInt(samples[start]) < 200 {
		start++
	}
	if start >= len(samples)-expectedSR/4 {
		t.Fatalf("recording appears silent (%d/%d samples below threshold)", start, len(samples))
	}
	tail := samples[start:]

	var sumSq float64
	for _, s := range tail {
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(tail)))
	if rms < 500 {
		t.Fatalf("RMS = %.0f, too low (likely silence)", rms)
	}

	zc := 0
	for i := 1; i < len(tail); i++ {
		if (tail[i-1] < 0 && tail[i] >= 0) || (tail[i-1] >= 0 && tail[i] < 0) {
			zc++
		}
	}
	zcRate := float64(zc) * float64(expectedSR) / float64(len(tail))
	expected := 2 * expectedFreq
	lo, hi := expected*0.7, expected*1.3
	if zcRate < lo || zcRate > hi {
		t.Fatalf("zero-crossings/s = %.0f (want %.0f ±30%%, range [%.0f,%.0f]) — recording does not contain a %g Hz tone",
			zcRate, expected, lo, hi, expectedFreq)
	}

	t.Logf("tone OK: rms=%.0f zc/s=%.0f (expected ~%.0f for %g Hz)", rms, zcRate, expected, expectedFreq)
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
