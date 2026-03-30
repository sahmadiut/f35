//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detachProcess configures cmd to start without a console window on Windows.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
