// Package wait implements `ao wait`: an agent registers what it waits for (a
// PR's CI, merge, or review, or a timer), ends its turn, and the daemon wakes
// it once with a complete result instead of the agent polling at full-context
// cost. PR facts come from what the SCM observer already persists; only a PR
// no live AO session tracks is read from the provider directly, on a slow
// shared cadence and only while a wait is pending on it.
package wait

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

const (
	// DefaultTick is the evaluator cadence, matching the SCM observer's poll.
	DefaultTick = 30 * time.Second
	// deliveryTimeout bounds one wake delivery, which may wake a hibernated
	// Chat session first.
	deliveryTimeout = 90 * time.Second
	// undeliverableGrace is how long past its deadline AO keeps retrying a
	// wake the session cannot take (it is blocked on the human, or its agent
	// exited) before giving up on it.
	undeliverableGrace = 6 * time.Hour
	// resolvedRetention keeps resolved waits for diagnosis before pruning.
	resolvedRetention = 7 * 24 * time.Hour
	pruneEvery        = time.Hour
)

// Store is the wait service's persistence boundary.
type Store interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	CreateWait(ctx context.Context, w domain.Wait) (domain.Wait, error)
	GetWait(ctx context.Context, id domain.WaitID) (domain.Wait, bool, error)
	ListPendingWaits(ctx context.Context) ([]domain.Wait, error)
	ListPendingWaitsBySession(ctx context.Context, id domain.SessionID) ([]domain.Wait, error)
	ResolveWait(ctx context.Context, id domain.WaitID, state domain.WaitState, outcome, message string, at time.Time) (bool, error)
	UpdateWaitTarget(ctx context.Context, id domain.WaitID, prURL string, baseline domain.WaitBaseline) (bool, error)
	RecordWaitDeliveryFailure(ctx context.Context, id domain.WaitID, lastError string) error
	PruneResolvedWaits(ctx context.Context, cutoff time.Time) (int64, error)

	GetPR(ctx context.Context, url string) (domain.PullRequest, bool, error)
	ListPRsByNumber(ctx context.Context, number int) ([]domain.PullRequest, error)
	ListChecks(ctx context.Context, prURL string) ([]domain.PullRequestCheck, error)
	ListPRReviews(ctx context.Context, prURL string) ([]domain.PullRequestReview, error)
	ListPRComments(ctx context.Context, prURL string) ([]domain.PullRequestComment, error)
}

// Deliverer sends a wake to a session the way AO sends its other automation
// messages (lifecycle.Manager.DeliverAutomationMessage).
type Deliverer interface {
	DeliverAutomationMessage(ctx context.Context, id domain.SessionID, msg string) (sessionguard.Outcome, error)
}

// PRReader reads a PR straight from the SCM provider. It is optional: without
// it, only PRs the SCM observer tracks can be waited on.
type PRReader interface {
	ParseRepository(remote string) (ports.SCMRepo, bool)
	FetchPullRequests(ctx context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error)
	FetchFailedCheckLogTail(ctx context.Context, repo ports.SCMRepo, check ports.SCMCheckObservation) (string, error)
	FetchReviewThreads(ctx context.Context, ref ports.SCMPRRef) (ports.SCMReviewObservation, error)
}

// Deps configures a Service. PRs, Clock, NewID, Logger and Tick are optional.
type Deps struct {
	Store     Store
	Deliverer Deliverer
	PRs       PRReader
	Clock     func() time.Time
	NewID     func() string
	Logger    *slog.Logger
	Tick      time.Duration
}

// RegisterInput is one validated `ao wait pr|time` request.
type RegisterInput struct {
	SessionID domain.SessionID
	Kind      domain.WaitKind
	// PRNumber and/or PRURL name the PR; a number resolves in the session's
	// project. Condition defaults to checks and Timeout to DefaultWaitTimeout.
	PRNumber  int
	PRURL     string
	Condition domain.WaitCondition
	Timeout   time.Duration
	// Duration is a timer's length.
	Duration time.Duration
	Note     string
}

// RegisterResult reports the wait and whether an identical pending one was
// returned instead of creating a second.
type RegisterResult struct {
	Wait     domain.Wait
	Existing bool
}

type headSighting struct {
	sha string
	at  time.Time
}

type resolution struct {
	state   domain.WaitState
	outcome string
	message string
	at      time.Time
}

// Service registers waits and resolves them.
type Service struct {
	store     Store
	deliverer Deliverer
	prs       PRReader
	now       func() time.Time
	newID     func() string
	logger    *slog.Logger
	tick      time.Duration
	wake      chan struct{}

	// mu serializes evaluator passes and guards the state below. Registration
	// never takes it, so a slow wake delivery cannot block `ao wait`.
	mu           sync.Mutex
	cache        map[string]*cachedPR
	backoffUntil time.Time
	heads        map[domain.WaitID]headSighting
	// delivered holds wakes that reached their session but whose resolution
	// is not durable yet, so the next pass records it instead of re-sending.
	delivered map[domain.WaitID]resolution
	lastPrune time.Time
}

// New constructs an idle Service; Start runs its evaluator.
func New(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.NewID == nil {
		d.NewID = func() string { return "w-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8] }
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Tick <= 0 {
		d.Tick = DefaultTick
	}
	return &Service{
		store: d.Store, deliverer: d.Deliverer, prs: d.PRs, now: d.Clock, newID: d.NewID,
		logger: d.Logger, tick: d.Tick, wake: make(chan struct{}, 1),
		cache: map[string]*cachedPR{}, heads: map[domain.WaitID]headSighting{}, delivered: map[domain.WaitID]resolution{},
	}
}

// Register validates and persists one wait for a live session.
func (s *Service) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	if strings.TrimSpace(string(in.SessionID)) == "" {
		return RegisterResult{}, apierr.Invalid("WAIT_SESSION_REQUIRED", "A session id is required", nil)
	}
	session, err := s.liveSession(ctx, in.SessionID)
	if err != nil {
		return RegisterResult{}, err
	}
	note := strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(note) > domain.MaxWaitNoteCharacters {
		return RegisterResult{}, apierr.Invalid("WAIT_NOTE_TOO_LONG", fmt.Sprintf("The note must be at most %d characters", domain.MaxWaitNoteCharacters), nil)
	}
	pending, err := s.store.ListPendingWaitsBySession(ctx, session.ID)
	if err != nil {
		return RegisterResult{}, err
	}
	now := s.now().UTC()
	w := domain.Wait{
		ID: domain.WaitID(s.newID()), SessionID: session.ID, ProjectID: session.ProjectID,
		Kind: in.Kind, Note: note, CreatedAt: now, State: domain.WaitPending,
	}
	switch in.Kind {
	case domain.WaitKindTime:
		if err := validDuration("WAIT_DURATION_INVALID", "duration", in.Duration); err != nil {
			return RegisterResult{}, err
		}
		if len(pending) >= domain.MaxPendingWaitsPerSession {
			return RegisterResult{}, tooManyWaits()
		}
		w.FireAt = now.Add(in.Duration)
		w.ExpiresAt = w.FireAt
	case domain.WaitKindPR:
		if err := s.preparePRWait(&w, session, in); err != nil {
			return RegisterResult{}, err
		}
		for _, p := range pending {
			if p.Kind == domain.WaitKindPR && p.Condition == w.Condition && p.PRNumber == w.PRNumber &&
				(w.PRURL == "" || p.PRURL == "" || strings.EqualFold(p.PRURL, w.PRURL)) {
				return RegisterResult{Wait: p, Existing: true}, nil
			}
		}
		if len(pending) >= domain.MaxPendingWaitsPerSession {
			return RegisterResult{}, tooManyWaits()
		}
		if err := s.baselinePRWait(ctx, &w); err != nil {
			return RegisterResult{}, err
		}
	default:
		return RegisterResult{}, apierr.Invalid("WAIT_KIND_INVALID", "Wait kind must be pr or time", nil)
	}
	created, err := s.store.CreateWait(ctx, w)
	if err != nil {
		return RegisterResult{}, err
	}
	s.Wake()
	return RegisterResult{Wait: created}, nil
}

func tooManyWaits() error {
	return apierr.TooManyRequests("WAIT_LIMIT", fmt.Sprintf("This session already has %d pending waits; cancel one with `ao wait cancel <wait-id>`", domain.MaxPendingWaitsPerSession))
}

func validDuration(code, what string, d time.Duration) error {
	if d < domain.MinWaitDuration || d > domain.MaxWaitDuration {
		return apierr.Invalid(code, fmt.Sprintf("The %s must be between %s and %s", what, domain.MinWaitDuration, roundDuration(domain.MaxWaitDuration)), nil)
	}
	return nil
}

func (s *Service) liveSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, error) {
	session, ok, err := s.store.GetSession(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, err
	}
	if !ok {
		return domain.SessionRecord{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if session.IsTerminated {
		return domain.SessionRecord{}, apierr.Invalid("WAIT_SESSION_TERMINATED", "The session is terminated", nil)
	}
	return session, nil
}

func (s *Service) preparePRWait(w *domain.Wait, session domain.SessionRecord, in RegisterInput) error {
	condition := in.Condition
	if condition == "" {
		condition = domain.WaitForChecks
	}
	if !condition.Valid() {
		return apierr.Invalid("WAIT_CONDITION_INVALID", "--for must be checks, merge, review, or any", nil)
	}
	timeout := in.Timeout
	if timeout == 0 {
		timeout = domain.DefaultWaitTimeout
	}
	if err := validDuration("WAIT_TIMEOUT_INVALID", "timeout", timeout); err != nil {
		return err
	}
	if raw := strings.TrimSpace(in.PRURL); raw != "" {
		ref, err := domain.ParseChangeRequestURL(raw)
		if err != nil {
			return apierr.Invalid("WAIT_PR_INVALID", "The PR must be a number or a complete GitHub PR / GitLab MR URL", nil)
		}
		if in.PRNumber > 0 && in.PRNumber != ref.Number {
			return apierr.Invalid("WAIT_PR_INVALID", "The PR number and URL disagree", nil)
		}
		w.PRNumber, w.PRURL = ref.Number, ref.URL
	} else {
		if in.PRNumber <= 0 {
			return apierr.Invalid("WAIT_PR_INVALID", "A positive PR number is required", nil)
		}
		if session.IsStandalone() {
			return apierr.Invalid("WAIT_PR_URL_REQUIRED", "This session has no project repository; pass the full PR URL", nil)
		}
		w.PRNumber = in.PRNumber
	}
	w.Condition = condition
	w.ExpiresAt = w.CreatedAt.Add(timeout)
	return nil
}

// baselinePRWait resolves the PR and records what is already known about it,
// so the wait only reports what happens after the agent asked. A PR no live
// session tracks is read from the provider once here, which also proves it
// exists; a transient read failure still registers the wait, and the first
// later observation becomes its baseline.
func (s *Service) baselinePRWait(ctx context.Context, w *domain.Wait) error {
	t, err := s.resolveTarget(ctx, *w)
	if err != nil {
		return err
	}
	if t.url != "" {
		w.PRURL = t.url
	}
	needReviews := w.Condition.NeedsReviews()
	switch {
	case t.hasRow && t.live:
		snap, err := s.storedSnapshot(ctx, t.tracked)
		if err != nil {
			return err
		}
		captureBaseline(&w.Baseline, snap, needReviews)
	case t.hasRef:
		snap, ok, err := s.readNow(ctx, t, needReviews)
		var apiErr *apierr.Error
		if errors.As(err, &apiErr) {
			return err
		}
		if err != nil {
			s.logger.Warn("wait: provider read at registration failed; baselining later", "pr", w.PRNumber, "err", err)
		}
		if ok {
			captureBaseline(&w.Baseline, snap, needReviews)
			w.PRURL = firstNonEmpty(w.PRURL, snap.URL)
		}
	default:
		msg := fmt.Sprintf("AO cannot observe PR #%d: no live AO session in this project tracks it and AO has no GitHub or GitLab credentials to read it", w.PRNumber)
		if s.prs != nil && t.baseErr != nil {
			msg = fmt.Sprintf("AO cannot observe PR #%d: %v", w.PRNumber, t.baseErr)
		}
		return apierr.Invalid("WAIT_PR_UNOBSERVABLE", msg, nil)
	}
	return nil
}

// List returns a live session's pending waits, oldest first.
func (s *Service) List(ctx context.Context, sessionID domain.SessionID) ([]domain.Wait, error) {
	if _, ok, err := s.store.GetSession(ctx, sessionID); err != nil {
		return nil, err
	} else if !ok {
		return nil, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	return s.store.ListPendingWaitsBySession(ctx, sessionID)
}

// Cancel resolves one of the session's pending waits without waking it.
func (s *Service) Cancel(ctx context.Context, sessionID domain.SessionID, id domain.WaitID) (domain.Wait, error) {
	w, ok, err := s.store.GetWait(ctx, id)
	if err != nil {
		return domain.Wait{}, err
	}
	if !ok || w.SessionID != sessionID {
		return domain.Wait{}, apierr.NotFound("WAIT_NOT_FOUND", "Unknown wait for this session")
	}
	if w.State == domain.WaitPending {
		resolved, err := s.store.ResolveWait(ctx, id, domain.WaitCancelled, domain.WaitOutcomeCancelled, "", s.now().UTC())
		if err != nil {
			return domain.Wait{}, err
		}
		if resolved {
			w, _, err = s.store.GetWait(ctx, id)
			return w, err
		}
		if w, _, err = s.store.GetWait(ctx, id); err != nil {
			return domain.Wait{}, err
		}
	}
	return domain.Wait{}, apierr.Conflict("WAIT_NOT_PENDING", fmt.Sprintf("Wait %s already resolved (%s)", id, w.State), map[string]any{"state": string(w.State)})
}

// Wake asks the evaluator to run a pass now.
func (s *Service) Wake() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start runs the evaluator until ctx is cancelled and returns its completion
// signal.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			if err := s.RunDue(ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("wait: evaluator pass failed", "err", err)
			}
			timer.Reset(s.tick)
		}
	}()
	return done
}

// RunDue runs one evaluator pass over every pending wait. Tests call it
// directly with a fake clock.
func (s *Service) RunDue(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if now.Sub(s.lastPrune) >= pruneEvery {
		if _, err := s.store.PruneResolvedWaits(ctx, now.Add(-resolvedRetention)); err != nil {
			s.logger.Warn("wait: prune resolved waits failed", "err", err)
		}
		s.lastPrune = now
	}
	waits, err := s.store.ListPendingWaits(ctx)
	if err != nil {
		return err
	}
	sessions := map[domain.SessionID]domain.SessionRecord{}
	targets := map[domain.WaitID]prTarget{}
	needReviews := map[string]bool{}
	pendingIDs := map[domain.WaitID]bool{}
	var due []domain.Wait
	for _, w := range waits {
		pendingIDs[w.ID] = true
		if r, ok := s.delivered[w.ID]; ok {
			s.finish(ctx, w, r)
			continue
		}
		session, ok := sessions[w.SessionID]
		if !ok {
			rec, found, err := s.store.GetSession(ctx, w.SessionID)
			if err != nil {
				return err
			}
			if !found {
				rec = domain.SessionRecord{ID: w.SessionID, IsTerminated: true}
			}
			sessions[w.SessionID], session = rec, rec
		}
		if session.IsTerminated {
			s.finish(ctx, w, resolution{state: domain.WaitCancelled, outcome: domain.WaitOutcomeSessionEnded, at: now})
			continue
		}
		if w.Kind == domain.WaitKindPR {
			t, err := s.resolveTarget(ctx, w)
			if err != nil {
				s.logger.Warn("wait: resolve PR target failed", "wait", w.ID, "err", err)
				continue
			}
			targets[w.ID] = t
			if t.needsProvider() && w.Condition.NeedsReviews() {
				needReviews[t.refKey] = true
			}
		}
		due = append(due, w)
	}
	s.refreshProvider(ctx, targets, needReviews, now)
	for _, w := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch w.Kind {
		case domain.WaitKindTime:
			if !now.Before(w.FireAt) {
				s.fire(ctx, w, domain.WaitFired, domain.WaitOutcomeTimer, renderTimerMessage(w), now)
			}
		case domain.WaitKindPR:
			s.evaluatePRWait(ctx, w, targets[w.ID], now)
		}
	}
	s.forget(pendingIDs, targets)
	return nil
}

func (s *Service) evaluatePRWait(ctx context.Context, w domain.Wait, t prTarget, now time.Time) {
	expired := !now.Before(w.ExpiresAt)
	snap, ok, err := s.snapshot(ctx, t)
	if err != nil {
		s.logger.Warn("wait: read PR facts failed", "wait", w.ID, "err", err)
		return
	}
	if !ok {
		if expired {
			s.fire(ctx, w, domain.WaitExpired, domain.WaitOutcomeExpired, renderUnobservedExpiry(w, now), now)
		}
		return
	}
	resolvedURL := w.PRURL == "" && snap.URL != ""
	if captureBaseline(&w.Baseline, snap, w.Condition.NeedsReviews()) || resolvedURL {
		w.PRURL = firstNonEmpty(w.PRURL, snap.URL)
		if _, err := s.store.UpdateWaitTarget(ctx, w.ID, w.PRURL, w.Baseline); err != nil {
			s.logger.Warn("wait: persist baseline failed", "wait", w.ID, "err", err)
		}
	}
	ev := evaluatePR(w, snap, now, s.headSince(w, snap.HeadSHA, now))
	if !ev.Fire && !expired {
		return
	}
	state, outcome := domain.WaitFired, ev.Outcome
	if !ev.Fire {
		state, outcome = domain.WaitExpired, domain.WaitOutcomeExpired
	}
	if !snap.Merged && !snap.Closed && len(ev.Checks.FailedChecks) > 0 {
		s.fillLogTails(ctx, t, &ev.Checks)
	}
	s.fire(ctx, w, state, outcome, renderPRMessage(w, snap, ev, !ev.Fire, now), now)
}

// headSince is when this evaluator first saw head for w. The baseline head
// counts from registration; a new head counts from its first sighting.
func (s *Service) headSince(w domain.Wait, head string, now time.Time) time.Time {
	h, ok := s.heads[w.ID]
	if ok && strings.EqualFold(h.sha, head) {
		return h.at
	}
	at := now
	if !ok && strings.EqualFold(head, w.Baseline.HeadSHA) {
		at = w.CreatedAt
	}
	s.heads[w.ID] = headSighting{sha: head, at: at}
	return at
}

// fire delivers w's wake and resolves it. A wake the session cannot take now
// stays pending and is retried on the next pass.
func (s *Service) fire(ctx context.Context, w domain.Wait, state domain.WaitState, outcome, msg string, now time.Time) {
	if s.deliverer == nil {
		return
	}
	deliverCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	res, err := s.deliverer.DeliverAutomationMessage(deliverCtx, w.SessionID, msg)
	cancel()
	switch res {
	case sessionguard.Sent:
		if err != nil {
			// The write was attempted; re-sending could duplicate the wake.
			s.logger.Warn("wait: wake delivery reported an error after writing", "wait", w.ID, "err", err)
		}
		r := resolution{state: state, outcome: outcome, message: msg, at: now}
		s.delivered[w.ID] = r
		s.finish(ctx, w, r)
	case sessionguard.SuppressedTerminated, sessionguard.SuppressedNotFound:
		s.finish(ctx, w, resolution{state: domain.WaitCancelled, outcome: domain.WaitOutcomeSessionEnded, at: now})
	default:
		reason := res.String()
		if err != nil {
			reason += ": " + err.Error()
		}
		if recErr := s.store.RecordWaitDeliveryFailure(ctx, w.ID, truncateBytes(reason, 500)); recErr != nil {
			s.logger.Warn("wait: record delivery failure failed", "wait", w.ID, "err", recErr)
		}
		deadline := w.ExpiresAt
		if w.Kind == domain.WaitKindTime {
			deadline = w.FireAt
		}
		if now.After(deadline.Add(undeliverableGrace)) {
			s.finish(ctx, w, resolution{state: domain.WaitExpired, outcome: domain.WaitOutcomeUndelivered, message: msg, at: now})
		}
	}
}

// finish records a resolution. A failed write keeps a delivered wake in
// s.delivered, so the next pass retries the write without re-sending.
func (s *Service) finish(ctx context.Context, w domain.Wait, r resolution) {
	if _, err := s.store.ResolveWait(ctx, w.ID, r.state, r.outcome, r.message, r.at); err != nil {
		s.logger.Warn("wait: record resolution failed; retrying next pass", "wait", w.ID, "err", err)
		return
	}
	delete(s.delivered, w.ID)
	delete(s.heads, w.ID)
}

// forget drops evaluator state for waits that are no longer pending and
// provider reads no pending wait needs.
func (s *Service) forget(pending map[domain.WaitID]bool, targets map[domain.WaitID]prTarget) {
	for id := range s.heads {
		if !pending[id] {
			delete(s.heads, id)
		}
	}
	for id := range s.delivered {
		if !pending[id] {
			delete(s.delivered, id)
		}
	}
	used := map[string]bool{}
	for _, t := range targets {
		if t.needsProvider() {
			used[t.refKey] = true
		}
	}
	for key := range s.cache {
		if !used[key] {
			delete(s.cache, key)
		}
	}
}
