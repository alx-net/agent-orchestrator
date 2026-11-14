/**
 * The global PRs page's view model: every open or draft pull request of every
 * live session, grouped by project and then by session, with the sessions whose
 * PRs need a person first.
 *
 * Pure and untranslated. A session's PRs are its PR facts from the workspace
 * list, overlaid with the full summary once the page has read it: the same merge
 * the session's own Pull requests tab does. What a row says about checks and
 * merging comes from lib/pr-merge-box.ts, so the two never disagree.
 */

import type { SessionPRSummary } from "../hooks/useSessionScmSummary";
import { sessionIsActive, type WorkspaceSession, type WorkspaceSummary } from "../types/workspace";
import { sessionActivityTime } from "./agents-overview";
import { sessionPRDisplaySummaries } from "./pr-display";
import { checksSummary, pullRequestsTabTone, type ChecksSummary, type MergeBoxPR, type PullRequestsTabTone } from "./pr-merge-box";

/** How many sessions' PR summaries the page reads, most recently active first. */
export const PRS_OVERVIEW_SESSION_CAP = 40;

/** How far back the "Recently merged" group reaches. */
export const RECENTLY_MERGED_MS = 24 * 60 * 60 * 1000;

/** Open or draft: a PR still in flight. */
export function prIsActive(pr: Pick<SessionPRSummary, "state">): boolean {
	return pr.state === "open" || pr.state === "draft";
}

/**
 * A live session with an open or draft PR in its facts. A terminated session is
 * no longer observed, so its open PR facts are history rather than state.
 */
export function sessionHasActivePR(session: WorkspaceSession): boolean {
	return sessionIsActive(session) && session.prs.some(prIsActive);
}

/** The page reads summaries from the local daemon only; a cloud or remote session shows its PR facts. */
function summaryReadable(session: WorkspaceSession): boolean {
	return !session.hostId && !session.cloud;
}

/** The sessions whose PR summaries the page reads: live, local, with an open or draft PR, most recently active first. */
export function prSessionsToLoad(workspaces: readonly WorkspaceSummary[], cap = PRS_OVERVIEW_SESSION_CAP): string[] {
	const candidates: WorkspaceSession[] = [];
	for (const workspace of workspaces) {
		for (const session of workspace.sessions) if (summaryReadable(session) && sessionHasActivePR(session)) candidates.push(session);
	}
	candidates.sort((a, b) => sessionActivityTime(b) - sessionActivityTime(a) || a.id.localeCompare(b.id));
	return candidates.slice(0, cap).map((session) => session.id);
}

/** What the page has read of one session's PR summary. */
export interface SessionPRsLoad {
	summaries?: SessionPRSummary[];
	pending: boolean;
	failed: boolean;
}

/**
 * Where a session's PR detail stands: the full summary is in, it is still on its
 * way, the last read failed (the last known state shows), or the page does not
 * read it (cloud, remote, or past the cap) and shows the PR facts alone.
 */
export type PRsDetail = "ready" | "loading" | "failed" | "facts";

/** Failing CI or conflicts first, then checks still running, then the rest. */
export type PRsTier = "attention" | "running" | "rest";

export interface PRsOverviewSession {
	session: WorkspaceSession;
	/** Its open and draft PRs, open first, then by number. */
	prs: MergeBoxPR[];
	tone: PullRequestsTabTone;
	tier: PRsTier;
	detail: PRsDetail;
}

export interface PRsOverviewProject {
	id: string;
	name: string;
	sessions: PRsOverviewSession[];
	/** Open and draft PRs across its sessions. */
	prCount: number;
}

export interface MergedPR {
	session: WorkspaceSession;
	pr: MergeBoxPR;
	mergedAt: string;
}

export interface PRsOverview {
	projects: PRsOverviewProject[];
	/** Open and draft PRs on the page. */
	open: number;
	/** PRs whose checks are failing. */
	failing: number;
	/** PRs with a check still queued or running, failing ones included. */
	running: number;
	/** PRs that conflict with their base. */
	conflicts: number;
	/** PRs merged in the last {@link RECENTLY_MERGED_MS}, newest first. */
	recentlyMerged: MergedPR[];
}

const TIER_RANK: Record<PRsTier, number> = { attention: 0, running: 1, rest: 2 };

export function tierOf(tone: PullRequestsTabTone): PRsTier {
	return tone === "failing" || tone === "conflicts" ? "attention" : tone === "running" ? "running" : "rest";
}

function timeOf(value: string | null | undefined): number {
	const parsed = value ? Date.parse(value) : Number.NaN;
	return Number.isFinite(parsed) ? parsed : 0;
}

/**
 * Attention first, then running checks, then newest session first. A session
 * moves only when its PRs change tier: activity while the agent works does not
 * reorder the page.
 */
function compareSessions(a: PRsOverviewSession, b: PRsOverviewSession): number {
	return TIER_RANK[a.tier] - TIER_RANK[b.tier] ||
		startedAt(b.session) - startedAt(a.session) ||
		a.session.id.localeCompare(b.session.id);
}

function startedAt(session: WorkspaceSession): number {
	return timeOf(session.createdAt) || timeOf(session.updatedAt);
}

function detailOf(load: SessionPRsLoad | undefined): PRsDetail {
	if (!load) return "facts";
	if (load.failed) return "failed";
	if (load.summaries === undefined) return load.pending ? "loading" : "facts";
	return "ready";
}

/** Some check is still queued or running, whatever the others say. */
export function prChecksRunning(checks: ChecksSummary): boolean {
	return checks.headline === "running" || checks.counts.running > 0;
}

function prConflicts(pr: MergeBoxPR): boolean {
	return pr.mergeability.state === "conflicting" || pr.mergeStateStatus === "DIRTY";
}

/** When a merged PR merged, as best the facts or summary say. */
function mergedAt(pr: MergeBoxPR): string | undefined {
	return pr.stateChangedAt || pr.updatedAt || undefined;
}

/**
 * Every live session's open and draft PRs as one list per project, plus the PRs
 * any session merged in the last day. `loads` holds what the page read for the
 * sessions {@link prSessionsToLoad} chose; any other session shows its facts.
 */
export function buildPRsOverview(
	workspaces: readonly WorkspaceSummary[],
	loads: ReadonlyMap<string, SessionPRsLoad>,
	now: number = Date.now(),
): PRsOverview {
	const projects: PRsOverviewProject[] = [];
	const recentlyMerged: MergedPR[] = [];
	let open = 0;
	let failing = 0;
	let running = 0;
	let conflicts = 0;
	for (const workspace of workspaces) {
		const sessions: PRsOverviewSession[] = [];
		let prCount = 0;
		for (const session of workspace.sessions) {
			const live = sessionIsActive(session);
			// Merged PRs count for the last-day group whether or not the session is still live.
			if (!live && !session.prs.some((pr) => pr.state === "merged")) continue;
			const load = loads.get(session.id);
			const all = sessionPRDisplaySummaries(session, load?.summaries) as MergeBoxPR[];
			for (const pr of all) {
				if (pr.state !== "merged") continue;
				const at = mergedAt(pr);
				if (at && now - timeOf(at) <= RECENTLY_MERGED_MS) recentlyMerged.push({ session, pr, mergedAt: at });
			}
			if (!live) continue;
			const prs = all.filter(prIsActive);
			if (prs.length === 0) continue;
			for (const pr of prs) {
				const checks = checksSummary(pr);
				if (checks.headline === "failing") failing += 1;
				// A failing PR can still have checks running.
				if (prChecksRunning(checks)) running += 1;
				if (prConflicts(pr)) conflicts += 1;
			}
			const tone = pullRequestsTabTone(prs);
			sessions.push({ session, prs, tone, tier: tierOf(tone), detail: detailOf(load) });
			prCount += prs.length;
		}
		if (sessions.length === 0) continue;
		open += prCount;
		projects.push({ id: workspace.id, name: workspace.name, sessions: sessions.sort(compareSessions), prCount });
	}
	recentlyMerged.sort((a, b) => timeOf(b.mergedAt) - timeOf(a.mergedAt) || a.pr.number - b.pr.number);
	return { projects, open, failing, running, conflicts, recentlyMerged };
}

export interface OpenPRsBadge {
	/** Open and draft PRs across every live session. */
	count: number;
	/** What those PRs most need, under the Pull requests tab's own rule. */
	tone: PullRequestsTabTone;
}

/** The sidebar's PRs count, from the sessions' PR facts alone: the sidebar reads no summaries. */
export function openPRsBadge(workspaces: readonly WorkspaceSummary[]): OpenPRsBadge {
	const prs: MergeBoxPR[] = [];
	for (const workspace of workspaces) {
		for (const session of workspace.sessions) {
			if (!sessionHasActivePR(session)) continue;
			for (const pr of sessionPRDisplaySummaries(session) as MergeBoxPR[]) if (prIsActive(pr)) prs.push(pr);
		}
	}
	return { count: prs.length, tone: pullRequestsTabTone(prs) };
}
