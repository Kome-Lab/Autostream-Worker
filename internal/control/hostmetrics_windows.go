//go:build windows

package control

import (
	"syscall"
	"unsafe"
)

var getSystemTimes = syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemTimes")

func cpuTimeCounters() *cpuTimes {
	var idle, kernel, user syscall.Filetime
	ok, _, _ := getSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ok == 0 {
		return nil
	}
	idleTicks := filetimeTicks(idle)
	return &cpuTimes{total: filetimeTicks(kernel) + filetimeTicks(user), idle: idleTicks}
}

func filetimeTicks(value syscall.Filetime) uint64 {
	return uint64(value.HighDateTime)<<32 | uint64(value.LowDateTime)
}

func filesystemMetrics() map[string]float64 {
	return nil
}

func networkByteCounters() map[string]float64 {
	return nil
}
