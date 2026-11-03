// Package mcpservers lists the MCP servers Claude Code agents can use, per
// subscription, and connects, tests, approves, adds and removes them. Claude Code
// is the source of truth: AO drives its own commands and control protocol, and
// reads its config files only to learn what a server is, never to guess a state.
//
// A change reaches running agents on its own. An agent takes it in place where
// Claude Code allows that, and otherwise restarts with its conversation kept once
// it is idle, so nobody ever has to restart anything.
package mcpservers

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Status is one server's state on one subscription, in AO's words.
const (
	StatusConnected     = "connected"
	StatusNeedsSignIn   = "needs_sign_in"
	StatusKeyRejected   = "key_rejected"
	StatusNeedsApproval = "needs_approval"
	StatusRejected      = "rejected"
	StatusFailed        = "failed"
	StatusStarting      = "starting"
	StatusDisabled      = "disabled"
	StatusNotChecked    = "not_checked"
	// StatusUnknown is a state this build has no word for. RawStatus says it.
	StatusUnknown = "unknown"
)

// Auth is how a server authenticates.
const (
	AuthOAuth     = "oauth"
	AuthAPIKey    = "api_key"
	AuthHelper    = "helper"
	AuthNone      = "none" // a local process: it has no sign-in of its own
	AuthConnector = "connector"
	AuthUnknown   = "unknown"
)

// Scope is where a server is defined.
const (
	ScopeGlobal    = "global"
	ScopeRepo      = "repo"
	ScopeLocal     = "local"
	ScopeConnector = "connector"
	ScopePlugin    = "plugin"
	ScopeOther     = "other"
)

// Action is the one thing a status view can offer to fix a server.
const (
	ActionConnect = "connect"
	ActionApprove = "approve"
	ActionEditKey = "edit_key"
	ActionManage  = "manage"
)

// ConnectorsURL is where claude.ai connectors are managed.
const ConnectorsURL = "https://claude.ai/settings/connectors"

// checkFreshFor is how long a check is reused. Checks start every server, so
// they take seconds and memory; they cost no tokens, and Refresh always runs one.
const checkFreshFor = 10 * time.Minute

// maxConcurrentChecks bounds how many Claude Code processes a check runs at once.
const maxConcurrentChecks = 2

var (
	// ErrUnknownServer is a server name Claude Code does not have.
	ErrUnknownServer = errors.New("no MCP server with that name")
	// ErrServerExists refuses to overwrite a server under the same name.
	ErrServerExists = errors.New("an MCP server with that name already exists")
	// ErrNotEditable is a server whose config AO does not own.
	ErrNotEditable = errors.New("AO does not own this server's config")
	// ErrInvalid is a request that makes no sense.
	ErrInvalid = errors.New("invalid MCP server request")
	// ErrUnknownOperation is a sign-in AO has no record of.
	ErrUnknownOperation = errors.New("unknown or expired sign-in")
	// ErrUnknownSession is a session id that does not exist.
	ErrUnknownSession = errors.New("unknown session")
	// ErrHarnessUnsupported is a session whose agent is not Claude Code.
	ErrHarnessUnsupported = errors.New("MCP servers are managed for Claude Code sessions")
	// ErrUnavailable means this daemon has no Claude Code to ask.
	ErrUnavailable = errors.New("Claude Code is not available")
	// ErrNotLive reports a session with no running agent to change.
	ErrNotLive = errors.New("the session has no running agent")
	// ErrInPlaceUnsupported reports a running agent that cannot take a change.
	ErrInPlaceUnsupported = errors.New("the running agent cannot change its servers in place")
)

// Session is what this service needs to know about a session.
type Session struct {
	ID            domain.SessionID
	AccountID     string
	WorkspacePath string
	ProjectID     string
	// Chat is false for a terminal agent, which AO cannot reach in place.
	Chat bool
}

// Sessions reads sessions.
type Sessions interface {
	// MCPSession returns a Claude Code session, ErrUnknownSession, or
	// ErrHarnessUnsupported.
	MCPSession(ctx context.Context, id domain.SessionID) (Session, error)
	// MCPRunningSessions lists Claude Code sessions with an agent running.
	MCPRunningSessions(ctx context.Context) ([]Session, error)
}

// LiveAgents reaches running agents. Each method returns ErrNotLive when none
// runs and ErrInPlaceUnsupported when the running one cannot do it.
type LiveAgents interface {
	LiveMCPServers(ctx context.Context, id domain.SessionID) ([]domain.ConversationMCPServer, error)
	ReconnectMCPServer(ctx context.Context, id domain.SessionID, name string) ([]domain.ConversationMCPServer, string, error)
	ApplyMCPServers(ctx context.Context, id domain.SessionID, change ports.ChatMCPChange) ([]domain.ConversationMCPServer, map[string]string, error)
}

// Restarter restarts an agent with its conversation kept, only once it is idle.
// done false means busy: ask again.
type Restarter interface {
	RestartAgentForToolServers(ctx context.Context, id domain.SessionID) (done bool, err error)
}

// Accounts reads Claude Code subscriptions.
type Accounts interface {
	ClaudeAccounts(ctx context.Context) ([]domain.AgentAccount, error)
	ClaudeAccount(ctx context.Context, id string) (domain.AgentAccount, error)
	// PrepareClaudeAccount brings the user's servers into the account's folder.
	PrepareClaudeAccount(ctx context.Context, account domain.AgentAccount) error
}

// Projects reads registered projects.
type Projects interface {
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
}

// Deps wires the service.
type Deps struct {
	// Context ends the service's background work.
	Context   context.Context
	CLI       ports.MCPCLI
	Accounts  Accounts
	Sessions  Sessions
	Live      LiveAgents
	Restarter Restarter
	Projects  Projects
	Log       *slog.Logger
}

// Service is the MCP server catalog and its operations.
type Service struct {
	ctx       context.Context
	cli       ports.MCPCLI
	accounts  Accounts
	sessions  Sessions
	live      LiveAgents
	restarter Restarter
	projects  Projects
	log       *slog.Logger
	now       func() time.Time
	newID     func() string

	loginLimit      time.Duration
	externalWait    time.Duration
	externalPoll    time.Duration
	restartPoll     time.Duration
	restartPatience time.Duration

	checkSlots chan struct{}

	mu       sync.Mutex
	checks   map[checkKey]*check
	ops      map[string]*Operation
	cancels  map[string]context.CancelFunc
	restarts map[domain.SessionID]bool
}

// New builds the service.
func New(deps Deps) *Service {
	ctx := deps.Context
	if ctx == nil {
		ctx = context.Background()
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		ctx: ctx, cli: deps.CLI, accounts: deps.Accounts, sessions: deps.Sessions, live: deps.Live,
		restarter: deps.Restarter, projects: deps.Projects, log: log,
		now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewString,
		loginLimit: 5 * time.Minute, externalWait: 5 * time.Minute, externalPoll: 15 * time.Second,
		restartPoll: 3 * time.Second, restartPatience: 24 * time.Hour,
		checkSlots: make(chan struct{}, maxConcurrentChecks),
		checks:     map[checkKey]*check{}, ops: map[string]*Operation{}, cancels: map[string]context.CancelFunc{},
		restarts: map[domain.SessionID]bool{},
	}
}

// AccountStatus is one server on one subscription.
type AccountStatus struct {
	AccountID string
	Status    string
	// RawStatus is Claude Code's own word for it.
	RawStatus string
	// ToolCount is how many tools a connected server exposes, once known.
	ToolCount *int
	// Error is the last failure in the server's or Claude Code's own words.
	Error string
	// CheckedAt is when this state was read; nil when it never was.
	CheckedAt *time.Time
	// Action is what to offer: ActionConnect, ActionApprove, ... or nothing.
	Action string
	// Live marks what a running agent reported, as opposed to a separate check.
	Live bool
}

// Server is one MCP server and its state per subscription.
type Server struct {
	Name        string
	Transport   string
	Target      string
	Scope       string
	ProjectID   string
	ProjectName string
	Auth        string
	// HeaderNames lists the headers it sends. Values never leave the config.
	HeaderNames []string
	// Editable: AO owns its config, so its key can be changed here.
	Editable bool
	// ManageURL is where it is managed, when that is outside AO.
	ManageURL string
	Accounts  []AccountStatus
}

// AccountRef names a subscription in a list.
type AccountRef struct {
	ID     string
	Email  string
	Device bool
}

// List is the catalog.
type List struct {
	Servers  []Server
	Accounts []AccountRef
	// Checking: a check is running and the list will fill in.
	Checking bool
}

// ListInput selects how fresh the catalog must be.
type ListInput struct {
	// Refresh runs a new check even over a fresh one.
	Refresh bool
	// Wait blocks until the check finishes.
	Wait bool
	// NoCheck returns only what is known, starting nothing.
	NoCheck bool
}

// List returns every server on every subscription.
func (s *Service) List(ctx context.Context, in ListInput) (List, error) {
	if s.cli == nil {
		return List{}, ErrUnavailable
	}
	accounts, err := s.accounts.ClaudeAccounts(ctx)
	if err != nil {
		return List{}, err
	}
	projects := s.projectsWithServers(ctx)
	if !in.NoCheck {
		var started []*check
		for _, account := range accounts {
			started = append(started, s.startCheck(account, "", in.Refresh))
			for _, project := range projects {
				started = append(started, s.startCheck(account, project.record.Path, in.Refresh))
			}
		}
		if in.Wait {
			for _, c := range started {
				select {
				case <-c.done:
				case <-ctx.Done():
					return List{}, ctx.Err()
				}
			}
		}
	}
	out := List{Checking: s.anyCheckRunning()}
	for _, account := range accounts {
		out.Accounts = append(out.Accounts, AccountRef{ID: account.ID, Email: account.Email, Device: account.Device()})
	}
	out.Servers = s.render(ctx, accounts, projects)
	return out, nil
}

// SessionList is what one session's agent has.
type SessionList struct {
	Servers   []Server
	AccountID string
	// Live: a running agent reported this. Otherwise it comes from a check of
	// what the session's account and folder would have.
	Live bool
}

// SessionServers reports what a session's running agent actually has right now.
// That can differ from what config says, so it asks the agent itself. A session
// with no running agent gets a check of its account in its folder instead.
func (s *Service) SessionServers(ctx context.Context, id domain.SessionID, refresh bool) (SessionList, error) {
	if s.cli == nil {
		return SessionList{}, ErrUnavailable
	}
	session, account, err := s.sessionAccount(ctx, id)
	if err != nil {
		return SessionList{}, err
	}
	project := projectServers{record: domain.ProjectRecord{ID: session.ProjectID, Path: session.WorkspacePath}}
	project.servers, _ = s.cli.MCPProjectServers(session.WorkspacePath)
	definitions := s.definitions(account, []projectServers{project})
	if servers, liveErr := s.live.LiveMCPServers(ctx, id); liveErr == nil {
		return SessionList{Servers: renderLive(account, definitions, servers, s.now()), AccountID: account.ID, Live: true}, nil
	}
	c := s.startCheck(account, session.WorkspacePath, refresh)
	select {
	case <-c.done:
	case <-ctx.Done():
		return SessionList{}, ctx.Err()
	}
	rows := map[rowKey]*Server{}
	s.addCheckRows(rows, account, session.WorkspacePath, definitions, c)
	return SessionList{Servers: sortedRows(rows), AccountID: account.ID}, nil
}

func (s *Service) sessionAccount(ctx context.Context, id domain.SessionID) (Session, domain.AgentAccount, error) {
	session, err := s.sessions.MCPSession(ctx, id)
	if err != nil {
		return Session{}, domain.AgentAccount{}, err
	}
	account, err := s.accounts.ClaudeAccount(ctx, session.AccountID)
	if err != nil {
		return Session{}, domain.AgentAccount{}, err
	}
	return session, account, nil
}

type projectServers struct {
	record  domain.ProjectRecord
	servers map[string]rawConfig
}

// projectsWithServers reads each project's .mcp.json. Reading is cheap and it
// runs nothing. An unreadable project is skipped: it says nothing about others.
func (s *Service) projectsWithServers(ctx context.Context) []projectServers {
	if s.projects == nil {
		return nil
	}
	records, err := s.projects.ListProjects(ctx)
	if err != nil {
		s.log.Warn("list projects for MCP servers", "error", err)
		return nil
	}
	var out []projectServers
	for _, record := range records {
		servers, err := s.cli.MCPProjectServers(record.Path)
		if err != nil || len(servers) == 0 {
			continue
		}
		out = append(out, projectServers{record: record, servers: servers})
	}
	return out
}

type rowKey struct {
	name string
	repo string // the project's path, for a server its .mcp.json defines
}

func sortedRows(rows map[rowKey]*Server) []Server {
	out := make([]Server, 0, len(rows))
	for _, row := range rows {
		sort.SliceStable(row.Accounts, func(i, j int) bool { return row.Accounts[i].AccountID < row.Accounts[j].AccountID })
		out = append(out, *row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return scopeOrder(out[i].Scope) < scopeOrder(out[j].Scope)
		}
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].ProjectName < out[j].ProjectName
	})
	return out
}

func scopeOrder(scope string) int {
	switch scope {
	case ScopeGlobal:
		return 0
	case ScopeRepo:
		return 1
	case ScopeLocal:
		return 2
	case ScopeConnector:
		return 3
	case ScopePlugin:
		return 4
	default:
		return 5
	}
}

func (s *Service) render(ctx context.Context, accounts []domain.AgentAccount, projects []projectServers) []Server {
	rows := map[rowKey]*Server{}
	for _, account := range accounts {
		definitions := s.definitions(account, projects)
		s.addCheckRows(rows, account, "", definitions, s.knownCheck(account, ""))
		for _, project := range projects {
			s.addCheckRows(rows, account, project.record.Path, definitions, s.knownCheck(account, project.record.Path))
		}
	}
	// A server with no reading yet still gets a row: the list is never empty
	// just because the first check has not finished.
	for _, account := range accounts {
		for key, definition := range s.definitions(account, projects) {
			row := rowFor(rows, key, definition, "")
			if !row.hasAccount(account.ID) {
				row.Accounts = append(row.Accounts, AccountStatus{
					AccountID: account.ID, Status: StatusNotChecked, Action: "",
				})
			}
		}
	}
	return sortedRows(rows)
}

func (r *Server) hasAccount(id string) bool {
	for _, status := range r.Accounts {
		if status.AccountID == id {
			return true
		}
	}
	return false
}

// addCheckRows adds what one check read. A check in a project's folder speaks
// only for the servers that folder defines: its own check covers the rest.
func (s *Service) addCheckRows(rows map[rowKey]*Server, account domain.AgentAccount, dir string, definitions map[rowKey]definition, c *check) {
	if c == nil {
		return
	}
	c.mu.Lock()
	entries, at, failure := c.entries, c.at, c.err
	c.mu.Unlock()
	if failure != nil && len(entries) == 0 {
		// The whole check failed: say so on every server it should have covered.
		for key, definition := range definitions {
			if key.repo == dir {
				row := rowFor(rows, key, definition, "")
				row.Accounts = append(row.Accounts, AccountStatus{
					AccountID: account.ID, Status: StatusUnknown, Error: failure.Error(), CheckedAt: timePointer(at),
				})
			}
		}
		return
	}
	for _, entry := range entries {
		key, definition, ok := lookupDefinition(definitions, entry, dir)
		if !ok {
			continue
		}
		row := rowFor(rows, key, definition, entry.Transport)
		if row.hasAccount(account.ID) {
			continue
		}
		row.Accounts = append(row.Accounts, statusFor(account, definition, entry, at))
	}
}

// lookupDefinition finds the row an entry belongs to. An entry no config names
// (a connector, a plugin's server, a type AO has not heard of) still gets a row
// made from what Claude Code said about it: nothing is hidden.
func lookupDefinition(definitions map[rowKey]definition, entry checkEntry, dir string) (rowKey, definition, bool) {
	if dir != "" {
		if d, ok := definitions[rowKey{entry.Name, dir}]; ok {
			return rowKey{entry.Name, dir}, d, true
		}
		return rowKey{}, definition{}, false // covered by the global check
	}
	if d, ok := definitions[rowKey{entry.Name, ""}]; ok {
		return rowKey{entry.Name, ""}, d, true
	}
	return rowKey{entry.Name, ""}, definitionFromEntry(entry), true
}

func rowFor(rows map[rowKey]*Server, key rowKey, d definition, transport string) *Server {
	row, ok := rows[key]
	if !ok {
		if d.Transport == "" {
			d.Transport = transport
		}
		row = &Server{
			Name: d.Name, Transport: d.Transport, Target: d.Target, Scope: d.Scope, ProjectID: d.ProjectID, ProjectName: d.ProjectName,
			Auth: d.Auth, HeaderNames: d.HeaderNames, Editable: d.Editable(),
		}
		if d.Auth == AuthConnector {
			row.ManageURL = ConnectorsURL
		}
		rows[key] = row
	}
	return row
}

func statusFor(account domain.AgentAccount, d definition, entry checkEntry, at time.Time) AccountStatus {
	status := AccountStatus{
		AccountID: account.ID, Status: statusFromRaw(d.Auth, entry.Status), RawStatus: entry.Raw,
		ToolCount: entry.ToolCount, Error: entry.Error, CheckedAt: timePointer(at),
	}
	status.Action = actionFor(d, status.Status)
	return status
}

func timePointer(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

// actionFor is the generic rule for every kind of server: no special case by
// name, only by how the server authenticates and where its config lives.
func actionFor(d definition, status string) string {
	switch {
	case status == StatusNeedsApproval:
		return ActionApprove
	case status == StatusNeedsSignIn:
		return ActionConnect
	case d.Editable():
		return ActionEditKey
	case d.Auth == AuthConnector:
		return ActionManage
	}
	return ""
}

// renderLive turns what a running agent reports into rows.
func renderLive(account domain.AgentAccount, definitions map[rowKey]definition, servers []domain.ConversationMCPServer, now time.Time) []Server {
	rows := map[rowKey]*Server{}
	for _, server := range servers {
		if hiddenLiveServer(server.Name) {
			continue
		}
		entry := checkEntry{
			Name: server.Name, Status: liveStatus(server.Status), Raw: server.Status, Error: server.Error,
			ToolCount: server.ToolCount, ProviderScope: server.Scope,
		}
		key := rowKey{server.Name, ""}
		d, ok := definitions[key]
		if !ok {
			for candidate, repo := range definitions {
				if candidate.name == server.Name && candidate.repo != "" {
					key, d, ok = candidate, repo, true
					break
				}
			}
		}
		if !ok {
			d = definitionFromEntry(entry)
		}
		row := rowFor(rows, key, d, "")
		status := statusFor(account, d, entry, now)
		status.Live = true
		row.Accounts = append(row.Accounts, status)
	}
	return sortedRows(rows)
}

// hiddenLiveServer names AO's own in-process tool servers, which are plumbing a
// user has nothing to do with.
func hiddenLiveServer(name string) bool {
	return name == "ao" || name == "claude_agent_acp"
}

func liveStatus(status string) string {
	switch status {
	case ports.ChatMCPStatusReady:
		return rawConnected
	case ports.ChatMCPStatusStarting:
		return rawPending
	case ports.ChatMCPStatusNeedsAuth:
		return rawNeedsAuth
	default:
		return status
	}
}
