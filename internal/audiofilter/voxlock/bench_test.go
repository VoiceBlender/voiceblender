package voxlock

import "testing"

// BenchmarkProcessFrame is a locked stream on the CPU, per rate.
func BenchmarkProcessFrame(b *testing.B) {
	install(b, testConfig(b))
	for _, rate := range Rates {
		b.Run(map[int]string{8000: "8k", 16000: "16k"}[rate], func(b *testing.B) {
			st, err := current().acquire(rate)
			if err != nil {
				b.Fatal(err)
			}
			defer st.release()
			in := talker(b)
			if rate == 8000 {
				for i := range len(in) / 2 {
					in[i] = in[2*i]
				}
				in = in[:len(in)/2]
			}
			frame := make([]int16, st.frame)
			for off := 0; !st.l.Locked(); off = (off + st.frame) % (len(in) - st.frame) {
				copy(frame, in[off:])
				st.process(frame)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				copy(frame, in[i*st.frame%(len(in)-st.frame):])
				if err := st.process(frame); err != nil {
					b.Fatal(err)
				}
			}
			ms := float64(st.frame) * 1000 / float64(rate)
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)*10/ms, "µs/10ms")
		})
	}
}
