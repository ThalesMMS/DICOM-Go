//go:build codecfull && !windows

package jpeg2000

import "time"

const nativeCostClockName = "time.Now monotonic"

func nativeCostNow() func() time.Time { return time.Now }
