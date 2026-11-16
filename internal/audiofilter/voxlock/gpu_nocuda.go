//go:build voxlock && !cuda

package voxlock

import (
	"errors"

	vl "github.com/VoiceBlender/voxlock-go"
)

// The CUDA loader links the binary dynamically against glibc, which the
// static Alpine image lacks, so the GPU is opt-in at build time.
func openDevice(int, int) (vl.Accelerator, string, func() error, error) {
	return nil, "", nil, errors.New("built without -tags cuda")
}
