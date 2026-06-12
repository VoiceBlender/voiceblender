package codec

import (
	"math"
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
