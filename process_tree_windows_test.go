//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const (
	processTreeHelperMode = "CODEXDOG_PROCESS_TREE_HELPER"
	processTreePIDFile    = "CODEXDOG_PROCESS_TREE_PID_FILE"
	synchronizeProcess    = 0x00100000
	waitObjectZero        = 0x00000000
)

func TestProcessTreeKillsDescendants(t *testing.T) {
	pidFile := t.TempDir() + "\\grandchild.pid"
	processes, err := newProcessTree()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
	parent.Env = append(os.Environ(), processTreeHelperMode+"=parent", processTreePIDFile+"="+pidFile)
	if err := processes.Start(parent, true); err != nil {
		t.Fatal(err)
	}
	parentDone := make(chan error, 1)
	go func() { parentDone <- parent.Wait() }()

	var grandchildPID int
	waitFor(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		grandchildPID, err = strconv.Atoi(string(data))
		return err == nil && grandchildPID > 0
	})
	t.Cleanup(func() {
		_ = processes.Close()
		if grandchildPID > 0 {
			if process, err := os.FindProcess(grandchildPID); err == nil {
				_ = process.Kill()
			}
		}
	})
	if err := processes.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parentDone:
	case <-time.After(3 * time.Second):
		t.Fatal("managed parent process did not exit")
	}
	waitFor(t, func() bool { return windowsProcessExited(grandchildPID) })
}

func TestProcessTreeHelper(t *testing.T) {
	switch os.Getenv(processTreeHelperMode) {
	case "parent":
		time.Sleep(200 * time.Millisecond)
		child := exec.Command(os.Args[0], "-test.run=^TestProcessTreeHelper$")
		child.Env = append(os.Environ(), processTreeHelperMode+"=grandchild")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv(processTreePIDFile), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		time.Sleep(24 * time.Hour)
	case "grandchild":
		time.Sleep(24 * time.Hour)
	}
}

func windowsProcessExited(pid int) bool {
	handle, err := syscall.OpenProcess(synchronizeProcess, false, uint32(pid))
	if err != nil {
		return true
	}
	defer syscall.CloseHandle(handle)
	result, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && result == waitObjectZero
}
