//go:build windows

package main

import (
	"os"
	"syscall"
)

var (
	stopSignals  = []os.Signal{os.Interrupt, syscall.SIGTERM}
	drainSignals []os.Signal
)

func isDrainSignal(os.Signal) bool { return false }
