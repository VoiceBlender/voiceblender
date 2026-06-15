package codec

import (
	"math"
	"testing"
)

func TestAMRNBTypeRegistration(t *testing.T) {
	if CodecAMRNB.String() != "AMR-NB" {
		t.Errorf("String() = %q, want AMR-NB", CodecAMRNB.String())
	}
	if CodecAMRNB.ClockRate() != 8000 {
		t.Errorf("ClockRate() = %d, want 8000", CodecAMRNB.ClockRate())
	}
	if CodecAMRNB.SampleRate() != 8000 {
		t.Errorf("SampleRate() = %d, want 8000", CodecAMRNB.SampleRate())
	}
	for _, name := range []string{"AMR-NB", "amrnb", "AMR"} {
		if CodecTypeFromName(name) != CodecAMRNB {
			t.Errorf("CodecTypeFromName(%q) did not resolve to CodecAMRNB", name)
		}
	}
}

// AMR-NB has no static payload type; the SDP parser must resolve it by rtpmap
// name, so CodecTypeFromPT must NOT claim the dynamic default PT.
func TestAMRNBNotStaticPT(t *testing.T) {
	if CodecTypeFromPT(97) == CodecAMRNB {
		t.Error("CodecTypeFromPT(97) should not statically map to AMR-NB")
	}
}

func TestAMRNBFactory(t *testing.T) {
	enc, err := NewEncoder(CodecAMRNB)
	if err != nil || enc == nil {
		t.Fatalf("NewEncoder(CodecAMRNB) = %v, %v", enc, err)
	}
	dec, err := NewDecoder(CodecAMRNB)
	if err != nil || dec == nil {
		t.Fatalf("NewDecoder(CodecAMRNB) = %v, %v", dec, err)
	}
}

// TestAMRNBFactoryRoundTrip exercises the integration glue (int mode selection
// and RFC 4867 framing) through the public factory for both payload formats.
func TestAMRNBFactoryRoundTrip(t *testing.T) {
	for _, octetAligned := range []bool{true, false} {
		enc, err := NewAMRNBEncoder(7, octetAligned)
		if err != nil {
			t.Fatalf("NewAMRNBEncoder(octetAligned=%v): %v", octetAligned, err)
		}
		dec := NewAMRNBDecoder(octetAligned)

		in := make([]int16, 160)
		for i := range in {
			in[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/8000))
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
		if len(out) != 160 {
			t.Errorf("Decode(octetAligned=%v) = %d samples, want 160", octetAligned, len(out))
		}
	}
}
