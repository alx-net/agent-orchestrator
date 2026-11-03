package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	deviceID  = "device-claude"
	accountID = "acct-1"
	repoDir   = "/repos/app"
	worktree  = "/worktrees/app-1"
)

// fakeCLI stands in for Claude Code. Every call is recorded with its target, so
// a test can see the right account folder and directory were used.
type fakeCLI struct {
	mu sync.Mutex

	user    map[string]map[string]json.RawMessage // by account home
	project map[string]map[string]json.RawMessage // by directory
	probes  map[string][]ports.MCPProbeServer     // by home + "|" + dir
	lists   map[string]string                     // by home + "|" + dir
	probeE  error

	loginOutput string
	loginErr    error
	loginBlock  bool

	commandResult ports.MCPCommandResult
	commandErr    error

	logins   []ports.MCPTarget
	commands []fakeCommand
	probed   []fakeProbe
	approved []string
	trusted  []string
	forgot   []string
}

type fakeCommand struct {
	target ports.MCPTarget
	args   []string
}

type fakeProbe struct {
	target  ports.MCPTarget
	request ports.MCPProbeRequest
}

func newFakeCLI() *fakeCLI {
	return &fakeCLI{
		user:    map[string]map[string]json.RawMessage{},
		project: map[string]map[string]json.RawMessage{},
		probes:  map[string][]ports.MCPProbeServer{},
		lists:   map[string]string{},
	}
}

func (f *fakeCLI) MCPCommand(_ context.Context, target ports.MCPTarget, args ...string) (ports.MCPCommandResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, fakeCommand{target, args})
	if len(args) >= 2 && args[0] == "mcp" && args[1] == "list" {
		return ports.MCPCommandResult{Stdout: f.lists[target.Home+"|"+target.Dir]}, nil
	}
	return f.commandResult, f.commandErr
}

func (f *fakeCLI) MCPProbe(_ context.Context, target ports.MCPTarget, request ports.MCPProbeRequest) ([]ports.MCPProbeServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, fakeProbe{target, request})
	if f.probeE != nil {
		return nil, f.probeE
	}
	return f.probes[target.Home+"|"+target.Dir], nil
}

func (f *fakeCLI) MCPLogin(ctx context.Context, target ports.MCPTarget, _ string, output func(string)) error {
	f.mu.Lock()
	f.logins = append(f.logins, target)
	text, err, block := f.loginOutput, f.loginErr, f.loginBlock
	f.mu.Unlock()
	if text != "" {
		output(text)
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeCLI) MCPUserServers(home string) (map[string]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user[home], nil
}

func (f *fakeCLI) MCPProjectServers(dir string) (map[string]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.project[dir], nil
}

func (f *fakeCLI) MCPApprove(home, dir, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, home+"|"+dir+"|"+name)
	return nil
}

func (f *fakeCLI) MCPTrust(home, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trusted = append(f.trusted, home+"|"+dir)
	return nil
}

func (f *fakeCLI) MCPForgetAuthCache(home string, names ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgot = append(f.forgot, home+"|"+strings.Join(names, ","))
	return nil
}

func (f *fakeCLI) forgotten() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forgot...)
}

func (f *fakeCLI) calls() []fakeCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCommand(nil), f.commands...)
}

type fakeAccounts struct {
	accounts []domain.AgentAccount
	prepared map[string]int
	mu       sync.Mutex
}

func (f *fakeAccounts) ClaudeAccounts(context.Context) ([]domain.AgentAccount, error) {
	return f.accounts, nil
}

func (f *fakeAccounts) ClaudeAccount(_ context.Context, id string) (domain.AgentAccount, error) {
	for _, account := range f.accounts {
		if account.ID == id {
			return account, nil
		}
	}
	return domain.AgentAccount{}, ports.ErrAgentAccountUnknown
}

func (f *fakeAccounts) PrepareClaudeAccount(_ context.Context, account domain.AgentAccount) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared[account.ID]++
	return nil
}

type fakeSessions struct {
	sessions map[domain.SessionID]Session
	running  []Session
}

func (f *fakeSessions) MCPSession(_ context.Context, id domain.SessionID) (Session, error) {
	session, ok := f.sessions[id]
	if !ok {
		return Session{}, ErrUnknownSession
	}
	return session, nil
}

func (f *fakeSessions) MCPRunningSessions(context.Context) ([]Session, error) {
	return f.running, nil
}

// fakeLive is a running agent: what it lists, and what it was asked to change.
type fakeLive struct {
	mu         sync.Mutex
	servers    map[domain.SessionID][]domain.ConversationMCPServer
	afterSet   map[domain.SessionID][]domain.ConversationMCPServer
	failures   map[string]string
	err        error
	reconnects []string
	applied    []ports.ChatMCPChange
}

func (f *fakeLive) LiveMCPServers(_ context.Context, id domain.SessionID) ([]domain.ConversationMCPServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.servers[id], nil
}

func (f *fakeLive) ReconnectMCPServer(_ context.Context, id domain.SessionID, name string) ([]domain.ConversationMCPServer, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, "", f.err
	}
	f.reconnects = append(f.reconnects, string(id)+":"+name)
	return f.servers[id], "", nil
}

func (f *fakeLive) ApplyMCPServers(_ context.Context, id domain.SessionID, change ports.ChatMCPChange) ([]domain.ConversationMCPServer, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, nil, f.err
	}
	f.applied = append(f.applied, change)
	if next, ok := f.afterSet[id]; ok {
		f.servers[id] = next
	}
	return f.servers[id], f.failures, nil
}

type fakeRestarter struct {
	mu    sync.Mutex
	calls int
	busy  int // how many first calls report busy
}

func (f *fakeRestarter) RestartAgentForToolServers(context.Context, domain.SessionID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.calls > f.busy, nil
}

func (f *fakeRestarter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeProjects struct{ records []domain.ProjectRecord }

func (f *fakeProjects) ListProjects(context.Context) ([]domain.ProjectRecord, error) {
	return f.records, nil
}

type rig struct {
	svc      *Service
	cli      *fakeCLI
	accounts *fakeAccounts
	sessions *fakeSessions
	live     *fakeLive
	restart  *fakeRestarter
	projects *fakeProjects
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &rig{
		cli: newFakeCLI(),
		accounts: &fakeAccounts{prepared: map[string]int{}, accounts: []domain.AgentAccount{
			{ID: deviceID, Provider: domain.AgentAccountProviderClaude},
			{ID: accountID, Provider: domain.AgentAccountProviderClaude, Home: "/acct", Email: "a@example.test"},
		}},
		sessions: &fakeSessions{sessions: map[domain.SessionID]Session{
			"s1": {ID: "s1", AccountID: accountID, WorkspacePath: worktree, ProjectID: "app", Chat: true},
		}},
		live:     &fakeLive{servers: map[domain.SessionID][]domain.ConversationMCPServer{}, afterSet: map[domain.SessionID][]domain.ConversationMCPServer{}},
		restart:  &fakeRestarter{},
		projects: &fakeProjects{},
	}
	r.svc = New(Deps{
		Context: ctx, CLI: r.cli, Accounts: r.accounts, Sessions: r.sessions, Live: r.live, Restarter: r.restart, Projects: r.projects,
	})
	r.svc.restartPoll = time.Millisecond
	r.svc.externalPoll = time.Millisecond
	return r
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func intp(n int) *int { return &n }

func listNow(t *testing.T, s *Service) List {
	t.Helper()
	list, err := s.List(context.Background(), ListInput{Wait: true})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func findServer(t *testing.T, servers []Server, name string) Server {
	t.Helper()
	for _, server := range servers {
		if server.Name == name {
			return server
		}
	}
	t.Fatalf("no server %q in %+v", name, servers)
	return Server{}
}

func statusOn(t *testing.T, server Server, account string) AccountStatus {
	t.Helper()
	for _, status := range server.Accounts {
		if status.AccountID == account {
			return status
		}
	}
	t.Fatalf("server %s has no status for %s: %+v", server.Name, account, server.Accounts)
	return AccountStatus{}
}

// Every kind of server gets the action its authentication calls for, by shape.
func TestListClassifiesEveryKindOfServer(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{
		"oauth":  raw(`{"type":"http","url":"https://oauth.test/mcp?token=hidden"}`),
		"keyed":  raw(`{"type":"http","url":"https://key.test/mcp","headers":{"X-Api-Key":"s3cret"}}`),
		"helper": raw(`{"type":"http","url":"https://helper.test/mcp","headersHelper":"/bin/helper"}`),
		"local":  raw(`{"type":"stdio","command":"npx","args":["-y","pkg","--token","s3cret"]}`),
	}
	r.cli.probes["|"] = []ports.MCPProbeServer{
		{Name: "oauth", Status: "needs-auth", Scope: "user", Transport: "http"},
		{Name: "keyed", Status: "connected", Scope: "user", Transport: "http", ToolCount: intp(7)},
		{Name: "helper", Status: "needs-auth", Scope: "user", Transport: "http"},
		{Name: "local", Status: "failed", Scope: "user", Transport: "stdio", Error: "spawn npx ENOENT"},
		{Name: "claude.ai Gmail", Status: "needs-auth", Scope: "claudeai", URL: "https://gmail.test/mcp"},
		{Name: "strange", Status: "hibernating", Scope: "future", Transport: "carrier-pigeon"},
	}
	list := listNow(t, r.svc)

	oauth := statusOn(t, findServer(t, list.Servers, "oauth"), deviceID)
	if oauth.Status != StatusNeedsSignIn || oauth.Action != ActionConnect || oauth.CheckedAt == nil {
		t.Fatalf("oauth = %+v", oauth)
	}
	if server := findServer(t, list.Servers, "oauth"); server.Auth != AuthOAuth || strings.Contains(server.Target, "hidden") {
		t.Fatalf("oauth row = %+v; a query string can hold a credential", server)
	}

	keyed := findServer(t, list.Servers, "keyed")
	status := statusOn(t, keyed, deviceID)
	if keyed.Auth != AuthAPIKey || !keyed.Editable || status.Status != StatusConnected || status.Action != ActionEditKey || *status.ToolCount != 7 {
		t.Fatalf("keyed = %+v, %+v", keyed, status)
	}
	if !reflect.DeepEqual(keyed.HeaderNames, []string{"X-Api-Key"}) {
		t.Fatalf("header names = %v", keyed.HeaderNames)
	}

	// A helper's 401 is not something a sign-in fixes, and AO does not own its key.
	helper := findServer(t, list.Servers, "helper")
	status = statusOn(t, helper, deviceID)
	if helper.Auth != AuthHelper || helper.Editable || status.Status != StatusKeyRejected || status.Action != "" {
		t.Fatalf("helper = %+v, %+v", helper, status)
	}

	local := findServer(t, list.Servers, "local")
	status = statusOn(t, local, deviceID)
	if local.Auth != AuthNone || status.Status != StatusFailed || status.Error != "spawn npx ENOENT" || status.Action != "" {
		t.Fatalf("local = %+v, %+v", local, status)
	}
	if strings.Contains(local.Target, "s3cret") {
		t.Fatalf("target leaks a secret: %q", local.Target)
	}

	connector := findServer(t, list.Servers, "claude.ai Gmail")
	status = statusOn(t, connector, deviceID)
	if connector.Scope != ScopeConnector || connector.ManageURL != ConnectorsURL || status.Action != ActionConnect {
		t.Fatalf("connector = %+v, %+v", connector, status)
	}

	// A kind of server nobody has heard of is listed, with its raw status.
	strange := findServer(t, list.Servers, "strange")
	status = statusOn(t, strange, deviceID)
	if status.Status != StatusUnknown || status.RawStatus != "hibernating" || strange.Transport != "carrier-pigeon" {
		t.Fatalf("strange = %+v, %+v", strange, status)
	}
}

func TestListGivesEachSubscriptionItsOwnStatus(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"linear": raw(`{"type":"http","url":"https://mcp.linear.app/mcp"}`)}
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "linear", Status: "needs-auth", Scope: "user"}}
	r.cli.probes["/acct|"] = []ports.MCPProbeServer{{Name: "linear", Status: "connected", Scope: "user", ToolCount: intp(30)}}
	list := listNow(t, r.svc)
	linear := findServer(t, list.Servers, "linear")
	if got := statusOn(t, linear, deviceID); got.Status != StatusNeedsSignIn {
		t.Fatalf("device = %+v", got)
	}
	if got := statusOn(t, linear, accountID); got.Status != StatusConnected || got.Action != "" || *got.ToolCount != 30 {
		t.Fatalf("account = %+v", got)
	}
	if len(list.Accounts) != 2 || !list.Accounts[0].Device || list.Accounts[1].Email != "a@example.test" {
		t.Fatalf("accounts = %+v", list.Accounts)
	}
	if r.accounts.prepared[accountID] == 0 {
		t.Fatal("the account's folder was not given the user's servers before the check")
	}
}

// A status view must never run a command from a .mcp.json nobody approved, and
// only the approved servers get connected.
func TestProjectCheckNeverStartsAnUnapprovedServer(t *testing.T) {
	r := newRig(t)
	r.projects.records = []domain.ProjectRecord{{ID: "app", Path: repoDir, DisplayName: "App"}}
	r.cli.project[repoDir] = map[string]json.RawMessage{
		"pending":  raw(`{"type":"stdio","command":"evil"}`),
		"approved": raw(`{"type":"http","url":"https://approved.test/mcp"}`),
	}
	r.cli.lists["|"+repoDir] = "Checking MCP server health…\n\n" +
		"pending: evil  - ⏸ Pending approval (run `claude` to approve)\n" +
		"approved: https://approved.test/mcp (HTTP) - ✔ Connected\n"
	r.cli.probes["|"+repoDir] = []ports.MCPProbeServer{{Name: "approved", Status: "connected", Scope: "dynamic", ToolCount: intp(2)}}

	list := listNow(t, r.svc)
	pending := findServer(t, list.Servers, "pending")
	status := statusOn(t, pending, deviceID)
	if pending.Scope != ScopeRepo || pending.ProjectName != "App" || status.Status != StatusNeedsApproval || status.Action != ActionApprove {
		t.Fatalf("pending = %+v, %+v", pending, status)
	}
	if got := statusOn(t, findServer(t, list.Servers, "approved"), deviceID); got.Status != StatusConnected || *got.ToolCount != 2 {
		t.Fatalf("approved = %+v", got)
	}
	for _, probe := range r.cli.probed {
		if probe.target.Dir != repoDir {
			continue
		}
		if _, startedIt := probe.request.Servers["pending"]; startedIt || probe.request.Servers == nil {
			t.Fatalf("a probe could start the unapproved server: %+v", probe.request)
		}
	}
}

func TestListFallsBackToTheCLIListWhenTheProbeFails(t *testing.T) {
	r := newRig(t)
	r.cli.probeE = errors.New("control protocol gone")
	r.cli.lists["|"] = "linear: https://mcp.linear.app/mcp (HTTP) - ! Needs authentication\n"
	r.cli.user[""] = map[string]json.RawMessage{"linear": raw(`{"type":"http","url":"https://mcp.linear.app/mcp"}`)}
	list := listNow(t, r.svc)
	if got := statusOn(t, findServer(t, list.Servers, "linear"), deviceID); got.Status != StatusNeedsSignIn {
		t.Fatalf("status = %+v", got)
	}
}

// The measured `claude mcp list` output of 2.1.314, each line kind.
func TestParseListReadsEveryLineKind(t *testing.T) {
	output := strings.Join([]string{
		"Checking MCP server health…",
		"",
		"mock: http://127.0.0.1:39871/mcp (HTTP) - ✔ Connected",
		"oauth: https://x.test/mcp (SSE) - ! Needs authentication",
		"stdiotool: /bin/cat -u - ✘ Failed to connect — -32601: Method not found",
		"dash-args: npx -y some - pkg - ✘ Failed to connect — exit - 1",
		"repo-server: http://127.0.0.1:39871/mcp (HTTP) - ⏸ Pending approval (run `claude` to approve)",
		"gone: http://127.0.0.1/mcp (HTTP) - ✘ Rejected (see disabledMcpjsonServers in settings)",
		"off: http://127.0.0.1/mcp (HTTP) - ⊘ Disabled for this project (re-enable via /mcp)",
		"claude.ai Gmail: https://gmail.test/mcp - ! Needs authentication",
		"plugin:acme:server: node a.js - ✔ Connected",
		"future: https://x.test (HTTP) - ★ Quantum entangled",
	}, "\n")
	got := map[string]checkEntry{}
	for _, entry := range parseList(output) {
		got[entry.Name] = entry
	}
	want := map[string]struct{ status, detail, transport string }{
		"mock":               {rawConnected, "", "http"},
		"oauth":              {rawNeedsAuth, "", "sse"},
		"stdiotool":          {rawFailed, "-32601: Method not found", "stdio"},
		"dash-args":          {rawFailed, "exit - 1", "stdio"},
		"repo-server":        {rawApproval, "", "http"},
		"gone":               {rawRejected, "", "http"},
		"off":                {rawDisabled, "", "http"},
		"claude.ai Gmail":    {rawNeedsAuth, "", ""},
		"plugin:acme:server": {rawConnected, "", "stdio"},
		"future":             {"Quantum entangled", "", "http"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for name, expected := range want {
		entry := got[name]
		if entry.Status != expected.status || entry.Error != expected.detail || entry.Transport != expected.transport {
			t.Errorf("%s = %+v, want %+v", name, entry, expected)
		}
	}
	if statusFromRaw(AuthOAuth, got["future"].Status) != StatusUnknown || got["future"].Raw != "Quantum entangled" {
		t.Fatalf("a state AO has no word for must stay visible: %+v", got["future"])
	}
}

func waitForState(t *testing.T, s *Service, id string, states ...string) Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := s.GetOperation(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, state := range states {
			if op.State == state {
				return op
			}
		}
		time.Sleep(time.Millisecond)
	}
	op, _ := s.GetOperation(id)
	t.Fatalf("operation stayed %+v, want %v", op, states)
	return op
}

func TestConnectUsesTheSessionsAccountAndFolderAndReachesItsAgent(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"linear": raw(`{"type":"http","url":"https://mcp.linear.app/mcp"}`)}
	r.cli.loginOutput = "Starting authentication for \"linear\"…\r\nIf the browser didn't open, visit:\r\n  \x1b]8;;https://auth.test/a?state=x\x07\x1b[94mhttps://auth.test/a?state=x\x1b[0m\x1b]8;;\x07\r\n\r\nWaiting for authorization… (^C to cancel)\r\n"
	session := r.sessions.sessions["s1"]
	r.sessions.running = []Session{session, {ID: "other-account", AccountID: deviceID, Chat: true}}
	r.live.servers["s1"] = []domain.ConversationMCPServer{{Name: "linear", Status: ports.ChatMCPStatusReady}}

	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "linear", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if started.AccountID != accountID || started.State != OpWaiting {
		t.Fatalf("started = %+v", started)
	}
	op := waitForState(t, r.svc, started.ID, OpConnected)
	if !reflect.DeepEqual(r.cli.logins, []ports.MCPTarget{{Home: "/acct", Dir: worktree}}) {
		t.Fatalf("login ran at %+v", r.cli.logins)
	}
	if op.AuthURL != "https://auth.test/a?state=x" {
		t.Fatalf("auth URL = %q", op.AuthURL)
	}
	if !reflect.DeepEqual(op.Applied.InPlace, []string{"s1"}) || len(op.Applied.Restarting) != 0 {
		t.Fatalf("applied = %+v", op.Applied)
	}
	// Only sessions on the account that signed in are touched.
	if !reflect.DeepEqual(r.live.reconnects, []string{"s1:linear"}) {
		t.Fatalf("reconnects = %v", r.live.reconnects)
	}
	if r.restart.count() != 0 {
		t.Fatal("a sign-in that reconnected in place must not restart anything")
	}
}

func TestConnectWithNoSessionUsesThatAccountAndAThrowawayFolder(t *testing.T) {
	r := newRig(t)
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "linear", AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, r.svc, started.ID, OpConnected)
	if !reflect.DeepEqual(r.cli.logins, []ports.MCPTarget{{Home: "/acct"}}) {
		t.Fatalf("login ran at %+v", r.cli.logins)
	}
}

func TestConnectReportsClaudeCodesOwnWordsOnFailure(t *testing.T) {
	r := newRig(t)
	r.cli.loginOutput = "Starting authentication for \"x\"…\r\n\x1b[31mCouldn't complete authentication for \"x\": the server said no\x1b[39m\r\n"
	r.cli.loginErr = errors.New("exit status 1")
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	op := waitForState(t, r.svc, started.ID, OpFailed)
	if op.Error != `Couldn't complete authentication for "x": the server said no` || op.FinishedAt == nil {
		t.Fatalf("op = %+v", op)
	}
}

func TestConnectTimesOut(t *testing.T) {
	r := newRig(t)
	r.svc.loginLimit = 10 * time.Millisecond
	r.cli.loginBlock = true
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	op := waitForState(t, r.svc, started.ID, OpTimedOut)
	if !strings.Contains(op.Error, "not finished") {
		t.Fatalf("op = %+v", op)
	}
}

func TestConnectCanBeCancelledAndADoubleClickOpensOneBrowser(t *testing.T) {
	r := newRig(t)
	r.cli.loginBlock = true
	first, err := r.svc.Connect(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.svc.Connect(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("a second click started another sign-in: %s, %s", first.ID, second.ID)
	}
	if _, err := r.svc.CancelOperation(first.ID); err != nil {
		t.Fatal(err)
	}
	waitForState(t, r.svc, first.ID, OpCancelled)
	if _, err := r.svc.GetOperation("no-such"); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectRefusesAServerWithNoSignInToDo(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{
		"keyed":  raw(`{"type":"http","url":"https://k.test","headers":{"X-Api-Key":"s"}}`),
		"helper": raw(`{"type":"http","url":"https://h.test","headersHelper":"/bin/h"}`),
		"local":  raw(`{"type":"stdio","command":"cat"}`),
	}
	for _, name := range []string{"keyed", "helper", "local"} {
		if _, err := r.svc.Connect(context.Background(), TargetInput{Name: name}); !errors.Is(err, ErrNoSignIn) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if len(r.cli.logins) != 0 {
		t.Fatalf("a sign-in ran: %+v", r.cli.logins)
	}
	if _, err := r.svc.Connect(context.Background(), TargetInput{Name: "--evil"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a name that reads as a flag: err = %v", err)
	}
}

func TestConnectApprovesARepoServerFirst(t *testing.T) {
	r := newRig(t)
	r.cli.project[worktree] = map[string]json.RawMessage{"posthog": raw(`{"type":"http","url":"https://p.test"}`)}
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "posthog", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, r.svc, started.ID, OpConnected)
	if !reflect.DeepEqual(r.cli.approved, []string{"/acct|" + worktree + "|posthog"}) {
		t.Fatalf("approved = %v", r.cli.approved)
	}
}

// claude.ai handles the sign-in and the CLI exits at once: AO must not call that
// connected, and watches until the connector really is.
func TestConnectorSignInWaitsForClaudeAI(t *testing.T) {
	r := newRig(t)
	r.cli.loginOutput = "Opening browser to authorize \"claude.ai Gmail\"…\nIf the browser didn't open, visit:\n  https://claude.ai/connect\n\nOnce authorized on claude.ai, the connector will be available the next time you start Claude Code.\n"
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "claude.ai Gmail", Status: "needs-auth", Scope: "claudeai"}}
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "claude.ai Gmail"})
	if err != nil {
		t.Fatal(err)
	}
	op := waitForState(t, r.svc, started.ID, OpExternal)
	if op.ManageURL != ConnectorsURL || op.AuthURL != "https://claude.ai/connect" {
		t.Fatalf("op = %+v", op)
	}
	r.cli.mu.Lock()
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "claude.ai Gmail", Status: "connected", Scope: "claudeai", ToolCount: intp(9)}}
	r.cli.mu.Unlock()
	waitForState(t, r.svc, started.ID, OpConnected)
}

func TestTestPassesWithToolsFromARealConnectOfOnlyThatServer(t *testing.T) {
	r := newRig(t)
	config := raw(`{"type":"http","url":"https://x.test"}`)
	r.cli.user[""] = map[string]json.RawMessage{"x": config, "other": raw(`{"type":"stdio","command":"cat"}`)}
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "x", Status: "connected", ToolCount: intp(2), Tools: []string{"a", "b"}}}
	result, err := r.svc.Test(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Status != StatusConnected || result.Reason != "Connected. It exposes 2 tools." || !reflect.DeepEqual(result.Tools, []string{"a", "b"}) {
		t.Fatalf("result = %+v", result)
	}
	probe := r.cli.probed[len(r.cli.probed)-1]
	if len(probe.request.Servers) != 1 || string(probe.request.Servers["x"]) != string(config) {
		t.Fatalf("probe = %+v; a test must start only its server", probe.request)
	}
	// The list shows what the test found.
	list, err := r.svc.List(context.Background(), ListInput{NoCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := statusOn(t, findServer(t, list.Servers, "x"), deviceID); got.Status != StatusConnected || *got.ToolCount != 2 {
		t.Fatalf("list after test = %+v", got)
	}
}

// The CLI answers needs-auth from memory while it remembers a refusal, which
// would make a just-fixed key read as refused: measured on 2.1.314.
func TestChecksAndTestsReallyConnectRatherThanTrustTheCLIsMemory(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "x", Status: "connected"}}
	if _, err := r.svc.Test(context.Background(), TargetInput{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.cli.forgotten(), []string{"|x"}) {
		t.Fatalf("forgot = %v", r.cli.forgotten())
	}
	listNow(t, r.svc)
	got := strings.Join(r.cli.forgotten(), " ")
	if !strings.Contains(got, "/acct|") {
		t.Fatalf("a check of the account did not clear its memory: %v", r.cli.forgotten())
	}
}

func TestSetKeyClearsEveryAccountsMemoryOfTheRefusal(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x","headers":{"K":"old"}}`)}
	if _, err := r.svc.SetKey(context.Background(), "x", map[string]string{"K": "new"}); err != nil {
		t.Fatal(err)
	}
	forgot := r.cli.forgotten()
	if !reflect.DeepEqual(forgot, []string{"|x", "/acct|x"}) {
		t.Fatalf("forgot = %v", forgot)
	}
}

func TestTestExplainsEachFailureInPlainWords(t *testing.T) {
	for _, tc := range []struct {
		name, config, status, errorText string
		wantStatus, wantReason          string
	}{
		{"oauth", `{"type":"http","url":"https://x"}`, "needs-auth", "", StatusNeedsSignIn, "ao mcp login oauth"},
		{"keyed", `{"type":"http","url":"https://x","headers":{"X":"s"}}`, "needs-auth", "", StatusKeyRejected, "refused the key"},
		{"local", `{"type":"stdio","command":"cat"}`, "failed", "spawn cat ENOENT", StatusFailed, "spawn cat ENOENT"},
		{"slow", `{"type":"stdio","command":"cat"}`, "pending", "", StatusStarting, "still starting"},
		{"future", `{"type":"http","url":"https://x"}`, "hibernating", "", StatusUnknown, "hibernating"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.cli.user[""] = map[string]json.RawMessage{tc.name: raw(tc.config)}
			r.cli.probes["|"] = []ports.MCPProbeServer{{Name: tc.name, Status: tc.status, Error: tc.errorText}}
			result, err := r.svc.Test(context.Background(), TargetInput{Name: tc.name})
			if err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Status != tc.wantStatus || !strings.Contains(result.Reason, tc.wantReason) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestTestOfAConnectorChecksEverythingAndNamesIt(t *testing.T) {
	r := newRig(t)
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "claude.ai Drive", Status: "connected", Scope: "claudeai", ToolCount: intp(5)}}
	result, err := r.svc.Test(context.Background(), TargetInput{Name: "claude.ai Drive"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Auth != AuthConnector {
		t.Fatalf("result = %+v", result)
	}
	probe := r.cli.probed[len(r.cli.probed)-1]
	if probe.request.Servers != nil || probe.request.Reconnect != "claude.ai Drive" {
		t.Fatalf("probe = %+v", probe.request)
	}
	if _, err := r.svc.Test(context.Background(), TargetInput{Name: "nope"}); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("unknown server: err = %v", err)
	}
}

// A check that says the server works, against an agent that lost it, is exactly
// when a reconnect helps.
func TestTestForASessionComparesAndHealsItsAgent(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	r.cli.probes["/acct|"+worktree] = []ports.MCPProbeServer{{Name: "x", Status: "connected", ToolCount: intp(1)}}
	r.live.servers["s1"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusNeedsAuth}}
	result, err := r.svc.Test(context.Background(), TargetInput{Name: "x", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Live == nil {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(r.live.reconnects, []string{"s1:x"}) {
		t.Fatalf("reconnects = %v", r.live.reconnects)
	}
}

func TestApproveWritesOnlyThatAccountsChoiceAndReachesItsAgents(t *testing.T) {
	r := newRig(t)
	r.projects.records = []domain.ProjectRecord{{ID: "app", Path: repoDir}}
	config := raw(`{"type":"http","url":"https://p.test"}`)
	r.cli.project[repoDir] = map[string]json.RawMessage{"posthog": config}
	r.sessions.running = []Session{
		{ID: "same", AccountID: accountID, ProjectID: "app", Chat: true},
		{ID: "other-project", AccountID: accountID, ProjectID: "else", Chat: true},
		{ID: "other-account", AccountID: deviceID, ProjectID: "app", Chat: true},
	}
	r.live.servers["same"] = []domain.ConversationMCPServer{{Name: "posthog", Status: ports.ChatMCPStatusReady}}
	applied, err := r.svc.Approve(context.Background(), TargetInput{Name: "posthog", AccountID: accountID, ProjectID: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.cli.approved, []string{"/acct|" + repoDir + "|posthog"}) {
		t.Fatalf("approved = %v", r.cli.approved)
	}
	if !reflect.DeepEqual(applied.InPlace, []string{"same"}) || !reflect.DeepEqual(r.live.reconnects, []string{"same:posthog"}) {
		t.Fatalf("applied = %+v, reconnects = %v", applied, r.live.reconnects)
	}
	if _, err := r.svc.Approve(context.Background(), TargetInput{Name: "posthog", AccountID: accountID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approve with no project: err = %v", err)
	}
}

func commandsWith(cli *fakeCLI, verb string) []fakeCommand {
	var out []fakeCommand
	for _, call := range cli.calls() {
		if len(call.args) > 1 && call.args[1] == verb {
			out = append(out, call)
		}
	}
	return out
}

func TestAddUsesTheComputersConfigAndReachesEveryRunningAgent(t *testing.T) {
	r := newRig(t)
	r.sessions.running = []Session{{ID: "a", AccountID: accountID, Chat: true}, {ID: "b", AccountID: deviceID, Chat: true}}
	r.live.afterSet["a"] = []domain.ConversationMCPServer{{Name: "acme", Status: ports.ChatMCPStatusReady}}
	r.live.afterSet["b"] = []domain.ConversationMCPServer{{Name: "acme", Status: ports.ChatMCPStatusReady}}
	applied, err := r.svc.Add(context.Background(), AddInput{Name: "acme", URL: "https://acme.test/mcp", Headers: map[string]string{"X-Api-Key": "s3cret"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := commandsWith(r.cli, "add-json")
	if len(calls) != 1 || calls[0].target.Home != "" {
		t.Fatalf("add-json calls = %+v", calls)
	}
	if !reflect.DeepEqual(calls[0].args[:6], []string{"mcp", "add-json", "--scope", "user", "--", "acme"}) {
		t.Fatalf("args = %v", calls[0].args[:6])
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(calls[0].args[6]), &config); err != nil {
		t.Fatal(err)
	}
	if config["type"] != "http" || config["url"] != "https://acme.test/mcp" {
		t.Fatalf("config = %v", config)
	}
	if len(applied.InPlace) != 2 || r.restart.count() != 0 {
		t.Fatalf("applied = %+v", applied)
	}
	if r.accounts.prepared[accountID] == 0 {
		t.Fatal("the new server was not shared into the account folder")
	}
}

func TestAddRefusesToOverwriteAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"linear": raw(`{"type":"http","url":"https://x"}`)}
	if _, err := r.svc.Add(context.Background(), AddInput{Name: "linear", URL: "https://y"}); !errors.Is(err, ErrServerExists) {
		t.Fatalf("err = %v", err)
	}
	if calls := commandsWith(r.cli, "add-json"); len(calls) != 0 {
		t.Fatalf("add-json ran: %+v", calls)
	}
	// The CLI's own refusal counts too, for a name AO could not see.
	r.cli.commandResult = ports.MCPCommandResult{ExitCode: 1, Stderr: "MCP server new already exists in user config"}
	if _, err := r.svc.Add(context.Background(), AddInput{Name: "new", URL: "https://y"}); !errors.Is(err, ErrServerExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestAddValidatesWhatItIsGiven(t *testing.T) {
	r := newRig(t)
	for _, in := range []AddInput{
		{Name: "", URL: "https://x"},
		{Name: "bad name", URL: "https://x"},
		{Name: "x"},
		{Name: "x", URL: "ftp://x"},
		{Name: "x", URL: "https://x", Command: "cat"},
		{Name: "x", URL: "https://x", Transport: "smoke"},
		{Name: "x", Command: "cat", Headers: map[string]string{"A": "b"}},
	} {
		if _, err := r.svc.Add(context.Background(), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: err = %v", in, err)
		}
	}
	if calls := commandsWith(r.cli, "add-json"); len(calls) != 0 {
		t.Fatalf("add-json ran: %+v", calls)
	}
}

// A CLI message that quotes its input must not carry a secret to the caller.
func TestAddNeverEchoesAHeaderValue(t *testing.T) {
	r := newRig(t)
	r.cli.commandResult = ports.MCPCommandResult{ExitCode: 1, Stderr: "Invalid config: Authorization: Bearer s3cret-value"}
	_, err := r.svc.Add(context.Background(), AddInput{Name: "x", URL: "https://x", Headers: map[string]string{"Authorization": "Bearer s3cret-value"}})
	if err == nil || strings.Contains(err.Error(), "s3cret-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveUsesTheComputersConfigAndOnlyGlobalServers(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	if _, err := r.svc.Remove(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	calls := commandsWith(r.cli, "remove")
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, []string{"mcp", "remove", "--scope", "user", "--", "x"}) {
		t.Fatalf("calls = %+v", calls)
	}
	if _, err := r.svc.Remove(context.Background(), "in-a-repo"); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v", err)
	}
}

// Measured on 2.1.314: a server loaded from config stays after removal and
// reconnect, so only a restart drops it. One added by setMcpServers just goes.
func TestRemoveRestartsOnlyAgentsThatStillHaveTheServer(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	r.sessions.running = []Session{{ID: "from-config", AccountID: accountID, Chat: true}, {ID: "dynamic", AccountID: deviceID, Chat: true}}
	r.live.servers["from-config"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusReady}}
	r.live.servers["dynamic"] = []domain.ConversationMCPServer{}
	applied, err := r.svc.Remove(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied.Restarting, []string{"from-config"}) || !reflect.DeepEqual(applied.InPlace, []string{"dynamic"}) {
		t.Fatalf("applied = %+v", applied)
	}
	awaitRestarts(t, r, 1)
}

func awaitRestarts(t *testing.T, r *rig, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.restart.count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("restarts = %d, want %d", r.restart.count(), want)
}

// Never mid-turn: a busy agent is asked again until it restarts, and only once.
func TestARestartWaitsForTheAgentToBeIdle(t *testing.T) {
	r := newRig(t)
	r.restart.busy = 3
	r.svc.scheduleRestart("s1")
	r.svc.scheduleRestart("s1") // a second change while it waits changes nothing
	awaitRestarts(t, r, 4)
	time.Sleep(20 * time.Millisecond)
	if got := r.restart.count(); got != 4 {
		t.Fatalf("restart asked %d times; it should stop once done", got)
	}
}

// An agent starting next must not read a key that works as refused because the
// agent it replaces wrote its old refusal back.
func TestARestartClearsTheCLIsMemoryFirst(t *testing.T) {
	r := newRig(t)
	r.svc.scheduleRestart("s1")
	awaitRestarts(t, r, 1)
	if got := r.cli.forgotten(); len(got) == 0 || got[0] != "/acct|" {
		t.Fatalf("forgot = %v", got)
	}
}

func TestAgentsThatCannotChangeInPlaceRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session Session
		liveErr error
	}{
		{"terminal agent", Session{ID: "tui", AccountID: deviceID, Chat: false}, nil},
		{"bridge without control", Session{ID: "old", AccountID: deviceID, Chat: true}, ErrInPlaceUnsupported},
		{"provider not answering", Session{ID: "stuck", AccountID: deviceID, Chat: true}, errors.New("no reply")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.sessions.running = []Session{tc.session}
			r.live.err = tc.liveErr
			applied, err := r.svc.Add(context.Background(), AddInput{Name: "x", URL: "https://x"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(applied.Restarting, []string{string(tc.session.ID)}) {
				t.Fatalf("applied = %+v", applied)
			}
			awaitRestarts(t, r, 1)
		})
	}
}

// A session with no agent running needs nothing: it reads current servers at start.
func TestAStoppedAgentIsLeftForItsNextStart(t *testing.T) {
	r := newRig(t)
	r.sessions.running = []Session{{ID: "gone", AccountID: deviceID, Chat: true}}
	r.live.err = ErrNotLive
	applied, err := r.svc.Add(context.Background(), AddInput{Name: "x", URL: "https://x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.InPlace)+len(applied.Restarting) != 0 || r.restart.count() != 0 {
		t.Fatalf("applied = %+v restarts = %d", applied, r.restart.count())
	}
}

// A sign-in on a server this agent never heard of: reconnect cannot add it, the
// server's config can.
func TestConnectBringsAServerTheAgentNeverHeardOf(t *testing.T) {
	r := newRig(t)
	config := raw(`{"type":"http","url":"https://new.test"}`)
	r.cli.user[""] = map[string]json.RawMessage{"new": config}
	r.sessions.running = []Session{{ID: "a", AccountID: deviceID, Chat: true}}
	r.live.afterSet["a"] = []domain.ConversationMCPServer{{Name: "new", Status: ports.ChatMCPStatusReady}}
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "new"})
	if err != nil {
		t.Fatal(err)
	}
	op := waitForState(t, r.svc, started.ID, OpConnected)
	if !reflect.DeepEqual(op.Applied.InPlace, []string{"a"}) {
		t.Fatalf("applied = %+v", op.Applied)
	}
	if len(r.live.applied) != 1 || string(r.live.applied[0].Set["new"]) != string(config) {
		t.Fatalf("applied = %+v", r.live.applied)
	}
}

// The token was saved and the agent still says it needs one: restart it.
func TestConnectRestartsAnAgentTheReconnectDidNotHelp(t *testing.T) {
	r := newRig(t)
	r.sessions.running = []Session{{ID: "a", AccountID: deviceID, Chat: true}}
	r.live.servers["a"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusNeedsAuth}}
	started, err := r.svc.Connect(context.Background(), TargetInput{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	op := waitForState(t, r.svc, started.ID, OpConnected)
	if !reflect.DeepEqual(op.Applied.Restarting, []string{"a"}) {
		t.Fatalf("applied = %+v", op.Applied)
	}
	awaitRestarts(t, r, 1)
}

// When the server itself fails, a restart would not help: say why instead.
func TestAFailingServerIsReportedNotRestarted(t *testing.T) {
	r := newRig(t)
	r.sessions.running = []Session{{ID: "a", AccountID: deviceID, Chat: true}}
	r.live.afterSet["a"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusFailed, Error: "exit 1"}}
	applied, err := r.svc.Add(context.Background(), AddInput{Name: "x", Command: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Failed["a"] != "exit 1" || r.restart.count() != 0 {
		t.Fatalf("applied = %+v", applied)
	}
}

func TestSetKeyKeepsTheRestOfTheConfigAndReachesAgentsInPlace(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x","headers":{"Keep":"kept","Change":"old"},"timeout":5}`)}
	r.sessions.running = []Session{{ID: "a", AccountID: accountID, Chat: true}}
	r.live.afterSet["a"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusReady}}
	applied, err := r.svc.SetKey(context.Background(), "x", map[string]string{"Keep": "", "Change": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(applied.InPlace, []string{"a"}) || r.restart.count() != 0 {
		t.Fatalf("applied = %+v", applied)
	}
	added := commandsWith(r.cli, "add-json")
	if len(commandsWith(r.cli, "remove")) != 1 || len(added) != 1 {
		t.Fatalf("calls = %+v", r.cli.calls())
	}
	var config struct {
		Headers map[string]string `json:"headers"`
		Timeout int               `json:"timeout"`
		URL     string            `json:"url"`
	}
	if err := json.Unmarshal([]byte(added[0].args[6]), &config); err != nil {
		t.Fatal(err)
	}
	if config.Headers["Keep"] != "kept" || config.Headers["Change"] != "new" || config.Timeout != 5 || config.URL != "https://x" {
		t.Fatalf("config = %+v", config)
	}
	if string(r.live.applied[0].Set["x"]) != added[0].args[6] {
		t.Fatal("the running agent did not get the new config")
	}
}

func TestSetKeyRefusesWhatAOCannotEdit(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{
		"helper": raw(`{"type":"http","url":"https://h","headersHelper":"/bin/h"}`),
		"local":  raw(`{"type":"stdio","command":"cat"}`),
	}
	for _, name := range []string{"helper", "local"} {
		if _, err := r.svc.SetKey(context.Background(), name, map[string]string{"A": "b"}); !errors.Is(err, ErrNotEditable) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if _, err := r.svc.SetKey(context.Background(), "unknown", map[string]string{"A": "b"}); !errors.Is(err, ErrUnknownServer) {
		t.Fatalf("err = %v", err)
	}
	if calls := r.cli.calls(); len(calls) != 0 {
		t.Fatalf("commands ran: %+v", calls)
	}
}

// Claude Code has no edit command, so a failed add must not lose the server.
func TestSetKeyRestoresTheServerWhenTheAddFails(t *testing.T) {
	r := newRig(t)
	old := `{"type":"http","url":"https://x","headers":{"K":"old"}}`
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(old)}
	cli := &failingAddCLI{fakeCLI: r.cli}
	r.svc.cli = cli
	if _, err := r.svc.SetKey(context.Background(), "x", map[string]string{"K": "new-secret"}); err == nil || strings.Contains(err.Error(), "new-secret") {
		t.Fatalf("err = %v", err)
	}
	added := commandsWith(r.cli, "add-json")
	if len(added) != 2 || added[1].args[6] != old {
		t.Fatalf("add-json calls = %+v; want the old config put back", added)
	}
}

type failingAddCLI struct {
	*fakeCLI
	adds int
}

func (f *failingAddCLI) MCPCommand(ctx context.Context, target ports.MCPTarget, args ...string) (ports.MCPCommandResult, error) {
	result, err := f.fakeCLI.MCPCommand(ctx, target, args...)
	if len(args) > 1 && args[1] == "add-json" {
		f.adds++
		if f.adds == 1 {
			return ports.MCPCommandResult{ExitCode: 1, Stderr: "Invalid: " + args[6]}, nil
		}
	}
	return result, err
}

// What a running agent reports is what it has, including what config never named.
func TestSessionServersComeFromTheRunningAgent(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"linear": raw(`{"type":"http","url":"https://mcp.linear.app/mcp"}`)}
	r.live.servers["s1"] = []domain.ConversationMCPServer{
		{Name: "linear", Status: ports.ChatMCPStatusNeedsAuth, Scope: "user"},
		{Name: "extra", Status: ports.ChatMCPStatusReady, ToolCount: intp(3), Scope: "dynamic"},
		{Name: "ao", Status: ports.ChatMCPStatusReady, Scope: "dynamic"},
		{Name: "claude_agent_acp", Status: ports.ChatMCPStatusReady, Scope: "dynamic"},
	}
	list, err := r.svc.SessionServers(context.Background(), "s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !list.Live || list.AccountID != accountID || len(list.Servers) != 2 {
		t.Fatalf("list = %+v; AO's own plumbing stays hidden", list)
	}
	linear := statusOn(t, findServer(t, list.Servers, "linear"), accountID)
	if linear.Status != StatusNeedsSignIn || linear.Action != ActionConnect || !linear.Live {
		t.Fatalf("linear = %+v", linear)
	}
	if extra := statusOn(t, findServer(t, list.Servers, "extra"), accountID); extra.Status != StatusConnected || *extra.ToolCount != 3 {
		t.Fatalf("extra = %+v", extra)
	}
	if len(r.cli.probed) != 0 {
		t.Fatalf("a running agent's answer needs no check: %+v", r.cli.probed)
	}
}

func TestSessionServersFallBackToACheckWithNoRunningAgent(t *testing.T) {
	r := newRig(t)
	r.live.err = ErrNotLive
	r.cli.lists["/acct|"+worktree] = "x: https://x (HTTP) - ✔ Connected\n"
	r.cli.project[worktree] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	list, err := r.svc.SessionServers(context.Background(), "s1", false)
	if err != nil {
		t.Fatal(err)
	}
	if list.Live {
		t.Fatalf("list = %+v", list)
	}
	if got := statusOn(t, findServer(t, list.Servers, "x"), accountID); got.Status != StatusConnected {
		t.Fatalf("x = %+v", got)
	}
	if _, err := r.svc.SessionServers(context.Background(), "nope", false); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("err = %v", err)
	}
}

func TestReconnectAsksTheSessionsAgent(t *testing.T) {
	r := newRig(t)
	r.live.servers["s1"] = []domain.ConversationMCPServer{{Name: "x", Status: ports.ChatMCPStatusReady}}
	list, err := r.svc.Reconnect(context.Background(), "s1", "x")
	if err != nil {
		t.Fatal(err)
	}
	if !list.Live || !reflect.DeepEqual(r.live.reconnects, []string{"s1:x"}) {
		t.Fatalf("list = %+v reconnects = %v", list, r.live.reconnects)
	}
}

func TestChecksAreCachedUntilRefreshOrChange(t *testing.T) {
	r := newRig(t)
	r.cli.user[""] = map[string]json.RawMessage{"x": raw(`{"type":"http","url":"https://x"}`)}
	r.cli.probes["|"] = []ports.MCPProbeServer{{Name: "x", Status: "connected"}}
	countDevice := func() int {
		r.cli.mu.Lock()
		defer r.cli.mu.Unlock()
		n := 0
		for _, probe := range r.cli.probed {
			if probe.target.Home == "" {
				n++
			}
		}
		return n
	}
	listNow(t, r.svc)
	listNow(t, r.svc)
	if got := countDevice(); got != 1 {
		t.Fatalf("probes = %d, want 1: a fresh reading is reused", got)
	}
	if _, err := r.svc.List(context.Background(), ListInput{Wait: true, Refresh: true}); err != nil {
		t.Fatal(err)
	}
	if got := countDevice(); got != 2 {
		t.Fatalf("probes = %d, want 2 after Refresh", got)
	}
	if _, err := r.svc.Add(context.Background(), AddInput{Name: "y", URL: "https://y"}); err != nil {
		t.Fatal(err)
	}
	listNow(t, r.svc)
	if got := countDevice(); got != 3 {
		t.Fatalf("probes = %d, want 3: a config change makes the reading stale", got)
	}
	list, err := r.svc.List(context.Background(), ListInput{NoCheck: true})
	if err != nil {
		t.Fatal(err)
	}
	if list.Checking {
		t.Fatal("no check should be running")
	}
}

func TestDisplayCommandMasksWhatLooksLikeASecret(t *testing.T) {
	got := displayCommand("npx", []string{"-y", "pkg", "--api-key", "abc", "TOKEN=def", "--port=3000", "plain"})
	if want := "npx -y pkg --api-key ••• TOKEN=••• --port=3000 plain"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := displayURL("https://user:pass@x.test/mcp?key=abc#frag"); got != "https://x.test/mcp" {
		t.Fatalf("url = %q", got)
	}
}

func TestLoginFailureFallsBackToTheError(t *testing.T) {
	if got := loginFailure("", fmt.Errorf("exit status 1")); got != "exit status 1" {
		t.Fatalf("got %q", got)
	}
	if got := loginFailure("Waiting for authorization… (^C to cancel)\r\n", nil); got != "The sign-in did not finish." {
		t.Fatalf("got %q", got)
	}
}
