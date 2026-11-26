package jpegls

import (
	"syscall"
	"testing"
	"unsafe"
)

// Use QPC for sub-millisecond cancellation latency; the runtime clock's
// observable tick on a Windows host can exceed the whole cancellation interval.
func newPerformanceClock(t *testing.T) performanceClock {
	t.Helper()
	kernel := syscall.NewLazyDLL("kernel32.dll")
	frequencyProc := kernel.NewProc("QueryPerformanceFrequency")
	counterProc := kernel.NewProc("QueryPerformanceCounter")
	var frequency int64
	if result, _, err := frequencyProc.Call(uintptr(unsafe.Pointer(&frequency))); result == 0 || frequency <= 0 {
		t.Fatalf("QueryPerformanceFrequency: %v", err)
	}
	return performanceClock{name: "QueryPerformanceCounter", frequency: frequency, now: func() int64 {
		var counter int64
		if result, _, err := counterProc.Call(uintptr(unsafe.Pointer(&counter))); result == 0 {
			panic(err)
		}
		return counter
	}}
}
