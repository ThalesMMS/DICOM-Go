//go:build !windows

package main

import "time"

func highResolutionClock() func() int64 {
	origin := time.Now()
	return func() int64 { return time.Since(origin).Nanoseconds() }
}
