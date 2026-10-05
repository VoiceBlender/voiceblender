package denoisegtcrn

import "testing"

func BenchmarkProcessFrame(b *testing.B) {
	if err := Install(); err != nil {
		b.Fatal(err)
	}
	defer Shutdown()
	for _, rate := range Rates {
		b.Run(itoa(rate), func(b *testing.B) {
			st, err := current().acquire(rate)
			if err != nil {
				b.Fatal(err)
			}
			defer st.release()
			in := noise(st.frame, 1, 2000)
			frame := make([]int16, st.frame)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				copy(frame, in)
				if err := st.process(frame); err != nil {
					b.Fatal(err)
				}
			}
			// One frame is 16 ms of audio.
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)*10/16, "µs/10ms")
		})
	}
}

func itoa(n int) string {
	return map[int]string{8000: "8k", 12000: "12k", 16000: "16k"}[n]
}
