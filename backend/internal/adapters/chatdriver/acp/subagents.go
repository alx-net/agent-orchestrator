package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AO's claude-agent-acp patch advertises these in InitializeResponse._meta
// under subagentControl.
const (
	subagentControlExtension = "subagentControl"
	subagentMessageMethod    = "_claude/subagent_message"
	subagentStopMethod       = "_claude/subagent_stop"
	// subagentInboxLimit matches the bridge's own cap on the reported list.
	subagentInboxLimit = 20
)

var _ ports.ChatSubagentController = (*conversation)(nil)

type subagentMessageResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// MessageSubagent queues text for the running subagent spawned by toolUseID.
// The bridge hands it over at the subagent's next tool call and reports the
// inbox on the spawning call, so success means queued.
func (c *conversation) MessageSubagent(
	ctx context.Context,
	toolUseID string,
	msg ports.ChatSubagentMessage,
) (ports.ChatSubagentMessageReceipt, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" || msg.ID == "" {
		return ports.ChatSubagentMessageReceipt{}, errors.New("subagent message needs an id and text")
	}
	sessionID, providerToolUseID, err := c.subagentTarget(toolUseID)
	if err != nil {
		return ports.ChatSubagentMessageReceipt{}, err
	}
	raw, err := c.conn.CallExtension(ctx, subagentMessageMethod, map[string]any{
		"sessionId": sessionID, "toolUseId": providerToolUseID, "text": text, "id": msg.ID,
	})
	if err != nil {
		return ports.ChatSubagentMessageReceipt{}, subagentCallError(subagentMessageMethod, err)
	}
	var response subagentMessageResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return ports.ChatSubagentMessageReceipt{}, fmt.Errorf("decode ACP %s response: %w", subagentMessageMethod, err)
	}
	if response.ID == "" || response.State == "" {
		return ports.ChatSubagentMessageReceipt{}, fmt.Errorf("ACP %s response is missing id or state", subagentMessageMethod)
	}
	return ports.ChatSubagentMessageReceipt{ID: response.ID, State: response.State}, nil
}

// StopSubagent stops the running subagent spawned by toolUseID. The main turn
// keeps going; the subagent's call settles when the provider reports it stopped.
func (c *conversation) StopSubagent(ctx context.Context, toolUseID string) error {
	sessionID, providerToolUseID, err := c.subagentTarget(toolUseID)
	if err != nil {
		return err
	}
	if _, err := c.conn.CallExtension(ctx, subagentStopMethod, map[string]any{
		"sessionId": sessionID, "toolUseId": providerToolUseID,
	}); err != nil {
		return subagentCallError(subagentStopMethod, err)
	}
	return nil
}

// subagentTarget resolves AO's provider item id for the spawning call back to
// the provider's own tool call id.
func (c *conversation) subagentTarget(toolUseID string) (string, string, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	closed := c.closed
	supported := c.capabilities.Has(ports.ChatCapabilitySubagents)
	c.mu.Unlock()
	if closed {
		return "", "", errConversationClosed
	}
	if !supported {
		return "", "", ports.ErrChatSubagentsUnsupported
	}
	if raw, ok := c.legacyProviderItemAlias(toolUseID); ok {
		toolUseID = raw
	}
	if toolUseID == "" {
		return "", "", ports.ErrChatSubagentNotRunning
	}
	return sessionID, toolUseID, nil
}

func subagentCallError(method string, err error) error {
	wrapped := fmt.Errorf("ACP %s: %w", method, err)
	if isACPMethodNotFound(err) {
		return fmt.Errorf("%w: %w", ports.ErrChatSubagentsUnsupported, wrapped)
	}
	var requestErr *acpsdk.RequestError
	if errors.As(err, &requestErr) && requestErr.Code == -32602 {
		if data, ok := requestErr.Data.(map[string]any); ok && data["reason"] == "subagentNotRunning" {
			return fmt.Errorf("%w: %w", ports.ErrChatSubagentNotRunning, wrapped)
		}
	}
	return wrapped
}

func subagentInbox(meta map[string]any) []any {
	inbox, _ := nestedMap(meta, "claudeCode")["subagentInbox"].([]any)
	return inbox
}

// subagentInboxDetail keeps the well-formed entries of the inbox AO's bridge
// patch reports on a subagent's Agent/Task call, at most the last 20.
func subagentInboxDetail(entries []any) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		id, _ := entry["id"].(string)
		text, _ := entry["text"].(string)
		state, _ := entry["state"].(string)
		at, atOK := number(entry["at"])
		if id == "" || !atOK || (state != "queued" && state != "delivered" && state != "dropped") {
			continue
		}
		item := map[string]any{"id": id, "text": text, "state": state, "at": int64(at)}
		if deliveredAt, ok := number(entry["deliveredAt"]); ok && deliveredAt > 0 {
			item["deliveredAt"] = int64(deliveredAt)
		}
		out = append(out, item)
	}
	if len(out) > subagentInboxLimit {
		out = out[len(out)-subagentInboxLimit:]
	}
	return out
}
