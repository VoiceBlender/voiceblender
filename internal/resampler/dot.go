package resampler

// dotF64Generic returns the dot product of a and b[:len(a)]. Independent
// accumulators break the FP-add dependency chain, which dominates the scalar
// loop's cost.
func dotF64Generic(a, b []float64) float64 {
	n := len(a)
	if n == 0 {
		return 0
	}
	b = b[:n]
	var s0, s1, s2, s3 float64
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}
