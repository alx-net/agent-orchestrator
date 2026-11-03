package mcpservers

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Claude Code's own status words, from the control protocol. The last two come
// only from `claude mcp list`.
const (
	rawConnected = "connected"
	rawNeedsAuth = "needs-auth"
	rawFailed    = "failed"
	rawPending   = "pending"
	rawDisabled  = "disabled"
	rawApproval  = "needs-approval"
	rawRejected  = "rejected"
)

type rawConfig = json.RawMessage

// checkKey is one subscription in one folder ("" is the global check).
type checkKey struct {
	accountID string
	dir       string
}

// checkEntry is one server as Claude Code reported it.
type checkEntry struct {
	Name          string
	Status        string // Claude Code's own word
	Raw           string // the text it printed, for a state AO has no word for
	Error         string
	ToolCount     *int
	Tools         []string
	Transport     string
	URL           string
	Command       string
	ProviderScope string
}

// check is one reading, running or finished.
type check struct {
	done chan struct{}

	mu      sync.Mutex
	entries []checkEntry
	err     error
	at      time.Time
	running bool
	stale   bool
}

// startCheck returns the check for this subscription and folder, starting one
// unless a fresh or running one exists.
func (s *Service) startCheck(account domain.AgentAccount, dir string, refresh bool) *check {
	key := checkKey{account.ID, dir}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.checks[key]; ok {
		current.mu.Lock()
		running := current.running
		fresh := !current.at.IsZero() && s.now().Sub(current.at) < checkFreshFor
		current.mu.Unlock()
		if running || (fresh && !refresh) {
			return current
		}
	}
	c := &check{done: make(chan struct{}), running: true}
	if previous := s.checks[key]; previous != nil {
		// Keep showing the last reading while the new one runs.
		previous.mu.Lock()
		c.entries, c.at = previous.entries, previous.at
		previous.mu.Unlock()
	}
	s.checks[key] = c
	go s.runCheck(account, dir, c)
	return c
}

func (s *Service) knownCheck(account domain.AgentAccount, dir string) *check {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checks[checkKey{account.ID, dir}]
}

func (s *Service) anyCheckRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.checks {
		c.mu.Lock()
		running := c.running
		c.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

func (s *Service) runCheck(account domain.AgentAccount, dir string, c *check) {
	defer close(c.done)
	select {
	case s.checkSlots <- struct{}{}:
		defer func() { <-s.checkSlots }()
	case <-s.ctx.Done():
		c.mu.Lock()
		c.running, c.err = false, s.ctx.Err()
		c.mu.Unlock()
		return
	}
	entries, err := s.read(s.ctx, account, dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	c.err = err
	if err == nil || len(entries) > 0 {
		c.entries = entries
	}
	if !c.stale {
		c.at = s.now()
	}
}

// read asks Claude Code. The global check uses the control protocol, which is
// exactly what an agent's own startup does and reports each server's tools. A
// project's folder is read first with `claude mcp list`, the only command that
// shows approval, and only servers already approved are then connected: a
// status view must never run a command from a .mcp.json nobody approved.
func (s *Service) read(ctx context.Context, account domain.AgentAccount, dir string) ([]checkEntry, error) {
	if err := s.accounts.PrepareClaudeAccount(ctx, account); err != nil {
		s.log.Warn("prepare account for MCP check", "account", account.ID, "error", err)
	}
	// A check must connect, not repeat what the CLI remembered about a refusal.
	if err := s.cli.MCPForgetAuthCache(account.Home); err != nil {
		s.log.Debug("forget MCP auth cache", "account", account.ID, "error", err)
	}
	target := ports.MCPTarget{Home: account.Home, Dir: dir}
	if dir == "" {
		probed, err := s.cli.MCPProbe(ctx, target, ports.MCPProbeRequest{})
		if err == nil {
			return entriesFromProbe(probed), nil
		}
		s.log.Debug("MCP probe failed, reading the list instead", "account", account.ID, "error", err)
		return s.readList(ctx, target)
	}
	// An agent runs here anyway, and only a trusted folder's settings count.
	if err := s.cli.MCPTrust(account.Home, dir); err != nil {
		s.log.Debug("trust folder for MCP check", "account", account.ID, "error", err)
	}
	listed, err := s.readList(ctx, target)
	if err != nil {
		return nil, err
	}
	defined, err := s.cli.MCPProjectServers(dir)
	if err != nil {
		return listed, nil //nolint:nilerr // the list's reading stands; only tool counts are missing
	}
	approved := map[string]json.RawMessage{}
	for _, entry := range listed {
		config, ok := defined[entry.Name]
		if ok && entry.Status != rawApproval && entry.Status != rawRejected && entry.Status != rawDisabled {
			approved[entry.Name] = config
		}
	}
	if len(approved) == 0 {
		return listed, nil
	}
	probed, err := s.cli.MCPProbe(ctx, target, ports.MCPProbeRequest{Servers: approved})
	if err != nil {
		return listed, nil //nolint:nilerr // the list's reading stands; only tool counts are missing
	}
	byName := map[string]checkEntry{}
	for _, entry := range entriesFromProbe(probed) {
		byName[entry.Name] = entry
	}
	for i, entry := range listed {
		if live, ok := byName[entry.Name]; ok {
			listed[i] = live
		}
	}
	return listed, nil
}

func (s *Service) readList(ctx context.Context, target ports.MCPTarget) ([]checkEntry, error) {
	result, err := s.cli.MCPCommand(ctx, target, "mcp", "list")
	if err != nil {
		return nil, err
	}
	return parseList(result.Stdout), nil
}

func entriesFromProbe(servers []ports.MCPProbeServer) []checkEntry {
	out := make([]checkEntry, 0, len(servers))
	for _, server := range servers {
		out = append(out, checkEntry{
			Name: server.Name, Status: server.Status, Raw: server.Status, Error: server.Error, ToolCount: server.ToolCount, Tools: server.Tools,
			Transport: server.Transport, URL: server.URL, Command: server.Command, ProviderScope: server.Scope,
		})
	}
	return out
}

// invalidateChecks forgets readings of this account, or every account for "".
func (s *Service) invalidateChecks(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, c := range s.checks {
		if accountID != "" && key.accountID != accountID {
			continue
		}
		c.mu.Lock()
		// The last reading stays on screen until a new one replaces it. A check
		// running now started before the change, so its reading is stale too.
		c.at = time.Time{}
		c.stale = true
		c.mu.Unlock()
	}
}

// statusFromRaw turns Claude Code's word into AO's, given how the server
// authenticates: a key the server refused is not something a sign-in can fix.
func statusFromRaw(auth, raw string) string {
	switch raw {
	case rawConnected:
		return StatusConnected
	case rawNeedsAuth:
		if auth == AuthAPIKey || auth == AuthHelper {
			return StatusKeyRejected
		}
		return StatusNeedsSignIn
	case rawFailed:
		return StatusFailed
	case rawPending:
		return StatusStarting
	case rawDisabled:
		return StatusDisabled
	case rawApproval:
		return StatusNeedsApproval
	case rawRejected:
		return StatusRejected
	}
	return StatusUnknown
}

// listMarker finds where a `claude mcp list` line's status starts. Symbols vary
// by terminal support, so any non-word symbol after " - " counts.
var listMarker = regexp.MustCompile(` - ([^\p{L}\p{N}\s])\s`)

// parseList reads `claude mcp list`, whose only format is lines of text:
// `name: target - <symbol> <status>`, or `<status> — <detail>` on failure. A
// line it cannot place is kept with its raw text rather than dropped.
func parseList(output string) []checkEntry {
	var out []checkEntry
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Checking MCP server health") || strings.HasPrefix(line, "No MCP servers configured") {
			continue
		}
		name, rest, ok := strings.Cut(line, ": ")
		if !ok || name == "" {
			continue
		}
		entry := checkEntry{Name: name, Raw: rest}
		target := rest
		if at := listMarker.FindStringIndex(rest); at != nil {
			target = rest[:at[0]]
			label := strings.TrimSpace(rest[at[0]+len(" - "):])
			if _, after, found := strings.Cut(label, " "); found {
				label = after
			}
			entry.Raw = label
			entry.Status, entry.Error = statusFromListLabel(label)
		} else if at := strings.LastIndex(rest, " - "); at >= 0 {
			target = rest[:at]
			entry.Raw = rest[at+len(" - "):]
			entry.Status, entry.Error = statusFromListLabel(entry.Raw)
		}
		entry.Transport, entry.URL, entry.Command = parseListTarget(target)
		out = append(out, entry)
	}
	return out
}

func statusFromListLabel(label string) (status, detail string) {
	head, detail, _ := strings.Cut(label, " — ")
	lower := strings.ToLower(head)
	switch {
	case strings.HasPrefix(lower, "connected"):
		return rawConnected, detail
	case strings.HasPrefix(lower, "needs authentication"):
		return rawNeedsAuth, detail
	case strings.HasPrefix(lower, "failed"):
		return rawFailed, detail
	case strings.HasPrefix(lower, "pending approval"):
		return rawApproval, detail
	case strings.HasPrefix(lower, "rejected"):
		return rawRejected, detail
	case strings.HasPrefix(lower, "disabled"):
		return rawDisabled, detail
	}
	return label, detail
}

func parseListTarget(target string) (transport, rawURL, command string) {
	target = strings.TrimSpace(target)
	for _, suffix := range []string{"HTTP", "SSE"} {
		if trimmed, ok := strings.CutSuffix(target, " ("+suffix+")"); ok {
			return strings.ToLower(suffix), trimmed, ""
		}
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return "", target, "" // a claude.ai connector prints a bare URL
	}
	return "stdio", "", target
}

// definition is what config says a server is. Raw holds any secrets it has.
type definition struct {
	Name        string
	Scope       string
	Transport   string
	Target      string
	Auth        string
	HeaderNames []string
	ProjectID   string
	ProjectName string
	Dir         string
	Raw         json.RawMessage
}

// Editable: AO can change this server's key, because the config is the user's
// global one and it holds the key itself rather than running a helper for it.
func (d definition) Editable() bool {
	return d.Scope == ScopeGlobal && d.Auth == AuthAPIKey
}

type serverConfig struct {
	Type          string            `json:"type"`
	URL           string            `json:"url"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Headers       map[string]string `json:"headers"`
	HeadersHelper string            `json:"headersHelper"`
}

// parseDefinition learns a server's shape from its config. An entry it cannot
// read still lists, with an unknown type.
func parseDefinition(name string, raw json.RawMessage, scope string) definition {
	d := definition{Name: name, Scope: scope, Raw: raw, Auth: AuthUnknown}
	var config serverConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return d
	}
	d.Transport = strings.ToLower(config.Type)
	if d.Transport == "" {
		switch {
		case config.Command != "":
			d.Transport = "stdio"
		case config.URL != "":
			d.Transport = "http"
		}
	}
	if config.URL != "" {
		d.Target = displayURL(config.URL)
	} else if config.Command != "" {
		d.Target = displayCommand(config.Command, config.Args)
	}
	for name := range config.Headers {
		d.HeaderNames = append(d.HeaderNames, name)
	}
	sort.Strings(d.HeaderNames)
	switch {
	case d.Transport == "stdio":
		d.Auth = AuthNone
	case config.HeadersHelper != "":
		d.Auth = AuthHelper
	case len(config.Headers) > 0:
		d.Auth = AuthAPIKey
	case d.Transport == "http" || d.Transport == "sse":
		d.Auth = AuthOAuth
	}
	return d
}

// definitionFromEntry describes a server no config file names, from what Claude
// Code itself said, so a connector or a new kind of server still lists.
func definitionFromEntry(entry checkEntry) definition {
	d := definition{Name: entry.Name, Scope: ScopeOther, Auth: AuthUnknown, Transport: entry.Transport}
	switch {
	case entry.URL != "":
		d.Target = displayURL(entry.URL)
	case entry.Command != "":
		d.Target = entry.Command
	}
	switch {
	case entry.ProviderScope == "claudeai" || strings.HasPrefix(entry.Name, "claude.ai "):
		d.Scope, d.Auth = ScopeConnector, AuthConnector
	case entry.ProviderScope == "plugin" || strings.HasPrefix(entry.Name, "plugin:"):
		d.Scope = ScopePlugin
	case entry.ProviderScope == "local":
		d.Scope = ScopeLocal
	case entry.ProviderScope == "user":
		d.Scope = ScopeGlobal
	case entry.ProviderScope == "project":
		d.Scope = ScopeRepo
	}
	if d.Auth == AuthUnknown {
		switch d.Transport {
		case "stdio":
			d.Auth = AuthNone
		case "http", "sse":
			d.Auth = AuthOAuth
		}
	}
	return d
}

// definitions is everything config says for this account: the computer's global
// servers, the account's own, and each project's.
func (s *Service) definitions(account domain.AgentAccount, projects []projectServers) map[rowKey]definition {
	out := map[rowKey]definition{}
	if user, err := s.cli.MCPUserServers(""); err == nil {
		for name, raw := range user {
			out[rowKey{name, ""}] = parseDefinition(name, raw, ScopeGlobal)
		}
	}
	if !account.Device() {
		if own, err := s.cli.MCPUserServers(account.Home); err == nil {
			for name, raw := range own {
				if _, shared := out[rowKey{name, ""}]; !shared {
					out[rowKey{name, ""}] = parseDefinition(name, raw, ScopeLocal)
				}
			}
		}
	}
	for _, project := range projects {
		for name, raw := range project.servers {
			d := parseDefinition(name, raw, ScopeRepo)
			d.ProjectID, d.ProjectName, d.Dir = project.record.ID, projectLabel(project.record), project.record.Path
			out[rowKey{name, project.record.Path}] = d
		}
	}
	return out
}

func projectLabel(record domain.ProjectRecord) string {
	if record.DisplayName != "" {
		return record.DisplayName
	}
	return record.ID
}

var secretArg = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[-_]?key|auth)`)

// displayURL drops what could be a credential from a URL before it is shown.
func displayURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.User = nil
	if parsed.RawQuery != "" {
		parsed.RawQuery = ""
	}
	parsed.Fragment = ""
	return parsed.String()
}

// displayCommand shows a command with anything credential-shaped masked.
func displayCommand(command string, args []string) string {
	parts := []string{command}
	maskNext := false
	for _, arg := range args {
		switch {
		case maskNext:
			parts = append(parts, "•••")
			maskNext = false
		case strings.Contains(arg, "=") && secretArg.MatchString(arg):
			key, _, _ := strings.Cut(arg, "=")
			parts = append(parts, key+"=•••")
		case strings.HasPrefix(arg, "-") && secretArg.MatchString(arg):
			parts = append(parts, arg)
			maskNext = true
		default:
			parts = append(parts, arg)
		}
	}
	return strings.Join(parts, " ")
}
