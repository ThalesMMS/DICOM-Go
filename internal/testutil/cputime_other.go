//go:build !unix && !windows

package testutil

import "time"

func processCPUTime() (time.Duration, bool) { return 0, false }
