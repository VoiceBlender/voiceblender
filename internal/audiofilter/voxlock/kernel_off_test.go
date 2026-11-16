//go:build !voxlock && !cuda

package voxlock

import (
	"strings"
	"testing"
)

func TestBuiltWithoutKernel(t *testing.T) {
	err := Install(Config{Model: "voxlock.bin", Int8: true, FrameMs: 16})
	if err == nil || !strings.Contains(err.Error(), "-tags voxlock") {
		t.Fatalf("Install = %v, want the missing build tag", err)
	}
	if Available() || Backend() != "" {
		t.Error("the filter must stay unavailable")
	}
}
