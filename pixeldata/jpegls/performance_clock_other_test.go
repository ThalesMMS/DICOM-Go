//go:build !windows

package jpegls

import (
	"testing"
	"time"
)

func newPerformanceClock(t *testing.T) performanceClock {
	t.Helper()
	start := time.Now()
	return performanceClock{name: "time.Since monotonic", frequency: 1e9, now: func() int64 { return time.Since(start).Nanoseconds() }}
}
