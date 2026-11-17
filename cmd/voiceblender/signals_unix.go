//go:build !windows

package main

import (
	"os"
	"syscall"
)

var (
	stopSignals  = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	drainSignals = []os.Signal{syscall.SIGUSR1}
)

func isDrainSignal(sig os.Signal) bool { return sig == syscall.SIGUSR1 }
