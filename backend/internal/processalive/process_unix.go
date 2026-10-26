//go:build !windows

// Package processalive probes whether an operating-system process id still
// maps to a live process.
package processalive

import (
	"errors"
	"syscall"
)

// Alive reports whether pid maps to a running process. EPERM counts as alive:
// the process exists even if the current user cannot signal it. Zombies are
// treated as not alive because the executable has already exited; only its
// parent has not reaped the process table entry yet.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !isZombie(pid)
}
