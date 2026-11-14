package ports

import (
	"context"
	"encoding/json"
	"time"
)

// MCPTarget is where a Claude Code MCP command runs: which account folder (empty
// is the computer's own) and which directory (empty is a throwaway one, which
// sees only servers that apply everywhere).
type MCPTarget struct {
	Home string
	Dir  string
}

// MCPCommandResult is what a finished `claude mcp ...` printed. The output can
// echo headers (`claude mcp get` prints them in full): never log it.
type MCPCommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// MCPProbeRequest asks Claude Code to connect to servers and report what it got.
type MCPProbeRequest struct {
	// Servers checks exactly these, from their config objects, and nothing else
	// the folder knows about. Nil checks everything the folder has. Values can
	// hold secrets: they never reach argv or a log.
	Servers map[string]json.RawMessage
	// Reconnect names a server to connect again before the last read.
	Reconnect string
	// Settle bounds the wait for servers still starting. Zero is the default.
	Settle time.Duration
}

// MCPProbeServer is one server as Claude Code itself reports it after connecting.
type MCPProbeServer struct {
	Name      string
	Status    string // connected, needs-auth, failed, pending, disabled, or newer
	Scope     string // user, project, local, dynamic, claudeai, ...
	Transport string
	URL       string
	Command   string
	Error     string
	ToolCount *int
	Tools     []string
}

// MCPCLI is the Claude Code MCP surface AO drives. Every method is free: none
// sends a prompt, so none spends tokens.
type MCPCLI interface {
	// MCPCommand runs `claude <args...>` and returns what it printed. A nonzero
	// exit is a result, not an error; the error means it could not run at all.
	MCPCommand(ctx context.Context, target MCPTarget, args ...string) (MCPCommandResult, error)
	// MCPProbe connects to servers over Claude Code's control protocol and
	// reports each one's real state, with its tools. It costs no tokens.
	MCPProbe(ctx context.Context, target MCPTarget, request MCPProbeRequest) ([]MCPProbeServer, error)
	// MCPLogin runs Claude Code's browser sign-in on a terminal, as it requires,
	// until it finishes. output receives what it prints, as it prints it.
	MCPLogin(ctx context.Context, target MCPTarget, name string, output func(string)) error
	// MCPUserServers reads the servers a folder's global config defines.
	MCPUserServers(home string) (map[string]json.RawMessage, error)
	// MCPProjectServers reads the servers dir's .mcp.json defines.
	MCPProjectServers(dir string) (map[string]json.RawMessage, error)
	// MCPApprove records, in the folder's own config, that this folder trusts dir
	// and approves server name from dir's .mcp.json. No repo file changes.
	MCPApprove(home, dir, name string) error
	// MCPTrust records only that the folder trusts dir.
	MCPTrust(home, dir string) error
	// MCPForgetAuthCache drops what the folder's CLI remembers about servers that
	// wanted a sign-in, so its next look really connects. No names drops it all.
	MCPForgetAuthCache(home string, names ...string) error
}
