//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureHiddenProcess(_ *exec.Cmd) {}

func terminationSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGHUP} }
