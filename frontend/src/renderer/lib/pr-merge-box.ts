/**
 * What a pull request's merge box says, derived from the daemon's PR summary:
 * the checks on the head commit grouped the way GitHub's merge box groups them,
 * one checks headline with counts, and one merge-state verdict (ready, out of
 * date with the base, conflicting, blocked, draft).
 *
 * Pure and untranslated: the panel turns these shapes into words. A daemon that
 * predates the full check list still sends `ci.failingChecks`, so the box falls
 * back to those and says the list is partial instead of claiming the rest passed.
 */

import type { SessionPRSummary } from "../hooks/useSessionScmSummary";
import { prBrowserUrl, prCanMerge, prChecksUrl } from "./pr-display";

// TODO(api): use generated types once openapi lands
export type PRCheckStatus = "queued" | "in_progress" | "passed" | "failed" | "cancelled" | "skipped" | "neutral";

// TODO(api): use generated types once openapi lands
export interface PRCheck {
	name: string;
	workflow?: string;
	status: PRCheckStatus;
	conclusion?: string;
	required: boolean;
	startedAt?: string;
	completedAt?: string;
	url?: string;
}

// TODO(api): use generated types once openapi lands
/** GitHub's raw merge state, passed through by the daemon. */
export type PRMergeStateStatus = "CLEAN" | "BLOCKED" | "BEHIND" | "DIRTY" | "UNSTABLE" | "DRAFT" | "HAS_HOOKS" | "UNKNOWN";

// TODO(api): use generated types once openapi lands
/** The PR summary with the fields the merge box reads ahead of the generated schema. */
export type MergeBoxPR = SessionPRSummary & {
	/** Every check on the head commit, pre-sorted failing → running → skipped → passed, required first. */
	checks?: PRCheck[];
	mergeStateStatus?: PRMergeStateStatus;
};

export type CheckGroupKey = "failing" | "running" | "skipped" | "passed";

/** Group order in the box: what needs a person first. */
export const CHECK_GROUP_ORDER: readonly CheckGroupKey[] = ["failing", "running", "skipped", "passed"];

/** Failing and running checks are worth reading; skipped and passed ones fold away. */
export const CHECK_GROUP_DEFAULT_OPEN: Readonly<Record<CheckGroupKey, boolean>> = {
	failing: true,
	running: true,
	skipped: false,
	passed: false,
};

export function checkGroupOf(status: PRCheckStatus): CheckGroupKey {
	switch (status) {
		case "failed":
		case "cancelled":
			return "failing";
		case "queued":
		case "in_progress":
			return "running";
		case "skipped":
		case "neutral":
			return "skipped";
		case "passed":
			return "passed";
	}
}

export interface PRCheckSet {
	checks: PRCheck[];
	/** Only the failing checks are known: the daemon sent no full check list. */
	partial: boolean;
}

export function prCheckSet(pr: MergeBoxPR): PRCheckSet {
	if (Array.isArray(pr.checks)) return { checks: pr.checks, partial: false };
	return {
		checks: (pr.ci.failingChecks ?? []).map((check) => ({
			name: check.name,
			status: check.status === "cancelled" ? "cancelled" : "failed",
			conclusion: check.conclusion || undefined,
			// The failing-check list does not say which checks are required.
			required: false,
			url: check.url || undefined,
		})),
		partial: true,
	};
}

export interface CheckGroup {
	key: CheckGroupKey;
	checks: PRCheck[];
	defaultOpen: boolean;
}

/** Non-empty groups in box order; required checks lead within a group, otherwise the daemon's order holds. */
export function groupChecks(checks: readonly PRCheck[]): CheckGroup[] {
	const byGroup = new Map<CheckGroupKey, PRCheck[]>();
	for (const check of checks) {
		const key = checkGroupOf(check.status);
		const list = byGroup.get(key);
		if (list) list.push(check);
		else byGroup.set(key, [check]);
	}
	return CHECK_GROUP_ORDER.flatMap((key) => {
		const list = byGroup.get(key);
		if (!list) return [];
		const ordered = [...list.filter((check) => check.required), ...list.filter((check) => !check.required)];
		return [{ key, checks: ordered, defaultOpen: CHECK_GROUP_DEFAULT_OPEN[key] }];
	});
}

export type ChecksHeadline = "failing" | "running" | "passed" | "none" | "unknown";

export interface ChecksSummary {
	headline: ChecksHeadline;
	counts: Record<CheckGroupKey, number>;
	total: number;
	partial: boolean;
}

export function checksSummary(pr: MergeBoxPR): ChecksSummary {
	const { checks, partial } = prCheckSet(pr);
	const counts: Record<CheckGroupKey, number> = { failing: 0, running: 0, skipped: 0, passed: 0 };
	for (const check of checks) counts[checkGroupOf(check.status)] += 1;
	return { headline: headline(pr, counts, checks.length, partial), counts, total: checks.length, partial };
}

function headline(pr: MergeBoxPR, counts: Record<CheckGroupKey, number>, total: number, partial: boolean): ChecksHeadline {
	if (counts.failing > 0) return "failing";
	if (counts.running > 0) return "running";
	// The full list is the head commit's truth; the rollup only fills in where it is missing.
	if (!partial && total > 0) return "passed";
	switch (pr.ci.state) {
		case "failing":
			return "failing";
		case "pending":
			return "running";
		case "passing":
			return partial ? "passed" : "none";
		case "unknown":
			return partial ? "unknown" : "none";
	}
}

/** The count parts in reading order, zero buckets left out: "1 failing, 8 skipped, 6 successful". */
export function checkCountParts(counts: Record<CheckGroupKey, number>): { key: CheckGroupKey; count: number }[] {
	return CHECK_GROUP_ORDER.filter((key) => counts[key] > 0).map((key) => ({ key, count: counts[key] }));
}

export function checkLabel(check: Pick<PRCheck, "name" | "workflow">): string {
	return check.workflow && check.workflow !== check.name ? `${check.workflow} / ${check.name}` : check.name;
}

export type CheckTiming =
	| { kind: "failedAfter" | "cancelledAfter" | "passedIn" | "completedIn" | "runningFor"; durationMs: number }
	| { kind: "failedAt" | "cancelledAt" | "passedAt" | "completedAt" | "skippedAt"; at: string }
	| { kind: "failed" | "cancelled" | "passed" | "completed" | "skipped" | "running" | "queued" };

/** How long a check ran, or when it settled when only the end is known. */
export function checkTiming(check: PRCheck, now: number = Date.now()): CheckTiming {
	const started = parseTime(check.startedAt);
	const completed = parseTime(check.completedAt);
	const ran = started !== undefined && completed !== undefined ? Math.max(0, completed - started) : undefined;
	switch (check.status) {
		case "failed":
			return ran !== undefined ? { kind: "failedAfter", durationMs: ran } : completed !== undefined ? { kind: "failedAt", at: check.completedAt! } : { kind: "failed" };
		case "cancelled":
			return ran !== undefined ? { kind: "cancelledAfter", durationMs: ran } : completed !== undefined ? { kind: "cancelledAt", at: check.completedAt! } : { kind: "cancelled" };
		case "passed":
			return ran !== undefined ? { kind: "passedIn", durationMs: ran } : completed !== undefined ? { kind: "passedAt", at: check.completedAt! } : { kind: "passed" };
		case "neutral":
			return ran !== undefined ? { kind: "completedIn", durationMs: ran } : completed !== undefined ? { kind: "completedAt", at: check.completedAt! } : { kind: "completed" };
		case "skipped":
			return completed !== undefined ? { kind: "skippedAt", at: check.completedAt! } : { kind: "skipped" };
		case "in_progress":
			return started !== undefined ? { kind: "runningFor", durationMs: Math.max(0, now - started) } : { kind: "running" };
		case "queued":
			return { kind: "queued" };
	}
}

function parseTime(value: string | undefined): number | undefined {
	if (!value) return undefined;
	const ms = Date.parse(value);
	return Number.isFinite(ms) ? ms : undefined;
}

/** "6s", "3m", "1m 12s", "2h 5m": seconds matter for a check, unlike a turn. */
export function formatCheckDuration(ms: number): string {
	const totalSeconds = Math.max(1, Math.floor(ms / 1000));
	if (totalSeconds < 60) return `${totalSeconds}s`;
	const totalMinutes = Math.floor(totalSeconds / 60);
	if (totalMinutes < 60) {
		const seconds = totalSeconds % 60;
		return seconds === 0 ? `${totalMinutes}m` : `${totalMinutes}m ${seconds}s`;
	}
	const hours = Math.floor(totalMinutes / 60);
	const minutes = totalMinutes % 60;
	return minutes === 0 ? `${hours}h` : `${hours}h ${minutes}m`;
}

/** The branch is behind its base: GitHub's BEHIND, or the daemon's behind_base reason. */
export function prIsStale(pr: MergeBoxPR): boolean {
	return pr.mergeStateStatus === "BEHIND" || (pr.mergeability.reasons ?? []).includes("behind_base");
}

export type MergeVerdict =
	| { kind: "merged"; at?: string }
	| { kind: "closed"; at?: string }
	| { kind: "draft" }
	| { kind: "conflicts"; files: { path: string; url?: string }[] }
	| { kind: "behind" }
	| { kind: "unavailable"; reason: "access" | "checks" }
	| { kind: "ready" }
	| { kind: "blocked"; checks: boolean; review?: "required" | "changes_requested" | "comments" }
	| { kind: "checking" };

/** The one line under the checks that says whether this PR can merge, and if not, what is first in the way. */
export function mergeVerdict(pr: MergeBoxPR): MergeVerdict {
	if (pr.state === "merged") return { kind: "merged", at: pr.stateChangedAt ?? undefined };
	if (pr.state === "closed") return { kind: "closed", at: pr.stateChangedAt ?? undefined };
	if (pr.state === "draft" || pr.mergeStateStatus === "DRAFT") return { kind: "draft" };
	if (pr.mergeability.state === "conflicting" || pr.mergeStateStatus === "DIRTY") {
		return { kind: "conflicts", files: pr.mergeability.conflictFiles ?? [] };
	}
	if (prIsStale(pr)) return { kind: "behind" };
	const reasons = pr.mergeability.reasons ?? [];
	if (reasons.includes("github_access_lost")) return { kind: "unavailable", reason: "access" };
	if (reasons.includes("github_checks_unavailable")) return { kind: "unavailable", reason: "checks" };
	// The same gate the Merge button uses, so the line never disagrees with it.
	if (prCanMerge(pr)) return { kind: "ready" };
	const checksFailing = checksSummary(pr).headline === "failing";
	const review = pr.review.decision === "changes_requested"
		? "changes_requested"
		: pr.review.decision === "review_required"
			? "required"
			: pr.review.hasUnresolvedHumanComments
				? "comments"
				: undefined;
	if (checksFailing || review) return { kind: "blocked", checks: checksFailing, review };
	if (pr.ci.state === "pending" || pr.ci.state === "unknown" || pr.mergeability.state === "unknown") return { kind: "checking" };
	return { kind: "blocked", checks: false };
}

export type PRGlyphTone = "merged" | "closed" | "draft" | "failing" | "attention" | "running" | "ready" | "neutral";

/** The PR glyph's tint: what the PR most needs, in the status colors. */
export function prGlyphTone(pr: MergeBoxPR): PRGlyphTone {
	if (pr.state === "merged") return "merged";
	if (pr.state === "closed") return "closed";
	if (pr.state === "draft") return "draft";
	const checks = checksSummary(pr).headline;
	if (checks === "failing") return "failing";
	const verdict = mergeVerdict(pr);
	if (verdict.kind === "conflicts" || verdict.kind === "behind" || verdict.kind === "blocked") return "attention";
	if (checks === "running" || verdict.kind === "checking") return "running";
	if (verdict.kind === "ready") return "ready";
	return "neutral";
}

export type PullRequestsTabTone = "failing" | "conflicts" | "running" | "idle";

/** The tab's count tint: failing CI, then conflicts, then checks still running, on the open PRs only. */
export function pullRequestsTabTone(prs: readonly MergeBoxPR[]): PullRequestsTabTone {
	const active = prs.filter((pr) => pr.state === "open" || pr.state === "draft");
	if (active.some((pr) => checksSummary(pr).headline === "failing")) return "failing";
	if (active.some((pr) => pr.mergeability.state === "conflicting" || pr.mergeStateStatus === "DIRTY")) return "conflicts";
	if (active.some((pr) => checksSummary(pr).headline === "running")) return "running";
	return "idle";
}

export function prIsSettled(pr: Pick<SessionPRSummary, "state">): boolean {
	return pr.state === "merged" || pr.state === "closed";
}

/** When the box's facts were read from the provider, for an honest "updated" line. */
export function prObservedAt(pr: MergeBoxPR): string | undefined {
	return pr.ciObservedAt || pr.observedAt || pr.updatedAt || undefined;
}

/** The draft "Fix this" starts in the composer for a failing check. */
export function fixCheckPrompt(check: PRCheck, pr: MergeBoxPR): string {
	return `Fix the failing check "${checkLabel(check)}" on PR #${pr.number}: ${check.url || prChecksUrl(pr) || prBrowserUrl(pr)}`;
}

/** The draft for a PR that cannot merge until its branch catches up with the base. */
export function fixMergePrompt(kind: "conflicts" | "behind", pr: MergeBoxPR): string {
	const target = pr.targetBranch || "the base branch";
	return kind === "conflicts"
		? `Resolve the merge conflicts on PR #${pr.number} with ${target}: ${prBrowserUrl(pr)}`
		: `Update PR #${pr.number} with the latest ${target}: ${prBrowserUrl(pr)}`;
}
