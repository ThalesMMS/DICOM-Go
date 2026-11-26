package testutil

import "time"

// CPUTime returns the CPU time (user and system) the process has used so
// far. Differences of it measure work in timing assertions without the time
// spent waiting for a core, which dominates wall time on a loaded machine.
// Platforms without a process CPU clock fall back to the wall clock.
func CPUTime() time.Duration {
	if d, ok := processCPUTime(); ok {
		return d
	}
	return time.Since(wallStart)
}

var wallStart = time.Now()
