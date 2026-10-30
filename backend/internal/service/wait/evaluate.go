package wait

import (
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// settleWindow is how long a checks wait distrusts a result AO already
	// had at registration. A head whose checks had finished when the agent
	// asked may be the head before a push the SCM observer has not reported
	// yet, so that result resolves the wait only after the observer had
	// several polls to report a newer head.
	settleWindow = 90 * time.Second
	// noChecksGrace is how long a head may report no checks at all before a
	// checks wait resolves with "no checks reported" instead of waiting for
	// CI that the repository may simply not run.
	noChecksGrace = 5 * time.Minute
	// reviewLookback bounds how old a review AO had not seen at registration
	// may be and still count as new: older ones were simply not loaded yet.
	reviewLookback = 15 * time.Minute
)

// prSnapshot is one point-in-time view of a pull request, read either from the
// facts the SCM observer persists or from a direct provider fetch.
type prSnapshot struct {
	URL            string
	Number         int
	Title          string
	Author         string
	HeadSHA        string
	SourceBranch   string
	TargetBranch   string
	MergeCommitSHA string
	Draft          bool
	Merged         bool
	Closed         bool
	CI             domain.CIState
	Review         domain.ReviewDecision
	Mergeability   domain.Mergeability
	// Checks are the checks reported for HeadSHA.
	Checks   []checkFact
	Reviews  []reviewFact
	Comments []commentFact
	// ReviewsLoaded is false when no review-thread observation backs Reviews
	// and Comments, so their absence says nothing.
	ReviewsLoaded bool
	// ObservedAt is when these facts were last known to be current.
	ObservedAt time.Time
}

type checkFact struct {
	Name        string
	Status      domain.PRCheckStatus
	URL         string
	Description string
	LogTail     string
	Required    bool
	// ProviderID lets AO fetch a failed check's log tail on demand.
	ProviderID string
}

type reviewFact struct {
	ID          string
	Author      string
	State       domain.ReviewDecision
	Body        string
	URL         string
	SubmittedAt time.Time
}

type commentFact struct {
	ID     string
	Author string
	File   string
	Line   int
	Body   string
	URL    string
}

// checkSummary rolls up the checks on a snapshot's head.
type checkSummary struct {
	Total, Passed, Failed, Skipped, Running int
	// Done is true once every check that gates the PR is terminal: the
	// required checks when the provider marks any, otherwise all of them.
	Done bool
	// None is true when the head reports no checks at all.
	None bool
	// FailedChecks are the failed or cancelled checks, gating ones first.
	FailedChecks []checkFact
	// RunningOptional counts non-required checks still running.
	RunningOptional int
	// GateFailed is true when a gating check failed or was cancelled.
	GateFailed bool
}

func terminalCheck(status domain.PRCheckStatus) bool {
	return status != domain.PRCheckQueued && status != domain.PRCheckInProgress
}

func failedCheck(status domain.PRCheckStatus) bool {
	return status == domain.PRCheckFailed || status == domain.PRCheckCancelled
}

func summarizeChecks(checks []checkFact) checkSummary {
	var sum checkSummary
	sum.Total = len(checks)
	if len(checks) == 0 {
		sum.None = true
		return sum
	}
	anyRequired := slices.ContainsFunc(checks, func(c checkFact) bool { return c.Required })
	gating := func(c checkFact) bool { return !anyRequired || c.Required }
	sum.Done = true
	var optionalFailed []checkFact
	for _, c := range checks {
		switch {
		case failedCheck(c.Status):
			sum.Failed++
			if gating(c) {
				sum.GateFailed = true
				sum.FailedChecks = append(sum.FailedChecks, c)
			} else {
				optionalFailed = append(optionalFailed, c)
			}
		case c.Status == domain.PRCheckSkipped:
			sum.Skipped++
		case !terminalCheck(c.Status):
			sum.Running++
			if gating(c) {
				sum.Done = false
			} else {
				sum.RunningOptional++
			}
		default:
			sum.Passed++
		}
	}
	sum.FailedChecks = append(sum.FailedChecks, optionalFailed...)
	return sum
}

// evaluation is the verdict for one wait against one snapshot.
type evaluation struct {
	Fire    bool
	Outcome string
	Checks  checkSummary
	// NewReviews and NewComments are the review activity since registration.
	NewReviews  []reviewFact
	NewComments []commentFact
}

// evaluatePR decides whether a PR wait resolves now. headSince is when this
// evaluator first saw the snapshot's head for this wait.
func evaluatePR(w domain.Wait, snap prSnapshot, now, headSince time.Time) evaluation {
	ev := evaluation{Checks: summarizeChecks(snap.Checks)}
	ev.NewReviews, ev.NewComments = newReviewActivity(w, snap)
	// Every PR wait resolves when the PR leaves the open state.
	if snap.Merged {
		ev.Fire, ev.Outcome = true, domain.WaitOutcomeMerged
		return ev
	}
	if snap.Closed {
		ev.Fire, ev.Outcome = true, domain.WaitOutcomeClosed
		return ev
	}
	if w.Condition.NeedsReviews() && (len(ev.NewReviews) > 0 || len(ev.NewComments) > 0) {
		ev.Fire, ev.Outcome = true, domain.WaitOutcomeReview
		return ev
	}
	if w.Condition == domain.WaitForChecks || w.Condition == domain.WaitForAny {
		if outcome, ok := checksOutcome(w, snap, ev.Checks, now, headSince); ok {
			ev.Fire, ev.Outcome = true, outcome
		}
	}
	return ev
}

func checksOutcome(w domain.Wait, snap prSnapshot, sum checkSummary, now, headSince time.Time) (string, bool) {
	newHead := w.Baseline.HeadSHA != "" && snap.HeadSHA != "" && !strings.EqualFold(snap.HeadSHA, w.Baseline.HeadSHA)
	switch {
	case sum.Done:
		if !newHead && w.Baseline.ChecksDone && now.Sub(w.CreatedAt) < settleWindow {
			return "", false
		}
		if sum.GateFailed {
			return domain.WaitOutcomeChecksFailed, true
		}
		return domain.WaitOutcomeChecksPassed, true
	case sum.None:
		if now.Sub(headSince) >= noChecksGrace && now.Sub(w.CreatedAt) >= noChecksGrace {
			return domain.WaitOutcomeNoChecks, true
		}
	}
	return "", false
}

// newReviewActivity returns the reviews and review comments that arrived after
// the wait's baseline. A review is new when AO had not seen its id at
// registration or the provider stamped it after registration. The PR author's
// own thread replies (comments, and the empty-body reviews that carry them)
// are the agent's own bookkeeping, not review feedback, so they never count;
// a review the author submits with a body (AO's own reviewer posts as the
// signed-in user) still does.
func newReviewActivity(w domain.Wait, snap prSnapshot) ([]reviewFact, []commentFact) {
	if !snap.ReviewsLoaded || !w.Baseline.ReviewsCaptured {
		return nil, nil
	}
	author := strings.TrimSpace(snap.Author)
	ownedByAuthor := func(login string) bool {
		return author != "" && strings.EqualFold(strings.TrimSpace(login), author)
	}
	var reviews []reviewFact
	for _, r := range snap.Reviews {
		if r.ID == "" {
			continue
		}
		submitted := !r.SubmittedAt.IsZero()
		if slices.Contains(w.Baseline.ReviewIDs, r.ID) && (!submitted || r.SubmittedAt.Before(w.CreatedAt)) {
			continue
		}
		if submitted && r.SubmittedAt.Before(w.CreatedAt.Add(-reviewLookback)) {
			continue
		}
		if ownedByAuthor(r.Author) && strings.TrimSpace(r.Body) == "" {
			continue
		}
		reviews = append(reviews, r)
	}
	var comments []commentFact
	for _, c := range snap.Comments {
		if c.ID == "" || slices.Contains(w.Baseline.CommentIDs, c.ID) || ownedByAuthor(c.Author) {
			continue
		}
		comments = append(comments, c)
	}
	return reviews, comments
}

// captureBaseline fills what the wait's baseline still lacks from snap: the
// head and its check state once, and the known review and comment ids once
// review data is loaded. It reports whether anything changed.
func captureBaseline(b *domain.WaitBaseline, snap prSnapshot, needReviews bool) bool {
	changed := false
	if !b.Captured {
		b.Captured, b.HeadSHA, b.ChecksDone = true, snap.HeadSHA, summarizeChecks(snap.Checks).Done
		changed = true
	}
	if needReviews && !b.ReviewsCaptured && snap.ReviewsLoaded {
		b.ReviewsCaptured = true
		for _, r := range snap.Reviews {
			if r.ID != "" {
				b.ReviewIDs = append(b.ReviewIDs, r.ID)
			}
		}
		for _, c := range snap.Comments {
			if c.ID != "" {
				b.CommentIDs = append(b.CommentIDs, c.ID)
			}
		}
		changed = true
	}
	return changed
}
