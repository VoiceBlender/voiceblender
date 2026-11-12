package codec

import (
	"math"
	"testing"

	"github.com/thesyncim/gopus"
)

// opusRun encodes frames of the test signal, decodes them with the frames from
// lostFrom to lostTo-1 concealed, and returns that next to a decode with no
// loss.
func opusRun(t *testing.T, frames, lostFrom, lostTo int) (out, clean []int16) {
	t.Helper()
	enc, err := NewOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := NewOpusDecoder()
	ref, _ := NewOpusDecoder()
	for f := range frames {
		payload, err := enc.Encode(voiced(48000, f*opusGapSamples, opusGapSamples))
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := ref.Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		clean = append(clean, pcm...)
		if f >= lostFrom && f < lostTo {
			pcm, err = dec.DecodeGap(nil)
			if err == nil && len(pcm) != opusGapSamples {
				t.Fatalf("DecodeGap returned %d samples, want %d", len(pcm), opusGapSamples)
			}
		} else {
			pcm, err = dec.Decode(payload)
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, pcm...)
	}
	return out, clean
}

// The decoder's own concealment joins the signal at both edges of a gap: no
// step there is larger than 1.5 times the largest step in the signal itself.
func TestOpusDecodeGapHasNoStepAtEitherEdge(t *testing.T) {
	for _, lost := range []int{1, 3} {
		out, clean := opusRun(t, 40, 20, 20+lost)
		// The encoder's first frames ramp up, so the signal is measured
		// after them.
		bound := maxStep(clean[5*opusGapSamples:]) * 3 / 2
		from, to := 20*opusGapSamples, (20+lost)*opusGapSamples
		if got := maxStep(out[from-opusGapSamples : to+opusGapSamples]); got > bound {
			t.Errorf("%d lost: largest step %d, bound %d", lost, got, bound)
		}
		if got, want := rms(out[from:from+opusGapSamples]), rms(clean[from-opusGapSamples:from]); got < 0.5*want {
			t.Errorf("%d lost: first concealed frame at %.0f rms, the signal was at %.0f", lost, got, want)
		}
	}
}

// The level falls from one concealed frame to the next. The decoder does not
// reach silence by itself: the gap filler stops after three frames.
func TestOpusDecodeGapFades(t *testing.T) {
	out, _ := opusRun(t, 40, 20, 24)
	prev := math.Inf(1)
	for f := 20; f < 24; f++ {
		got := rms(out[f*opusGapSamples : (f+1)*opusGapSamples])
		if got >= prev {
			t.Errorf("concealed frame %d at %.0f rms, the one before at %.0f", f-20, got, prev)
		}
		prev = got
	}
}

// Three frames after a gap the decoder is back on the signal.
func TestOpusDecodeGapReturnsToTheSignal(t *testing.T) {
	out, clean := opusRun(t, 40, 20, 21)
	from := 24 * opusGapSamples
	var errSum float64
	for i := from; i < len(out); i++ {
		d := float64(out[i]) - float64(clean[i])
		errSum += d * d
	}
	if e, ref := math.Sqrt(errSum/float64(len(out)-from)), rms(clean[from:]); e > 0.05*ref {
		t.Errorf("output is %.0f rms away from the signal (%.0f rms) after the gap", e, ref)
	}
}

func TestOpusDecodeFEC(t *testing.T) {
	enc, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: 48000, Channels: 1, Application: gopus.ApplicationVoIP})
	if err != nil {
		t.Fatal(err)
	}
	enc.SetFEC(true)
	if err := enc.SetPacketLoss(20); err != nil {
		t.Fatal(err)
	}
	if err := enc.SetBitrate(24000); err != nil {
		t.Fatal(err)
	}
	fecDec, _ := NewOpusDecoder()
	plcDec, _ := NewOpusDecoder()
	ref, _ := NewOpusDecoder()
	buf := make([]byte, 4000)

	const lost = 20
	var want []int16
	for f := range 30 {
		n, err := enc.EncodeInt16(voiced(48000, f*opusGapSamples, opusGapSamples), buf)
		if err != nil {
			t.Fatal(err)
		}
		payload := buf[:n]
		pcm, err := ref.Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		if f == lost {
			want = pcm
			continue
		}
		if f == lost+1 {
			fec, err := fecDec.DecodeFEC(nil, payload)
			if err != nil || len(fec) != opusGapSamples {
				t.Fatalf("DecodeFEC: %d samples, error %v", len(fec), err)
			}
			plc, err := plcDec.DecodeGap(nil)
			if err != nil {
				t.Fatal(err)
			}
			var fecErr, plcErr float64
			for i := range want {
				df := float64(fec[i]) - float64(want[i])
				dp := float64(plc[i]) - float64(want[i])
				fecErr += df * df
				plcErr += dp * dp
			}
			if fecErr > plcErr/4 {
				t.Errorf("frame rebuilt from redundancy is %.0f rms off, concealment %.0f rms: want under half",
					math.Sqrt(fecErr/opusGapSamples), math.Sqrt(plcErr/opusGapSamples))
			}
		}
		if _, err := fecDec.Decode(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := plcDec.Decode(payload); err != nil {
			t.Fatal(err)
		}
	}
}

// A packet with no redundancy, or too short to carry any, gives concealment.
func TestOpusDecodeFECWithoutRedundancy(t *testing.T) {
	enc, _ := NewOpusEncoder()
	dec, _ := NewOpusDecoder()
	var payload []byte
	for f := range 10 {
		payload, _ = enc.Encode(voiced(48000, f*opusGapSamples, opusGapSamples))
		if _, err := dec.Decode(payload); err != nil {
			t.Fatal(err)
		}
	}
	for _, next := range [][]byte{payload, {payload[0]}} {
		pcm, err := dec.DecodeFEC(nil, next)
		if err != nil || len(pcm) != opusGapSamples {
			t.Fatalf("DecodeFEC with a %d byte packet: %d samples, error %v", len(next), len(pcm), err)
		}
		if rms(pcm) == 0 {
			t.Errorf("DecodeFEC with a %d byte packet gave silence", len(next))
		}
	}
}
