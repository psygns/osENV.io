//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// setGroup gives an engine its own process group (CREATE_NEW_PROCESS_GROUP).
func setGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killGroup stops an engine and every process it started.
func killGroup(c *exec.Cmd) {
	if c != nil && c.Process != nil {
		exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(c.Process.Pid)).Run()
	}
}

// cmdPath is how an engine's shell calls this binary. On Windows, muse, qwen and claude all run
// hooks through cmd.exe with their runtime's own escaping, and a quoted path dies there (VM test,
// 0.1: every hook failed silently). Unquoted forward slashes work in cmd, PowerShell and bash;
// a path with spaces gets its 8.3 short name.
func cmdPath(bin string) string {
	if strings.ContainsAny(bin, " &()^%!;,=") {
		if p, err := syscall.UTF16PtrFromString(bin); err == nil {
			buf := make([]uint16, 1024)
			if n, err := syscall.GetShortPathName(p, &buf[0], uint32(len(buf))); err == nil && n > 0 && int(n) < len(buf) {
				bin = syscall.UTF16ToString(buf[:n])
			}
		}
	}
	return filepath.ToSlash(bin)
}

// procAlive: tasklist lists the pid in its PID column (an exact field match: a substring match would find
// pid 12 inside "12,345 K" and stall the task until run_timeout).
func procAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Split(line, ","); len(f) > 1 && strings.Trim(strings.TrimSpace(f[1]), `"`) == fmt.Sprint(pid) {
			return true
		}
	}
	return false
}

// openLog opens a run log for appending with FILE_SHARE_DELETE, so it can be moved or deleted while a process
// still holds it: a server a hybrid left running inherited the handle, and Go's own open would have blocked the
// retire (Windows 0.3 run).
func openLog(p string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(name, syscall.FILE_APPEND_DATA|syscall.SYNCHRONIZE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), p), nil
}
