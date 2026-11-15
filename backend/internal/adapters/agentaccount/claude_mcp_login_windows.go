//go:build windows

package agentaccount

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// MCPLogin needs a pseudo-terminal the CLI accepts as its stdin, which AO has
// not verified on Windows, so it says so instead of hanging.
func (c *Claude) MCPLogin(context.Context, ports.MCPTarget, string, func(string)) error {
	return errors.New("connecting an MCP server from AO is not available on Windows yet; run `claude mcp login` in a terminal")
}
