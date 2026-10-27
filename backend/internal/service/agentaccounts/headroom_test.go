package agentaccounts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// headroomProvider reports limits per folder; a folder with no entry fails
// its limits read.
type headroomProvider struct {
	*fakeClaude
	mu        sync.Mutex
	limits    map[string][]domain.AgentAccountLimitWindow
	needsAuth map[string][]string
}

func (p *headroomProvider) Limits(_ context.Context, home string) (domain.AgentAccountLimits, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	windows, ok := p.limits[home]
	if !ok {
		return domain.AgentAccountLimits{}, errors.New("get_usage: unavailable")
	}
	return domain.AgentAccountLimits{Windows: windows}, nil
}

func (p *headroomProvider) MCPNeedsAuth(home string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.needsAuth[home]
}

func (p *headroomProvider) set(home string, windows ...domain.AgentAccountLimitWindow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limits[home] = windows
}

func window(label string, used float64, resetsAt *time.Time) domain.AgentAccountLimitWindow {
	return domain.AgentAccountLimitWindow{ID: label, Label: label, UsedPercent: used, ResetsAt: resetsAt}
}

// headroomFixture is the user's setup: the computer's login and three saved
// accounts, the session on the computer's login.
func headroomFixture(t *testing.T) (*Service, *memStore, *headroomProvider, *time.Time) {
	t.Helper()
	store := &memStore{
		accounts: []domain.AgentAccount{
			{ID: "kugel", Provider: domain.AgentAccountProviderClaude, Home: "/kugel", Email: "alex@kugelaudio.com"},
			{ID: "seever", Provider: domain.AgentAccountProviderClaude, Home: "/seever", Email: "alex.seeverlp@gmail.com"},
			{ID: "spare", Provider: domain.AgentAccountProviderClaude, Home: "/spare", Email: "spare@example.com"},
		},
		defaults: map[domain.AgentAccountProvider]string{},
		bindings: map[domain.SessionID]string{"s1": "device-claude"},
	}
	provider := &headroomProvider{
		fakeClaude: &fakeClaude{signedInAs: map[string]string{
			"": "alex.netz1@web.de", "/kugel": "alex@kugelaudio.com",
			"/seever": "alex.seeverlp@gmail.com", "/spare": "spare@example.com",
		}},
		limits:    map[string][]domain.AgentAccountLimitWindow{},
		needsAuth: map[string][]string{},
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	svc := New(store, []ports.AgentAccountProviderCLI{provider}, t.TempDir(), nil, nil)
	svc.now = func() time.Time { return now }
	return svc, store, provider, &now
}

func TestHeadroomAccountPicksTheMostUsageLeft(t *testing.T) {
	svc, _, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.set("", window("Session (5h)", 100, &reset), window("Weekly", 40, nil))
	provider.set("/kugel", window("Session (5h)", 0, nil), window("Weekly", 100, nil))
	provider.set("/seever", window("Session (5h)", 0, nil), window("Weekly", 81, nil), window("Weekly · Fable", 0, nil))
	// /spare's limits cannot be read: it still qualifies, after known headroom.

	got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, "")
	if err != nil || got.ID != "seever" {
		t.Fatalf("target = %q, %v; want seever, the one account with known headroom", got.ID, err)
	}
	if got, err = svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, "", "seever"); err != nil || got.ID != "spare" {
		t.Fatalf("next target = %q, %v; want spare, whose limits are unknown", got.ID, err)
	}
	if _, err = svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, "", "seever", "spare"); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("exhausted catalog = %v; want ErrAgentAccountNoHeadroom (kugel's weekly limit is used up)", err)
	}
}

func TestHeadroomAccountCountsOnlyTheSessionModelsWindow(t *testing.T) {
	svc, _, provider, _ := headroomFixture(t)
	ctx := context.Background()
	provider.signedInAs = map[string]string{"": "alex.netz1@web.de", "/seever": "alex.seeverlp@gmail.com"}
	provider.set("/seever", window("Session (5h)", 10, nil), window("Weekly", 10, nil), window("Weekly · Opus", 100, nil))

	if _, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, "claude-opus-4-1"); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("opus session moved to an account out of Opus usage: %v", err)
	}
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, "sonnet"); err != nil || got.ID != "seever" {
		t.Fatalf("sonnet target = %q, %v; the Opus window does not apply to it", got.ID, err)
	}
}

func TestHeadroomAccountReadsStaleLimitsBeforeDeciding(t *testing.T) {
	svc, _, provider, now := headroomFixture(t)
	ctx := context.Background()
	provider.signedInAs = map[string]string{"": "alex.netz1@web.de", "/seever": "alex.seeverlp@gmail.com"}
	provider.set("/seever", window("Session (5h)", 10, nil))
	if _, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil {
		t.Fatal(err)
	}
	// It ran out since; a reading two minutes old must not send a session there.
	provider.set("/seever", window("Session (5h)", 100, nil))
	*now = now.Add(2 * time.Minute)
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("target = %q, %v; want the stale reading read again", got.ID, err)
	}
}

// A saved folder signed in as the computer's own login is the same quota
// without the computer's MCP sign-ins: the computer's login is chosen.
func TestHeadroomAccountPrefersTheComputersLoginOverItsDuplicate(t *testing.T) {
	svc, store, provider, _ := headroomFixture(t)
	ctx := context.Background()
	provider.signedInAs = map[string]string{"": "alex.seeverlp@gmail.com", "/seever": "alex.seeverlp@gmail.com", "/kugel": "alex@kugelaudio.com"}
	provider.set("", window("Session (5h)", 0, nil), window("Weekly", 81, nil))
	provider.set("/seever", window("Session (5h)", 0, nil), window("Weekly", 70, nil)) // reads as more headroom
	provider.set("/kugel", window("Session (5h)", 100, nil))
	store.bindings["s1"] = "kugel"
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "device-claude" {
		t.Fatalf("target = %q, %v; want device-claude, never its managed duplicate", got.ID, err)
	}
}

func TestHeadroomAccountTieGoesToSignedInMCPServers(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	provider.set("", window("Session (5h)", 100, nil))
	provider.set("/kugel", window("Session (5h)", 20, nil))
	provider.set("/seever", window("Session (5h)", 23, nil))
	provider.set("/spare", window("Session (5h)", 100, nil))
	provider.needsAuth["/kugel"] = []string{"linear", "posthog"}
	store.bindings["s1"] = "spare"
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "seever" {
		t.Fatalf("target = %q, %v; within the tie, the account whose MCP servers are signed in wins", got.ID, err)
	}
	provider.set("/seever", window("Session (5h)", 60, nil))
	*now = now.Add(2 * time.Minute)
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "kugel" {
		t.Fatalf("target = %q, %v; clearly more headroom outranks MCP sign-ins", got.ID, err)
	}
	if names := svc.MCPNeedsAuth("kugel"); len(names) != 2 || names[0] != "linear" {
		t.Fatalf("kugel needs auth for %v", names)
	}
}

// Two folders signed in as one email share one quota: a session that ran out
// on one never "switches" to the other, even on a reading that still shows
// the other with usage left.
func TestMarkUsageLimitedCountsTheLoginsOtherFolderAsLimited(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.signedInAs = map[string]string{"": "alex.netz1@web.de", "/seever": "alex.seeverlp@gmail.com", "/kugel": "alex@kugelaudio.com", "/spare": "Alex.Seeverlp@gmail.com"}
	provider.set("", window("Session (5h)", 100, nil))
	provider.set("/kugel", window("Session (5h)", 50, nil))
	provider.set("/seever", window("Session (5h)", 0, nil))
	provider.set("/spare", window("Session (5h)", 0, nil))
	store.accounts[2].Email = "Alex.Seeverlp@gmail.com"
	if _, err := svc.List(ctx, false); err != nil {
		t.Fatal(err)
	}
	// seever runs out; spare, the same login, still reads as having headroom.
	provider.set("/seever", window("Session (5h)", 100, &reset))
	store.bindings["s1"] = "seever"
	from, restart, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, &reset)
	if err != nil || from.ID != "seever" || restart {
		t.Fatalf("limited account = %q restart=%v, %v", from.ID, restart, err)
	}
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "kugel" {
		t.Fatalf("target = %q, %v; want kugel, never spare, the same login", got.ID, err)
	}
}

func TestMarkUsageLimitedPoolsTheLoginWhenItsReadingAgrees(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.signedInAs = map[string]string{"": "alex.seeverlp@gmail.com", "/seever": "alex.seeverlp@gmail.com", "/kugel": "alex@kugelaudio.com"}
	provider.set("/seever", window("Session (5h)", 100, &reset))
	provider.set("", window("Session (5h)", 0, nil)) // stale display reading of the same quota
	provider.set("/kugel", window("Session (5h)", 50, nil))
	store.bindings["s1"] = "seever"
	if _, restart, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, nil); err != nil || restart {
		t.Fatalf("restart=%v, %v", restart, err)
	}
	if until := svc.limitedLogins["alex.seeverlp@gmail.com"]; !until.Equal(reset) {
		t.Fatalf("login limited until %s, want the window's reset %s", until, reset)
	}
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "kugel" {
		t.Fatalf("target = %q, %v; want kugel", got.ID, err)
	}
	// Another session on another account also never lands on that login.
	store.bindings["s2"] = "kugel"
	provider.set("/kugel", window("Session (5h)", 100, nil))
	if got, err := svc.HeadroomAccount(ctx, "s2", domain.HarnessClaudeCode, ""); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("s2 target = %q, %v; the computer's folder is the limited login", got.ID, err)
	}
}

// After "Sign in again" with another login, the computer's folder holds a
// login with usage left while a running agent still uses the old token. The
// refusal still counts as a limit: the session restarts once on the folder to
// pick up its current login, and never goes back to it before the reset.
func TestMarkUsageLimitedStaleTokenStillCountsAsLimited(t *testing.T) {
	svc, _, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.signedInAs = map[string]string{"": "alex.seeverlp@gmail.com", "/seever": "alex.seeverlp@gmail.com", "/kugel": "alex@kugelaudio.com"}
	provider.set("", window("Session (5h)", 0, nil), window("Weekly", 81, nil))
	provider.set("/seever", window("Session (5h)", 0, nil), window("Weekly", 81, nil))
	provider.set("/kugel", window("Session (5h)", 0, nil), window("Weekly", 100, nil))

	from, restart, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, &reset)
	if err != nil || from.ID != "device-claude" || !restart {
		t.Fatalf("limited = %q restart=%v, %v; want a restart on the computer's login", from.ID, restart, err)
	}
	if len(svc.limitedLogins) != 0 {
		t.Fatalf("login marked limited from a stale token: %v", svc.limitedLogins)
	}
	// Refused again after the restart: no second restart, and no move to the
	// managed duplicate of the computer's login.
	if _, restart, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, &reset); err != nil || restart {
		t.Fatalf("second refusal restart=%v, %v", restart, err)
	}
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("target = %q, %v; want none: seever duplicates the computer's login, kugel is used up", got.ID, err)
	}
}

func TestHeadroomAccountMovesBackOnceTheLimitResets(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.signedInAs = map[string]string{"": "alex.netz1@web.de", "/seever": "alex.seeverlp@gmail.com"}
	provider.set("", window("Session (5h)", 100, &reset))
	provider.set("/seever", window("Session (5h)", 10, nil))
	if _, _, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, &reset); err != nil {
		t.Fatal(err)
	}
	store.bindings["s1"] = "seever"
	provider.set("/seever", window("Session (5h)", 100, nil))
	if _, _, err := svc.MarkUsageLimited(ctx, "s1", domain.HarnessClaudeCode, nil); err != nil {
		t.Fatal(err)
	}
	// The computer's reading now even shows headroom: still not before its reset.
	provider.set("", window("Session (5h)", 0, nil))
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); !errors.Is(err, ports.ErrAgentAccountNoHeadroom) {
		t.Fatalf("target = %q, %v; want no loop back before the reset", got.ID, err)
	}
	*now = reset.Add(time.Second)
	if got, err := svc.HeadroomAccount(ctx, "s1", domain.HarnessClaudeCode, ""); err != nil || got.ID != "device-claude" {
		t.Fatalf("after reset target = %q, %v; want device-claude", got.ID, err)
	}
}

func TestNewSessionsAvoidAUsedUpDefault(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.set("", window("Session (5h)", 100, &reset), window("Weekly", 30, nil))
	provider.set("/kugel", window("Session (5h)", 0, nil), window("Weekly", 100, nil))
	provider.set("/seever", window("Session (5h)", 0, nil), window("Weekly", 81, nil))
	provider.set("/spare", window("Session (5h)", 50, nil), window("Weekly", 50, nil))

	if err := svc.BindSession(ctx, "new", domain.HarnessClaudeCode, ""); err != nil || store.bindings["new"] != "spare" {
		t.Fatalf("new session bound to %q, %v; want spare, the most headroom", store.bindings["new"], err)
	}
	if _, picked := store.defaults[domain.AgentAccountProviderClaude]; picked {
		t.Fatalf("the user's default changed: %v", store.defaults)
	}
	if svc.RunsOnComputerLogin(ctx, domain.HarnessClaudeCode, "") {
		t.Fatal("launch gate expects the computer's login, the session starts on spare")
	}

	// A default with usage left keeps new sessions.
	provider.set("", window("Session (5h)", 20, nil), window("Weekly", 30, nil))
	*now = now.Add(6 * time.Minute)
	if err := svc.BindSession(ctx, "next", domain.HarnessClaudeCode, ""); err != nil || store.bindings["next"] != "device-claude" {
		t.Fatalf("new session bound to %q, %v; want the default", store.bindings["next"], err)
	}
}

func TestNewSessionsKeepAUsedUpDefaultWhenNoAccountHasHeadroom(t *testing.T) {
	svc, store, provider, now := headroomFixture(t)
	ctx := context.Background()
	reset := now.Add(41 * time.Minute)
	provider.signedInAs = map[string]string{"": "alex.netz1@web.de", "/kugel": "alex@kugelaudio.com"}
	provider.set("", window("Session (5h)", 100, &reset))
	provider.set("/kugel", window("Weekly", 100, nil))
	if err := svc.BindSession(ctx, "new", domain.HarnessClaudeCode, ""); err != nil || store.bindings["new"] != "device-claude" {
		t.Fatalf("new session bound to %q, %v; want today's default", store.bindings["new"], err)
	}
}

func TestWindowModelMatching(t *testing.T) {
	for _, tc := range []struct {
		label, model string
		applies      bool
	}{
		{"Session (5h)", "", true},
		{"Weekly", "claude-opus-4-1", true},
		{"Weekly · Opus", "claude-opus-4-1", true},
		{"Weekly · Opus", "opus[1m]", true},
		{"Weekly · Opus", "sonnet", false},
		{"Weekly · Opus", "", false},
		{"Weekly · Fable", "fable", true},
		{"GPT-5.3-Codex-Spark · Weekly", "gpt-5.3-codex-spark", true},
		{"GPT-5.3-Codex-Spark · Session (5h)", "gpt-5.3-codex", false},
	} {
		limits := &domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{window(tc.label, 100, nil)}}
		_, applies := limitHeadroom(limits, tc.model, time.Now())
		if applies != tc.applies {
			t.Errorf("%q for model %q applies = %v, want %v", tc.label, tc.model, applies, tc.applies)
		}
	}
	past := time.Now().Add(-time.Minute)
	limits := &domain.AgentAccountLimits{Windows: []domain.AgentAccountLimitWindow{window("Session (5h)", 100, &past)}}
	if percent, _ := limitHeadroom(limits, "", time.Now()); percent != 100 {
		t.Fatalf("a window whose reset passed has %v%% left, want 100", percent)
	}
}
