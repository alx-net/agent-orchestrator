package wait

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	// maxMessageBytes keeps a wake message small enough to be cheaper than
	// the polling it replaces, with room for every failure detail that fits.
	maxMessageBytes = 4000
	// maxLogTailLines and maxLogTailBytes bound each failed check's log tail.
	maxLogTailLines = 15
	maxLogTailBytes = 900
	// maxFailedChecksShown bounds the failed checks listed in full.
	maxFailedChecksShown = 4
	// maxCommentsShown and maxExcerptRunes bound review activity.
	maxCommentsShown = 6
	maxExcerptRunes  = 280
)

// shortSHA abbreviates a commit for one-line summaries.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func clean(s string) string { return domain.SanitizeControlChars(s) }

// prLabel is `PR #12 "title" (feat/x → main)`, the identity AO's other PR
// messages use, so an agent with several PRs can tell which one this is.
func prLabel(number int, snap prSnapshot) string {
	label := fmt.Sprintf("PR #%d", number)
	if snap.Title != "" {
		label += fmt.Sprintf(" %q", clean(snap.Title))
	}
	if snap.SourceBranch != "" && snap.TargetBranch != "" {
		label += fmt.Sprintf(" (%s → %s)", clean(snap.SourceBranch), clean(snap.TargetBranch))
	}
	return label
}

func waitingFor(w domain.Wait) string {
	switch w.Condition {
	case domain.WaitForMerge:
		return fmt.Sprintf("#%d to merge or close", w.PRNumber)
	case domain.WaitForReview:
		return fmt.Sprintf("a new review on #%d", w.PRNumber)
	case domain.WaitForAny:
		return fmt.Sprintf("CI, a review, or a merge on #%d", w.PRNumber)
	default:
		return fmt.Sprintf("CI on #%d", w.PRNumber)
	}
}

func humanState(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	if value == "" {
		return "unknown"
	}
	return value
}

// stateLine is the one-line PR status every PR wake carries.
func stateLine(snap prSnapshot, sum checkSummary) string {
	parts := []string{}
	if snap.HeadSHA != "" {
		parts = append(parts, "head "+shortSHA(clean(snap.HeadSHA)))
	}
	parts = append(parts, checksLine(sum))
	if !snap.Merged && !snap.Closed {
		state := "open"
		if snap.Draft {
			state = "draft"
		}
		parts = append(parts, state, "merge state: "+humanState(string(snap.Mergeability)))
	}
	parts = append(parts, "review: "+humanState(string(snap.Review)))
	return strings.Join(parts, " · ")
}

func checksLine(sum checkSummary) string {
	if sum.None {
		return "no checks reported"
	}
	counts := []string{}
	for _, c := range []struct {
		n     int
		label string
	}{{sum.Passed, "passed"}, {sum.Failed, "failed"}, {sum.Skipped, "skipped"}, {sum.Running, "running"}} {
		if c.n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", c.n, c.label))
		}
	}
	return fmt.Sprintf("%d checks: %s", sum.Total, strings.Join(counts, ", "))
}

// renderPRMessage builds the wake message for a resolved PR wait. The message
// is everything the agent would have learned by polling, kept compact.
func renderPRMessage(w domain.Wait, snap prSnapshot, ev evaluation, expired bool, now time.Time) string {
	var b strings.Builder
	id := "[AO wait " + string(w.ID) + "] "
	label := prLabel(w.PRNumber, snap)
	switch {
	case expired:
		fmt.Fprintf(&b, "%sGave up waiting for %s after %s: the wait expired. %s is still open.\n",
			id, waitingFor(w), roundDuration(now.Sub(w.CreatedAt)), label)
	case ev.Outcome == domain.WaitOutcomeMerged:
		fmt.Fprintf(&b, "%s%s was MERGED", id, label)
		if snap.MergeCommitSHA != "" {
			fmt.Fprintf(&b, " (merge commit %s)", shortSHA(clean(snap.MergeCommitSHA)))
		}
		b.WriteString(".\n")
	case ev.Outcome == domain.WaitOutcomeClosed:
		fmt.Fprintf(&b, "%s%s was CLOSED without merging.\n", id, label)
	case ev.Outcome == domain.WaitOutcomeReview:
		fmt.Fprintf(&b, "%sNew review activity on %s: %s.\n", id, label, reviewCounts(ev))
	case ev.Outcome == domain.WaitOutcomeChecksFailed:
		fmt.Fprintf(&b, "%sCI finished on %s: FAILED.\n", id, label)
	case ev.Outcome == domain.WaitOutcomeNoChecks:
		fmt.Fprintf(&b, "%sNo CI checks were reported on %s within %s; treating CI as finished.\n", id, label, roundDuration(noChecksGrace))
	default:
		fmt.Fprintf(&b, "%sCI finished on %s: PASSED.\n", id, label)
	}
	b.WriteString(stateLine(snap, ev.Checks))
	b.WriteString("\n")
	if w.Baseline.HeadSHA != "" && snap.HeadSHA != "" && !strings.EqualFold(w.Baseline.HeadSHA, snap.HeadSHA) {
		fmt.Fprintf(&b, "Head moved since you started waiting: %s → %s.\n", shortSHA(clean(w.Baseline.HeadSHA)), shortSHA(clean(snap.HeadSHA)))
	}
	if ev.Checks.RunningOptional > 0 && ev.Checks.Done {
		fmt.Fprintf(&b, "Required checks are done; %d optional check(s) still running.\n", ev.Checks.RunningOptional)
	}
	if note := strings.TrimSpace(w.Note); note != "" {
		fmt.Fprintf(&b, "Your note: %s\n", clean(note))
	}
	tail := footer(w, snap, ev, expired)
	body := detailBlocks(snap, ev)
	return fitMessage(b.String(), body, tail)
}

func reviewCounts(ev evaluation) string {
	parts := []string{}
	if n := len(ev.NewReviews); n > 0 {
		parts = append(parts, plural(n, "review", "reviews"))
	}
	if n := len(ev.NewComments); n > 0 {
		parts = append(parts, plural(n, "comment", "comments"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// detailBlocks are the per-item details, in priority order: failed checks
// with their log tails, then review activity.
func detailBlocks(snap prSnapshot, ev evaluation) []string {
	var blocks []string
	if !snap.Merged && !snap.Closed {
		for i, c := range ev.Checks.FailedChecks {
			if i >= maxFailedChecksShown {
				blocks = append(blocks, fmt.Sprintf("…and %d more failed check(s).", len(ev.Checks.FailedChecks)-i))
				break
			}
			blocks = append(blocks, failedCheckBlock(c))
		}
	}
	for _, r := range ev.NewReviews {
		var rb strings.Builder
		fmt.Fprintf(&rb, "Review by @%s (%s)", clean(orUnknown(r.Author)), humanState(string(r.State)))
		if body := excerpt(r.Body); body != "" {
			fmt.Fprintf(&rb, ":\n%s", body)
		}
		if r.URL != "" {
			fmt.Fprintf(&rb, "\n%s", clean(r.URL))
		}
		blocks = append(blocks, rb.String())
	}
	for i, c := range ev.NewComments {
		if i >= maxCommentsShown {
			blocks = append(blocks, fmt.Sprintf("…and %d more comment(s).", len(ev.NewComments)-i))
			break
		}
		location := "(general)"
		if c.File != "" {
			location = clean(c.File)
			if c.Line > 0 {
				location = fmt.Sprintf("%s:%d", location, c.Line)
			}
		}
		blocks = append(blocks, fmt.Sprintf("Comment by @%s on %s:\n%s", clean(orUnknown(c.Author)), location, excerpt(c.Body)))
	}
	return blocks
}

func failedCheckBlock(c checkFact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Failed: %s (%s)", clean(orUnknown(c.Name)), humanState(string(c.Status)))
	if c.URL != "" {
		fmt.Fprintf(&b, "\n%s", clean(c.URL))
	}
	if tail := logTail(c.LogTail); tail != "" {
		fence := codeFence(tail)
		fmt.Fprintf(&b, "\nLog tail:\n%s\n%s\n%s", fence, tail, fence)
	} else if d := strings.TrimSpace(c.Description); d != "" {
		fmt.Fprintf(&b, "\n%s", clean(d))
	}
	return b.String()
}

func footer(w domain.Wait, snap prSnapshot, ev evaluation, expired bool) string {
	var b strings.Builder
	if url := firstNonEmpty(snap.URL, w.PRURL); url != "" {
		fmt.Fprintf(&b, "PR: %s\n", clean(url))
	}
	if expired {
		fmt.Fprintf(&b, "Run `ao wait pr %d --for %s` to keep waiting.", w.PRNumber, w.Condition)
		return b.String()
	}
	if ev.Outcome != domain.WaitOutcomeMerged && ev.Outcome != domain.WaitOutcomeClosed {
		fmt.Fprintf(&b, "This wait is resolved; run `ao wait pr %d` again after your next push instead of polling.", w.PRNumber)
	} else {
		b.WriteString("This wait is resolved.")
	}
	return b.String()
}

// fitMessage joins head, details and tail, dropping or cutting details from
// the end so the message stays within maxMessageBytes.
func fitMessage(head string, blocks []string, tail string) string {
	budget := maxMessageBytes - len(head) - len(tail) - 2
	var kept []string
	for i, block := range blocks {
		need := len(block) + 2
		if need <= budget {
			kept = append(kept, block)
			budget -= need
			continue
		}
		if budget > 200 {
			kept = append(kept, truncateBytes(block, budget-40)+"\n…(truncated)")
		}
		if rest := len(blocks) - i - 1; rest > 0 {
			kept = append(kept, fmt.Sprintf("…%d more item(s) omitted to keep this message short.", rest))
		}
		break
	}
	var b strings.Builder
	b.WriteString(head)
	for _, block := range kept {
		b.WriteString("\n")
		b.WriteString(block)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(tail)
	return truncateBytes(b.String(), maxMessageBytes)
}

// renderTimerMessage builds the wake for a resolved timer.
func renderTimerMessage(w domain.Wait) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[AO wait %s] Timer elapsed: %s since %s.", w.ID, roundDuration(w.FireAt.Sub(w.CreatedAt)), w.CreatedAt.UTC().Format("15:04 UTC"))
	if note := strings.TrimSpace(w.Note); note != "" {
		fmt.Fprintf(&b, "\nYour note: %s", clean(note))
	}
	return b.String()
}

// renderUnobservedExpiry is the expiry wake when AO never managed to read the PR.
func renderUnobservedExpiry(w domain.Wait, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[AO wait %s] Gave up waiting for %s after %s: the wait expired and AO could not read the PR's state.", w.ID, waitingFor(w), roundDuration(now.Sub(w.CreatedAt)))
	if note := strings.TrimSpace(w.Note); note != "" {
		fmt.Fprintf(&b, "\nYour note: %s", clean(note))
	}
	if w.PRURL != "" {
		fmt.Fprintf(&b, "\nPR: %s", clean(w.PRURL))
	}
	return b.String()
}

// roundDuration renders d compactly: "24h", "1h30m", "20m", "45s".
func roundDuration(d time.Duration) string {
	if d >= time.Hour {
		d = d.Round(time.Minute)
	} else {
		d = d.Round(time.Second)
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// excerpt collapses and bounds a provider-authored body.
func excerpt(body string) string {
	body = strings.TrimSpace(clean(body))
	if utf8.RuneCountInString(body) <= maxExcerptRunes {
		return body
	}
	runes := []rune(body)
	return strings.TrimSpace(string(runes[:maxExcerptRunes])) + "…"
}

// logTail keeps the last lines of a sanitized CI log.
func logTail(raw string) string {
	tail := strings.TrimRight(clean(raw), "\n ")
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	if len(lines) > maxLogTailLines {
		lines = lines[len(lines)-maxLogTailLines:]
	}
	tail = strings.Join(lines, "\n")
	if len(tail) > maxLogTailBytes {
		tail = "…" + tailBytes(tail, maxLogTailBytes)
	}
	return tail
}

func codeFence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// tailBytes keeps the last n bytes of s on a rune boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
