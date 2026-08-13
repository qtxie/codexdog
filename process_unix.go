//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type managedProcess struct {
	command  *exec.Cmd
	isolated bool
}

type processTree struct {
	mu        sync.Mutex
	processes []managedProcess
	closed    bool
}

func newProcessTree() (*processTree, error) {
	return &processTree{}, nil
}

func (p *processTree) Start(command *exec.Cmd, isolated bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("process tree is closed")
	}
	if isolated {
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := command.Start(); err != nil {
		return err
	}
	p.processes = append(p.processes, managedProcess{command: command, isolated: isolated})
	return nil
}

func (p *processTree) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	processes := append([]managedProcess(nil), p.processes...)
	p.processes = nil
	p.mu.Unlock()
	var firstError error
	for _, managed := range processes {
		if managed.command.Process == nil {
			continue
		}
		var err error
		if managed.isolated {
			err = syscall.Kill(-managed.command.Process.Pid, syscall.SIGKILL)
		} else {
			err = managed.command.Process.Kill()
		}
		if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) && firstError == nil {
			firstError = err
		}
	}
	return firstError
}

func terminationSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGHUP} }
