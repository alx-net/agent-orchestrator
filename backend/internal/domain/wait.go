package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// WaitID identifies one registered `ao wait`.
type WaitID string

// WaitKind is what a wait is for: a pull request or a timer.
type WaitKind string

// Wait kinds.
const (
	WaitKindPR   WaitKind = "pr"
	WaitKindTime WaitKind = "time"
)

// WaitCondition is the PR event a PR wait resolves on.
type WaitCondition string

// Wait conditions. Every PR wait also resolves when the PR merges or closes.
const (
	// WaitForChecks resolves once every check on the PR's current head has
	// reached a terminal state.
	WaitForChecks WaitCondition = "checks"
	// WaitForMerge resolves once the PR is merged or closed.
	WaitForMerge WaitCondition = "merge"
	// WaitForReview resolves on the first review or review comment that
	// arrives after the wait was registered.
	WaitForReview WaitCondition = "review"
	// WaitForAny resolves on whichever of the above happens first.
	WaitForAny WaitCondition = "any"
)

// Valid reports whether c is a known PR wait condition.
func (c WaitCondition) Valid() bool {
	switch c {
	case WaitForChecks, WaitForMerge, WaitForReview, WaitForAny:
		return true
	}
	return false
}

// WaitState is a wait's durable lifecycle.
type WaitState string

// Wait states. Only pending waits are evaluated; every other state is final.
const (
	WaitPending   WaitState = "pending"
	WaitFired     WaitState = "fired"
	WaitExpired   WaitState = "expired"
	WaitCancelled WaitState = "cancelled"
)

// Wait outcomes record why a wait resolved.
const (
	WaitOutcomeChecksPassed = "checks_passed"
	WaitOutcomeChecksFailed = "checks_failed"
	WaitOutcomeNoChecks     = "no_checks"
	WaitOutcomeMerged       = "merged"
	WaitOutcomeClosed       = "closed"
	WaitOutcomeReview       = "review"
	WaitOutcomeTimer        = "timer"
	WaitOutcomeExpired      = "expired"
	WaitOutcomeCancelled    = "cancelled"
	WaitOutcomeSessionEnded = "session_ended"
	WaitOutcomeUndelivered  = "undeliverable"
)

// Wait limits.
const (
	// DefaultWaitTimeout bounds a PR wait that sets no --timeout.
	DefaultWaitTimeout = 24 * time.Hour
	// MinWaitDuration is the shortest timer or timeout AO accepts: shorter
	// waits are cheaper as a plain sleep than as a turn.
	MinWaitDuration = 30 * time.Second
	// MaxWaitDuration is the longest timer or timeout AO accepts.
	MaxWaitDuration = 7 * 24 * time.Hour
	// MaxWaitNoteCharacters bounds the note echoed back in the wake message.
	MaxWaitNoteCharacters = 500
	// MaxPendingWaitsPerSession bounds one session's outstanding waits.
	MaxPendingWaitsPerSession = 20
)

// WaitBaseline is what AO knew about the PR when the wait was registered, so
// "new" means new since the agent asked. Captured is false when the PR was not
// yet observable then; the first observation becomes the baseline. Review ids
// are captured separately, once review data is loaded, because a PR's review
// threads are fetched on a slower cadence than its head and checks.
type WaitBaseline struct {
	Captured bool   `json:"captured,omitempty"`
	HeadSHA  string `json:"headSha,omitempty"`
	// ChecksDone records that the head's checks had already finished at
	// registration. Such a result may predate a push the observer has not seen
	// yet, so it only resolves the wait after a settle window.
	ChecksDone      bool     `json:"checksDone,omitempty"`
	ReviewsCaptured bool     `json:"reviewsCaptured,omitempty"`
	ReviewIDs       []string `json:"reviewIds,omitempty"`
	CommentIDs      []string `json:"commentIds,omitempty"`
}

// NeedsReviews reports whether the condition depends on review activity.
func (c WaitCondition) NeedsReviews() bool {
	return c == WaitForReview || c == WaitForAny
}

// Wait is one registered zero-token wait.
type Wait struct {
	ID        WaitID
	SessionID SessionID
	ProjectID ProjectID
	Kind      WaitKind
	// Condition and PRNumber are set for PR waits only.
	Condition WaitCondition
	PRNumber  int
	// PRURL is the PR's canonical URL once AO resolved it.
	PRURL    string
	Note     string
	Baseline WaitBaseline
	// FireAt is a timer's deadline; zero for PR waits.
	FireAt time.Time
	// ExpiresAt is when an unresolved PR wait gives up.
	ExpiresAt time.Time
	CreatedAt time.Time

	State            WaitState
	Outcome          string
	Message          string
	ResolvedAt       time.Time
	DeliveryAttempts int
	LastError        string
}

// ErrInvalidWait reports a wait whose fields cannot be persisted.
var ErrInvalidWait = errors.New("invalid wait")

// Validate checks the invariants the session_waits table also enforces.
func (w Wait) Validate() error {
	if strings.TrimSpace(string(w.ID)) == "" || strings.TrimSpace(string(w.SessionID)) == "" {
		return ErrInvalidWait
	}
	if utf8.RuneCountInString(w.Note) > MaxWaitNoteCharacters || w.CreatedAt.IsZero() || w.ExpiresAt.IsZero() {
		return ErrInvalidWait
	}
	switch w.Kind {
	case WaitKindPR:
		if !w.Condition.Valid() || w.PRNumber <= 0 || !w.FireAt.IsZero() {
			return ErrInvalidWait
		}
	case WaitKindTime:
		if w.Condition != "" || w.PRNumber != 0 || w.FireAt.IsZero() {
			return ErrInvalidWait
		}
	default:
		return ErrInvalidWait
	}
	return nil
}
