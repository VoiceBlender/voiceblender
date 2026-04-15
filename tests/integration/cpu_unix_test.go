//go:build integration && (linux || darwin)

package integration

import (
	"runtime"
	"syscall"
	"testing"
	"time"
)

// cpuTimes records cumulative process CPU usage at a point in time.
// User + system time come from getrusage(RUSAGE_SELF) — process-wide,
// so they include the test runner itself, but in benchScale the
// dominant work is the bench so process CPU ≈ bench CPU.
type cpuTimes struct {
	user time.Duration
	sys  time.Duration
}

func snapCPU() cpuTimes {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return cpuTimes{}
	}
	return cpuTimes{
		user: time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond,
		sys:  time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond,
	}
}

func (a cpuTimes) sub(b cpuTimes) cpuTimes {
	return cpuTimes{user: a.user - b.user, sys: a.sys - b.sys}
}

// logCPU reports CPU consumption normalized two ways: (a) as a
// percentage of one core, (b) as a percentage of all available cores,
// (c) per-room cost in microseconds of CPU per second of wall-clock.
func logCPU(t testing.TB, label string, cpu cpuTimes, wall time.Duration, numRooms int) {
	t.Helper()
	if wall <= 0 {
		return
	}
	totalCPU := cpu.user + cpu.sys
	wallSec := wall.Seconds()
	cores := runtime.NumCPU()
	pctOneCore := 100 * totalCPU.Seconds() / wallSec
	pctAllCores := pctOneCore / float64(cores)

	if numRooms > 0 {
		perRoomUs := totalCPU.Seconds() * 1e6 / wallSec / float64(numRooms)
		t.Logf("%s: user=%v sys=%v total=%v in %v wall (%.1f%% of 1 core, %.1f%% of %d cores, %.0fµs CPU per room-second)",
			label, cpu.user.Round(time.Millisecond), cpu.sys.Round(time.Millisecond), totalCPU.Round(time.Millisecond),
			wall.Round(time.Millisecond), pctOneCore, pctAllCores, cores, perRoomUs)
	} else {
		t.Logf("%s: user=%v sys=%v total=%v in %v wall (%.1f%% of 1 core, %.1f%% of %d cores)",
			label, cpu.user.Round(time.Millisecond), cpu.sys.Round(time.Millisecond), totalCPU.Round(time.Millisecond),
			wall.Round(time.Millisecond), pctOneCore, pctAllCores, cores)
	}
}
