package resampler

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func dotNaive(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func checkDot(t *testing.T, a, b []float64) {
	t.Helper()
	want := dotNaive(a, b)
	var mag float64
	for i := range a {
		mag += math.Abs(a[i] * b[i])
	}
	tol := 1e-12 * (mag + 1)
	if got := dotF64(a, b); math.Abs(got-want) > tol {
		t.Fatalf("dotF64 len=%d: got %v want %v", len(a), got, want)
	}
	if got := dotF64Generic(a, b); math.Abs(got-want) > tol {
		t.Fatalf("dotF64Generic len=%d: got %v want %v", len(a), got, want)
	}
}

func randVec(r *rand.Rand, n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = r.Float64()*2 - 1
	}
	return v
}

func TestDotF64Lengths(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n <= 300; n++ {
		// Offset by one element so loads are not 32-byte aligned.
		a := randVec(r, n+1)[1:]
		b := randVec(r, n+3)[1:]
		checkDot(t, a, b)
	}
}

func TestDotF64FilterLengths(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	lens := map[int]bool{}
	for q := range qualityMap {
		for _, rates := range [][2]int{{8000, 16000}, {16000, 8000}, {44100, 8000}, {48000, 8000}, {48000, 16000}} {
			rs := New(1, rates[0], rates[1], q)
			lens[rs.filtLen] = true
		}
	}
	for n := range lens {
		checkDot(t, randVec(r, n), randVec(r, n))
	}
}

func FuzzDotF64(f *testing.F) {
	f.Add(int64(0), uint16(16))
	f.Add(int64(1), uint16(129))
	f.Fuzz(func(t *testing.T, seed int64, n uint16) {
		r := rand.New(rand.NewSource(seed))
		size := int(n % 1024)
		checkDot(t, randVec(r, size), randVec(r, size))
	})
}

func BenchmarkDotF64(b *testing.B) {
	r := rand.New(rand.NewSource(3))
	for _, n := range []int{64, 128, 352} {
		x, y := randVec(r, n), randVec(r, n)
		b.Run(fmt.Sprintf("n=%d/dispatch", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = dotF64(x, y)
			}
		})
		b.Run(fmt.Sprintf("n=%d/generic", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = dotF64Generic(x, y)
			}
		})
		b.Run(fmt.Sprintf("n=%d/naive", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = dotNaive(x, y)
			}
		})
	}
}
