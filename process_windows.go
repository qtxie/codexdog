//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureHiddenProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func terminationSignals() []os.Signal { return nil }
