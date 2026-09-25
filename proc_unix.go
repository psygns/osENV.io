//go:build !windows

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// setGroup runs an engine in its own process group, so a stop takes its whole tree.
func setGroup(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(c *exec.Cmd) {
	if c != nil && c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGTERM)
		time.AfterFunc(10*time.Second, func() { syscall.Kill(-c.Process.Pid, syscall.SIGKILL) })
	}
}

// cmdPath is how an engine's shell (sh/bash) calls this binary: single-quoted, spaces and all.
func cmdPath(bin string) string { return "'" + strings.ReplaceAll(bin, "'", `'\''`) + "'" }

// procAlive: the process still exists (EPERM means it exists but isn't ours to signal).
func procAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// openLog opens a run log for appending (moving a file a process holds is fine here).
func openLog(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}
