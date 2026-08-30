//go:build windows

package main

import (
	"errors"
	"syscall"
)

const processExistsSynchronize = 0x00100000

func processExists(pid int) bool {
	handle, err := syscall.OpenProcess(processExistsSynchronize, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer syscall.CloseHandle(handle)
	result, err := syscall.WaitForSingleObject(handle, 0)
	return err == nil && result == syscall.WAIT_TIMEOUT
}
