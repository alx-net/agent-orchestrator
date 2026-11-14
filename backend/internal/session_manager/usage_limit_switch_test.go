package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// limitAccounts is the account catalog's usage-limit half: the session's
// account ran out, and targets are offered in order, skipping tried ones.
type limitAccounts struct {
	*transitionAccounts
	restart bool
	targets []string
	marked  []string
}

func (a *limitAccounts) MarkUsageLimited(_ context.Context, id domain.SessionID, _ domain.AgentHarness, _ *time.Time) (domain.AgentAccount, bool, error) {
	current := a.boundTo(id)
	a.mu.Lock()
	a.marked = append(a.marked, current)
	a.mu.Unlock()
	return domain.AgentAccount{ID: current}, a.restart, nil
}

func (a *limitAccounts) HeadroomAccount(_ context.Context, id domain.SessionID, _ domain.AgentHarness, _ string, skip ...string) (domain.AgentAccount, error) {
	current := a.boundTo(id)
	for _, target := range a.targets {
		if target != current && !slices.Contains(skip, target) {
			return domain.AgentAccount{ID: target}, nil
		}
	}
	return domain.AgentAccount{}, ports.ErrAgentAccountNoHeadroom
}

func (a *limitAccounts) MCPNeedsAuth(accountID string) []string {
	if accountID == "acct-b" {
		return []string{"linear"}
	}
	return nil
}

// limitChat is Chat's usage-limit half: one refused turn, the notice it shows,
// and the continuation on the new account.
type limitChat struct {
	*transitionChat
	accounts  *limitAccounts
	mu        sync.Mutex
	turnErr   error
	switches  []domain.UsageLimitSwitch
	continued []string // the account bound when the turn ran again
	lookups   int
}

func (c *limitChat) UsageLimitedTurn(_ context.Context, _ domain.SessionID, providerTurnID string) (domain.ConversationTurn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookups++
	if c.turnErr != nil {
		return domain.ConversationTurn{}, c.turnErr
	}
	reset := time.Now().Add(41 * time.Minute)
	return domain.ConversationTurn{ID: "turn-for-" + providerTurnID, State: domain.TurnStateFailed,
		UsageLimit: &domain.ConversationUsageLimit{ResetsAt: &reset}}, nil
}

func (c *limitChat) RecordUsageLimitSwitch(_ context.Context, _ domain.SessionID, _ string, sw domain.UsageLimitSwitch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.switches = append(c.switches, sw)
	return nil
}

func (c *limitChat) ContinueAfterAccountSwitch(_ context.Context, id domain.SessionID, turnID string) (domain.ConversationTurn, error) {
	bound := c.accounts.boundTo(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.continued = append(c.continued, bound)
	return domain.ConversationTurn{ID: "retry-of-" + turnID}, nil
}

func (c *limitChat) shown() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.switches))
	for _, sw := range c.switches {
		out = append(out, fmt.Sprintf("%s:%s->%s", sw.Phase, sw.FromAccountID, sw.ToAccountID))
	}
	return fmt.Sprint(out)
}

func newUsageLimitManager(t *testing.T) (*Manager, *limitAccounts, *limitChat) {
	t.Helper()
	manager, _, _, chat, accounts, _ := newAccountTransitionManager(t, domain.SessionModeChat)
	accounts.homes["acct-c"] = "/accounts/c"
	limits := &limitAccounts{transitionAccounts: accounts, targets: []string{"acct-b"}}
	manager.SetAgentAccounts(limits)
	limitedChat := &limitChat{transitionChat: chat, accounts: limits}
	manager.chat = limitedChat
	return manager, limits, limitedChat
}

// awaitUsageLimitSwitch waits until the session has no move under way.
func awaitUsageLimitSwitch(t *testing.T, manager *Manager, id domain.SessionID) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		manager.usageLimitSwitchMu.Lock()
		_, running := manager.usageLimitSwitches[id]
		manager.usageLimitSwitchMu.Unlock()
		if !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("usage-limit move did not finish")
}

func TestUsageLimitMovesTheSessionAndContinuesTheTurn(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	if got := chat.shown(); got != "[switching:device-claude->acct-b continued:device-claude->acct-b]" {
		t.Fatalf("notice = %s", got)
	}
	if got := accounts.boundTo("session-1"); got != "acct-b" {
		t.Fatalf("binding = %q, want acct-b", got)
	}
	if fmt.Sprint(chat.continued) != "[acct-b]" {
		t.Fatalf("turn ran again on %v, want once, after the binding moved to acct-b", chat.continued)
	}
	if fmt.Sprint(accounts.marked) != "[device-claude]" {
		t.Fatalf("limited accounts = %v", accounts.marked)
	}
	if last := chat.switches[len(chat.switches)-1]; fmt.Sprint(last.MCPNeedsAuth) != "[linear]" {
		t.Fatalf("notice names missing MCP servers %v, want [linear]", last.MCPNeedsAuth)
	}
}

// With no account that has usage left, nothing moves: the turn keeps today's
// usage-limit notice, to resume by hand or at the reset.
func TestUsageLimitWithNoAccountWithHeadroomKeepsTheNotice(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)
	accounts.targets = nil

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	if got := chat.shown(); got != "[]" {
		t.Fatalf("notice = %s, want today's notice untouched", got)
	}
	if got := accounts.boundTo("session-1"); got != "device-claude" {
		t.Fatalf("binding = %q, want unchanged", got)
	}
	if len(chat.continued) != 0 {
		t.Fatal("the turn ran again on the account that ran out")
	}
}

func TestUsageLimitTriesTheNextAccountWhenOneIsSignedOut(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)
	accounts.targets = []string{"acct-c", "acct-b"}
	accounts.validateErrs = []error{ports.ErrAgentAccountSignedOut}

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	want := "[switching:device-claude->acct-c switching:device-claude->acct-b continued:device-claude->acct-b]"
	if got := chat.shown(); got != want {
		t.Fatalf("notice = %s, want %s", got, want)
	}
	if got := accounts.boundTo("session-1"); got != "acct-b" {
		t.Fatalf("binding = %q, want acct-b", got)
	}
}

func TestUsageLimitShowsAMoveThatFailed(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)
	accounts.validateErrs = []error{errors.New("keychain is locked")}

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	if got := chat.shown(); got != "[switching:device-claude->acct-b failed:device-claude->acct-b]" {
		t.Fatalf("notice = %s", got)
	}
	if last := chat.switches[len(chat.switches)-1]; last.Error != "keychain is locked" {
		t.Fatalf("failure detail = %q", last.Error)
	}
	if got := accounts.boundTo("session-1"); got != "device-claude" || len(chat.continued) != 0 {
		t.Fatalf("binding = %q continued = %v, want neither", got, chat.continued)
	}
}

// A refusal the account's folder cannot have made (it now holds a login with
// usage left) restarts the agent on the same account to pick that login up.
func TestUsageLimitRestartsOnTheSameAccountForAStaleLogin(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)
	accounts.restart = true

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	if got := chat.shown(); got != "[switching:device-claude->device-claude continued:device-claude->device-claude]" {
		t.Fatalf("notice = %s", got)
	}
	if fmt.Sprint(chat.continued) != "[device-claude]" {
		t.Fatalf("continued = %v", chat.continued)
	}
	// A user still cannot "move" a session to the account it is on.
	if _, err := manager.StartAccountTransition(context.Background(), "session-1", "device-claude", domain.SessionInterfaceTransitionDrain); !errors.Is(err, ports.ErrAgentAccountUnchanged) {
		t.Fatalf("manual same-account move = %v, want ErrAgentAccountUnchanged", err)
	}
}

func TestUsageLimitSwitchOffLeavesTheSession(t *testing.T) {
	manager, accounts, chat := newUsageLimitManager(t)
	manager.SetUsageLimitSwitch(func() bool { return false })

	manager.UsageLimitReached("session-1", "provider-turn-1")
	awaitUsageLimitSwitch(t, manager, "session-1")

	if chat.lookups != 0 || accounts.boundTo("session-1") != "device-claude" {
		t.Fatalf("setting off, yet the session was examined (%d) or moved (%q)", chat.lookups, accounts.boundTo("session-1"))
	}
}

// trustAgent records the worktree trust a launch would write, and where.
type trustAgent struct {
	transitionAgent
	mu      sync.Mutex
	trusted []string
}

func (a *trustAgent) PreLaunch(_ context.Context, cfg ports.LaunchConfig) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trusted = append(a.trusted, cfg.WorkspacePath+"@"+cfg.Env["CLAUDE_CONFIG_DIR"])
	return nil
}

// A move trusts the worktree in the new account's own config before the
// agent resumes there, as the spawn did for the old one.
func TestAccountMoveTrustsTheWorktreeInTheTargetAccountsConfig(t *testing.T) {
	manager, store, _, _, _, _ := newAccountTransitionManager(t, domain.SessionModeChat)
	agent := &trustAgent{}
	manager.agents = singleAgent{agent: agent}

	transition, err := manager.StartAccountTransition(context.Background(), "session-1", "acct-b", domain.SessionInterfaceTransitionDrain)
	if err != nil {
		t.Fatal(err)
	}
	if settled := awaitTransition(t, store, transition.ID); settled.Phase != domain.SessionInterfaceTransitionCompleted {
		t.Fatalf("phase = %s %s", settled.Phase, settled.ErrorDetail)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !slices.Contains(agent.trusted, "/ws/session-1@/accounts/b") {
		t.Fatalf("trusted = %v, want the worktree in acct-b's config", agent.trusted)
	}
}

func TestAgentsRunningOnAccountIgnoresTheLaunchingSession(t *testing.T) {
	manager, store, _, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeChat)
	ctx := context.Background()
	accounts.bound["session-1"] = "acct-b"
	if manager.AgentsRunningOnAccount(ctx, "acct-b", "session-1") {
		t.Fatal("the launching session counted as another agent on its account")
	}
	if !manager.AgentsRunningOnAccount(ctx, "acct-b", "") {
		t.Fatal("an idle agent on acct-b did not count as running")
	}
	rec := store.sessions["session-1"]
	rec.Activity.State = domain.ActivityExited
	store.sessions["session-1"] = rec
	if manager.AgentsRunningOnAccount(ctx, "acct-b", "") {
		t.Fatal("an exited agent counted as running")
	}
	if manager.AgentsRunningOnAccount(ctx, "device-claude", "") {
		t.Fatal("no session is on device-claude")
	}
}

// A refusal heard while the session is still moving is handled after it.
func TestUsageLimitHeardDuringAMoveIsHandledAfterIt(t *testing.T) {
	manager, _, chat := newUsageLimitManager(t)
	chat.preflightStarted = make(chan struct{}, 1)
	chat.preflightRelease = make(chan struct{})

	manager.UsageLimitReached("session-1", "provider-turn-1")
	<-chat.preflightStarted
	manager.UsageLimitReached("session-1", "provider-turn-2")
	close(chat.preflightRelease)
	awaitUsageLimitSwitch(t, manager, "session-1")

	chat.mu.Lock()
	defer chat.mu.Unlock()
	if chat.lookups != 2 {
		t.Fatalf("refused turns examined = %d, want both", chat.lookups)
	}
}
