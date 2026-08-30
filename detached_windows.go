//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func startDetachedCommand(command *exec.Cmd) error {
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
		HideWindow:    true,
	}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
