package reaper

import (
	"os"
	"testing"
)

func TestProcessAliveSelf(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Fatal("expected our own pid to be alive")
	}
}

func TestProcessAliveImpossiblePID(t *testing.T) {
	// PID 0 is a sentinel; ProcessAlive must reject it.
	if ProcessAlive(0) {
		t.Fatal("PID 0 must report dead")
	}
	if ProcessAlive(-5) {
		t.Fatal("negative PID must report dead")
	}
}

func TestProcessAliveImaginary(t *testing.T) {
	// PIDs above the kernel default ceiling are virtually never live.
	// 4194303 is the conventional Linux pid_max ceiling; macOS rejects
	// anything past 99998 as ESRCH on signal(0).
	if ProcessAlive(2_000_000_000) {
		t.Fatal("astronomically large PID should not be alive")
	}
}
