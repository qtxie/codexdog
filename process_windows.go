//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x00002000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type processTree struct {
	mu     sync.Mutex
	job    syscall.Handle
	closed bool
}

func newProcessTree() (*processTree, error) {
	result, _, callErr := createJobObjectW.Call(0, 0)
	if result == 0 {
		return nil, windowsCallError("CreateJobObjectW", callErr)
	}
	job := syscall.Handle(result)
	information := jobObjectExtendedLimitInformation{}
	information.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	result, _, callErr = setInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformationClass,
		uintptr(unsafe.Pointer(&information)),
		unsafe.Sizeof(information),
	)
	if result == 0 {
		_ = syscall.CloseHandle(job)
		return nil, windowsCallError("SetInformationJobObject", callErr)
	}
	return &processTree{job: job}, nil
}

func (p *processTree) Start(command *exec.Cmd, hidden bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("process tree is closed")
	}
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: hidden}
	if err := command.Start(); err != nil {
		return err
	}
	if err := assignToJob(p.job, command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return fmt.Errorf("contain child process %d: %w", command.Process.Pid, err)
	}
	return nil
}

func (p *processTree) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	job := p.job
	p.job = 0
	p.mu.Unlock()
	if job == 0 {
		return nil
	}
	return syscall.CloseHandle(job)
}

func assignToJob(job syscall.Handle, pid int) error {
	process, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer syscall.CloseHandle(process)
	result, _, callErr := assignProcessToJobObject.Call(uintptr(job), uintptr(process))
	if result == 0 {
		return windowsCallError("AssignProcessToJobObject", callErr)
	}
	return nil
}

func windowsCallError(operation string, err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return errors.New(operation + " failed")
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func terminationSignals() []os.Signal { return nil }
