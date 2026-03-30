//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess configures cmd to start a new session so it is fully
// detached from the parent's controlling terminal.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
