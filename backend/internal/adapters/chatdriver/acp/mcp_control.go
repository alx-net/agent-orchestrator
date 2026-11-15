package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AO's claude-agent-acp patch advertises these in InitializeResponse._meta
// under mcpControl. They reach the live SDK query, so what they report is what
// the running agent has, which can differ from what the config says.
const (
	mcpControlExtension = "mcpControl"
	mcpStatusMethod     = "_claude/mcp_status"
	mcpReconnectMethod  = "_claude/mcp_reconnect"
	mcpApplyMethod      = "_claude/mcp_apply"
	mcpControlTimeout   = 30 * time.Second
)

// Status reports are cheap local calls that cost no tokens. A session reports
// when it starts, as its servers come up, and at each turn's end, which is when
// a server whose sign-in expired during the turn shows.
var (
	mcpStartupReportDelays = []time.Duration{time.Second, 3 * time.Second, 8 * time.Second, 20 * time.Second}
	mcpTurnEndReportDelays = []time.Duration{0, 3 * time.Second}
)

var (
	_ ports.ChatMCPController = (*conversation)(nil)
	_ ports.ChatMCPReloader   = (*conversation)(nil)
)

// errMCPControlUnsupported is a bridge without AO's patch: a provider host that
// started before an update, or any other ACP agent.
var errMCPControlUnsupported = errors.New("this agent's bridge has no MCP control")

type mcpBridgeServer struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Scope     string `json:"scope,omitempty"`
	Error     string `json:"error,omitempty"`
	ToolCount *int   `json:"toolCount,omitempty"`
}

type mcpBridgeResponse struct {
	Servers []mcpBridgeServer `json:"servers"`
	Error   string            `json:"error,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// mcpStatusFromBridge maps the SDK's words to the ones AO's chat already uses
// for tool servers. Anything new passes through unchanged, so a status this
// build has not heard of is still shown rather than hidden.
func mcpStatusFromBridge(status string) string {
	switch status {
	case "connected":
		return ports.ChatMCPStatusReady
	case "pending":
		return ports.ChatMCPStatusStarting
	case "needs-auth":
		return ports.ChatMCPStatusNeedsAuth
	default:
		return status
	}
}

func chatMCPServers(servers []mcpBridgeServer) []ports.ChatMCPServer {
	out := make([]ports.ChatMCPServer, 0, len(servers))
	for _, server := range servers {
		if server.Name == "" {
			continue
		}
		out = append(out, ports.ChatMCPServer{
			Name: server.Name, Status: mcpStatusFromBridge(server.Status), Scope: server.Scope,
			Error: server.Error, ToolCount: server.ToolCount,
		})
	}
	return out
}

func (c *conversation) callMCPControl(ctx context.Context, method string, params map[string]any) (mcpBridgeResponse, error) {
	if !c.Capabilities().Has(ports.ChatCapabilityMCPReload) {
		return mcpBridgeResponse{}, errMCPControlUnsupported
	}
	c.mu.Lock()
	sessionID := c.sessionID
	closed := c.closed
	c.mu.Unlock()
	if closed || sessionID == "" {
		return mcpBridgeResponse{}, errors.New("provider session is not running")
	}
	ctx, cancel := context.WithTimeout(ctx, mcpControlTimeout)
	defer cancel()
	params["sessionId"] = sessionID
	raw, err := c.conn.CallExtension(ctx, method, params)
	if err != nil {
		return mcpBridgeResponse{}, fmt.Errorf("%s: %w", method, err)
	}
	var response mcpBridgeResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return mcpBridgeResponse{}, fmt.Errorf("decode %s response: %w", method, err)
	}
	return response, nil
}

// MCPServerStatus reports the servers the running agent has right now.
func (c *conversation) MCPServerStatus(ctx context.Context) ([]ports.ChatMCPServer, error) {
	response, err := c.callMCPControl(ctx, mcpStatusMethod, map[string]any{})
	if err != nil {
		return nil, err
	}
	return chatMCPServers(response.Servers), nil
}

// ReconnectMCPServer has the agent connect again to one server it knows, which
// loads a token another process saved. Safe during a turn: only that server's
// connection changes. A refused reconnect still returns the servers' state,
// with the provider's words for why.
func (c *conversation) ReconnectMCPServer(ctx context.Context, name string) (ports.ChatMCPReconnectResult, error) {
	response, err := c.callMCPControl(ctx, mcpReconnectMethod, map[string]any{"serverName": name})
	if err != nil {
		return ports.ChatMCPReconnectResult{}, err
	}
	return ports.ChatMCPReconnectResult{Servers: chatMCPServers(response.Servers), Error: response.Error}, nil
}

// ApplyMCPServers adds or replaces servers in the running agent, and drops ones
// an earlier call added. It cannot drop a server the agent loaded from config at
// its start: the returned state still lists one, so the caller can restart it.
func (c *conversation) ApplyMCPServers(ctx context.Context, change ports.ChatMCPChange) (ports.ChatMCPApplyResult, error) {
	set := make(map[string]json.RawMessage, len(change.Set))
	for name, config := range change.Set {
		set[name] = config
	}
	remove := change.Remove
	if remove == nil {
		remove = []string{}
	}
	response, err := c.callMCPControl(ctx, mcpApplyMethod, map[string]any{"set": set, "remove": remove})
	if err != nil {
		return ports.ChatMCPApplyResult{}, err
	}
	return ports.ChatMCPApplyResult{Servers: chatMCPServers(response.Servers), Errors: response.Errors}, nil
}

// ReloadMCPServers reconnects every enabled server that is not already up, and
// reports where they all landed. A connected server keeps its connection.
func (c *conversation) ReloadMCPServers(ctx context.Context) (ports.ChatMCPReloadResult, error) {
	servers, err := c.MCPServerStatus(ctx)
	if err != nil {
		return ports.ChatMCPReloadResult{}, err
	}
	for _, server := range servers {
		if server.Status == ports.ChatMCPStatusReady || server.Status == ports.ChatMCPStatusDisabled {
			continue
		}
		if _, err := c.ReconnectMCPServer(ctx, server.Name); err != nil {
			return ports.ChatMCPReloadResult{}, err
		}
	}
	servers, err = c.MCPServerStatus(ctx)
	if err != nil {
		// The reconnects happened; only the read after them failed.
		c.log.Debug("mcp status after reload failed", "error", err)
		return ports.ChatMCPReloadResult{}, nil
	}
	return ports.ChatMCPReloadResult{Servers: servers, Authoritative: true}, nil
}

// reportMCPStatus publishes the live servers now and after each delay, until
// none is still starting. One report loop runs at a time per conversation.
func (c *conversation) reportMCPStatus(delays []time.Duration) {
	if !c.Capabilities().Has(ports.ChatCapabilityMCPReload) {
		return
	}
	c.mu.Lock()
	if c.mcpReporting || c.closed {
		c.mu.Unlock()
		return
	}
	c.mcpReporting = true
	c.mu.Unlock()
	go func() {
		defer func() {
			c.mu.Lock()
			c.mcpReporting = false
			c.mu.Unlock()
		}()
		for _, delay := range delays {
			time.Sleep(delay)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			servers, err := c.MCPServerStatus(ctx)
			cancel()
			if err != nil {
				c.log.Debug("mcp status report failed", "error", err)
				return
			}
			c.publishMCPServers(servers)
			starting := false
			for _, server := range servers {
				starting = starting || server.Status == ports.ChatMCPStatusStarting
			}
			if !starting {
				return
			}
		}
	}()
}

// publishMCPServers tells the controller the full list, when it changed.
func (c *conversation) publishMCPServers(servers []ports.ChatMCPServer) {
	encoded, err := json.Marshal(servers)
	if err != nil {
		return
	}
	c.mu.Lock()
	changed := c.mcpReported != string(encoded)
	c.mcpReported = string(encoded)
	c.mu.Unlock()
	if changed {
		c.emit(ports.ChatEvent{Kind: ports.ChatEventMCPServers, MCPServers: servers, MCPServersComplete: true})
	}
}
