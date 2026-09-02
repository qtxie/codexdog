//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const (
	detachedHelperMode    = "CODEXDOG_DETACHED_HELPER"
	detachedHelperPIDFile = "CODEXDOG_DETACHED_PID_FILE"
)

func TestDetachedSysProcAttrIsolatedFromParent(t *testing.T) {
	attr := detachedSysProcAttr()
	for _, flag := range []uint32{createNewProcessGroup, detachedProcess, createBreakawayFromJob} {
		if attr.CreationFlags&flag == 0 {
			t.Fatalf("creation flags %#x do not include %#x", attr.CreationFlags, flag)
		}
	}
	if !attr.HideWindow || !attr.NoInheritHandles {
		t.Fatalf("detached process attributes = %#v", attr)
	}
}

func TestDetachedCommandOutlivesManagedLauncher(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "detached.pid")
	processes, err := newProcessTreeWithLimits(jobObjectLimitKillOnJobClose | jobObjectLimitBreakawayOK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	launcher := exec.Command(os.Args[0], "-test.run=^TestDetachedCommandHelper$")
	launcher.Env = append(os.Environ(), detachedHelperMode+"=launcher", detachedHelperPIDFile+"="+pidFile)
	if err := processes.Start(launcher, true); err != nil {
		t.Fatal(err)
	}
	launcherDone := make(chan error, 1)
	go func() { launcherDone <- launcher.Wait() }()
	var pid int
	waitFor(t, func() bool {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(data))
		return err == nil && pid > 0
	})
	t.Cleanup(func() {
		if pid > 0 && processExists(pid) {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
	})
	if err := processes.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-launcherDone:
	case <-time.After(3 * time.Second):
		t.Fatal("managed launcher did not exit")
	}
	if !processExists(pid) {
		t.Fatalf("detached process %d exited with its launcher", pid)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !processExists(pid) })
}

func TestDetachedCommandHelper(t *testing.T) {
	switch os.Getenv(detachedHelperMode) {
	case "launcher":
		child := exec.Command(os.Args[0], "-test.run=^TestDetachedCommandHelper$")
		child.Env = append(os.Environ(), detachedHelperMode+"=child")
		if err := startDetachedCommand(child); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv(detachedHelperPIDFile), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(24 * time.Hour)
	case "child":
		time.Sleep(24 * time.Hour)
	}
}
