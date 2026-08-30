//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func startDetachedCommand(command *exec.Cmd) error {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}
