//go:build !purego

package resampler

// dotF64NEON is implemented in dot_arm64.s. It consumes whole groups of eight
// and ignores any remainder.
//
//go:noescape
func dotF64NEON(a, b []float64) float64

func dotF64(a, b []float64) float64 {
	n := len(a)
	if n < 16 {
		return dotF64Generic(a, b)
	}
	b = b[:n]
	whole := n &^ 7
	s := dotF64NEON(a[:whole], b[:whole])
	for i := whole; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
