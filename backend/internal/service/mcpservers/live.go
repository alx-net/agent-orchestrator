package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type changeKind int

const (
	changeConnect changeKind = iota
	changeApprove
	changeAdd
	changeKey
	changeRemove
)

// change is what just happened to a server, to bring into running agents.
type change struct {
	kind changeKind
	name string
	// config is the server's config object, which can hold secrets.
	config json.RawMessage
	// accountID limits it to one subscription (a sign-in, an approval).
	accountID string
	// projectID limits it to one project (an approval).
	projectID string
}

// propagate brings a change into every running agent it affects, so nobody has
// to restart anything. Each agent takes it in place if it can; what Claude Code
// cannot do in place (measured on 2.1.314: removing a server loaded from config)
// restarts that agent with its conversation kept, as soon as it is idle. A session
// with no running agent needs nothing: its next start reads the current servers.
func (s *Service) propagate(ctx context.Context, ch change) Propagation {
	out := Propagation{Failed: map[string]string{}}
	if s.sessions == nil {
		return out
	}
	sessions, err := s.sessions.MCPRunningSessions(ctx)
	if err != nil {
		s.log.Warn("list sessions to bring an MCP change to", "error", err)
		return out
	}
	for _, session := range sessions {
		if ch.accountID != "" && session.AccountID != ch.accountID {
			continue
		}
		if ch.projectID != "" && session.ProjectID != ch.projectID {
			continue
		}
		if !session.Chat {
			// AO cannot reach into a terminal agent.
			s.scheduleRestart(session.ID)
			out.Restarting = append(out.Restarting, string(session.ID))
			continue
		}
		s.applyToChat(ctx, session, ch, &out)
	}
	return out
}

func (s *Service) applyToChat(ctx context.Context, session Session, ch change, out *Propagation) {
	id := session.ID
	var servers []domain.ConversationMCPServer
	var err error
	var failure string
	switch ch.kind {
	case changeConnect, changeApprove:
		servers, _, err = s.live.ReconnectMCPServer(ctx, id, ch.name)
		if err == nil && ch.config != nil {
			if _, known := liveEntry(servers, ch.name); !known {
				// A server this agent never heard of (added after it started):
				// reconnect has nothing to reconnect, but this can add it.
				var failures map[string]string
				servers, failures, err = s.live.ApplyMCPServers(ctx, id, ports.ChatMCPChange{Set: map[string]json.RawMessage{ch.name: ch.config}})
				failure = failures[ch.name]
			}
		}
	case changeAdd, changeKey:
		var failures map[string]string
		servers, failures, err = s.live.ApplyMCPServers(ctx, id, ports.ChatMCPChange{Set: map[string]json.RawMessage{ch.name: ch.config}})
		failure = failures[ch.name]
	case changeRemove:
		servers, _, err = s.live.ApplyMCPServers(ctx, id, ports.ChatMCPChange{Remove: []string{ch.name}})
	}
	switch {
	case errors.Is(err, ErrNotLive):
		return // its next start reads the current servers
	case errors.Is(err, ErrInPlaceUnsupported):
		s.restartLater(id, out)
		return
	case err != nil:
		// A provider that did not answer: a restart is the sure way to apply it.
		s.log.Warn("bring MCP change into a running agent", "session", id, "server", ch.name, "error", err)
		s.restartLater(id, out)
		return
	}
	current, listed := liveEntry(servers, ch.name)
	switch {
	case ch.kind == changeRemove:
		if listed {
			// It came from config at the agent's start; only a restart drops it.
			s.restartLater(id, out)
			return
		}
		out.InPlace = append(out.InPlace, string(id))
	case failure != "":
		out.Failed[string(id)] = failure
	case !listed:
		s.restartLater(id, out)
	case current.Status == ports.ChatMCPStatusNeedsAuth && ch.kind == changeConnect:
		// The token was saved and the agent still cannot use it.
		s.restartLater(id, out)
	case current.Status == ports.ChatMCPStatusNeedsAuth && ch.kind == changeKey:
		out.Failed[string(id)] = "The server refused the new key."
	case current.Status == ports.ChatMCPStatusFailed:
		// The server itself is the problem: a restart would not help, say why.
		out.Failed[string(id)] = current.Error
	default:
		out.InPlace = append(out.InPlace, string(id))
	}
}

func (s *Service) restartLater(id domain.SessionID, out *Propagation) {
	s.scheduleRestart(id)
	out.Restarting = append(out.Restarting, string(id))
}

// forgetBeforeRestart clears what the account's CLI remembers about refused
// servers just before an agent may start. The agent running now can write a stale
// entry back, and the new one would then skip connecting to that server and read
// a key that works as refused for the rest of the entry's life.
func (s *Service) forgetBeforeRestart(ctx context.Context, id domain.SessionID) {
	_, account, err := s.sessionAccount(ctx, id)
	if err != nil {
		return
	}
	if err := s.cli.MCPForgetAuthCache(account.Home); err != nil {
		s.log.Debug("forget MCP auth cache", "account", account.ID, "error", err)
	}
}

// scheduleRestart restarts the agent as soon as it is idle: right away if it is
// now, otherwise when its turn ends, and never under a turn.
func (s *Service) scheduleRestart(id domain.SessionID) {
	if s.restarter == nil {
		return
	}
	s.mu.Lock()
	if s.restarts[id] {
		s.mu.Unlock()
		return
	}
	s.restarts[id] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.restarts, id)
			s.mu.Unlock()
		}()
		giveUp := time.Now().Add(s.restartPatience)
		for time.Now().Before(giveUp) {
			ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
			s.forgetBeforeRestart(ctx, id)
			done, err := s.restarter.RestartAgentForToolServers(ctx, id)
			cancel()
			if err != nil {
				s.log.Warn("restart agent for an MCP change", "session", id, "error", err)
			}
			if done {
				return
			}
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(s.restartPoll):
			}
		}
		s.log.Warn("gave up restarting an agent for an MCP change", "session", id)
	}()
}
