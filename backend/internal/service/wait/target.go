package wait

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// providerRefresh is the minimum interval between direct provider reads
	// of one PR AO does not already observe.
	providerRefresh = time.Minute
	// providerReviewRefresh is the slower cadence for that PR's review threads.
	providerReviewRefresh = 2 * time.Minute
	// providerBackoff pauses direct provider reads after a failed batch, so a
	// rate-limited or offline provider is not retried on every tick.
	providerBackoff = 2 * time.Minute
	// providerBatch matches the SCM observer's GraphQL batch size.
	providerBatch = 25
	// maxLogTailFetches bounds on-demand log reads for one wake message.
	maxLogTailFetches = 3
)

// prTarget is where AO reads one PR wait's facts from.
type prTarget struct {
	// url is the PR's canonical URL when known.
	url string
	// tracked is the PR row the SCM observer persists, when there is one.
	tracked domain.PullRequest
	// live means the SCM observer keeps tracked current: its owning session
	// is still observed, or the PR already reached a terminal state.
	live    bool
	hasRow  bool
	ref     ports.SCMPRRef
	hasRef  bool
	refKey  string
	baseErr error
}

// resolveTarget finds the PR a wait points at. A wait registered with a URL
// keeps it; a number resolves against the PRs AO tracks for the session's
// project, then against the project's repository.
func (s *Service) resolveTarget(ctx context.Context, w domain.Wait) (prTarget, error) {
	var t prTarget
	if w.PRURL != "" {
		t.url = w.PRURL
		pr, ok, err := s.store.GetPR(ctx, w.PRURL)
		if err != nil {
			return t, err
		}
		if ok {
			t.tracked, t.hasRow = pr, true
		}
	} else {
		pr, ok, err := s.trackedByNumber(ctx, w.ProjectID, w.PRNumber)
		if err != nil {
			return t, err
		}
		if ok {
			t.tracked, t.hasRow, t.url = pr, true, pr.URL
		}
	}
	if t.hasRow {
		live, err := s.observed(ctx, t.tracked)
		if err != nil {
			return t, err
		}
		t.live = live
	}
	if s.prs != nil {
		ref, err := s.providerRef(ctx, w, t)
		if err == nil {
			t.ref, t.hasRef, t.refKey = ref, true, refKey(ref)
		} else {
			t.baseErr = err
		}
	}
	return t, nil
}

// trackedByNumber picks the PR row with this number in the project's
// repositories, preferring an open one (the query orders active rows first).
func (s *Service) trackedByNumber(ctx context.Context, projectID domain.ProjectID, number int) (domain.PullRequest, bool, error) {
	if projectID == "" || number <= 0 {
		return domain.PullRequest{}, false, nil
	}
	rows, err := s.store.ListPRsByNumber(ctx, number)
	if err != nil || len(rows) == 0 {
		return domain.PullRequest{}, false, err
	}
	project, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return domain.PullRequest{}, false, err
	}
	repos := map[string]bool{}
	if ok {
		for _, raw := range []string{project.Config.CanonicalRepoURL, project.RepoOriginURL} {
			if id, err := domain.ParseRepositoryIdentity(raw); err == nil {
				repos[strings.ToLower(id.Host+"/"+id.Namespace+"/"+id.Name)] = true
			}
		}
	}
	for _, pr := range rows {
		if pr.Repo != "" && pr.Host != "" && repos[strings.ToLower(pr.Host+"/"+pr.Repo)] {
			return pr, true, nil
		}
		owner, found, err := s.store.GetSession(ctx, pr.SessionID)
		if err != nil {
			return domain.PullRequest{}, false, err
		}
		if found && owner.ProjectID == projectID {
			return pr, true, nil
		}
	}
	return domain.PullRequest{}, false, nil
}

// observed reports whether the SCM observer keeps pr current. It polls the
// PRs of live sessions only; a terminal PR no longer changes.
func (s *Service) observed(ctx context.Context, pr domain.PullRequest) (bool, error) {
	if pr.Merged || pr.Closed {
		return true, nil
	}
	owner, ok, err := s.store.GetSession(ctx, pr.SessionID)
	if err != nil {
		return false, err
	}
	return ok && !owner.IsTerminated, nil
}

// providerRef builds the provider reference for a direct read.
func (s *Service) providerRef(ctx context.Context, w domain.Wait, t prTarget) (ports.SCMPRRef, error) {
	if t.hasRow && t.tracked.Repo != "" && t.tracked.Provider != "" && t.tracked.Host != "" {
		owner, name, ok := strings.Cut(t.tracked.Repo, "/")
		if ok {
			return ports.SCMPRRef{
				Repo:   ports.SCMRepo{Provider: t.tracked.Provider, Host: t.tracked.Host, Owner: owner, Name: name, Repo: t.tracked.Repo},
				Number: t.tracked.Number, URL: t.tracked.URL,
			}, nil
		}
	}
	if t.url != "" {
		ref, err := domain.ParseChangeRequestURL(t.url)
		if err != nil {
			return ports.SCMPRRef{}, err
		}
		repo, ok := s.prs.ParseRepository("https://" + ref.Host + "/" + ref.Repository)
		if !ok {
			return ports.SCMPRRef{}, fmt.Errorf("unsupported repository %s/%s", ref.Host, ref.Repository)
		}
		return ports.SCMPRRef{Repo: repo, Number: ref.Number, URL: t.url}, nil
	}
	project, ok, err := s.store.GetProject(ctx, string(w.ProjectID))
	if err != nil {
		return ports.SCMPRRef{}, err
	}
	if !ok {
		return ports.SCMPRRef{}, errors.New("session has no project repository")
	}
	for _, raw := range []string{project.Config.CanonicalRepoURL, project.RepoOriginURL} {
		if raw == "" {
			continue
		}
		if repo, ok := s.prs.ParseRepository(raw); ok {
			return ports.SCMPRRef{Repo: repo, Number: w.PRNumber}, nil
		}
	}
	return ports.SCMPRRef{}, errors.New("project has no supported GitHub or GitLab repository")
}

func refKey(ref ports.SCMPRRef) string {
	return strings.ToLower(fmt.Sprintf("%s|%s|%s|%d", ref.Repo.Provider, ref.Repo.Host, ref.Repo.Repo, ref.Number))
}

// cachedPR is one directly read PR, shared by every wait on it.
type cachedPR struct {
	obs       ports.SCMObservation
	fetchedAt time.Time
	notFound  bool
	reviews   ports.SCMReviewObservation
	reviewsAt time.Time
	reviewsOK bool
}

// needsProvider reports whether t is read from the provider rather than from
// the observer's persisted facts.
func (t prTarget) needsProvider() bool { return t.hasRef && !t.live }

// refreshProvider reads every stale directly-read PR among targets in
// observer-sized batches. Failures back off instead of retrying every tick.
func (s *Service) refreshProvider(ctx context.Context, targets map[domain.WaitID]prTarget, needReviews map[string]bool, now time.Time) {
	if s.prs == nil || now.Before(s.backoffUntil) {
		return
	}
	var refs []ports.SCMPRRef
	seen := map[string]bool{}
	for _, t := range targets {
		if !t.needsProvider() || seen[t.refKey] {
			continue
		}
		seen[t.refKey] = true
		if c := s.cache[t.refKey]; c != nil && now.Sub(c.fetchedAt) < providerRefresh {
			continue
		}
		refs = append(refs, t.ref)
	}
	for start := 0; start < len(refs); start += providerBatch {
		batch := refs[start:min(start+providerBatch, len(refs))]
		obs, err := s.prs.FetchPullRequests(ctx, batch)
		if err != nil {
			s.logger.Warn("wait: provider PR read failed; backing off", "prs", len(batch), "err", err)
			s.backoffUntil = now.Add(providerBackoff)
			return
		}
		for i, ref := range batch {
			if i >= len(obs) {
				break
			}
			key := refKey(ref)
			c := s.cache[key]
			if c == nil {
				c = &cachedPR{}
				s.cache[key] = c
			}
			switch {
			case obs[i].Fetched:
				c.obs, c.fetchedAt, c.notFound = obs[i], now, false
			case errors.Is(obs[i].Error, ports.ErrSCMNotFound):
				c.fetchedAt, c.notFound = now, true
			}
		}
	}
	for key := range needReviews {
		c := s.cache[key]
		if c == nil || c.notFound || !c.obs.Fetched || now.Sub(c.reviewsAt) < providerReviewRefresh {
			continue
		}
		ref := ports.SCMPRRef{Repo: repoOf(c.obs), Number: c.obs.PR.Number, URL: c.obs.PR.URL}
		review, err := s.prs.FetchReviewThreads(ctx, ref)
		c.reviewsAt = now
		if err != nil {
			s.logger.Warn("wait: provider review read failed", "pr", c.obs.PR.URL, "err", err)
			continue
		}
		c.reviews, c.reviewsOK = review, true
	}
}

func repoOf(obs ports.SCMObservation) ports.SCMRepo {
	owner, name, _ := strings.Cut(obs.Repo, "/")
	return ports.SCMRepo{Provider: obs.Provider, Host: obs.Host, Owner: owner, Name: name, Repo: obs.Repo}
}

// snapshot reads the PR facts for t: the observer's persisted row when the
// observer keeps it current, otherwise the cached provider read. A row nobody
// refreshes any more is never evaluated: its finished checks may be long stale.
func (s *Service) snapshot(ctx context.Context, t prTarget) (prSnapshot, bool, error) {
	if t.hasRow && t.live {
		snap, err := s.storedSnapshot(ctx, t.tracked)
		return snap, err == nil, err
	}
	if t.hasRef {
		if c := s.cache[t.refKey]; c != nil && c.obs.Fetched {
			return providerSnapshot(c), true, nil
		}
	}
	return prSnapshot{}, false, nil
}

func (s *Service) storedSnapshot(ctx context.Context, pr domain.PullRequest) (prSnapshot, error) {
	snap := prSnapshot{
		URL: pr.URL, Number: pr.Number, Title: pr.Title, Author: pr.Author, HeadSHA: pr.HeadSHA,
		SourceBranch: pr.SourceBranch, TargetBranch: pr.TargetBranch, MergeCommitSHA: pr.MergeCommitSHA,
		Draft: pr.Draft, Merged: pr.Merged, Closed: pr.Closed,
		CI: pr.CI, Review: pr.Review, Mergeability: pr.Mergeability,
		ReviewsLoaded: !pr.ReviewObservedAt.IsZero(), ObservedAt: pr.UpdatedAt,
	}
	checks, err := s.store.ListChecks(ctx, pr.URL)
	if err != nil {
		return prSnapshot{}, err
	}
	for _, c := range checks {
		if pr.HeadSHA != "" && c.CommitHash != "" && !strings.EqualFold(c.CommitHash, pr.HeadSHA) {
			continue
		}
		snap.Checks = append(snap.Checks, checkFact{
			Name: c.Name, Status: c.Status, URL: c.URL, Description: c.Description,
			LogTail: c.LogTail, Required: c.Required, ProviderID: c.Details,
		})
	}
	reviews, err := s.store.ListPRReviews(ctx, pr.URL)
	if err != nil {
		return prSnapshot{}, err
	}
	for _, r := range reviews {
		snap.Reviews = append(snap.Reviews, reviewFact{ID: r.ID, Author: r.Author, State: r.State, Body: r.Body, URL: r.URL, SubmittedAt: r.SubmittedAt})
	}
	comments, err := s.store.ListPRComments(ctx, pr.URL)
	if err != nil {
		return prSnapshot{}, err
	}
	for _, c := range comments {
		if !domain.IsActionableReviewComment(false, c.IsBot, c.File, c.Line) {
			continue
		}
		snap.Comments = append(snap.Comments, commentFact{ID: c.ID, Author: c.Author, File: c.File, Line: c.Line, Body: c.Body, URL: c.URL})
	}
	return snap, nil
}

func providerSnapshot(c *cachedPR) prSnapshot {
	o := c.obs
	snap := prSnapshot{
		URL: firstNonEmpty(o.PR.URL, o.PR.HTMLURL), Number: o.PR.Number, Title: o.PR.Title, Author: o.PR.Author,
		HeadSHA: firstNonEmpty(o.CI.HeadSHA, o.PR.HeadSHA), SourceBranch: o.PR.SourceBranch, TargetBranch: o.PR.TargetBranch,
		MergeCommitSHA: o.PR.MergeCommitSHA, Draft: o.PR.Draft, Merged: o.PR.Merged, Closed: o.PR.Closed,
		CI:           domain.CIState(firstNonEmpty(o.CI.Summary, string(domain.CIUnknown))),
		Review:       domain.ReviewDecision(firstNonEmpty(o.Review.Decision, string(domain.ReviewNone))),
		Mergeability: domain.Mergeability(firstNonEmpty(o.Mergeability.State, string(domain.MergeUnknown))),
		ObservedAt:   c.fetchedAt,
	}
	for _, ch := range o.CI.Checks {
		snap.Checks = append(snap.Checks, checkFact{
			Name: ch.Name, Status: domain.PRCheckStatus(ch.Status), URL: ch.URL, Description: ch.Description,
			LogTail: ch.LogTail, Required: ch.Required, ProviderID: ch.ProviderID,
		})
	}
	if c.reviewsOK {
		snap.ReviewsLoaded = true
		for _, r := range c.reviews.Reviews {
			snap.Reviews = append(snap.Reviews, reviewFact{ID: r.ID, Author: r.Author, State: domain.ReviewDecision(r.State), Body: r.Body, URL: r.URL, SubmittedAt: r.SubmittedAt})
		}
		for _, th := range c.reviews.Threads {
			for _, cm := range th.Comments {
				if !domain.IsActionableReviewComment(false, cm.IsBot, th.Path, th.Line) {
					continue
				}
				snap.Comments = append(snap.Comments, commentFact{ID: cm.ID, Author: cm.Author, File: th.Path, Line: th.Line, Body: cm.Body, URL: cm.URL})
			}
		}
	}
	return snap
}

// fillLogTails fetches the log tails a wake message is missing for its failed
// checks, bounded to a few reads and only when the message is about to go out.
func (s *Service) fillLogTails(ctx context.Context, t prTarget, sum *checkSummary) {
	if s.prs == nil || !t.hasRef {
		return
	}
	fetched := 0
	for i := range sum.FailedChecks {
		c := &sum.FailedChecks[i]
		if c.LogTail != "" || c.ProviderID == "" || fetched >= maxLogTailFetches {
			continue
		}
		fetched++
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		tail, err := s.prs.FetchFailedCheckLogTail(readCtx, t.ref.Repo, ports.SCMCheckObservation{Name: c.Name, Status: string(c.Status), URL: c.URL, ProviderID: c.ProviderID})
		cancel()
		if err != nil {
			s.logger.Debug("wait: failed check log read failed", "check", c.Name, "err", err)
			continue
		}
		c.LogTail = tail
	}
}

// readNow performs the synchronous provider read registration uses to
// validate and baseline a PR AO does not observe yet.
func (s *Service) readNow(ctx context.Context, t prTarget, needReviews bool) (prSnapshot, bool, error) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	obs, err := s.prs.FetchPullRequests(readCtx, []ports.SCMPRRef{t.ref})
	if err != nil {
		return prSnapshot{}, false, err
	}
	if len(obs) == 0 || !obs[0].Fetched {
		if len(obs) > 0 && errors.Is(obs[0].Error, ports.ErrSCMNotFound) {
			return prSnapshot{}, false, apierr.NotFound("WAIT_PR_NOT_FOUND", fmt.Sprintf("PR #%d was not found in %s", t.ref.Number, t.ref.Repo.Repo))
		}
		if len(obs) > 0 && obs[0].Error != nil {
			return prSnapshot{}, false, obs[0].Error
		}
		return prSnapshot{}, false, errors.New("provider returned no observation")
	}
	c := &cachedPR{obs: obs[0], fetchedAt: s.now()}
	if needReviews {
		review, err := s.prs.FetchReviewThreads(readCtx, ports.SCMPRRef{Repo: t.ref.Repo, Number: t.ref.Number, URL: firstNonEmpty(obs[0].PR.URL, t.ref.URL)})
		if err == nil {
			c.reviews, c.reviewsAt, c.reviewsOK = review, c.fetchedAt, true
		}
	}
	return providerSnapshot(c), true, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
