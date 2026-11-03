package codec

import (
	"math"
	"math/rand/v2"
	"testing"

	amrnb "github.com/VoiceBlender/goamr-nb"
	amrwb "github.com/VoiceBlender/goamr-wb"
)

func amrTone(n, rate int) []int16 {
	out := make([]int16, n)
	for i := range out {
		t := float64(i) / float64(rate)
		out[i] = int16(6000*math.Sin(2*math.Pi*300*t) + 3000*math.Sin(2*math.Pi*1100*t))
	}
	return out
}

func amrNoise(n int, amp float64, seed uint64) []int16 {
	rng := rand.New(rand.NewPCG(seed, 1))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * (2*rng.Float64() - 1))
	}
	return out
}

type amrVariant struct {
	name         string
	frameSamples int
	rate         int
	sidFT        int
	newSession   func(AMRConfig) (Encoder, Decoder, error)
	// peerPayload encodes one frame on an independent encoder that asks this
	// side for mode m.
	peerPayload func(t *testing.T, octetAligned bool, m int) []byte
}

var amrVariants = []amrVariant{
	{
		name: "AMR-WB", frameSamples: amrwb.FrameSamples, rate: amrwb.SampleRate, sidFT: amrwb.FTSID,
		newSession: NewAMRWBSession,
		peerPayload: func(t *testing.T, octetAligned bool, m int) []byte {
			enc, err := amrwb.NewEncoder(amrwb.EncoderConfig{Mode: amrwb.Mode1265, OctetAligned: octetAligned})
			if err != nil {
				t.Fatal(err)
			}
			enc.SetModeRequest(amrwb.Mode(m))
			p, err := enc.Encode(amrTone(amrwb.FrameSamples, amrwb.SampleRate))
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
	},
	{
		name: "AMR-NB", frameSamples: amrnb.FrameSamples, rate: amrnb.SampleRate, sidFT: amrnb.FTSID,
		newSession: NewAMRNBSession,
		peerPayload: func(t *testing.T, octetAligned bool, m int) []byte {
			enc, err := amrnb.NewEncoder(amrnb.EncoderConfig{Mode: amrnb.Mode0795, OctetAligned: octetAligned})
			if err != nil {
				t.Fatal(err)
			}
			enc.SetModeRequest(amrnb.Mode(m))
			p, err := enc.Encode(amrTone(amrnb.FrameSamples, amrnb.SampleRate))
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
	},
}

// TestAMRSessionDTX feeds stationary noise through a session pair: the encoder
// must settle into SID updates with empty payloads between them, and the
// decoder must report DTX and fill the empty intervals with comfort noise.
func TestAMRSessionDTX(t *testing.T) {
	for _, v := range amrVariants {
		for _, octet := range []bool{true, false} {
			enc, dec, err := v.newSession(AMRConfig{Mode: 2, OctetAligned: octet})
			if err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			gd := dec.(GapDecoder)
			var sid, empty int
			var noise float64
			var out []byte
			var pcm []int16
			for f := 0; f < 300; f++ {
				out, err = EncodeInto(enc, out, amrNoise(v.frameSamples, 800, uint64(f)))
				if err != nil {
					t.Fatal(err)
				}
				if len(out) == 0 {
					empty++
					if !gd.InDTX() {
						t.Fatalf("%s octet=%v frame %d: empty payload while decoder not in DTX", v.name, octet, f)
					}
					pcm, err = gd.DecodeGap(pcm)
				} else {
					if _, ft, ok := amrToC(out, octet); ok && ft == v.sidFT {
						sid++
					}
					pcm, err = DecodeInto(dec, pcm, out)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(pcm) != v.frameSamples {
					t.Fatalf("%s: %d samples, want %d", v.name, len(pcm), v.frameSamples)
				}
				if f >= 150 {
					for _, s := range pcm {
						noise += float64(s) * float64(s)
					}
				}
			}
			if sid == 0 || empty < 5*sid {
				t.Errorf("%s octet=%v: %d SID and %d empty payloads, want DTX", v.name, octet, sid, empty)
			}
			if noise == 0 {
				t.Errorf("%s octet=%v: comfort noise is digital silence", v.name, octet)
			}
		}
	}
}

func TestAMRSessionTalkspurt(t *testing.T) {
	for _, v := range amrVariants {
		enc, _, err := v.newSession(AMRConfig{Mode: 2, OctetAligned: true})
		if err != nil {
			t.Fatal(err)
		}
		ts := enc.(DTXEncoder)
		speech := amrTone(v.frameSamples, v.rate)
		if _, err := enc.Encode(speech); err != nil {
			t.Fatal(err)
		}
		if !ts.Talkspurt() {
			t.Errorf("%s: first speech payload is not a talkspurt", v.name)
		}
		if _, err := enc.Encode(speech); err != nil {
			t.Fatal(err)
		}
		if ts.Talkspurt() {
			t.Errorf("%s: second speech payload marked as talkspurt", v.name)
		}
	}
}

// TestAMRSessionFollowsModeRequest checks that a CMR the decoder sees lowers
// the paired encoder's mode, and that a request above the configured mode is
// capped at it.
func TestAMRSessionFollowsModeRequest(t *testing.T) {
	for _, v := range amrVariants {
		for _, octet := range []bool{true, false} {
			for _, c := range []struct{ ceiling, request, want int }{
				{ceiling: 7, request: 1, want: 1},
				{ceiling: 3, request: 6, want: 3},
			} {
				enc, dec, err := v.newSession(AMRConfig{Mode: c.ceiling, OctetAligned: octet})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := dec.Decode(v.peerPayload(t, octet, c.request)); err != nil {
					t.Fatalf("%s: decode: %v", v.name, err)
				}
				p, err := enc.Encode(amrTone(v.frameSamples, v.rate))
				if err != nil {
					t.Fatal(err)
				}
				if _, ft, ok := amrToC(p, octet); !ok || ft != c.want {
					t.Errorf("%s octet=%v ceiling=%d request=%d: frame type %d, want %d",
						v.name, octet, c.ceiling, c.request, ft, c.want)
				}
			}
		}
	}
}

func TestAMRSessionModeRequestStaysInModeSet(t *testing.T) {
	for _, v := range amrVariants {
		enc, dec, err := v.newSession(AMRConfig{Mode: 7, OctetAligned: true, ModeSet: []int{0, 4, 7}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Decode(v.peerPayload(t, true, 6)); err != nil {
			t.Fatal(err)
		}
		p, err := enc.Encode(amrTone(v.frameSamples, v.rate))
		if err != nil {
			t.Fatal(err)
		}
		if _, ft, _ := amrToC(p, true); ft != 4 {
			t.Errorf("%s: request 6 with mode-set 0,4,7 sent frame type %d, want 4", v.name, ft)
		}
	}
}

func TestAMRSessionRejectsModeOutsideModeSet(t *testing.T) {
	if _, _, err := NewAMRWBSession(AMRConfig{Mode: 8, ModeSet: []int{0, 2}}); err == nil {
		t.Error("AMR-WB: mode 8 outside mode-set 0,2 accepted")
	}
	if _, _, err := NewAMRNBSession(AMRConfig{Mode: 7, ModeSet: []int{0, 2}}); err == nil {
		t.Error("AMR-NB: mode 7 outside mode-set 0,2 accepted")
	}
}

func TestAMRSessionIntoReusesBuffers(t *testing.T) {
	enc, dec, err := NewAMRWBSession(AMRConfig{Mode: 8, OctetAligned: true})
	if err != nil {
		t.Fatal(err)
	}
	speech := amrTone(amrwb.FrameSamples, amrwb.SampleRate)
	out, _ := EncodeInto(enc, nil, speech)
	pcm, _ := DecodeInto(dec, nil, out)
	allocs := testing.AllocsPerRun(50, func() {
		out, _ = EncodeInto(enc, out, speech)
		pcm, _ = DecodeInto(dec, pcm, out)
	})
	if allocs != 0 {
		t.Errorf("EncodeInto/DecodeInto allocate %.1f times per frame, want 0", allocs)
	}
}

func TestAMRToC(t *testing.T) {
	for _, c := range []struct {
		name   string
		p      []byte
		octet  bool
		frames int
		lastFT int
		wantOK bool
	}{
		{name: "octet single", p: []byte{0xf0, 0x44, 0, 0}, octet: true, frames: 1, lastFT: 8, wantOK: true},
		{name: "octet two", p: []byte{0xf0, 0x94, 0x7c}, octet: true, frames: 2, lastFT: 15, wantOK: true},
		{name: "octet unterminated", p: []byte{0xf0, 0x94}, octet: true},
		{name: "octet empty", p: []byte{0xf0}, octet: true},
		// CMR=15, F=0, FT=9, Q=1: 1111 0 1001 1 -> 0xf4 0xc0
		{name: "be single", p: []byte{0xf4, 0xc0}, frames: 1, lastFT: 9, wantOK: true},
		// CMR=15, F=1 FT=2 Q=1, F=0 FT=15 Q=1: 1111 1 0010 1 0 1111 1 -> 0xf9 0x4f 0x80
		{name: "be two", p: []byte{0xf9, 0x5f, 0x80}, frames: 2, lastFT: 15, wantOK: true},
		{name: "be short", p: []byte{0xf8}},
	} {
		frames, ft, ok := amrToC(c.p, c.octet)
		if ok != c.wantOK || frames != c.frames || ft != c.lastFT {
			t.Errorf("%s: amrToC = (%d, %d, %v), want (%d, %d, %v)", c.name, frames, ft, ok, c.frames, c.lastFT, c.wantOK)
		}
	}
}
