package screens

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	performanceCount = kernel32.NewProc("QueryPerformanceCounter")
	performanceFreq  = kernel32.NewProc("QueryPerformanceFrequency")
)

// uptimeUS is now on the clock Windows capture stamps frames with (the
// performance counter, wincap_now_us), in microseconds; 0 if unreadable.
func uptimeUS() int64 {
	var count, freq int64
	if ok, _, _ := performanceCount.Call(uintptr(unsafe.Pointer(&count))); ok == 0 {
		return 0
	}
	if ok, _, _ := performanceFreq.Call(uintptr(unsafe.Pointer(&freq))); ok == 0 || freq == 0 {
		return 0
	}
	return count/freq*1_000_000 + count%freq*1_000_000/freq
}
