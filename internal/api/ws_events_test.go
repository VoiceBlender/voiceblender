package api

import "testing"

func TestIsDropLogThreshold(t *testing.T) {
	cases := []struct {
		n    int64
		want bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{2, false},
		{9, false},
		{10, true},
		{11, false},
		{99, false},
		{100, true},
		{500, false},
		{1000, true},
		{10000, true},
		{99999, false},
		{100000, true},
		{1_000_000, true},
	}
	for _, c := range cases {
		if got := isDropLogThreshold(c.n); got != c.want {
			t.Errorf("isDropLogThreshold(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}
