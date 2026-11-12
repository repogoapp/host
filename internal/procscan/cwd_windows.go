//go:build windows

package procscan

import (
	"context"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Cwds is each pid's working folder, read from its process parameters in its
// own memory, as Process Explorer does. A pid this user can't read is left
// out, as on Unix.
func Cwds(_ context.Context, pids []int) map[int]string {
	out := map[int]string{}
	for _, pid := range pids {
		if cwd, ok := cwdOf(pid); ok {
			out[pid] = cwd
		}
	}
	return out
}

func cwdOf(pid int) (string, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var info windows.PROCESS_BASIC_INFORMATION
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation,
		unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), nil); err != nil || info.PebBaseAddress == nil {
		return "", false
	}
	var peb windows.PEB
	if !readMemory(h, uintptr(unsafe.Pointer(info.PebBaseAddress)), unsafe.Pointer(&peb), unsafe.Sizeof(peb)) ||
		peb.ProcessParameters == nil {
		return "", false
	}
	// Only as far as the current directory: the struct grows with each
	// Windows release, and an older process's is shorter.
	var params windows.RTL_USER_PROCESS_PARAMETERS
	upTo := unsafe.Offsetof(params.CurrentDirectory) + unsafe.Sizeof(params.CurrentDirectory)
	if !readMemory(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), unsafe.Pointer(&params), upTo) {
		return "", false
	}
	dos := params.CurrentDirectory.DosPath
	if dos.Length == 0 || dos.Buffer == nil {
		return "", false
	}
	buf := make([]uint16, dos.Length/2)
	if !readMemory(h, uintptr(unsafe.Pointer(dos.Buffer)), unsafe.Pointer(&buf[0]), uintptr(dos.Length)) {
		return "", false
	}
	cwd := windows.UTF16ToString(buf)
	// Windows keeps the trailing separator (C:\Users\jo\); a drive's root keeps it.
	if len(cwd) > 3 {
		cwd = strings.TrimRight(cwd, `\`)
	}
	return cwd, true
}

func readMemory(h windows.Handle, addr uintptr, into unsafe.Pointer, size uintptr) bool {
	var n uintptr
	return windows.ReadProcessMemory(h, addr, (*byte)(into), size, &n) == nil && n == size
}
