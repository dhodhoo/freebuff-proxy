//go:build !windows

package session

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid is a live process. Unix: signal 0.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
