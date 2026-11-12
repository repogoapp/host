//go:build windows

package ports

import (
	"context"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/repogoapp/host/internal/procscan"
)

var getExtendedTcpTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

// tcpTableOwnerPIDListener is TCP_TABLE_OWNER_PID_LISTENER: listening
// sockets only, each with its owner's pid.
const tcpTableOwnerPIDListener = 3

// listPlatformPorts reads the IPv4 and IPv6 listening sockets from
// GetExtendedTcpTable, with each owner's program name and folder.
func listPlatformPorts(ctx context.Context) []listener {
	var rows []tcpRow
	rows = append(rows, tcpTable(windows.AF_INET)...)
	rows = append(rows, tcpTable(windows.AF_INET6)...)
	if len(rows) == 0 {
		return nil
	}
	var pids []int
	names := map[int]string{}
	for _, r := range rows {
		if _, ok := names[r.pid]; !ok && r.pid > 0 {
			names[r.pid] = programName(r.pid)
			pids = append(pids, r.pid)
		}
	}
	cwds := procscan.Cwds(ctx, pids)
	seen := map[tcpRow]bool{} // one row per pid:port: IPv4 and IPv6 both report it
	out := make([]listener, 0, len(rows))
	for _, r := range rows {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, listener{port: r.port, pid: r.pid, command: names[r.pid], cwd: cwds[r.pid]})
	}
	return out
}

// tcpTable is family's listening sockets. The table can grow between asking
// its size and reading it, so a short buffer is tried again.
func tcpTable(family uint32) []tcpRow {
	size := uint32(4096)
	for range 4 {
		buf := make([]byte, size)
		ret, _, _ := getExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			0, uintptr(family), tcpTableOwnerPIDListener, 0)
		switch windows.Errno(ret) {
		case 0:
			return parseTCPTable(buf[:size], family == windows.AF_INET6)
		case windows.ERROR_INSUFFICIENT_BUFFER:
			size += 1024
		default:
			return nil
		}
	}
	return nil
}

// programName is pid's program without its folder or .exe, as a Unix
// process's name reads: node, not C:\Program Files\nodejs\node.exe.
func programName(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	name := filepath.Base(windows.UTF16ToString(buf[:n]))
	if strings.EqualFold(filepath.Ext(name), ".exe") {
		name = name[:len(name)-4]
	}
	return name
}
