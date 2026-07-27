package resampler

import (
	"fmt"
	"math"
	"testing"
)

var (
	wave = []float64{
		//-1 to 1 (16 samples)
		-1, -0.75, -0.5, -0.25, 0, 0.25, 0.5, 0.75, 1, 1, 1, 1, 1, 1, 1, 1,
		//1 to -1 (16 samples)
		1, 0.75, 0.5, 0.25, 0, -0.25, -0.5, -0.75, -1, -1, -1, -1, -1, -1, -1, -1,
	}
)

func resample64(t *testing.T, inSamplerate, outSamplerate, q int) []float64 {
	r := New(1, inSamplerate, outSamplerate, q)
	t.Log("samplerate:", "in:", inSamplerate, "out:", outSamplerate)
	t.Log("latency:", "in:", r.InputLatency(), "out:", r.OutputLatency())

	in := make([]float64, inSamplerate/100)
	out := make([]float64, outSamplerate/100)

	copy(in, wave)
	read, written := r.ProcessFloat64(0, in, out)
	t.Log("read:", read)
	t.Log("written:", written)

	s := ""
	out = out[r.OutputLatency():][:(32*outSamplerate)/inSamplerate]
	for _, v := range out {
		s += fmt.Sprintf("%0.3f ", v)
	}
	t.Log("wave:", s)
	return out
}

func resample32(t *testing.T, inSamplerate, outSamplerate, q int) []float32 {
	r := New(1, inSamplerate, outSamplerate, q)
	t.Log("samplerate:", "in:", inSamplerate, "out:", outSamplerate)
	t.Log("latency:", "in:", r.InputLatency(), "out:", r.OutputLatency())

	in := make([]float32, inSamplerate/100)
	out := make([]float32, outSamplerate/100)
	for i, s := range wave {
		in[i] = float32(s)
	}

	read, written := r.ProcessFloat32(0, in, out)
	t.Log("read:", read)
	t.Log("written:", written)

	s := ""
	out = out[r.OutputLatency():][:(32*outSamplerate)/inSamplerate]
	for _, v := range out {
		s += fmt.Sprintf("%0.3f ", v)
	}
	t.Log("wave:", s)
	return out
}

func TestSame64(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample64(t, 48000, 48000, q)
		if math.Abs(out[8]-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(out[24] - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestSame32(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample32(t, 48000, 48000, q)
		if math.Abs(float64(out[8])-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(float64(out[24]) - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestDirectDownSampling64(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample64(t, 48000, 24000, q)
		if math.Abs(out[4]-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(out[12] - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestDirectDownSampling32(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample32(t, 48000, 24000, q)
		if math.Abs(float64(out[4])-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(float64(out[12]) - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestDirectUpSampling64(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample64(t, 24000, 48000, q)
		if math.Abs(out[16]-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(out[48] - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestDirectUpSampling32(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample32(t, 24000, 48000, q)
		if math.Abs(float64(out[16])-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(float64(out[48]) - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestInterpolateDownSampling64(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample64(t, 48000, 23999, q)
		if math.Abs(out[4]-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(out[12] - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestInterpolateDownSampling32(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample32(t, 48000, 23999, q)
		if math.Abs(float64(out[4])-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(float64(out[12]) - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestInterpolateUpSampling64(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample64(t, 23999, 48000, q)
		if math.Abs(out[16]-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(out[48] - -1.0) > 0.1 {
			t.Fail()
		}
	}
}

func TestInterpolateUpSampling32(t *testing.T) {
	for q := 0; q < 11; q++ {
		out := resample32(t, 23999, 48000, q)
		if math.Abs(float64(out[16])-1.0) > 0.1 {
			t.Fail()
		}
		if math.Abs(float64(out[48]) - -1.0) > 0.1 {
			t.Fail()
		}
	}
}
