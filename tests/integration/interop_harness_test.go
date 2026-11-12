//go:build integration && interop

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/codec"
	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
	_ "github.com/VoiceBlender/voiceblender/tests/integration/peers/freeswitch"
	"github.com/go-audio/wav"
)

func TestMain(m *testing.M) {
	code := m.Run()
	peers.ShutdownAll()
	os.Exit(code)
}

// The peer band sits below every band ports_test.go hands out.
const (
	interopPeerBandMin = 6000
	interopPeerBandMax = 8191
)

func TestInterop_PeerPortsOutsideHarnessBands(t *testing.T) {
	if interopPeerBandMax >= sipPortBandMin {
		t.Fatalf("peer band %d-%d overlaps the SIP band from %d", interopPeerBandMin, interopPeerBandMax, sipPortBandMin)
	}
	if interopPeerBandMax >= rtpPoolMin || interopPeerBandMax >= pionPortMin {
		t.Fatalf("peer band %d-%d overlaps an RTP pool", interopPeerBandMin, interopPeerBandMax)
	}
}

// forEachPeer runs fn as a subtest against every selected endpoint.
func forEachPeer(t *testing.T, fn func(t *testing.T, p peers.Peer)) {
	t.Helper()
	for _, name := range peers.Selected() {
		t.Run(name, func(t *testing.T) {
			p, err := peers.Get(name)
			if err != nil {
				t.Fatalf("start %s: %v", name, err)
			}
			fn(t, p)
		})
	}
}

func requireCap(t *testing.T, has bool, what string) {
	t.Helper()
	if !has {
		t.Skipf("peer does not support %s", what)
	}
}

var interopVBCodecs = []string{"PCMU", "PCMA", "G722", "OPUS"}

// interopCodecs is every codec both VoiceBlender and p can run.
func interopCodecs(p peers.Peer) []string {
	var out []string
	for _, c := range interopVBCodecs {
		if p.Caps().HasCodec(c) {
			out = append(out, c)
		}
	}
	return out
}

func codecRate(name string) int {
	switch strings.ToUpper(name) {
	case "G722":
		return 16000
	case "OPUS":
		return 48000
	}
	return 8000
}

func isWideband(name string) bool { return codecRate(name) > 8000 }

// newInteropVB is a harness instance that can run every codec the interop
// suite exercises.
func newInteropVB(t *testing.T, name string, mutate func(*config.Config)) *testInstance {
	t.Helper()
	return newTestInstanceFull(t, name, func(c *config.Config) {
		c.SIPTLSInsecure = true
		if mutate != nil {
			mutate(c)
		}
	}, []codec.CodecType{codec.CodecPCMU, codec.CodecPCMA, codec.CodecG722, codec.CodecOpus})
}

func newInteropTag() string {
	var b [8]byte
	rand.Read(b[:])
	return "it-" + hex.EncodeToString(b[:])
}

func interopCtx(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// Call setup
// ---------------------------------------------------------------------------

type interopDial struct {
	To      string // overrides the URI built from User
	User    string
	Codecs  []string
	SRTP    *bool
	TLS     bool
	Auth    bool
	Headers map[string]string
	Extra   map[string]interface{}
}

// vbDialsPeer places a call from inst to p and returns the VoiceBlender leg
// and p's side of the call, still unanswered.
func vbDialsPeer(t *testing.T, inst *testInstance, p peers.Peer, d interopDial) (string, peers.Call) {
	t.Helper()
	legID, tag := vbDialsPeerNoWait(t, inst, p, d)
	call, err := p.WaitInbound(interopCtx(t, 10*time.Second), tag)
	if err != nil {
		t.Fatalf("peer never saw the INVITE: %v", err)
	}
	return legID, call
}

func vbDialsPeerNoWait(t *testing.T, inst *testInstance, p peers.Peer, d interopDial) (legID, tag string) {
	t.Helper()
	tag = newInteropTag()
	t.Cleanup(func() { p.HangupAll(tag) })
	if d.User == "" {
		d.User = "interop"
	}
	headers := map[string]string{peers.TagHeader: tag}
	for k, v := range d.Headers {
		headers[k] = v
	}
	to := d.To
	if to == "" {
		to = p.URI(d.User, peers.URIOpts{TLS: d.TLS, Auth: d.Auth})
	}
	body := map[string]interface{}{
		"type":    "sip",
		"to":      to,
		"headers": headers,
	}
	if len(d.Codecs) > 0 {
		body["codecs"] = d.Codecs
	}
	if d.SRTP != nil {
		body["srtp"] = *d.SRTP
	}
	for k, v := range d.Extra {
		body[k] = v
	}
	resp := httpPost(t, inst.baseURL()+"/v1/legs", body)
	if resp.StatusCode != http.StatusCreated {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		resp.Body.Close()
		t.Fatalf("create leg: status %d: %s", resp.StatusCode, b.String())
	}
	var lv legView
	decodeJSON(t, resp, &lv)
	return lv.ID, tag
}

// peerDialsVB has p call inst and returns p's call and the ringing leg.
func peerDialsVB(t *testing.T, inst *testInstance, p peers.Peer, o peers.OriginateOpts) (peers.Call, legView) {
	t.Helper()
	if o.Tag == "" {
		o.Tag = newInteropTag()
	}
	if o.To == "" {
		o.To = fmt.Sprintf("sip:interop@127.0.0.1:%d", inst.sipPort)
	}
	tag := o.Tag
	t.Cleanup(func() { p.HangupAll(tag) })
	call, err := p.Originate(interopCtx(t, 10*time.Second), o)
	if err != nil {
		t.Fatalf("originate: %v", err)
	}
	return call, waitForTaggedLeg(t, inst, tag, 10*time.Second)
}

// waitForTaggedLeg finds the inbound leg whose INVITE carried tag.
func waitForTaggedLeg(t *testing.T, inst *testInstance, tag string, timeout time.Duration) legView {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp := httpGet(t, inst.baseURL()+"/v1/legs")
		var legs []legView
		decodeJSON(t, resp, &legs)
		for _, l := range legs {
			for k, v := range l.SIPHeaders {
				if strings.EqualFold(k, peers.TagHeader) && v == tag && l.Type == "sip_inbound" {
					return l
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no inbound leg tagged %s within %v", tag, timeout)
	return legView{}
}

func answerOnVB(t *testing.T, inst *testInstance, legID string, body interface{}) {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/answer", inst.baseURL(), legID), body)
	if resp.StatusCode != http.StatusAccepted {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		resp.Body.Close()
		t.Fatalf("answer %s: status %d: %s", legID, resp.StatusCode, b.String())
	}
	resp.Body.Close()
}

func answerOnPeer(t *testing.T, call peers.Call, o peers.AnswerOpts) {
	t.Helper()
	if err := call.Answer(o); err != nil {
		t.Fatalf("peer answer: %v", err)
	}
}

// connectVBToPeer is a call from inst to p, answered by p.
func connectVBToPeer(t *testing.T, inst *testInstance, p peers.Peer, d interopDial, a peers.AnswerOpts) (string, peers.Call) {
	t.Helper()
	legID, call := vbDialsPeer(t, inst, p, d)
	answerOnPeer(t, call, a)
	waitForLegState(t, inst.baseURL(), legID, "connected", 10*time.Second)
	if err := call.WaitAnswered(interopCtx(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	return legID, call
}

// connectPeerToVB is a call from p to inst, answered through the API.
func connectPeerToVB(t *testing.T, inst *testInstance, p peers.Peer, o peers.OriginateOpts, answer interface{}) (peers.Call, string) {
	t.Helper()
	call, lv := peerDialsVB(t, inst, p, o)
	answerOnVB(t, inst, lv.ID, answer)
	waitForLegState(t, inst.baseURL(), lv.ID, "connected", 10*time.Second)
	if err := call.WaitAnswered(interopCtx(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	return call, lv.ID
}

func hangupOnVB(t *testing.T, inst *testInstance, legID string) {
	t.Helper()
	resp := httpDelete(t, fmt.Sprintf("%s/v1/legs/%s", inst.baseURL(), legID))
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete leg %s: status %d", legID, resp.StatusCode)
	}
}

func waitDisconnect(t *testing.T, inst *testInstance, legID string, timeout time.Duration) *events.LegDisconnectedData {
	t.Helper()
	e := inst.collector.waitForMatch(t, events.LegDisconnected, func(e events.Event) bool {
		return e.Data.GetLegID() == legID
	}, timeout)
	return e.Data.(*events.LegDisconnectedData)
}

func waitPeerHangup(t *testing.T, call peers.Call, timeout time.Duration) peers.HangupInfo {
	t.Helper()
	info, err := call.WaitHangup(interopCtx(t, timeout))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// ---------------------------------------------------------------------------
// Audio
// ---------------------------------------------------------------------------

// sineWAV is a mono 16-bit WAV of hz lasting d. d must hold a whole number
// of cycles so the file loops without a click.
func sineWAV(rate, hz int, d time.Duration) []byte {
	samples := generateSineSamples(rate, hz, d, 0.3)
	var buf bytes.Buffer
	data := len(samples) * 2
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+data))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(data))
	binary.Write(&buf, binary.LittleEndian, samples)
	return buf.Bytes()
}

// playToneOnVB loops a hz sine into legID until the leg ends.
func playToneOnVB(t *testing.T, inst *testInstance, legID string, hz int) string {
	t.Helper()
	wavBytes := sineWAV(16000, hz, 2*time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wavBytes)
	}))
	t.Cleanup(srv.Close)
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/play", inst.baseURL(), legID), map[string]interface{}{
		"url":       srv.URL + "/tone.wav",
		"mime_type": "audio/wav",
		"repeat":    -1,
	})
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		t.Fatalf("play on %s: status %d", legID, resp.StatusCode)
	}
	var out struct {
		PlaybackID string `json:"playback_id"`
	}
	decodeJSON(t, resp, &out)
	return out.PlaybackID
}

func stopPlaybackOnVB(t *testing.T, inst *testInstance, legID, playbackID string) {
	t.Helper()
	httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/play/%s", inst.baseURL(), legID, playbackID)).Body.Close()
}

// readWAVChannel returns one channel of a 16-bit WAV.
func readWAVChannel(t *testing.T, path string, ch int) ([]int16, int) {
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
	nch := int(dec.NumChans)
	if ch >= nch {
		t.Fatalf("%s has %d channels, want channel %d", path, nch, ch)
	}
	out := make([]int16, 0, len(buf.Data)/nch)
	for i := ch; i < len(buf.Data); i += nch {
		out = append(out, int16(buf.Data[i]))
	}
	return out, int(dec.SampleRate)
}

func rmsInt16(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

// toneReport describes how clearly hz stands out in a recording.
type toneReport struct {
	RMS       float64
	Ratio     float64 // power at hz over the strongest other probe
	Strongest float64
}

// analyzeTone takes the loudest one-second window after skip and compares
// the power at hz with probes spread across the band.
func analyzeTone(samples []int16, rate int, hz float64, skip time.Duration) toneReport {
	start := int(skip.Seconds() * float64(rate))
	if start >= len(samples) {
		return toneReport{}
	}
	s := samples[start:]
	win := rate
	if len(s) < win {
		win = len(s)
	}
	best := s[:win]
	bestRMS := rmsInt16(best)
	for off := win / 2; off+win <= len(s); off += win / 2 {
		if r := rmsInt16(s[off : off+win]); r > bestRMS {
			best, bestRMS = s[off:off+win], r
		}
	}
	target := goertzelPower(best, rate, hz)
	var other, otherHz float64
	nyq := float64(rate) / 2
	for f := 150.0; f < nyq-100 && f < 7900; f += 50 {
		if math.Abs(f-hz) < 120 {
			continue
		}
		if p := goertzelPower(best, rate, f); p > other {
			other, otherHz = p, f
		}
	}
	ratio := math.Inf(1)
	if other > 0 {
		ratio = target / other
	}
	return toneReport{RMS: bestRMS, Ratio: ratio, Strongest: otherHz}
}

func assertToneSamples(t *testing.T, label string, samples []int16, rate int, hz float64) {
	t.Helper()
	r := analyzeTone(samples, rate, hz, 300*time.Millisecond)
	t.Logf("%s: %v Hz tone at %d Hz rate: rms=%.0f ratio=%.1f (next strongest %.0f Hz)", label, hz, rate, r.RMS, r.Ratio, r.Strongest)
	if r.RMS < 300 {
		t.Errorf("%s: no audio (rms %.0f), want the %v Hz tone", label, r.RMS, hz)
		return
	}
	if r.Ratio < 10 {
		t.Errorf("%s: %v Hz is not dominant: only %.1fx the power at %.0f Hz", label, hz, r.Ratio, r.Strongest)
	}
}

func assertNoToneSamples(t *testing.T, label string, samples []int16, rate int, hz float64) {
	t.Helper()
	r := analyzeTone(samples, rate, hz, 300*time.Millisecond)
	if r.RMS >= 300 && r.Ratio >= 10 {
		t.Errorf("%s: %v Hz should be absent but dominates (rms %.0f, ratio %.1f)", label, hz, r.RMS, r.Ratio)
	}
}

// recordVBLeg records what legID receives for d and returns those samples.
func recordVBLeg(t *testing.T, inst *testInstance, legID string, d time.Duration) ([]int16, int) {
	t.Helper()
	resp := httpPost(t, fmt.Sprintf("%s/v1/legs/%s/record", inst.baseURL(), legID), map[string]interface{}{})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("start recording %s: status %d", legID, resp.StatusCode)
	}
	resp.Body.Close()
	time.Sleep(d)
	resp = httpDelete(t, fmt.Sprintf("%s/v1/legs/%s/record", inst.baseURL(), legID))
	var rec recordingResponse
	decodeJSON(t, resp, &rec)
	if rec.File == "" {
		t.Fatalf("stop recording %s: no file", legID)
	}
	return readWAVChannel(t, rec.File, 0)
}

// recordPeer records what call receives for d.
func recordPeer(t *testing.T, call peers.Call, d time.Duration) ([]int16, int) {
	t.Helper()
	stop, err := call.StartRecording()
	if err != nil {
		t.Fatalf("peer recording: %v", err)
	}
	time.Sleep(d)
	path, err := stop()
	if err != nil {
		t.Fatalf("peer recording: %v", err)
	}
	return readWAVChannel(t, path, 0)
}

func assertVBHearsTone(t *testing.T, inst *testInstance, legID string, hz int) {
	t.Helper()
	s, rate := recordVBLeg(t, inst, legID, 2*time.Second)
	assertToneSamples(t, "VoiceBlender hears peer", s, rate, float64(hz))
}

func assertPeerHearsTone(t *testing.T, call peers.Call, hz int) {
	t.Helper()
	s, rate := recordPeer(t, call, 2*time.Second)
	assertToneSamples(t, "peer hears VoiceBlender", s, rate, float64(hz))
}

// assertMediaBothWays plays a different tone from each side and checks each
// arrives at the other.
func assertMediaBothWays(t *testing.T, inst *testInstance, legID string, call peers.Call) {
	t.Helper()
	const vbHz, peerHz = 1100, 700
	pb := playToneOnVB(t, inst, legID, vbHz)
	if err := call.PlayTone(peerHz); err != nil {
		t.Fatalf("peer tone: %v", err)
	}
	assertPeerHearsTone(t, call, vbHz)
	assertVBHearsTone(t, inst, legID, peerHz)
	stopPlaybackOnVB(t, inst, legID, pb)
	call.StopMedia()
}
