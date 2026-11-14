//go:build !windows

package agentaccount

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// mcpLoginStopGrace is how long a cancelled sign-in gets to close its callback
// listener before it is killed.
const mcpLoginStopGrace = 2 * time.Second

// MCPLogin runs `claude mcp login <name>` on a pseudo-terminal until it ends.
// Claude Code aborts a sign-in whose stdin is not a terminal, in browser mode
// too (measured on 2.1.314, "stdin isn't a terminal"), so a plain pipe can never
// work. It opens the browser itself; the terminal stays open with nothing typed
// into it while it waits for the browser's redirect.
func (c *Claude) MCPLogin(ctx context.Context, target ports.MCPTarget, name string, output func(string)) error {
	bin, err := c.Binary(ctx)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Not CommandContext: pty.Start makes the child a session leader, and a
	// cancel here stops its whole group, not only the leader.
	cmd := c.mcpLoginCommand(bin, target, name)
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 200})
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	closeTerminal := func() { closeOnce.Do(func() { _ = terminal.Close() }) }
	defer closeTerminal()

	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = aoprocess.TerminateGroup(cmd.Process.Pid, mcpLoginStopGrace)
		case <-stopped:
		}
	}()
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		buf := make([]byte, 4096)
		for {
			n, readErr := terminal.Read(buf)
			if n > 0 && output != nil {
				output(string(buf[:n]))
			}
			if readErr != nil {
				return // EIO: the child is gone
			}
		}
	}()
	waitErr := cmd.Wait()
	close(stopped)
	// A grandchild can hold the terminal open; closing our side ends the read.
	closeTerminal()
	<-reader
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) && exit.Sys() != nil {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return errors.New("the sign-in was stopped")
		}
	}
	return waitErr
}
