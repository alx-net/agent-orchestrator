package agentaccounts

import (
	"context"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// minHeadroom is the share of a limit window, in percent, below which it
// counts as used up. The meter shows less than this as 0% left.
const minHeadroom = 1.0

// limitFallback is how long a usage limit that refused a session is honored
// when neither the refusal nor the account's limits name its reset.
const limitFallback = time.Hour

// moveFreshFor is the oldest reading a move after a usage limit decides on;
// an older one is read again first.
const moveFreshFor = time.Minute

// spawnReadLimit bounds the limits read a new session waits for.
const spawnReadLimit = 15 * time.Second

// headroomTie is how close, in percentage points, two accounts' headroom is
// for the choice between them to fall to their MCP sign-ins.
const headroomTie = 5.0

// limitPeriod matches the period half of a window label. The other half of a
// label such as "Weekly · Opus" or "GPT-5.3-Codex-Spark · Weekly" is a model.
var limitPeriod = regexp.MustCompile(`^(Session \(5h\)|Weekly|Limit|\d+h)$`)

// headroom is how much of its limits an account has left for one model.
type headroom struct {
	account domain.AgentAccount
	// percent is the lowest remaining share over the windows that apply.
	percent float64
	// known is false when AO could not read the account's limits.
	known bool
	// mcpNeedsAuth counts MCP servers not signed in on the account.
	mcpNeedsAuth int
}

// better ranks known headroom above unknown, then more above less. Headroom
// within headroomTie is a tie: fewer MCP servers to sign in to wins, then the
// computer's own login, then the larger headroom.
func (h headroom) better(other headroom) bool {
	if h.known != other.known {
		return h.known
	}
	if math.Abs(h.percent-other.percent) >= headroomTie {
		return h.percent > other.percent
	}
	if h.mcpNeedsAuth != other.mcpNeedsAuth {
		return h.mcpNeedsAuth < other.mcpNeedsAuth
	}
	if h.account.Device() != other.account.Device() {
		return h.account.Device()
	}
	return h.percent > other.percent
}

// MarkUsageLimited records that the provider refused the session's agent for
// its account's usage limit, until resetsAt or, when that is unknown, the
// account's own latest reset. The refusal decides, whatever the account's
// reading says: the session never moves back to the account before then.
//
// Other folders signed in as the same login share its quota, so they count as
// limited too, unless a fresh reading shows that login with usage left on
// every meter. Then the agent ran on a login the folder no longer holds (a
// stale token after "Sign in again"), and the folder's login did not run out.
// restart reports that case the first time per session and account: the
// agent restarted on its own account starts on the folder's current login.
func (s *Service) MarkUsageLimited(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, resetsAt *time.Time) (account domain.AgentAccount, restart bool, err error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return domain.AgentAccount{}, false, ports.ErrAgentAccountProviderMismatch
	}
	account, _, err = s.SessionAccount(ctx, id, p)
	if err != nil {
		return domain.AgentAccount{}, false, err
	}
	r := s.read(ctx, account)
	now := s.now()
	until := now.Add(limitFallback)
	if resetsAt != nil && resetsAt.After(now) {
		until = *resetsAt
	} else if r != nil {
		if reset := latestExhaustedReset(r.limits, now); !reset.IsZero() {
			until = reset
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limitedFor == nil {
		s.limitedFor = map[domain.SessionID]map[string]time.Time{}
	}
	if s.limitedFor[id] == nil {
		s.limitedFor[id] = map[string]time.Time{}
	}
	refusedBefore := s.limitedFor[id][account.ID].After(now)
	s.limitedFor[id][account.ID] = until
	if !showsHeadroom(r, now) {
		if s.limitedLogins == nil {
			s.limitedLogins = map[string]time.Time{}
		}
		s.limitedLogins[loginOf(account, r)] = until
		return account, false, nil
	}
	return account, !refusedBefore, nil
}

// HeadroomAccount is the account a session refused for its usage limit moves
// to: another signed-in account of the same provider whose login has not run
// out, that has not refused this session, and that has usage left on every
// limit that applies to model. Readings older than a minute are read again
// first. The most headroom wins; an account whose limits AO cannot read ranks
// after every account with known headroom. skip names accounts already tried.
func (s *Service) HeadroomAccount(ctx context.Context, id domain.SessionID, harness domain.AgentHarness, model string, skip ...string) (domain.AgentAccount, error) {
	p, ok := domain.AgentAccountProviderFor(harness)
	if !ok || s.providers[p] == nil {
		return domain.AgentAccount{}, ports.ErrAgentAccountProviderMismatch
	}
	current, _, err := s.SessionAccount(ctx, id, p)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	now := s.now()
	s.mu.Lock()
	refused := maps.Clone(s.limitedFor[id])
	s.mu.Unlock()
	return s.mostHeadroom(ctx, p, model, moveFreshFor, func(account domain.AgentAccount) bool {
		return account.ID == current.ID || slices.Contains(skip, account.ID) || refused[account.ID].After(now)
	})
}

// MCPNeedsAuth names the MCP servers not signed in on an account, from its
// last reading.
func (s *Service) MCPNeedsAuth(accountID string) []string {
	if r := s.cachedReading(accountID); r != nil {
		return slices.Clone(r.mcpNeedsAuth)
	}
	return nil
}

// mostHeadroom reads the provider's accounts again when their reading is
// older than maxAge, and returns the eligible one with the most headroom.
func (s *Service) mostHeadroom(ctx context.Context, p domain.AgentAccountProvider, model string, maxAge time.Duration, exclude func(domain.AgentAccount) bool) (domain.AgentAccount, error) {
	accounts, err := s.accounts(ctx)
	if err != nil {
		return domain.AgentAccount{}, err
	}
	var candidates []domain.AgentAccount
	for _, account := range accounts {
		if account.Provider == p && !s.beingRemoved(account.ID) && !exclude(account) {
			candidates = append(candidates, account)
		}
	}
	var wg sync.WaitGroup
	for _, account := range candidates {
		if s.readingOlderThan(account.ID, maxAge) {
			wg.Go(func() { s.read(ctx, account) })
		}
	}
	wg.Wait()
	// A saved folder signed in as the computer's own login is the same
	// subscription in a folder without the computer's MCP sign-ins: a session
	// goes to the computer's login, never to that duplicate.
	deviceLogin := ""
	device := domain.AgentAccount{ID: domain.DeviceAgentAccountID(p), Provider: p}
	if r := s.cachedReading(device.ID); r != nil && r.identity.SignedIn && !r.mismatch {
		deviceLogin = loginOf(device, r)
	}
	now := s.now()
	var best *headroom
	for _, account := range candidates {
		if !account.Device() && deviceLogin != "" && loginOf(account, s.cachedReading(account.ID)) == deviceLogin {
			continue
		}
		h, ok := s.headroomOf(account, model, now)
		if ok && (best == nil || h.better(*best)) {
			best = &h
		}
	}
	if best == nil {
		return domain.AgentAccount{}, ports.ErrAgentAccountNoHeadroom
	}
	return best.account, nil
}

// headroomOf reports an account's headroom for model, and false when a
// session should not move to it: not signed in as itself, its login refused a
// session that has not reset, or a limit that applies is used up.
func (s *Service) headroomOf(account domain.AgentAccount, model string, now time.Time) (headroom, bool) {
	r := s.cachedReading(account.ID)
	if r == nil || r.mismatch || !r.identity.SignedIn || s.loginLimited(account, r, now) {
		return headroom{}, false
	}
	percent, known := limitHeadroom(r.limits, model, now)
	if known && percent < minHeadroom {
		return headroom{}, false
	}
	return headroom{account: account, percent: percent, known: known, mcpNeedsAuth: len(r.mcpNeedsAuth)}, true
}

// avoidUsedUp keeps a new session off an account that has run out of usage,
// where it would stop at its first turn: it starts on the account with the
// most headroom instead, when one has some. The user's default stays as it is.
func (s *Service) avoidUsedUp(ctx context.Context, p domain.AgentAccountProvider, account domain.AgentAccount) domain.AgentAccount {
	r := s.cachedReading(account.ID)
	if r == nil || s.now().Sub(r.at) > freshFor {
		readCtx, cancel := context.WithTimeout(ctx, spawnReadLimit)
		r = s.read(readCtx, account)
		cancel()
	}
	now := s.now()
	if r == nil || !s.usedUp(account, r, now) {
		return account
	}
	other, err := s.mostHeadroom(ctx, p, "", freshFor, func(candidate domain.AgentAccount) bool { return candidate.ID == account.ID })
	if err != nil {
		return account
	}
	return other
}

// usedUp reports an account a new session should not start on: its login
// refused a session for a limit that has not reset, or its reading shows a
// limit every model counts against used up.
func (s *Service) usedUp(account domain.AgentAccount, r *reading, now time.Time) bool {
	if s.loginLimited(account, r, now) {
		return true
	}
	percent, known := limitHeadroom(r.limits, "", now)
	return known && percent < minHeadroom
}

func (s *Service) loginLimited(account domain.AgentAccount, r *reading, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limitedLogins[loginOf(account, r)].After(now)
}

func (s *Service) readingOlderThan(id string, maxAge time.Duration) bool {
	r := s.cachedReading(id)
	return r == nil || r.identityErr != "" || s.now().Sub(r.at) > maxAge
}

// loginOf names the login behind an account. Two folders signed in as the same
// email share one subscription, so one quota. The computer's own folder is
// named by whoever its reading found signed in.
func loginOf(account domain.AgentAccount, r *reading) string {
	email := account.Email
	if account.Device() && r != nil {
		email = r.identity.Email
	}
	if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
		return email
	}
	return "account:" + account.ID
}

// limitHeadroom is the lowest remaining share over the windows that apply to
// model. A window whose reset has passed is whole again. known is false
// without limits, or when none of them applies.
func limitHeadroom(limits *domain.AgentAccountLimits, model string, now time.Time) (percent float64, known bool) {
	if limits == nil {
		return 0, false
	}
	percent = 100
	for _, w := range limits.Windows {
		if name := windowModel(w.Label); name != "" && !modelMatches(name, model) {
			continue
		}
		known = true
		percent = min(percent, windowRemaining(w, now))
	}
	if !known {
		return 0, false
	}
	return percent, true
}

func windowRemaining(w domain.AgentAccountLimitWindow, now time.Time) float64 {
	if w.ResetsAt != nil && !w.ResetsAt.After(now) {
		return 100
	}
	return 100 - w.UsedPercent
}

// showsHeadroom reports a reading of the folder's login with usage left on
// every meter, model meters included.
func showsHeadroom(r *reading, now time.Time) bool {
	if r == nil || r.identityErr != "" || r.mismatch || r.limits == nil || len(r.limits.Windows) == 0 {
		return false
	}
	for _, w := range r.limits.Windows {
		if windowRemaining(w, now) < minHeadroom {
			return false
		}
	}
	return true
}

// latestExhaustedReset is the latest future reset of a used-up window.
func latestExhaustedReset(limits *domain.AgentAccountLimits, now time.Time) time.Time {
	var latest time.Time
	if limits == nil {
		return latest
	}
	for _, w := range limits.Windows {
		if windowRemaining(w, now) < minHeadroom && w.ResetsAt != nil && w.ResetsAt.After(latest) {
			latest = *w.ResetsAt
		}
	}
	return latest
}

// windowModel is the model a limit window counts, or "" for a window every
// model counts against.
func windowModel(label string) string {
	parts := strings.Split(label, " · ")
	if len(parts) < 2 {
		return ""
	}
	for _, part := range parts {
		if part = strings.TrimSpace(part); !limitPeriod.MatchString(part) {
			return part
		}
	}
	return ""
}

// modelMatches reports a session model a model window counts: the same name,
// or a model id with the window's model family as one of its parts, as
// "claude-opus-4-1" is counted by "Opus". An unknown model matches none.
func modelMatches(name, model string) bool {
	name, model = strings.ToLower(strings.TrimSpace(name)), strings.ToLower(strings.TrimSpace(model))
	if name == "" || model == "" {
		return false
	}
	if name == model {
		return true
	}
	family := strings.Fields(name)[0]
	parts := strings.FieldsFunc(model, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' })
	return slices.Contains(parts, family)
}
