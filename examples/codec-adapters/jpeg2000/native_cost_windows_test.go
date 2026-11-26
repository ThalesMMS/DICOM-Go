//go:build codecfull && windows

package jpeg2000

import (
	"syscall"
	"time"
	"unsafe"
)

const nativeCostClockName = "Windows QueryPerformanceCounter"

func nativeCostNow() func() time.Time {
	dll := syscall.NewLazyDLL("kernel32.dll")
	counter := dll.NewProc("QueryPerformanceCounter")
	var frequency, base int64
	if ok, _, e := dll.NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&frequency))); ok == 0 || frequency <= 0 {
		panic(e)
	}
	read := func() int64 {
		var ticks int64
		if ok, _, e := counter.Call(uintptr(unsafe.Pointer(&ticks))); ok == 0 {
			panic(e)
		}
		return ticks
	}
	base = read()
	epoch := time.Now()
	return func() time.Time {
		ticks := read() - base
		return epoch.Add(time.Duration(float64(ticks) * float64(time.Second) / float64(frequency)))
	}
}
