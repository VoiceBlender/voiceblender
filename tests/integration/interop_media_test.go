//go:build integration && interop

package integration

import (
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/tests/integration/peers"
)

// interopCallSetups places a call in each direction and returns it connected.
var interopCallSetups = []struct {
	name    string
	connect func(t *testing.T, inst *testInstance, p peers.Peer, codecs []string, ptime int) (string, peers.Call)
}{
	{"vb-dials", func(t *testing.T, inst *testInstance, p peers.Peer, codecs []string, ptime int) (string, peers.Call) {
		return connectVBToPeer(t, inst, p, interopDial{Codecs: codecs}, peers.AnswerOpts{Codec: codecs[0], Ptime: ptime})
	}},
	{"peer-dials", func(t *testing.T, inst *testInstance, p peers.Peer, codecs []string, ptime int) (string, peers.Call) {
		call, legID := connectPeerToVB(t, inst, p, peers.OriginateOpts{Codecs: codecs, Ptime: ptime}, nil)
		return legID, call
	}},
}

// assertNegotiated checks both ends run codec.
func assertNegotiated(t *testing.T, inst *testInstance, legID string, call peers.Call, codec string) {
	t.Helper()
	if got := primaryStream(t, inst.baseURL(), legID); !strings.EqualFold(got.Codec, codec) {
		t.Errorf("VoiceBlender runs %s at %d Hz, want %s", got.Codec, got.SampleRate, codec)
	}
	if got := call.Codec(); !strings.EqualFold(got, codec) {
		t.Errorf("peer runs %s, want %s", got, codec)
	}
}

func TestInterop_Media_Codecs(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, codec := range interopCodecs(p) {
			for _, setup := range interopCallSetups {
				t.Run(codec+"/"+setup.name, func(t *testing.T) {
					legID, call := setup.connect(t, inst, p, []string{codec}, 0)
					assertNegotiated(t, inst, legID, call, codec)
					assertMediaBothWays(t, inst, legID, call)
					hangupOnVB(t, inst, legID)
					waitPeerHangup(t, call, 5*time.Second)
				})
			}
		}
	})
}

// A 5 kHz tone survives only if the call stays wideband end to end; any hop
// through 8 kHz removes it.
func TestInterop_Media_WidebandEndToEnd(t *testing.T) {
	const probeHz = 5000
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, codec := range interopCodecs(p) {
			if !isWideband(codec) {
				continue
			}
			for _, setup := range interopCallSetups {
				t.Run(codec+"/"+setup.name, func(t *testing.T) {
					legID, call := setup.connect(t, inst, p, []string{codec}, 0)

					pb := playToneOnVB(t, inst, legID, probeHz)
					assertPeerHearsTone(t, call, probeHz)
					stopPlaybackOnVB(t, inst, legID, pb)

					if err := call.PlayTone(probeHz); err != nil {
						t.Fatal(err)
					}
					assertVBHearsTone(t, inst, legID, probeHz)
					call.StopMedia()
				})
			}
		}
	})
}

func TestInterop_Media_EchoRoundTrip(t *testing.T) {
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, codec := range interopCodecs(p) {
			t.Run(codec, func(t *testing.T) {
				legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{codec}}, peers.AnswerOpts{Codec: codec})
				if err := call.Echo(); err != nil {
					t.Fatal(err)
				}
				playToneOnVB(t, inst, legID, 1300)
				time.Sleep(300 * time.Millisecond)
				assertVBHearsTone(t, inst, legID, 1300)
			})
		}
	})
}

// assertContinuous fails on any 100 ms window much quieter than the tone,
// which is how a dropout from a timestamp or jitter-buffer slip shows up.
func assertContinuous(t *testing.T, label string, s []int16, rate int) {
	t.Helper()
	win := rate / 10
	skip := rate / 2
	if len(s) < skip+win*10 {
		t.Fatalf("%s: only %d samples", label, len(s))
	}
	var levels []float64
	for off := skip; off+win <= len(s)-win; off += win {
		levels = append(levels, rmsInt16(s[off:off+win]))
	}
	sorted := append([]float64(nil), levels...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	var gaps []int
	for i, l := range levels {
		if l < median/2 {
			gaps = append(gaps, i)
		}
	}
	t.Logf("%s: %d windows, median rms %.0f, %d quiet", label, len(levels), median, len(gaps))
	if median < 300 {
		t.Fatalf("%s: no tone (median rms %.0f)", label, median)
	}
	if len(gaps) > 0 {
		t.Errorf("%s: %d dropouts in %d windows (first at %d ms)", label, len(gaps), len(levels), 500+gaps[0]*100)
	}
}

func TestInterop_Media_Continuity(t *testing.T) {
	if testing.Short() {
		t.Skip("20 s per direction")
	}
	forEachPeer(t, func(t *testing.T, p peers.Peer) {
		inst := newInteropVB(t, "vb", nil)
		for _, codec := range []string{"PCMU", "OPUS"} {
			if !p.Caps().HasCodec(codec) {
				continue
			}
			t.Run(codec, func(t *testing.T) {
				legID, call := connectVBToPeer(t, inst, p, interopDial{Codecs: []string{codec}}, peers.AnswerOpts{Codec: codec})
				playToneOnVB(t, inst, legID, 1100)
				if err := call.PlayTone(700); err != nil {
					t.Fatal(err)
				}
				stop, err := call.StartRecording()
				if err != nil {
					t.Fatal(err)
				}
				vbSamples, vbRate := recordVBLeg(t, inst, legID, 20*time.Second)
				path, err := stop()
				if err != nil {
					t.Fatal(err)
				}
				peerSamples, peerRate := readWAVChannel(t, path, 0)
				assertContinuous(t, "peer -> VoiceBlender", vbSamples, vbRate)
				assertContinuous(t, "VoiceBlender -> peer", peerSamples, peerRate)
			})
		}
	})
}
