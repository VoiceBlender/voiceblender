//go:build cuda

package voxlock

import (
	"errors"
	"fmt"

	vl "github.com/VoiceBlender/voxlock-go"
	"github.com/VoiceBlender/voxlock-go/cuda"
)

func openDevice(ordinal, calls int) (vl.Accelerator, string, func() error, error) {
	if cuda.Devices() == 0 {
		return nil, "", nil, errors.New("no CUDA GPU or driver")
	}
	d, err := cuda.Open(cuda.Options{GPU: ordinal, Slots: calls, Voices: calls})
	if err != nil {
		return nil, "", nil, err
	}
	prec, lib := "fp32", "own kernels"
	if d.FP16() {
		prec = "fp16"
	}
	if d.CuBLAS() {
		lib = "cuBLAS"
	}
	return d, fmt.Sprintf("%s (%s, %s)", d.Name(), prec, lib), d.Close, nil
}
