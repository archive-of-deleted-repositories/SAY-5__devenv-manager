package reaper

import (
	"errors"
	"os"
	"syscall"
)

// ProcessAlive reports whether a process with the given PID exists and is
// reachable by signal 0. On Unix this is the canonical liveness probe.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// ESRCH = no such process. EPERM = exists but we can't signal.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
