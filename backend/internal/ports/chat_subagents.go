package ports

import (
	"context"
	"errors"
)

// ChatSubagentController is implemented by drivers whose provider can take a
// message for, or stop, one running subagent directly. The subagent is named by
// the provider item id of the tool call that spawned it, the parent activity's
// providerItemId in AO's timeline.
//
// A message is queued for the subagent and handed to it at its next tool call,
// so acceptance means "queued", not "read". The provider reports the inbox on
// the parent activity's detail as it changes.
type ChatSubagentController interface {
	MessageSubagent(ctx context.Context, toolUseID string, msg ChatSubagentMessage) (ChatSubagentMessageReceipt, error)
	StopSubagent(ctx context.Context, toolUseID string) error
}

// ChatSubagentMessage is a message for one subagent. ID is AO's, so a retry
// names the same message.
type ChatSubagentMessage struct {
	ID   string
	Text string
}

// ChatSubagentMessageReceipt is the provider's state for an accepted message.
type ChatSubagentMessageReceipt struct {
	ID    string
	State string
}

var (
	// ErrChatSubagentNotRunning means no running subagent was spawned by that
	// tool call: it finished, was stopped, or never existed.
	ErrChatSubagentNotRunning = errors.New("subagent is not running")
	// ErrChatSubagentsUnsupported means the provider cannot address a single
	// subagent. Permanent for that conversation.
	ErrChatSubagentsUnsupported = errors.New("chat provider cannot message or stop a single subagent")
)
