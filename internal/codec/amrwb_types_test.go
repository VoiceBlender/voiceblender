package codec

import (
	"math"
	"strconv"
	"testing"
)

func TestAMRWBTypeRegistration(t *testing.T) {
	if CodecAMRWB.String() != "AMR-WB" {
		t.Errorf("String() = %q, want AMR-WB", CodecAMRWB.String())
	}
	if CodecAMRWB.ClockRate() != 16000 {
		t.Errorf("ClockRate() = %d, want 16000", CodecAMRWB.ClockRate())
	}
	if CodecAMRWB.SampleRate() != 16000 {
		t.Errorf("SampleRate() = %d, want 16000", CodecAMRWB.SampleRate())
	}
	if CodecTypeFromName("AMR-WB") != CodecAMRWB {
		t.Error("CodecTypeFromName(\"AMR-WB\") did not resolve to CodecAMRWB")
	}
	if CodecTypeFromName("amrwb") != CodecAMRWB {
		t.Error("CodecTypeFromName(\"amrwb\") did not resolve to CodecAMRWB")
	}
}

// AMR-WB has no static payload type; the SDP parser must resolve it by rtpmap
// name, so CodecTypeFromPT must NOT claim the dynamic default PT.
func TestAMRWBNotStaticPT(t *testing.T) {
	if CodecTypeFromPT(96) == CodecAMRWB {
		t.Error("CodecTypeFromPT(96) should not statically map to AMR-WB")
	}
}

func TestAMRWBFactory(t *testing.T) {
	enc, err := NewEncoder(CodecAMRWB)
	if err != nil || enc == nil {
		t.Fatalf("NewEncoder(CodecAMRWB) = %v, %v", enc, err)
	}
	dec, err := NewDecoder(CodecAMRWB)
	if err != nil || dec == nil {
		t.Fatalf("NewDecoder(CodecAMRWB) = %v, %v", dec, err)
	}
}

// TestAMRWBFactoryRoundTrip exercises the integration glue (int mode selection
// and RFC 4867 framing) through the public factory for both payload formats.
func TestAMRWBFactoryRoundTrip(t *testing.T) {
	for _, octetAligned := range []bool{true, false} {
		enc, err := NewAMRWBEncoder(8, octetAligned)
		if err != nil {
			t.Fatalf("NewAMRWBEncoder(octetAligned=%v): %v", octetAligned, err)
		}
		dec := NewAMRWBDecoder(octetAligned)

		in := make([]int16, 320)
		for i := range in {
			in[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/16000))
		}
		payload, err := enc.Encode(in)
		if err != nil {
			t.Fatalf("Encode(octetAligned=%v): %v", octetAligned, err)
		}
		if len(payload) == 0 {
			t.Fatalf("Encode(octetAligned=%v): empty payload", octetAligned)
		}
		out, err := dec.Decode(payload)
		if err != nil {
			t.Fatalf("Decode(octetAligned=%v): %v", octetAligned, err)
		}
		if len(out) != 320 {
			t.Errorf("Decode(octetAligned=%v) = %d samples, want 320", octetAligned, len(out))
		}
	}
}

// TestAMRWBAmplitudePreserved guards the encoder's 6 dB pre-attenuation
// (added in goamr-wb to keep the LPC synthesis filter from saturating at the
// remote decoder) and that no later change re-introduces unbounded gain.
// Feeds 1 s of a 1 kHz sine at -6 dBFS through encode→decode for every mode
// and both payload formats. Expected decoded peak is roughly -12 dBFS
// (input was -6, encoder attenuates 6 dB, decoder roughly preserves).
func TestAMRWBAmplitudePreserved(t *testing.T) {
	const (
		sampleRate   = 16000
		freq         = 1000.0
		seconds      = 1
		inputPeak    = 16384 // ~ -6 dBFS for int16
		samplesPerFr = 320   // 20 ms at 16 kHz
		framesPerSec = sampleRate / samplesPerFr
		minAllowed   = 3000  // ~ -20 dBFS — catches gross attenuation
		maxAllowed   = 16000 // ~ -6 dBFS — catches the original blow-up bug
	)

	in := make([]int16, sampleRate*seconds)
	for i := range in {
		in[i] = int16(float64(inputPeak) * math.Sin(2*math.Pi*freq*float64(i)/sampleRate))
	}

	for _, octetAligned := range []bool{true, false} {
		for mode := 0; mode <= 8; mode++ {
			label := "BE"
			if octetAligned {
				label = "OA"
			}
			t.Run(label+"/mode="+strconv.Itoa(mode), func(t *testing.T) {
				enc, err := NewAMRWBEncoder(mode, octetAligned)
				if err != nil {
					t.Fatalf("NewAMRWBEncoder(mode=%d, octetAligned=%v): %v", mode, octetAligned, err)
				}
				dec := NewAMRWBDecoder(octetAligned)

				var decoded []int16
				for f := 0; f < framesPerSec*seconds; f++ {
					frame := in[f*samplesPerFr : (f+1)*samplesPerFr]
					payload, err := enc.Encode(frame)
					if err != nil {
						t.Fatalf("Encode frame %d: %v", f, err)
					}
					out, err := dec.Decode(payload)
					if err != nil {
						t.Fatalf("Decode frame %d: %v", f, err)
					}
					decoded = append(decoded, out...)
				}

				// Skip the codec's analysis warmup (~2 frames) before measuring.
				measured := decoded[samplesPerFr*2:]
				peak := int16(0)
				for _, s := range measured {
					a := s
					if a < 0 {
						a = -a
					}
					if a > peak {
						peak = a
					}
				}
				t.Logf("mode=%d %s input peak=%d decoded peak=%d", mode, label, inputPeak, peak)
				if peak < minAllowed {
					t.Errorf("mode=%d %s: decoded peak %d below %d (input was %d) — encoder/decoder attenuating signal",
						mode, label, peak, minAllowed, inputPeak)
				}
				if peak > maxAllowed {
					t.Errorf("mode=%d %s: decoded peak %d above %d (input was %d) — encoder/decoder amplifying signal, likely the Microsip 'loud/clipping' bug",
						mode, label, peak, maxAllowed, inputPeak)
				}
			})
		}
	}
}
