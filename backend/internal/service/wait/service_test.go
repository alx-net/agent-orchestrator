package wait

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const testPRURL = "https://github.com/acme/app/pull/5807"

type sentMessage struct {
	session domain.SessionID
	msg     string
}

// fakeDeliverer records wakes. Outcomes queued in next are returned first;
// after that every delivery is Sent.
type fakeDeliverer struct {
	mu   sync.Mutex
	next []sessionguard.Outcome
	sent []sentMessage
}

func (f *fakeDeliverer) DeliverAutomationMessage(_ context.Context, id domain.SessionID, msg string) (sessionguard.Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.next) > 0 {
		outcome := f.next[0]
		f.next = f.next[1:]
		if outcome != sessionguard.Sent {
			return outcome, nil
		}
	}
	f.sent = append(f.sent, sentMessage{session: id, msg: msg})
	return sessionguard.Sent, nil
}

func (f *fakeDeliverer) messages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMessage(nil), f.sent...)
}

// fakePRs is a provider holding one observation per PR number.
type fakePRs struct {
	mu            sync.Mutex
	obs           map[int]ports.SCMObservation
	reviews       ports.SCMReviewObservation
	fetches       int
	reviewFetches int
	logFetches    int
}

func (f *fakePRs) ParseRepository(remote string) (ports.SCMRepo, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSuffix(remote, ".git"), "https://github.com/")
	owner, name, found := strings.Cut(rest, "/")
	if !ok || !found {
		return ports.SCMRepo{}, false
	}
	return ports.SCMRepo{Provider: "github", Host: "github.com", Owner: owner, Name: name, Repo: owner + "/" + name}, true
}

func (f *fakePRs) FetchPullRequests(_ context.Context, refs []ports.SCMPRRef) ([]ports.SCMObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetches++
	out := make([]ports.SCMObservation, len(refs))
	for i, ref := range refs {
		if obs, ok := f.obs[ref.Number]; ok {
			out[i] = obs
			continue
		}
		out[i] = ports.SCMObservation{Error: ports.ErrSCMNotFound}
	}
	return out, nil
}

func (f *fakePRs) FetchFailedCheckLogTail(_ context.Context, _ ports.SCMRepo, check ports.SCMCheckObservation) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logFetches++
	return "provider log for " + check.Name + "\nexit status 1", nil
}

func (f *fakePRs) FetchReviewThreads(context.Context, ports.SCMPRRef) (ports.SCMReviewObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewFetches++
	return f.reviews, nil
}

func (f *fakePRs) set(number int, obs ports.SCMObservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.obs[number] = obs
}

func (f *fakePRs) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetches, f.reviewFetches, f.logFetches
}

type harness struct {
	t       *testing.T
	ctx     context.Context
	store   *sqlite.Store
	now     time.Time
	deliver *fakeDeliverer
	prs     *fakePRs
	svc     *Service
	session domain.SessionRecord
	ids     int
}

func newHarness(t *testing.T, withProvider bool) *harness {
	t.Helper()
	return newHarnessOn(t, sqlitetest.MustOpen(t), withProvider, true)
}

func newHarnessOn(t *testing.T, st *sqlite.Store, withProvider, seed bool) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), store: st, now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), deliver: &fakeDeliverer{}}
	if seed {
		if err := st.UpsertProject(h.ctx, domain.ProjectRecord{
			ID: "app", Path: "/tmp/app", RepoOriginURL: "https://github.com/acme/app", RegisteredAt: h.now,
		}); err != nil {
			t.Fatal(err)
		}
		sess, err := st.CreateSession(h.ctx, domain.SessionRecord{
			ProjectID: "app", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
			Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: h.now},
			Metadata:  domain.SessionMetadata{Branch: "feat/wait", WorkspacePath: "/ws"},
			CreatedAt: h.now, UpdatedAt: h.now,
		})
		if err != nil {
			t.Fatal(err)
		}
		h.session = sess
	}
	h.newService(withProvider)
	return h
}

func (h *harness) newService(withProvider bool) {
	deps := Deps{
		Store: h.store, Deliverer: h.deliver, Clock: func() time.Time { return h.now },
		NewID: func() string { h.ids++; return fmt.Sprintf("w-%04d", h.ids) },
	}
	if withProvider {
		if h.prs == nil {
			h.prs = &fakePRs{obs: map[int]ports.SCMObservation{}}
		}
		deps.PRs = h.prs
	}
	h.svc = New(deps)
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) run() {
	h.t.Helper()
	if err := h.svc.RunDue(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) register(in RegisterInput) domain.Wait {
	h.t.Helper()
	if in.SessionID == "" {
		in.SessionID = h.session.ID
	}
	res, err := h.svc.Register(h.ctx, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Wait
}

type prFacts struct {
	head     string
	checks   []domain.PullRequestCheck
	merged   bool
	closed   bool
	reviews  []domain.PullRequestReview
	comments []domain.PullRequestComment
	review   domain.ReviewDecision
}

// writePR persists PR facts the way the SCM observer does, owned by the
// harness session so the observer counts as keeping them current.
func (h *harness) writePR(f prFacts) {
	h.t.Helper()
	pr := domain.PullRequest{
		URL: testPRURL, SessionID: h.session.ID, Number: 5807, Provider: "github", Host: "github.com", Repo: "acme/app",
		Title: "Add ao wait", SourceBranch: "feat/wait", TargetBranch: "main", Author: "alex", HeadSHA: f.head,
		Merged: f.merged, Closed: f.closed, CI: domain.CIPending, Review: domain.ReviewNone, Mergeability: domain.MergeBlocked,
		UpdatedAt: h.now, ObservedAt: h.now, CIObservedAt: h.now, ReviewObservedAt: h.now,
	}
	if f.review != "" {
		pr.Review = f.review
	}
	if f.merged {
		pr.MergeCommitSHA = "feedface00000000000000000000000000000000"
	}
	checks := make([]domain.PullRequestCheck, len(f.checks))
	for i, c := range f.checks {
		c.CommitHash = f.head
		c.CreatedAt = h.now
		checks[i] = c
	}
	var threads []domain.PullRequestReviewThread
	for _, c := range f.comments {
		threads = append(threads, domain.PullRequestReviewThread{ThreadID: c.ThreadID, Path: c.File, Line: c.Line, UpdatedAt: h.now})
	}
	if err := h.store.WriteSCMObservation(h.ctx, pr, checks, f.reviews, threads, f.comments, ports.ReviewWriteReplace); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) wait(id domain.WaitID) domain.Wait {
	h.t.Helper()
	w, ok, err := h.store.GetWait(h.ctx, id)
	if err != nil || !ok {
		h.t.Fatalf("get wait %s: ok=%v err=%v", id, ok, err)
	}
	return w
}

func check(name string, status domain.PRCheckStatus) domain.PullRequestCheck {
	return domain.PullRequestCheck{Name: name, Status: status, URL: "https://github.com/acme/app/actions/runs/1/job/" + name}
}

func comment(id, author, file string, line int, body string) domain.PullRequestComment {
	return domain.PullRequestComment{ID: id, ThreadID: "t-" + id, Author: author, File: file, Line: line, Body: body, URL: "https://github.com/acme/app/pull/5807#discussion_" + id}
}

func apiCode(err error) string {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestRegisterValidation(t *testing.T) {
	h := newHarness(t, false)
	terminated := h.session
	terminated.ID = ""
	terminated.Metadata.Branch = "feat/other"
	created, err := h.store.CreateSession(h.ctx, terminated)
	if err != nil {
		t.Fatal(err)
	}
	created.IsTerminated = true
	if err := h.store.UpdateSession(h.ctx, created); err != nil {
		t.Fatal(err)
	}
	standalone, err := h.store.CreateSession(h.ctx, domain.SessionRecord{
		Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Activity: domain.Activity{State: domain.ActivityIdle},
		CreatedAt: h.now, UpdatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		in   RegisterInput
		code string
	}{
		{"missing session", RegisterInput{Kind: domain.WaitKindTime, Duration: time.Minute}, "WAIT_SESSION_REQUIRED"},
		{"unknown session", RegisterInput{SessionID: "nope-1", Kind: domain.WaitKindTime, Duration: time.Minute}, "SESSION_NOT_FOUND"},
		{"terminated session", RegisterInput{SessionID: created.ID, Kind: domain.WaitKindTime, Duration: time.Minute}, "WAIT_SESSION_TERMINATED"},
		{"unknown kind", RegisterInput{SessionID: h.session.ID, Kind: "sleep"}, "WAIT_KIND_INVALID"},
		{"timer too short", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindTime, Duration: time.Second}, "WAIT_DURATION_INVALID"},
		{"timer too long", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindTime, Duration: 30 * 24 * time.Hour}, "WAIT_DURATION_INVALID"},
		{"note too long", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindTime, Duration: time.Minute, Note: strings.Repeat("x", 501)}, "WAIT_NOTE_TOO_LONG"},
		{"bad condition", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRNumber: 5807, Condition: "deploy"}, "WAIT_CONDITION_INVALID"},
		{"bad timeout", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRNumber: 5807, Timeout: time.Second}, "WAIT_TIMEOUT_INVALID"},
		{"no pr", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR}, "WAIT_PR_INVALID"},
		{"bad url", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRURL: "https://example.com/x"}, "WAIT_PR_INVALID"},
		{"standalone needs url", RegisterInput{SessionID: standalone.ID, Kind: domain.WaitKindPR, PRNumber: 5807}, "WAIT_PR_URL_REQUIRED"},
		{"untracked without provider", RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRNumber: 77}, "WAIT_PR_UNOBSERVABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.Register(h.ctx, tc.in)
			if got := apiCode(err); got != tc.code {
				t.Fatalf("code = %q (err %v), want %q", got, err, tc.code)
			}
		})
	}
}

func TestRegisterListCancel(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckInProgress)}})
	pr := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Note: "after the flaky-test fix"})
	timer := h.register(RegisterInput{Kind: domain.WaitKindTime, Duration: 20 * time.Minute})
	if pr.PRURL != testPRURL || pr.Condition != domain.WaitForChecks || !pr.ExpiresAt.Equal(h.now.Add(24*time.Hour)) {
		t.Fatalf("pr wait = %+v", pr)
	}
	if !pr.Baseline.Captured || pr.Baseline.HeadSHA != "aaaa1111" || pr.Baseline.ChecksDone {
		t.Fatalf("baseline = %+v", pr.Baseline)
	}
	if !timer.FireAt.Equal(h.now.Add(20 * time.Minute)) {
		t.Fatalf("timer fireAt = %v", timer.FireAt)
	}

	listed, err := h.svc.List(h.ctx, h.session.ID)
	if err != nil || len(listed) != 2 || listed[0].ID != pr.ID || listed[1].ID != timer.ID {
		t.Fatalf("list = %+v err=%v", listed, err)
	}
	cancelled, err := h.svc.Cancel(h.ctx, h.session.ID, pr.ID)
	if err != nil || cancelled.State != domain.WaitCancelled || cancelled.ResolvedAt.IsZero() {
		t.Fatalf("cancel = %+v err=%v", cancelled, err)
	}
	if _, err := h.svc.Cancel(h.ctx, h.session.ID, pr.ID); apiCode(err) != "WAIT_NOT_PENDING" {
		t.Fatalf("second cancel err = %v", err)
	}
	if _, err := h.svc.Cancel(h.ctx, "app-99", timer.ID); apiCode(err) != "WAIT_NOT_FOUND" {
		t.Fatalf("cross-session cancel err = %v", err)
	}
	listed, _ = h.svc.List(h.ctx, h.session.ID)
	if len(listed) != 1 || listed[0].ID != timer.ID {
		t.Fatalf("list after cancel = %+v", listed)
	}
	// A cancelled wait never wakes the session.
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckPassed)}})
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("cancelled wait delivered %+v", got)
	}
}

func TestRegisterReturnsExistingPendingWait(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111"})
	first := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807})
	res, err := h.svc.Register(h.ctx, RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRURL: testPRURL})
	if err != nil || !res.Existing || res.Wait.ID != first.ID {
		t.Fatalf("re-register = %+v err=%v", res, err)
	}
	other, err := h.svc.Register(h.ctx, RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRNumber: 5807, Condition: domain.WaitForMerge})
	if err != nil || other.Existing {
		t.Fatalf("different condition = %+v err=%v", other, err)
	}
}

func TestChecksWaitFiresOnceWhenHeadChecksFinish(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111bbbb", checks: []domain.PullRequestCheck{
		check("lint", domain.PRCheckPassed), check("backend-test", domain.PRCheckInProgress),
	}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Note: "rerun after fix"})

	h.advance(30 * time.Second)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("delivered while CI running: %+v", got)
	}

	h.advance(30 * time.Second)
	failed := check("backend-test", domain.PRCheckFailed)
	failed.LogTail = "--- FAIL: TestWait (0.01s)\n    wait_test.go:12: boom\nFAIL"
	h.writePR(prFacts{head: "aaaa1111bbbb", checks: []domain.PullRequestCheck{check("lint", domain.PRCheckPassed), failed, check("docs", domain.PRCheckSkipped)}})
	h.run()
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly one", len(got))
	}
	msg := got[0].msg
	for _, want := range []string{
		"[AO wait " + string(w.ID) + "]", `CI finished on PR #5807 "Add ao wait" (feat/wait → main): FAILED`,
		"head aaaa1111", "3 checks: 1 passed, 1 failed, 1 skipped", "merge state: blocked",
		"Your note: rerun after fix", "Failed: backend-test (failed)", "--- FAIL: TestWait", "wait_test.go:12: boom",
		"PR: " + testPRURL,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if len(msg) > maxMessageBytes {
		t.Fatalf("message is %d bytes", len(msg))
	}
	resolved := h.wait(w.ID)
	if resolved.State != domain.WaitFired || resolved.Outcome != domain.WaitOutcomeChecksFailed || resolved.Message != msg {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestChecksWaitSettlesOnResultKnownAtRegistration(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckPassed)}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807})
	if !w.Baseline.ChecksDone {
		t.Fatalf("baseline = %+v", w.Baseline)
	}
	// The finished result may predate a push the observer has not seen yet.
	h.run()
	h.advance(60 * time.Second)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("delivered inside the settle window: %+v", got)
	}
	h.advance(31 * time.Second)
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 || !strings.Contains(got[0].msg, "PASSED") {
		t.Fatalf("deliveries = %+v", got)
	}
}

func TestChecksWaitFollowsNewHead(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckFailed)}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807})
	// The agent pushed: a new head with checks that have not even started.
	h.advance(20 * time.Second)
	h.writePR(prFacts{head: "bbbb2222"})
	h.run()
	h.advance(30 * time.Second)
	h.writePR(prFacts{head: "bbbb2222", checks: []domain.PullRequestCheck{check("test", domain.PRCheckQueued)}})
	h.run()
	h.advance(2 * time.Minute)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("delivered before the new head finished: %+v", got)
	}
	h.advance(30 * time.Second)
	h.writePR(prFacts{head: "bbbb2222", checks: []domain.PullRequestCheck{check("test", domain.PRCheckPassed)}})
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d", len(got))
	}
	if !strings.Contains(got[0].msg, "PASSED") || !strings.Contains(got[0].msg, "Head moved since you started waiting: aaaa1111 → bbbb2222") {
		t.Fatalf("message:\n%s", got[0].msg)
	}
	if h.wait(w.ID).Outcome != domain.WaitOutcomeChecksPassed {
		t.Fatalf("outcome = %q", h.wait(w.ID).Outcome)
	}
}

func TestMergeWaitFiresOnMergeOrClose(t *testing.T) {
	for _, tc := range []struct {
		name    string
		facts   prFacts
		outcome string
		want    string
	}{
		{"merged", prFacts{head: "aaaa1111", merged: true}, domain.WaitOutcomeMerged, "was MERGED (merge commit feedface)"},
		{"closed", prFacts{head: "aaaa1111", closed: true}, domain.WaitOutcomeClosed, "was CLOSED without merging"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckPassed)}})
			merge := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Condition: domain.WaitForMerge})
			checks := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Condition: domain.WaitForReview})
			h.advance(5 * time.Minute)
			h.run()
			if got := h.deliver.messages(); len(got) != 0 {
				t.Fatalf("merge wait fired on an open PR: %+v", got)
			}
			h.writePR(tc.facts)
			h.run()
			got := h.deliver.messages()
			if len(got) != 2 || !strings.Contains(got[0].msg, tc.want) {
				t.Fatalf("deliveries = %+v", got)
			}
			for _, id := range []domain.WaitID{merge.ID, checks.ID} {
				if w := h.wait(id); w.State != domain.WaitFired || w.Outcome != tc.outcome {
					t.Fatalf("wait %s = %+v", id, w)
				}
			}
		})
	}
}

func TestReviewWaitFiresOnNewReviewActivity(t *testing.T) {
	h := newHarness(t, false)
	old := comment("c1", "bob", "x.go", 3, "old comment")
	h.writePR(prFacts{head: "aaaa1111", comments: []domain.PullRequestComment{old}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Condition: domain.WaitForReview})
	if !w.Baseline.ReviewsCaptured || len(w.Baseline.CommentIDs) != 1 {
		t.Fatalf("baseline = %+v", w.Baseline)
	}
	// The PR author's own thread reply is bookkeeping, not review feedback.
	h.advance(time.Minute)
	reply := comment("c2", "alex", "x.go", 3, "Fixed in abc123")
	h.writePR(prFacts{head: "aaaa1111", comments: []domain.PullRequestComment{old, reply}})
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("author reply woke the session: %+v", got)
	}
	h.advance(time.Minute)
	fresh := comment("c3", "carol", "backend/wait.go", 42, "This leaks the timer; stop it on every return path.")
	review := domain.PullRequestReview{ID: "r1", Author: "carol", State: domain.ReviewChangesRequest, Body: "Two issues below.", SubmittedAt: h.now}
	h.writePR(prFacts{head: "aaaa1111", review: domain.ReviewChangesRequest, reviews: []domain.PullRequestReview{review}, comments: []domain.PullRequestComment{old, reply, fresh}})
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d", len(got))
	}
	for _, want := range []string{
		"New review activity on PR #5807", "1 review, 1 comment", "Review by @carol (changes requested):\nTwo issues below.",
		"Comment by @carol on backend/wait.go:42:\nThis leaks the timer", "review: changes requested",
	} {
		if !strings.Contains(got[0].msg, want) {
			t.Errorf("message missing %q:\n%s", want, got[0].msg)
		}
	}
	if strings.Contains(got[0].msg, "old comment") || strings.Contains(got[0].msg, "Fixed in abc123") {
		t.Fatalf("message repeats baseline or author activity:\n%s", got[0].msg)
	}
}

func TestTimeWaitFiresAfterDuration(t *testing.T) {
	h := newHarness(t, false)
	w := h.register(RegisterInput{Kind: domain.WaitKindTime, Duration: 20 * time.Minute, Note: "check the deploy"})
	h.advance(19 * time.Minute)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("timer fired early: %+v", got)
	}
	h.advance(time.Minute)
	h.run()
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 || got[0].session != h.session.ID {
		t.Fatalf("deliveries = %+v", got)
	}
	if want := "[AO wait " + string(w.ID) + "] Timer elapsed: 20m since 12:00 UTC.\nYour note: check the deploy"; got[0].msg != want {
		t.Fatalf("message = %q, want %q", got[0].msg, want)
	}
	if r := h.wait(w.ID); r.State != domain.WaitFired || r.Outcome != domain.WaitOutcomeTimer {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestPRWaitExpiresWithCurrentState(t *testing.T) {
	h := newHarness(t, false)
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckInProgress), check("lint", domain.PRCheckPassed)}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Timeout: time.Hour})
	h.advance(59 * time.Minute)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("delivered before expiry: %+v", got)
	}
	h.advance(time.Minute)
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d", len(got))
	}
	for _, want := range []string{"Gave up waiting for CI on #5807 after 1h: the wait expired.", "2 checks: 1 passed, 1 running", "ao wait pr 5807 --for checks"} {
		if !strings.Contains(got[0].msg, want) {
			t.Errorf("message missing %q:\n%s", want, got[0].msg)
		}
	}
	if r := h.wait(w.ID); r.State != domain.WaitExpired || r.Outcome != domain.WaitOutcomeExpired {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestWakeRetriesWhileSessionCannotTakeIt(t *testing.T) {
	h := newHarness(t, false)
	w := h.register(RegisterInput{Kind: domain.WaitKindTime, Duration: time.Minute})
	h.deliver.next = []sessionguard.Outcome{sessionguard.SuppressedAwaitingUser, sessionguard.SuppressedStartupPending}
	h.advance(time.Minute)
	h.run()
	h.run()
	if r := h.wait(w.ID); r.State != domain.WaitPending || r.DeliveryAttempts != 2 || !strings.Contains(r.LastError, "suppressed") {
		t.Fatalf("after refused deliveries = %+v", r)
	}
	h.run()
	h.run()
	if got := h.deliver.messages(); len(got) != 1 {
		t.Fatalf("deliveries = %d, want exactly one", len(got))
	}
	if r := h.wait(w.ID); r.State != domain.WaitFired {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestTerminatedSessionCancelsPendingWait(t *testing.T) {
	h := newHarness(t, false)
	w := h.register(RegisterInput{Kind: domain.WaitKindTime, Duration: time.Minute})
	h.session.IsTerminated = true
	if err := h.store.UpdateSession(h.ctx, h.session); err != nil {
		t.Fatal(err)
	}
	h.advance(time.Minute)
	h.run()
	if got := h.deliver.messages(); len(got) != 0 {
		t.Fatalf("delivered to a terminated session: %+v", got)
	}
	if r := h.wait(w.ID); r.State != domain.WaitCancelled || r.Outcome != domain.WaitOutcomeSessionEnded {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestPendingWaitSurvivesDaemonRestart(t *testing.T) {
	dir := t.TempDir()
	st, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, st, false, true)
	h.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckInProgress)}, comments: []domain.PullRequestComment{comment("c1", "bob", "x.go", 1, "nit")}})
	w := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 5807, Condition: domain.WaitForAny, Note: "survives"})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := newHarnessOn(t, reopened, false, false)
	restarted.session, restarted.now = h.session, h.now
	loaded := restarted.wait(w.ID)
	if loaded.State != domain.WaitPending || loaded.Note != "survives" || loaded.Condition != domain.WaitForAny ||
		loaded.Baseline.HeadSHA != "aaaa1111" || !loaded.Baseline.ReviewsCaptured || len(loaded.Baseline.CommentIDs) != 1 ||
		!loaded.CreatedAt.Equal(w.CreatedAt) || !loaded.ExpiresAt.Equal(w.ExpiresAt) {
		t.Fatalf("reloaded wait = %+v, registered %+v", loaded, w)
	}
	restarted.advance(time.Minute)
	restarted.writePR(prFacts{head: "aaaa1111", checks: []domain.PullRequestCheck{check("test", domain.PRCheckPassed)}, comments: []domain.PullRequestComment{comment("c1", "bob", "x.go", 1, "nit")}})
	restarted.run()
	restarted.run()
	got := restarted.deliver.messages()
	if len(got) != 1 || !strings.Contains(got[0].msg, "CI finished on PR #5807") || strings.Contains(got[0].msg, "nit") {
		t.Fatalf("deliveries after restart = %+v", got)
	}
}

func providerObs(head string, checks ...ports.SCMCheckObservation) ports.SCMObservation {
	return ports.SCMObservation{
		Fetched: true, Provider: "github", Host: "github.com", Repo: "acme/app",
		PR: ports.SCMPRObservation{
			URL: "https://github.com/acme/app/pull/77", Number: 77, Title: "Human PR", SourceBranch: "fix/y", TargetBranch: "main",
			HeadSHA: head, Author: "dana", State: "open",
		},
		CI:           ports.SCMCIObservation{Summary: "pending", HeadSHA: head, Checks: checks},
		Review:       ports.SCMReviewObservation{Decision: "review_required"},
		Mergeability: ports.SCMMergeabilityObservation{State: "blocked"},
	}
}

func TestUntrackedPRUsesSharedProviderReads(t *testing.T) {
	h := newHarness(t, true)
	h.prs.set(77, providerObs("cccc3333", ports.SCMCheckObservation{Name: "build", Status: "in_progress"}))
	first := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 77})
	second := h.register(RegisterInput{Kind: domain.WaitKindPR, PRNumber: 77, Condition: domain.WaitForMerge})
	if first.PRURL != "https://github.com/acme/app/pull/77" || first.Baseline.HeadSHA != "cccc3333" {
		t.Fatalf("registered = %+v", first)
	}
	registrationReads, _, _ := h.prs.counts()

	h.run()
	h.advance(30 * time.Second)
	h.run()
	if reads, _, _ := h.prs.counts(); reads != registrationReads+1 {
		t.Fatalf("provider reads = %d, want one shared read for both waits within a minute", reads-registrationReads)
	}
	h.prs.set(77, providerObs("cccc3333",
		ports.SCMCheckObservation{Name: "build", Status: "failed", ProviderID: "991"},
		ports.SCMCheckObservation{Name: "lint", Status: "passed"}))
	h.advance(31 * time.Second)
	h.run()
	got := h.deliver.messages()
	if len(got) != 1 {
		t.Fatalf("deliveries = %d", len(got))
	}
	for _, want := range []string{`PR #77 "Human PR" (fix/y → main): FAILED`, "Failed: build (failed)", "provider log for build"} {
		if !strings.Contains(got[0].msg, want) {
			t.Errorf("message missing %q:\n%s", want, got[0].msg)
		}
	}
	if _, _, logs := h.prs.counts(); logs != 1 {
		t.Fatalf("log reads = %d", logs)
	}
	if h.wait(first.ID).State != domain.WaitFired || h.wait(second.ID).State != domain.WaitPending {
		t.Fatalf("first=%+v second=%+v", h.wait(first.ID), h.wait(second.ID))
	}
}

func TestUntrackedPRMustExist(t *testing.T) {
	h := newHarness(t, true)
	if _, err := h.svc.Register(h.ctx, RegisterInput{SessionID: h.session.ID, Kind: domain.WaitKindPR, PRNumber: 404}); apiCode(err) != "WAIT_PR_NOT_FOUND" {
		t.Fatalf("err = %v", err)
	}
}

func TestWakeMessageStaysUnder4KB(t *testing.T) {
	var checks []checkFact
	for i := range 8 {
		checks = append(checks, checkFact{Name: fmt.Sprintf("job-%d", i), Status: domain.PRCheckFailed, URL: "https://ci/job", LogTail: strings.Repeat(strings.Repeat("x", 99)+"\n", 200)})
	}
	w := domain.Wait{ID: "w-big", PRNumber: 1, Condition: domain.WaitForChecks, Note: strings.Repeat("n", 500), CreatedAt: time.Now()}
	snap := prSnapshot{Number: 1, Title: strings.Repeat("t", 300), HeadSHA: "abc", Checks: checks}
	ev := evaluation{Fire: true, Outcome: domain.WaitOutcomeChecksFailed, Checks: summarizeChecks(checks)}
	msg := renderPRMessage(w, snap, ev, false, time.Now())
	if len(msg) > maxMessageBytes {
		t.Fatalf("message is %d bytes", len(msg))
	}
	// The first failure keeps its (bounded) log tail; what does not fit is
	// cut and counted rather than silently dropped.
	if !strings.Contains(msg, "Failed: job-0 (failed)") || !strings.Contains(msg, "Log tail:") ||
		!strings.Contains(msg, "omitted to keep this message short") || !strings.HasSuffix(msg, "instead of polling.") {
		t.Fatalf("message:\n%s", msg)
	}
}

func TestSummarizeChecksGatesOnRequiredChecksWhenMarked(t *testing.T) {
	sum := summarizeChecks([]checkFact{
		{Name: "required-test", Status: domain.PRCheckPassed, Required: true},
		{Name: "optional-bench", Status: domain.PRCheckInProgress},
		{Name: "optional-lint", Status: domain.PRCheckFailed},
	})
	if !sum.Done || sum.GateFailed || sum.RunningOptional != 1 || len(sum.FailedChecks) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	sum = summarizeChecks([]checkFact{{Name: "a", Status: domain.PRCheckPassed}, {Name: "b", Status: domain.PRCheckQueued}})
	if sum.Done {
		t.Fatalf("summary = %+v, want running", sum)
	}
}
