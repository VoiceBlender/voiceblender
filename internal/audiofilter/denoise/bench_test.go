package denoise

import (
	"context"
	"sync"
	"testing"
)

// BenchmarkProcessHop reports the cost of one 10 ms hop. Divide ns/op by 1e7
// to read it as a fraction of one core per concurrent stream.
func BenchmarkProcessHop(b *testing.B) {
	if err := Install(context.Background(), 0); err != nil {
		b.Fatal(err)
	}
	defer Shutdown()
	s, err := current().acquire()
	if err != nil {
		b.Fatal(err)
	}
	defer s.release()

	frame := make([]int16, Hop)
	src := bandLimitedB(Hop, 3, 2000, 3400)
	for i := 0; i < 200; i++ {
		copy(frame, src)
		s.process(frame)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(frame, src)
		if err := s.process(frame); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e7*100, "%core/stream")
}

// BenchmarkConcurrentStreams drives many states at once, as separate legs
// would, to expose per-instance lock contention.
func BenchmarkConcurrentStreams(b *testing.B) {
	for _, streams := range []int{1, 8, 24, 48} {
		b.Run(name(streams), func(b *testing.B) {
			if err := Install(context.Background(), DefaultStatesPerInstance); err != nil {
				b.Fatal(err)
			}
			defer Shutdown()
			states := make([]*state, streams)
			for i := range states {
				s, err := current().acquire()
				if err != nil {
					b.Fatal(err)
				}
				states[i] = s
				defer s.release()
			}
			src := bandLimitedB(Hop, 3, 2000, 3400)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var wg sync.WaitGroup
				for _, s := range states {
					wg.Add(1)
					go func(s *state) {
						defer wg.Done()
						frame := make([]int16, Hop)
						copy(frame, src)
						s.process(frame)
					}(s)
				}
				wg.Wait()
			}
		})
	}
}

func name(n int) string {
	if n < 10 {
		return string(rune('0'+n)) + "_streams"
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10)) + "_streams"
}

func bandLimitedB(n int, seed int64, amp, fc float64) []int16 {
	return bandLimitedRaw(n, seed, amp, fc)
}
