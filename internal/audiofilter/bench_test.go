package audiofilter

import (
	"io"
	"testing"
)

func benchChain(b *testing.B, srcRate, dstRate int, specs []Spec) {
	b.Helper()
	data := toBytes(tone(srcRate*120, srcRate, 440, 8000))
	src := &chunkSource{data: data, n: srcRate / 50 * 2}
	r, err := Build(src, srcRate, dstRate, specs)
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, dstRate/50*2)
	for i := 0; i < 100; i++ {
		r.Read(buf)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(buf); err == io.EOF {
			src.off = 0
			b.StopTimer()
			r, _ = Build(src, srcRate, dstRate, specs)
			b.StartTimer()
		}
	}
}

func BenchmarkBandpass16k(b *testing.B) {
	benchChain(b, 16000, 16000, []Spec{{Type: "bandpass"}})
}

func BenchmarkBandpassGain16k(b *testing.B) {
	benchChain(b, 16000, 16000, []Spec{{Type: "bandpass"}, {Type: "gain", Params: Params{"volume": 1}}})
}

func BenchmarkBandpass8kTo16k(b *testing.B) {
	benchChain(b, 8000, 16000, []Spec{{Type: "bandpass"}})
}
