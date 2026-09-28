//go:build (!amd64 && !arm64) || purego

package resampler

func dotF64(a, b []float64) float64 { return dotF64Generic(a, b) }
