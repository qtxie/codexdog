//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
)

func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		// The hub must outlive the supervisor that created it. A detached
		// process is still assigned to the caller's Job Object unless this
		// flag is requested explicitly.
		CreationFlags:    createNewProcessGroup | detachedProcess | createBreakawayFromJob,
		HideWindow:       true,
		NoInheritHandles: true,
	}
}

func startDetachedCommand(command *exec.Cmd) error {
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	command.SysProcAttr = detachedSysProcAttr()
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
