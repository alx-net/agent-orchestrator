package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Sign-in states.
const (
	OpWaiting = "waiting"
	// OpExternal: claude.ai took over, and AO is watching for it to finish there.
	OpExternal  = "waiting_external"
	OpConnected = "connected"
	OpFailed    = "failed"
	OpTimedOut  = "timed_out"
	OpCancelled = "cancelled"
)

// finishedOperationsKept is how long a finished sign-in stays readable.
const finishedOperationsKept = 30 * time.Minute

// Operation is one sign-in in progress or finished.
type Operation struct {
	ID        string
	Name      string
	AccountID string
	SessionID string
	State     string
	// AuthURL is the page to open if the browser did not. It carries a one-time
	// state value: show it to the user who asked, never log it.
	AuthURL string
	// Error is Claude Code's own reason when it failed.
	Error string
	// ManageURL is where to finish, for a claude.ai connector.
	ManageURL string
	// Applied says what happened in running agents once it finished.
	Applied    Propagation
	StartedAt  time.Time
	FinishedAt *time.Time
}

// Propagation says how a change reached running agents.
type Propagation struct {
	// InPlace lists sessions that have it now, with no restart.
	InPlace []string
	// Restarting lists sessions that restart once idle, conversation kept.
	Restarting []string
	// Failed lists sessions where the server itself failed, with the reason.
	Failed map[string]string
}

// TargetInput says whose server an operation is about.
type TargetInput struct {
	Name      string
	SessionID string
	AccountID string
	ProjectID string
}

type target struct {
	account   domain.AgentAccount
	dir       string
	projectID string
	session   domain.SessionID
}

func (s *Service) resolveTarget(ctx context.Context, in TargetInput) (target, error) {
	if s.cli == nil {
		return target{}, ErrUnavailable
	}
	if !validServerName(in.Name) {
		return target{}, fmt.Errorf("%w: a server name is required", ErrInvalid)
	}
	if in.SessionID != "" {
		session, account, err := s.sessionAccount(ctx, domain.SessionID(in.SessionID))
		if err != nil {
			return target{}, err
		}
		return target{account: account, dir: session.WorkspacePath, projectID: session.ProjectID, session: session.ID}, nil
	}
	id := in.AccountID
	if id == "" {
		id = domain.DeviceAgentAccountID(domain.AgentAccountProviderClaude)
	}
	account, err := s.accounts.ClaudeAccount(ctx, id)
	if err != nil {
		return target{}, err
	}
	out := target{account: account, projectID: in.ProjectID}
	if in.ProjectID != "" {
		for _, project := range s.projectsWithServers(ctx) {
			if string(project.record.ID) == in.ProjectID {
				out.dir = project.record.Path
			}
		}
	}
	return out, nil
}

// validServerName refuses what would read as a flag or break a line of output.
// Claude Code's own rules are narrower for a new name, wider for existing ones
// (`claude.ai Gmail`, `plugin:x:y`), so it stays the judge of the rest.
func validServerName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	return !strings.ContainsAny(name, "\n\r\x00")
}

// definitionAt is what config says about name from this target's folder.
func (s *Service) definitionAt(t target, name string) (definition, bool) {
	if t.dir != "" {
		if servers, err := s.cli.MCPProjectServers(t.dir); err == nil {
			if raw, ok := servers[name]; ok {
				d := parseDefinition(name, raw, ScopeRepo)
				d.Dir = t.dir
				return d, true
			}
		}
	}
	if user, err := s.cli.MCPUserServers(""); err == nil {
		if raw, ok := user[name]; ok {
			return parseDefinition(name, raw, ScopeGlobal), true
		}
	}
	if !t.account.Device() {
		if own, err := s.cli.MCPUserServers(t.account.Home); err == nil {
			if raw, ok := own[name]; ok {
				return parseDefinition(name, raw, ScopeLocal), true
			}
		}
	}
	return definition{}, false
}

// Connect starts Claude Code's browser sign-in for a server. A second request
// for one already running gets that one, so a double click opens one browser.
func (s *Service) Connect(ctx context.Context, in TargetInput) (Operation, error) {
	t, err := s.resolveTarget(ctx, in)
	if err != nil {
		return Operation{}, err
	}
	d, known := s.definitionAt(t, in.Name)
	if known && (d.Auth == AuthAPIKey || d.Auth == AuthHelper) {
		return Operation{}, fmt.Errorf("%w: %s uses a key in its config, so there is nothing to sign in to", ErrNotEditable, in.Name)
	}
	if known && d.Transport == "stdio" {
		return Operation{}, fmt.Errorf("%w: %s is a local process, so there is nothing to sign in to", ErrNotEditable, in.Name)
	}
	if err := s.accounts.PrepareClaudeAccount(ctx, t.account); err != nil {
		s.log.Warn("prepare account for sign-in", "account", t.account.ID, "error", err)
	}
	if known && d.Scope == ScopeRepo {
		// Claude Code refuses to sign in to a server it has not approved. Asking
		// to connect it is the approval.
		if err := s.cli.MCPApprove(t.account.Home, t.dir, in.Name); err != nil {
			return Operation{}, fmt.Errorf("approve %s: %w", in.Name, err)
		}
	}
	s.mu.Lock()
	s.pruneOperationsLocked()
	for _, op := range s.ops {
		if op.Name == in.Name && op.AccountID == t.account.ID && (op.State == OpWaiting || op.State == OpExternal) {
			current := *op
			s.mu.Unlock()
			return current, nil
		}
	}
	op := &Operation{
		ID: s.newID(), Name: in.Name, AccountID: t.account.ID, SessionID: string(t.session),
		State: OpWaiting, StartedAt: s.now(),
	}
	loginCtx, cancel := context.WithTimeout(s.ctx, s.loginLimit)
	s.ops[op.ID] = op
	s.cancels[op.ID] = cancel
	started := *op
	s.mu.Unlock()
	go s.runLogin(loginCtx, cancel, op.ID, t, d, known)
	return started, nil
}

func (s *Service) pruneOperationsLocked() {
	for id, op := range s.ops {
		if op.FinishedAt != nil && s.now().Sub(*op.FinishedAt) > finishedOperationsKept {
			delete(s.ops, id)
		}
	}
}

func (s *Service) runLogin(ctx context.Context, cancel context.CancelFunc, id string, t target, d definition, known bool) {
	defer cancel()
	name := s.operationName(id)
	var outputMu sync.Mutex
	var output strings.Builder
	err := s.cli.MCPLogin(ctx, ports.MCPTarget{Home: t.account.Home, Dir: t.dir}, name, func(chunk string) {
		outputMu.Lock()
		defer outputMu.Unlock()
		output.WriteString(chunk)
		if authURL := loginURL(output.String()); authURL != "" {
			s.updateOperation(id, func(op *Operation) { op.AuthURL = authURL })
		}
	})
	outputMu.Lock()
	text := output.String()
	outputMu.Unlock()
	switch {
	case err == nil && connectorLogin(text):
		// Claude Code opened claude.ai and exited: the sign-in happens there.
		s.updateOperation(id, func(op *Operation) { op.State, op.ManageURL = OpExternal, ConnectorsURL })
		s.watchConnector(ctx, id, t)
	case err == nil:
		s.finishConnected(id, t, d, known)
	case errors.Is(err, context.DeadlineExceeded):
		s.finishOperation(id, OpTimedOut, fmt.Sprintf("The sign-in was not finished within %s. Try again.", s.loginLimit.Round(time.Minute)))
	case errors.Is(err, context.Canceled):
		s.finishOperation(id, OpCancelled, "")
	default:
		s.finishOperation(id, OpFailed, loginFailure(text, err))
	}
}

// watchConnector waits for a claude.ai sign-in AO cannot see directly, with a
// fresh check of the account each time, and brings it to running agents when it
// lands. A claude.ai page closed without finishing simply times out.
func (s *Service) watchConnector(ctx context.Context, id string, t target) {
	deadline := time.After(s.externalWait)
	name := s.operationName(id)
	for {
		select {
		case <-deadline:
			s.finishOperation(id, OpTimedOut, "The connector was not connected on claude.ai in time. It connects by itself once you finish there.")
			return
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				s.finishOperation(id, OpCancelled, "")
			} else {
				s.finishOperation(id, OpTimedOut, "The connector was not connected on claude.ai in time. It connects by itself once you finish there.")
			}
			return
		case <-time.After(s.externalPoll):
		}
		probed, err := s.cli.MCPProbe(ctx, ports.MCPTarget{Home: t.account.Home}, ports.MCPProbeRequest{})
		if err != nil {
			continue
		}
		for _, server := range probed {
			if server.Name == name && server.Status == rawConnected {
				s.finishConnected(id, t, definition{}, false)
				return
			}
		}
	}
}

func (s *Service) finishConnected(id string, t target, d definition, known bool) {
	s.invalidateChecks(t.account.ID)
	name := s.operationName(id)
	change := change{kind: changeConnect, name: name, accountID: t.account.ID}
	if known {
		change.config = d.Raw
	}
	applied := s.propagate(s.ctx, change)
	at := s.now()
	s.updateOperation(id, func(op *Operation) {
		op.State, op.Applied, op.FinishedAt = OpConnected, applied, &at
	})
}

func (s *Service) finishOperation(id, state, message string) {
	at := s.now()
	s.updateOperation(id, func(op *Operation) { op.State, op.Error, op.FinishedAt = state, message, &at })
}

func (s *Service) updateOperation(id string, change func(*Operation)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok {
		change(op)
	}
}

func (s *Service) operationName(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok {
		return op.Name
	}
	return ""
}

// GetOperation reads a sign-in.
func (s *Service) GetOperation(id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return Operation{}, ErrUnknownOperation
	}
	return *op, nil
}

// CancelOperation stops a sign-in still waiting. A finished one stays as it is.
func (s *Service) CancelOperation(id string) (Operation, error) {
	s.mu.Lock()
	op, ok := s.ops[id]
	if !ok {
		s.mu.Unlock()
		return Operation{}, ErrUnknownOperation
	}
	cancel := s.cancels[id]
	current := *op
	s.mu.Unlock()
	if current.FinishedAt == nil && cancel != nil {
		cancel()
	}
	return current, nil
}

// TestResult is a real check of one server.
type TestResult struct {
	OK        bool
	Name      string
	AccountID string
	Status    string
	RawStatus string
	// Reason says, in plain words, why it passed or what is wrong.
	Reason    string
	Error     string
	ToolCount *int
	Tools     []string
	Auth      string
	Action    string
	CheckedAt time.Time
	// Live is what the session's running agent has now, when asked for a session.
	Live *LiveStatus
}

// LiveStatus is one server in a running agent.
type LiveStatus struct {
	Status    string
	ToolCount *int
	Error     string
}

// Test connects to one server and lists its tools, the way an agent's own
// startup would, and nothing else: only this server is started. For a session it
// also compares the running agent, and reconnects it if the two disagree.
func (s *Service) Test(ctx context.Context, in TargetInput) (TestResult, error) {
	t, err := s.resolveTarget(ctx, in)
	if err != nil {
		return TestResult{}, err
	}
	if err := s.accounts.PrepareClaudeAccount(ctx, t.account); err != nil {
		s.log.Warn("prepare account for test", "account", t.account.ID, "error", err)
	}
	d, known := s.definitionAt(t, in.Name)
	request := ports.MCPProbeRequest{Reconnect: in.Name}
	if known {
		request = ports.MCPProbeRequest{Servers: map[string]json.RawMessage{in.Name: d.Raw}}
	} else {
		d = definition{Name: in.Name, Auth: AuthUnknown}
	}
	if known && d.Scope == ScopeRepo && t.dir != "" {
		if err := s.cli.MCPTrust(t.account.Home, t.dir); err != nil {
			s.log.Debug("trust folder for MCP test", "account", t.account.ID, "error", err)
		}
	}
	probed, err := s.cli.MCPProbe(ctx, ports.MCPTarget{Home: t.account.Home, Dir: t.dir}, request)
	if err != nil {
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
		result := TestResult{Name: in.Name, AccountID: t.account.ID, Status: StatusUnknown, Auth: d.Auth, CheckedAt: s.now(), Error: err.Error()}
		result.Reason = "Claude Code could not run the check: " + err.Error()
		return result, nil
	}
	var entry *checkEntry
	for _, candidate := range entriesFromProbe(probed) {
		if candidate.Name == in.Name {
			found := candidate
			entry = &found
			break
		}
	}
	if entry == nil {
		return TestResult{}, fmt.Errorf("%w: %s", ErrUnknownServer, in.Name)
	}
	if !known {
		d = definitionFromEntry(*entry)
	}
	result := TestResult{
		Name: in.Name, AccountID: t.account.ID, Status: statusFromRaw(d.Auth, entry.Status), RawStatus: entry.Raw, Error: entry.Error,
		ToolCount: entry.ToolCount, Tools: entry.Tools, Auth: d.Auth, CheckedAt: s.now(),
	}
	result.OK = result.Status == StatusConnected
	result.Reason = testReason(result, entry)
	result.Action = actionFor(d, result.Status)
	s.rememberTest(t, *entry)
	if t.session != "" {
		result.Live = s.compareLive(ctx, t.session, in.Name, result.OK)
	}
	return result, nil
}

func testReason(result TestResult, entry *checkEntry) string {
	switch result.Status {
	case StatusConnected:
		count := 0
		if entry.ToolCount != nil {
			count = *entry.ToolCount
		}
		if count == 1 {
			return "Connected. It exposes 1 tool."
		}
		return fmt.Sprintf("Connected. It exposes %d tools.", count)
	case StatusNeedsSignIn:
		return fmt.Sprintf("It needs a sign-in on this subscription. Run: ao mcp login %s", result.Name)
	case StatusKeyRejected:
		return "The server refused the key in its config."
	case StatusFailed:
		if entry.Error != "" {
			return "It did not connect: " + entry.Error
		}
		return "It did not connect."
	case StatusStarting:
		return "It is still starting after the wait, so it is slow or stuck."
	case StatusDisabled:
		return "It is turned off for this project."
	}
	return "Claude Code reports: " + entry.Raw
}

// rememberTest folds a test into the last check, so the list shows it.
func (s *Service) rememberTest(t target, entry checkEntry) {
	key := checkKey{t.account.ID, ""}
	if d, ok := s.definitionAt(t, entry.Name); ok && d.Scope == ScopeRepo {
		key.dir = t.dir
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.checks[key]
	if !ok {
		c = &check{done: make(chan struct{}), at: s.now()}
		close(c.done)
		s.checks[key] = c
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.entries {
		if c.entries[i].Name == entry.Name {
			c.entries[i] = entry
			return
		}
	}
	c.entries = append(c.entries, entry)
}

// compareLive reports what the running agent has. A server that works but that
// the agent lost gets reconnected: the check just proved it would help.
func (s *Service) compareLive(ctx context.Context, id domain.SessionID, name string, healthy bool) *LiveStatus {
	servers, err := s.live.LiveMCPServers(ctx, id)
	if err != nil {
		return nil
	}
	current, ok := liveEntry(servers, name)
	if healthy && (!ok || current.Status != ports.ChatMCPStatusReady) {
		if refreshed, _, reconnectErr := s.live.ReconnectMCPServer(ctx, id, name); reconnectErr == nil {
			if updated, found := liveEntry(refreshed, name); found {
				current, ok = updated, true
			}
		}
	}
	if !ok {
		return &LiveStatus{Status: StatusNotChecked}
	}
	return &LiveStatus{Status: statusFromRaw(AuthUnknown, liveStatus(current.Status)), ToolCount: current.ToolCount, Error: current.Error}
}

func liveEntry(servers []domain.ConversationMCPServer, name string) (domain.ConversationMCPServer, bool) {
	for _, server := range servers {
		if server.Name == name {
			return server, true
		}
	}
	return domain.ConversationMCPServer{}, false
}

// Approve records that this subscription approves a repo server, one click for
// what `claude` would otherwise ask interactively. See ports.MCPCLI.MCPApprove.
func (s *Service) Approve(ctx context.Context, in TargetInput) (Propagation, error) {
	t, err := s.resolveTarget(ctx, in)
	if err != nil {
		return Propagation{}, err
	}
	if t.dir == "" {
		return Propagation{}, fmt.Errorf("%w: approving a server needs its project", ErrInvalid)
	}
	d, known := s.definitionAt(t, in.Name)
	if !known || d.Scope != ScopeRepo {
		return Propagation{}, fmt.Errorf("%w: %s is not in this project's .mcp.json", ErrUnknownServer, in.Name)
	}
	if err := s.cli.MCPApprove(t.account.Home, t.dir, in.Name); err != nil {
		return Propagation{}, fmt.Errorf("approve %s: %w", in.Name, err)
	}
	s.invalidateChecks(t.account.ID)
	return s.propagate(ctx, change{kind: changeApprove, name: in.Name, accountID: t.account.ID, projectID: t.projectID, config: d.Raw}), nil
}

// Reconnect has one session's running agent connect to a server again.
func (s *Service) Reconnect(ctx context.Context, id domain.SessionID, name string) (SessionList, error) {
	t, err := s.resolveTarget(ctx, TargetInput{Name: name, SessionID: string(id)})
	if err != nil {
		return SessionList{}, err
	}
	d, known := s.definitionAt(t, name)
	ch := change{kind: changeConnect, name: name}
	if known {
		ch.config = d.Raw
	}
	s.applyToChat(ctx, Session{ID: id, Chat: true, AccountID: t.account.ID, ProjectID: t.projectID}, ch, &Propagation{Failed: map[string]string{}})
	return s.SessionServers(ctx, id, false)
}

var (
	oscLink  = regexp.MustCompile("\x1b\\]8;;([^\x07\x1b]+)(?:\x07|\x1b\\\\)")
	escapes  = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]")
	plainURL = regexp.MustCompile(`https?://[^\s\x1b\x07]+`)
)

// loginURL finds the page Claude Code wants opened. The terminal link's target
// is whole; printed text can wrap.
func loginURL(text string) string {
	if match := oscLink.FindStringSubmatch(text); match != nil {
		return match[1]
	}
	if !strings.Contains(text, "visit") {
		return ""
	}
	return plainURL.FindString(escapes.ReplaceAllString(text, ""))
}

func cleanTerminal(text string) string {
	return strings.ReplaceAll(escapes.ReplaceAllString(text, ""), "\r", "")
}

// connectorLogin recognizes the sign-in that hands off to claude.ai and exits.
func connectorLogin(text string) bool {
	return strings.Contains(cleanTerminal(text), "Once authorized on claude.ai")
}

// loginFailure is what the user should read: Claude Code's own last words, minus
// its prompts and the sign-in link.
func loginFailure(text string, err error) string {
	var lines []string
	for _, line := range strings.Split(cleanTerminal(text), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "",
			strings.HasPrefix(line, "Starting authentication"),
			strings.HasPrefix(line, "If the browser didn't open"),
			strings.HasPrefix(line, "Visit this URL"),
			strings.HasPrefix(line, "Waiting for authorization"),
			strings.HasPrefix(line, "Or paste the redirect URL"),
			strings.HasPrefix(line, "http://"), strings.HasPrefix(line, "https://"):
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		return strings.Join(lines, " ")
	}
	if err != nil {
		return err.Error()
	}
	return "The sign-in did not finish."
}
