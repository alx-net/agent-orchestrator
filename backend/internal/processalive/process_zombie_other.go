//go:build !windows && !darwin && !linux

package processalive

import (
	"bytes"
	"strconv"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

func isZombie(pid int) bool {
	out, err := aoprocess.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return bytes.HasPrefix(bytes.TrimSpace(out), []byte("Z"))
}
