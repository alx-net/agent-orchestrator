import { describe, expect, it } from "vitest";
import type { MergeBoxPR, PRCheck } from "./pr-merge-box";
import { buildPRsOverview, openPRsBadge, prSessionsToLoad, type SessionPRsLoad } from "./prs-overview";
import type { PullRequestFacts, WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const NOW = Date.parse("2026-10-27T12:00:00.000Z");
const hoursAgo = (hours: number) => new Date(NOW - hours * 3_600_000).toISOString();

function fact(number: number, overrides: Partial<PullRequestFacts> = {}): PullRequestFacts {
	return {
		url: `https://github.com/acme/app/pull/${number}`,
		number,
		state: "open",
		ci: "passing",
		review: "none",
		mergeability: "mergeable",
		reviewComments: false,
		updatedAt: hoursAgo(1),
		...overrides,
	};
}

function session(id: string, prs: PullRequestFacts[], overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "app",
		workspaceName: "app",
		title: id,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		createdAt: hoursAgo(10),
		updatedAt: hoursAgo(1),
		prs,
		...overrides,
	};
}

function workspace(id: string, sessions: WorkspaceSession[]): WorkspaceSummary {
	return { id, name: id, path: `/repos/${id}`, sessions };
}

function summary(number: number, overrides: Partial<MergeBoxPR> = {}): MergeBoxPR {
	const url = `https://github.com/acme/app/pull/${number}`;
	return {
		url,
		htmlUrl: url,
		number,
		title: `PR ${number}`,
		state: "open",
		provider: "github",
		repo: "acme/app",
		author: "ada",
		sourceBranch: `feat/${number}`,
		targetBranch: "main",
		headSha: `sha-${number}`,
		additions: 1,
		deletions: 1,
		changedFiles: 1,
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: url, conflictFiles: [] },
		updatedAt: hoursAgo(1),
		...overrides,
	};
}

const check = (name: string, status: PRCheck["status"]): PRCheck => ({ name, status, required: false });

describe("prSessionsToLoad", () => {
	it("reads only live local sessions with an open or draft PR, most recently active first", () => {
		const workspaces = [workspace("app", [
			session("older", [fact(1)], { activity: { state: "idle", lastActivityAt: hoursAgo(5) }, updatedAt: hoursAgo(5) }),
			session("newer", [fact(2, { state: "draft" })], { activity: { state: "active", lastActivityAt: hoursAgo(0.1) } }),
			session("merged-only", [fact(3, { state: "merged" })]),
			session("no-prs", []),
			session("ended", [fact(4)], { isTerminated: true, status: "terminated" }),
			session("cloud", [fact(5)], { cloud: { orgId: "org" } }),
			session("remote", [fact(6)], { hostId: "host-1" }),
		])];
		expect(prSessionsToLoad(workspaces)).toEqual(["newer", "older"]);
		expect(prSessionsToLoad(workspaces, 1)).toEqual(["newer"]);
	});
});

describe("buildPRsOverview", () => {
	it("groups open and draft PRs by project and session and leaves merged, closed and ended ones out", () => {
		const overview = buildPRsOverview([
			workspace("app", [
				session("both", [fact(10), fact(11, { state: "draft" }), fact(9, { state: "closed" })]),
				session("merged-only", [fact(12, { state: "merged", updatedAt: hoursAgo(30) })]),
				session("ended", [fact(13)], { isTerminated: true, status: "terminated" }),
			]),
			workspace("quiet", [session("none", [])]),
			workspace("docs", [session("docs-pr", [fact(20)], { workspaceId: "docs" })]),
		], new Map(), NOW);

		expect(overview.projects.map((project) => project.id)).toEqual(["app", "docs"]);
		const app = overview.projects[0]!;
		expect(app.sessions.map((entry) => entry.session.id)).toEqual(["both"]);
		expect(app.sessions[0]!.prs.map((pr) => [pr.number, pr.state])).toEqual([[10, "open"], [11, "draft"]]);
		expect(app.prCount).toBe(2);
		expect(overview.open).toBe(3);
		expect(overview.recentlyMerged).toEqual([]);
	});

	it("puts failing CI and conflicts first, then running checks, then the newest session", () => {
		const overview = buildPRsOverview([workspace("app", [
			session("old-quiet", [fact(1)], { createdAt: hoursAgo(20) }),
			session("new-quiet", [fact(2)], { createdAt: hoursAgo(2) }),
			session("running", [fact(3, { ci: "pending" })], { createdAt: hoursAgo(1) }),
			session("conflicting", [fact(4, { mergeability: "conflicting" })], { createdAt: hoursAgo(9) }),
			session("failing", [fact(5, { ci: "failing", failingChecks: [{ name: "test", status: "failed", conclusion: "failure" }] })], { createdAt: hoursAgo(15) }),
		])], new Map(), NOW);

		const sessions = overview.projects[0]!.sessions;
		expect(sessions.map((entry) => [entry.session.id, entry.tier])).toEqual([
			["conflicting", "attention"],
			["failing", "attention"],
			["running", "running"],
			["new-quiet", "rest"],
			["old-quiet", "rest"],
		]);
		expect(sessions.map((entry) => entry.tone)).toEqual(["conflicts", "failing", "running", "idle", "idle"]);
		expect(overview).toMatchObject({ open: 5, failing: 1, running: 1, conflicts: 1 });
	});

	it("does not reorder sessions while their agents work", () => {
		const quiet = session("quiet", [fact(1)], { createdAt: hoursAgo(5) });
		const busy = session("busy", [fact(2)], { createdAt: hoursAgo(6) });
		const before = buildPRsOverview([workspace("app", [quiet, busy])], new Map(), NOW);
		const after = buildPRsOverview([workspace("app", [
			quiet,
			{ ...busy, activity: { state: "active", lastActivityAt: new Date(NOW).toISOString() }, updatedAt: new Date(NOW).toISOString() },
		])], new Map(), NOW);
		const order = (overview: typeof before) => overview.projects[0]!.sessions.map((entry) => entry.session.id);
		expect(order(after)).toEqual(order(before));
	});

	it("counts a failing PR whose other checks still run as both failing and running", () => {
		const failingAndRunning = summary(50, {
			ci: { autoInjectCI: true, state: "failing", failingChecks: [{ name: "test", status: "failed", conclusion: "failure" }] },
			checks: [check("test", "failed"), check("e2e", "in_progress")],
		});
		const overview = buildPRsOverview(
			[workspace("app", [session("s", [fact(50)])])],
			new Map([["s", { summaries: [failingAndRunning], pending: false, failed: false }]]),
			NOW,
		);
		expect(overview).toMatchObject({ failing: 1, running: 1 });
		expect(overview.projects[0]!.sessions[0]).toMatchObject({ tone: "failing", tier: "attention" });
	});

	it("reads the full summary over the facts, and says where each session's detail stands", () => {
		const running = summary(30, {
			title: "Add pausing",
			ci: { autoInjectCI: true, state: "pending", failingChecks: [] },
			checks: [check("lint", "passed"), check("unit", "in_progress"), check("e2e", "queued")],
		});
		const loads = new Map<string, SessionPRsLoad>([
			["ready", { summaries: [running], pending: false, failed: false }],
			["loading", { pending: true, failed: false }],
			["failed", { summaries: [summary(32)], pending: false, failed: true }],
		]);
		const overview = buildPRsOverview([workspace("app", [
			// The facts still say passing; the summary knows checks are running.
			session("ready", [fact(30)], { createdAt: hoursAgo(1) }),
			session("loading", [fact(31)], { createdAt: hoursAgo(2) }),
			session("failed", [fact(32)], { createdAt: hoursAgo(3) }),
			session("facts", [fact(33)], { createdAt: hoursAgo(4), cloud: { orgId: "org" } }),
		])], loads, NOW);

		const byId = new Map(overview.projects[0]!.sessions.map((entry) => [entry.session.id, entry]));
		expect(byId.get("ready")).toMatchObject({ detail: "ready", tier: "running" });
		expect(byId.get("ready")!.prs[0]).toBe(running);
		expect(byId.get("loading")!.detail).toBe("loading");
		expect(byId.get("failed")!.detail).toBe("failed");
		expect(byId.get("facts")!.detail).toBe("facts");
		expect(overview.running).toBe(1);
	});

	it("moves a PR the summary saw merge to the last day's merged group, newest first", () => {
		const loads = new Map<string, SessionPRsLoad>([
			["live", { summaries: [summary(40, { state: "merged", stateChangedAt: hoursAgo(2) }), summary(41)], pending: false, failed: false }],
		]);
		const overview = buildPRsOverview([workspace("app", [
			session("live", [fact(40), fact(41)]),
			session("cleaned-up", [fact(42, { state: "merged", updatedAt: hoursAgo(1) })], { isTerminated: true, status: "terminated" }),
			session("long-ago", [fact(43, { state: "merged", updatedAt: hoursAgo(25) })]),
		])], loads, NOW);

		expect(overview.projects[0]!.sessions.map((entry) => entry.prs.map((pr) => pr.number))).toEqual([[41]]);
		expect(overview.recentlyMerged.map((merged) => [merged.pr.number, merged.session.id])).toEqual([[42, "cleaned-up"], [40, "live"]]);
		expect(overview.open).toBe(1);
	});
});

describe("openPRsBadge", () => {
	it("counts open and draft PRs of live sessions, tinted by failing CI before conflicts", () => {
		const workspaces = [workspace("app", [
			session("a", [fact(1), fact(2, { state: "draft" }), fact(3, { state: "merged" })]),
			session("b", [fact(4, { mergeability: "conflicting" })]),
			session("ended", [fact(5, { ci: "failing" })], { isTerminated: true, status: "terminated" }),
		])];
		expect(openPRsBadge(workspaces)).toEqual({ count: 3, tone: "conflicts" });

		workspaces[0]!.sessions.push(session("c", [fact(6, { ci: "failing" })]));
		expect(openPRsBadge(workspaces)).toEqual({ count: 4, tone: "failing" });
	});

	it("is empty and calm with no open PRs", () => {
		expect(openPRsBadge([workspace("app", [session("a", [fact(1, { state: "merged" })])])])).toEqual({ count: 0, tone: "idle" });
	});

	it("says running while only checks are in flight", () => {
		expect(openPRsBadge([workspace("app", [session("a", [fact(1, { ci: "pending" })])])])).toEqual({ count: 1, tone: "running" });
	});
});
