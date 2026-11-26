//go:build !windows

package imagefilter

import (
	"testing"
	"time"
)

func phase2DCampaignClock(t *testing.T) (func() int64, int64, string) {
	t.Helper()
	start := time.Now()
	return func() int64 { return time.Since(start).Nanoseconds() }, 1e9, "Go monotonic time"
}
