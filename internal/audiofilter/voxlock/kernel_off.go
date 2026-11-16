//go:build !voxlock && !cuda

package voxlock

import "errors"

// Compiled reports whether the build carries the kernel.
const Compiled = false

type kernel struct{ backend string }

type stream struct{ frame int }

func newKernel(Config) (*kernel, error) {
	return nil, errors.New(Name + " built without -tags voxlock")
}

func (*kernel) Close() error                 { return nil }
func (*kernel) Stats() Stats                 { return Stats{} }
func (*kernel) acquire(int) (*stream, error) { return nil, errUnavailable{} }
func (*stream) process([]int16) error        { return nil }
func (*stream) release()                     {}
