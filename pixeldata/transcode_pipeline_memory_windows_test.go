package pixeldata_test

import (
	"syscall"
	"unsafe"
)

func transcode1294ProcessMemory() (transcode1294Memory, error) {
	// PROCESS_MEMORY_COUNTERS uses SIZE_T for all fields after the DWORD pair.
	var counters struct {
		Size, PageFaultCount                               uint32
		PeakWorkingSetSize, WorkingSetSize                 uintptr
		QuotaPeakPagedPoolUsage, QuotaPagedPoolUsage       uintptr
		QuotaPeakNonPagedPoolUsage, QuotaNonPagedPoolUsage uintptr
		PagefileUsage, PeakPagefileUsage                   uintptr
	}
	counters.Size = uint32(unsafe.Sizeof(counters))
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return transcode1294Memory{}, err
	}
	getMemory := syscall.NewLazyDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	ok, _, callErr := getMemory.Call(uintptr(process), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	if ok == 0 {
		return transcode1294Memory{}, callErr
	}
	return transcode1294Memory{uint64(counters.WorkingSetSize), uint64(counters.PeakWorkingSetSize)}, nil
}
