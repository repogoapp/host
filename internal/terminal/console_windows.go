package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// startConsole runs cmd on a new pseudo console (ConPTY) of cols by rows.
// exec.Cmd can't attach a pseudo console, so the shell is created directly,
// with cmd's path, arguments, folder and environment.
func startConsole(cmd *exec.Cmd, cols, rows int) (console, shellProcess, error) {
	// The console reads what the shell is sent from inRead and writes what it
	// prints to outWrite; the host holds the other ends.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, nil, fmt.Errorf("console input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		closeHandles(inRead, inWrite)
		return nil, nil, fmt.Errorf("console output pipe: %w", err)
	}
	var hpc windows.Handle
	err := windows.CreatePseudoConsole(coord(cols, rows), inRead, outWrite, 0, &hpc)
	// The console keeps its own copies of its ends.
	closeHandles(inRead, outWrite)
	if err != nil {
		closeHandles(inWrite, outRead)
		return nil, nil, fmt.Errorf("create the pseudo console: %w", err)
	}
	c := &conPTY{hpc: hpc, in: os.NewFile(uintptr(inWrite), "conin"), out: os.NewFile(uintptr(outRead), "conout")}

	p, err := createOnConsole(cmd, hpc)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	// The output pipe doesn't end when the shell exits, only when its console
	// closes, so the console closes then and the reader sees the end.
	go func() {
		_, _ = windows.WaitForSingleObject(p.handle, windows.INFINITE)
		c.closeConsole()
	}()
	return c, p, nil
}

func createOnConsole(cmd *exec.Cmd, hpc windows.Handle) (*conProcess, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself, not a pointer to it.
	value := *(*unsafe.Pointer)(unsafe.Pointer(&hpc))
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, value, unsafe.Sizeof(hpc)); err != nil {
		return nil, fmt.Errorf("attach the pseudo console: %w", err)
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// No standard handles of its own: without this the shell takes the host's,
	// which the service sends to host.log, and writes there, not the console.
	si.Flags |= windows.STARTF_USESTDHANDLES

	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		return nil, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return nil, err
		}
	}
	env := environmentBlock(cmd.Env)
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, line, nil, nil, false, flags, &env[0], dir, &si.StartupInfo, &pi); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	_ = windows.CloseHandle(pi.Thread)
	return &conProcess{handle: pi.Process, pid: int(pi.ProcessId)}, nil
}

// environmentBlock is env as CreateProcess takes it: sorted, case-blind
// like Windows' own, each NAME=value ended by a NUL and the block by another.
// A later entry for a name wins, as exec does.
func environmentBlock(env []string) []uint16 {
	last := map[string]string{}
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		key := strings.ToUpper(name)
		if _, seen := last[key]; !seen {
			names = append(names, key)
		}
		last[key] = kv
	}
	sort.Strings(names)
	var block []uint16
	for _, key := range names {
		block = append(block, utf16.Encode([]rune(last[key]))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0)
}

func coord(cols, rows int) windows.Coord { return windows.Coord{X: int16(cols), Y: int16(rows)} }

func closeHandles(hs ...windows.Handle) {
	for _, h := range hs {
		_ = windows.CloseHandle(h)
	}
}

// conPTY is a pseudo console: the shell's input and output pipes.
type conPTY struct {
	hpc     windows.Handle
	in, out *os.File
	once    sync.Once
}

func (c *conPTY) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(c.hpc, coord(cols, rows))
}

// closeConsole ends the console, which ends what still runs on it and lets
// the output pipe reach its end.
func (c *conPTY) closeConsole() { c.once.Do(func() { windows.ClosePseudoConsole(c.hpc) }) }

func (c *conPTY) Close() error {
	c.closeConsole()
	_ = c.in.Close()
	return c.out.Close()
}

// conProcess is the shell, by its process handle.
type conProcess struct {
	handle windows.Handle
	pid    int
}

func (p *conProcess) Pid() int { return p.pid }

func (p *conProcess) Wait() int {
	defer windows.CloseHandle(p.handle)
	if _, err := windows.WaitForSingleObject(p.handle, windows.INFINITE); err != nil {
		return -1
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.handle, &code); err != nil {
		return -1
	}
	return int(code)
}
