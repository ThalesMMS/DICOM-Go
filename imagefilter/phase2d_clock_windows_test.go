package imagefilter

import (
	"syscall"
	"testing"
	"unsafe"
)

func phase2DCampaignClock(t *testing.T) (func() int64, int64, string) {
	t.Helper()
	kernel := syscall.NewLazyDLL("kernel32.dll")
	var frequency int64
	if ok, _, err := kernel.NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&frequency))); ok == 0 || frequency <= 0 {
		t.Fatalf("QueryPerformanceFrequency: %v", err)
	}
	counter := kernel.NewProc("QueryPerformanceCounter")
	return func() int64 {
		var value int64
		if ok, _, err := counter.Call(uintptr(unsafe.Pointer(&value))); ok == 0 {
			panic(err)
		}
		return value
	}, frequency, "QueryPerformanceCounter"
}
