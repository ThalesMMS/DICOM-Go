//go:build !windows

package render

import (
	"testing"
	"time"
)

func volumeLoadClock(t *testing.T) (func() int64, int64) {
	t.Helper()
	origin := time.Now()
	return func() int64 { return time.Since(origin).Nanoseconds() }, 1e9
}
