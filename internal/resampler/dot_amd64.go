//go:build !purego

package resampler

import "golang.org/x/sys/cpu"

var useFMA = cpu.X86.HasAVX2 && cpu.X86.HasFMA

// dotF64AVX2 is implemented in dot_amd64.s. It consumes whole groups of four
// and ignores any remainder.
//
//go:noescape
func dotF64AVX2(a, b []float64) float64

func dotF64(a, b []float64) float64 {
	n := len(a)
	if !useFMA || n < 16 {
		return dotF64Generic(a, b)
	}
	b = b[:n]
	whole := n &^ 3
	s := dotF64AVX2(a[:whole], b[:whole])
	for i := whole; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
