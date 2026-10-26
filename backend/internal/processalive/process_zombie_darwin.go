//go:build darwin

package processalive

import "golang.org/x/sys/unix"

// szomb is SZOMB from <sys/proc.h>; x/sys/unix does not export it.
const szomb = 5

// isZombie reads the process state through sysctl instead of forking ps on
// every Alive call. A failed lookup reports not-a-zombie, like the ps fallback.
func isZombie(pid int) bool {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false
	}
	return info.Proc.P_stat == szomb
}
