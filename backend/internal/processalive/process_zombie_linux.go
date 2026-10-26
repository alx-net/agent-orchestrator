//go:build linux

package processalive

import (
	"bytes"
	"os"
	"strconv"
)

// isZombie reads the state field of /proc/<pid>/stat instead of forking ps on
// every Alive call. A failed read reports not-a-zombie, like the ps fallback.
func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// The command name is parenthesized and may itself contain ')', so the
	// state is the field after the last ')'.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return false
	}
	state := bytes.TrimLeft(data[end+1:], " ")
	return len(state) > 0 && state[0] == 'Z'
}
