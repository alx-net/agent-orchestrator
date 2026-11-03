package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AddInput is a new server at the user's global scope. Header and env values
// are secrets: they go to Claude Code's config and nowhere else.
type AddInput struct {
	Name      string
	URL       string
	Transport string // http (default for a URL) or sse
	Command   string
	Args      []string
	Env       map[string]string
	Headers   map[string]string
}

// Claude Code's own rule for a new server's name.
var newServerName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Add adds a server through `claude mcp add-json` to the computer's own config,
// and brings it to every running agent. AO shares that config into its accounts.
func (s *Service) Add(ctx context.Context, in AddInput) (Propagation, error) {
	if s.cli == nil {
		return Propagation{}, ErrUnavailable
	}
	config, err := configFor(in)
	if err != nil {
		return Propagation{}, err
	}
	user, err := s.cli.MCPUserServers("")
	if err != nil {
		return Propagation{}, err
	}
	if _, exists := user[in.Name]; exists {
		return Propagation{}, fmt.Errorf("%w: %s. Remove it first, or change its key", ErrServerExists, in.Name)
	}
	if err := s.addUserServer(ctx, in.Name, config, in.Headers); err != nil {
		return Propagation{}, err
	}
	s.afterConfigChange(ctx)
	return s.propagate(ctx, change{kind: changeAdd, name: in.Name, config: config}), nil
}

func configFor(in AddInput) (json.RawMessage, error) {
	if !newServerName.MatchString(in.Name) {
		return nil, fmt.Errorf("%w: a server name can use letters, numbers, hyphens and underscores", ErrInvalid)
	}
	switch {
	case in.URL != "" && in.Command != "":
		return nil, fmt.Errorf("%w: give a URL or a command, not both", ErrInvalid)
	case in.URL != "":
		if !strings.HasPrefix(in.URL, "http://") && !strings.HasPrefix(in.URL, "https://") {
			return nil, fmt.Errorf("%w: the URL must start with http:// or https://", ErrInvalid)
		}
		transport := strings.ToLower(in.Transport)
		if transport == "" {
			transport = "http"
		}
		if transport != "http" && transport != "sse" {
			return nil, fmt.Errorf("%w: the transport of a URL is http or sse", ErrInvalid)
		}
		config := map[string]any{"type": transport, "url": in.URL}
		if len(in.Headers) > 0 {
			config["headers"] = in.Headers
		}
		return json.Marshal(config)
	case in.Command != "":
		if len(in.Headers) > 0 {
			return nil, fmt.Errorf("%w: a local command has no headers; use env values", ErrInvalid)
		}
		config := map[string]any{"type": "stdio", "command": in.Command, "args": in.Args}
		if in.Args == nil {
			config["args"] = []string{}
		}
		if len(in.Env) > 0 {
			config["env"] = in.Env
		}
		return json.Marshal(config)
	}
	return nil, fmt.Errorf("%w: give a URL or a command", ErrInvalid)
}

func (s *Service) addUserServer(ctx context.Context, name string, config json.RawMessage, secrets map[string]string) error {
	result, err := s.cli.MCPCommand(ctx, ports.MCPTarget{}, "mcp", "add-json", "--scope", "user", "--", name, string(config))
	if err != nil {
		return fmt.Errorf("add %s: %s", name, scrub(err.Error(), secrets))
	}
	if result.ExitCode != 0 {
		message := strings.TrimSpace(result.Stderr)
		if message == "" {
			message = strings.TrimSpace(result.Stdout)
		}
		message = scrub(message, secrets)
		if strings.Contains(message, "already exists") {
			return fmt.Errorf("%w: %s", ErrServerExists, message)
		}
		return fmt.Errorf("%w: %s", ErrInvalid, message)
	}
	return nil
}

// Remove removes a global server through `claude mcp remove`, and takes it out of
// running agents. A server in a repo's .mcp.json belongs to the repo.
func (s *Service) Remove(ctx context.Context, name string) (Propagation, error) {
	if s.cli == nil {
		return Propagation{}, ErrUnavailable
	}
	if !validServerName(name) {
		return Propagation{}, fmt.Errorf("%w: a server name is required", ErrInvalid)
	}
	user, err := s.cli.MCPUserServers("")
	if err != nil {
		return Propagation{}, err
	}
	if _, exists := user[name]; !exists {
		return Propagation{}, fmt.Errorf("%w: %s is not in your global config. A repo's .mcp.json belongs to that repo", ErrUnknownServer, name)
	}
	result, err := s.cli.MCPCommand(ctx, ports.MCPTarget{}, "mcp", "remove", "--scope", "user", "--", name)
	if err != nil {
		return Propagation{}, err
	}
	if result.ExitCode != 0 {
		return Propagation{}, errors.New(strings.TrimSpace(result.Stderr + " " + result.Stdout))
	}
	s.afterConfigChange(ctx)
	return s.propagate(ctx, change{kind: changeRemove, name: name}), nil
}

// SetKey replaces the headers of a global server whose config holds its key. An
// empty value keeps that header's current one, so a form that masks existing
// values does not have to know them. Claude Code has no command to edit a
// server, so this removes and adds it, and puts the old config back on failure.
func (s *Service) SetKey(ctx context.Context, name string, headers map[string]string) (Propagation, error) {
	if s.cli == nil {
		return Propagation{}, ErrUnavailable
	}
	user, err := s.cli.MCPUserServers("")
	if err != nil {
		return Propagation{}, err
	}
	raw, exists := user[name]
	if !exists {
		return Propagation{}, fmt.Errorf("%w: %s is not in your global config", ErrUnknownServer, name)
	}
	if d := parseDefinition(name, raw, ScopeGlobal); d.Auth == AuthHelper || d.Transport == "stdio" {
		return Propagation{}, fmt.Errorf("%w: %s does not keep a key in its headers", ErrNotEditable, name)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw, &config); err != nil {
		return Propagation{}, fmt.Errorf("read %s: %w", name, err)
	}
	var current map[string]string
	_ = json.Unmarshal(config["headers"], &current)
	merged := make(map[string]string, len(headers))
	for key, value := range headers {
		if value == "" {
			value = current[key]
		}
		if value != "" {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return Propagation{}, fmt.Errorf("%w: enter at least one header", ErrInvalid)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return Propagation{}, err
	}
	config["headers"] = encoded
	updated, err := json.Marshal(config)
	if err != nil {
		return Propagation{}, err
	}
	removed, err := s.cli.MCPCommand(ctx, ports.MCPTarget{}, "mcp", "remove", "--scope", "user", "--", name)
	if err != nil {
		return Propagation{}, err
	}
	if removed.ExitCode != 0 {
		return Propagation{}, errors.New(scrub(strings.TrimSpace(removed.Stderr+" "+removed.Stdout), merged))
	}
	if err := s.addUserServer(ctx, name, updated, merged); err != nil {
		if restoreErr := s.addUserServer(ctx, name, raw, current); restoreErr != nil {
			return Propagation{}, fmt.Errorf("%w; and could not restore the old config: %w", err, restoreErr)
		}
		return Propagation{}, err
	}
	s.afterConfigChange(ctx)
	return s.propagate(ctx, change{kind: changeKey, name: name, config: updated}), nil
}

// afterConfigChange carries the user's global config into every account folder
// and forgets the readings it made stale.
func (s *Service) afterConfigChange(ctx context.Context) {
	accounts, err := s.accounts.ClaudeAccounts(ctx)
	if err != nil {
		s.log.Warn("list accounts after MCP config change", "error", err)
	}
	for _, account := range accounts {
		if err := s.accounts.PrepareClaudeAccount(ctx, account); err != nil {
			s.log.Warn("share MCP config into account", "account", account.ID, "error", err)
		}
	}
	s.invalidateChecks("")
}

// scrub removes secret values from text Claude Code printed, in case a message
// ever quotes its input.
func scrub(text string, secrets map[string]string) string {
	for _, value := range secrets {
		if value != "" {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	return text
}
