package main

import (
	"syscall"
	"unsafe"
)

func highResolutionClock() func() int64 {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	frequencyProc := kernel.NewProc("QueryPerformanceFrequency")
	counterProc := kernel.NewProc("QueryPerformanceCounter")
	var frequency int64
	if result, _, err := frequencyProc.Call(uintptr(unsafe.Pointer(&frequency))); result == 0 || frequency <= 0 {
		panic(err)
	}
	var counter int64
	if result, _, err := counterProc.Call(uintptr(unsafe.Pointer(&counter))); result == 0 {
		panic(err)
	}
	origin := counter
	return func() int64 {
		if result, _, err := counterProc.Call(uintptr(unsafe.Pointer(&counter))); result == 0 {
			panic(err)
		}
		elapsed := counter - origin
		return (elapsed/frequency)*1e9 + (elapsed%frequency)*1e9/frequency
	}
}
