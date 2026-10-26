import { describe, expect, it } from "vitest";
import {
	checkCountParts,
	checkLabel,
	checksSummary,
	checkTiming,
	fixCheckPrompt,
	fixMergePrompt,
	formatCheckDuration,
	groupChecks,
	mergeVerdict,
	prCheckSet,
	prGlyphTone,
	prIsStale,
	pullRequestsTabTone,
	type MergeBoxPR,
	type PRCheck,
} from "./pr-merge-box";

function pr(overrides: Partial<MergeBoxPR> = {}): MergeBoxPR {
	return {
		url: "https://api.github.com/repos/acme/app/pulls/12",
		htmlUrl: "https://github.com/acme/app/pull/12",
		number: 12,
		title: "Add pausing to sessions",
		state: "open",
		provider: "github",
		repo: "acme/app",
		author: "ada",
		sourceBranch: "feat/pause",
		targetBranch: "main",
		headSha: "abc123",
		additions: 40,
		deletions: 3,
		changedFiles: 4,
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: "https://github.com/acme/app/pull/12", conflictFiles: [] },
		updatedAt: "2026-10-26T10:00:00Z",
		...overrides,
	};
}

const at = (iso: string) => iso;
function check(overrides: Partial<PRCheck> & Pick<PRCheck, "name" | "status">): PRCheck {
	return { required: false, ...overrides };
}

/** The screenshot's PR: one failing required check, eight skipped, six passed. */
function githubLikeChecks(): PRCheck[] {
	return [
		check({ name: "test", workflow: "CI", status: "failed", required: true, startedAt: at("2026-10-26T10:00:00Z"), completedAt: at("2026-10-26T10:00:06Z"), url: "https://github.com/acme/app/actions/runs/1/job/2" }),
		...Array.from({ length: 8 }, (_, index) => check({ name: `deploy-${index}`, workflow: "Release", status: "skipped", completedAt: at("2026-10-26T08:00:00Z") })),
		...Array.from({ length: 6 }, (_, index) => check({ name: `lint-${index}`, workflow: "CI", status: "passed", required: index === 0, startedAt: at("2026-10-26T10:00:00Z"), completedAt: at("2026-10-26T10:01:12Z") })),
	];
}

describe("check groups and counts", () => {
	it("groups failing, running, skipped and successful checks in that order, failing and running open", () => {
		const groups = groupChecks([
			check({ name: "a", status: "passed" }),
			check({ name: "b", status: "skipped" }),
			check({ name: "c", status: "in_progress" }),
			check({ name: "d", status: "queued" }),
			check({ name: "e", status: "cancelled" }),
			check({ name: "f", status: "neutral" }),
			check({ name: "g", status: "failed" }),
		]);
		expect(groups.map((group) => [group.key, group.checks.map((entry) => entry.name), group.defaultOpen])).toEqual([
			["failing", ["e", "g"], true],
			["running", ["c", "d"], true],
			["skipped", ["b", "f"], false],
			["passed", ["a"], false],
		]);
	});

	it("leaves empty groups out and puts required checks first within a group", () => {
		const groups = groupChecks([
			check({ name: "optional", status: "passed" }),
			check({ name: "required", status: "passed", required: true }),
		]);
		expect(groups).toHaveLength(1);
		expect(groups[0]!.checks.map((entry) => entry.name)).toEqual(["required", "optional"]);
	});

	it("counts the screenshot case as one failing, eight skipped and six successful", () => {
		const summary = checksSummary(pr({ ci: { autoInjectCI: true, state: "failing", failingChecks: [] }, checks: githubLikeChecks() }));
		expect(summary.headline).toBe("failing");
		expect(summary.partial).toBe(false);
		expect(checkCountParts(summary.counts)).toEqual([
			{ key: "failing", count: 1 },
			{ key: "skipped", count: 8 },
			{ key: "passed", count: 6 },
		]);
	});

	it("says checks are running while any check is queued or in progress and none failed", () => {
		const summary = checksSummary(pr({ checks: [check({ name: "a", status: "queued" }), check({ name: "b", status: "passed" })] }));
		expect(summary.headline).toBe("running");
		expect(summary.counts).toEqual({ failing: 0, running: 1, skipped: 0, passed: 1 });
	});

	it("says all checks passed when every check passed or was skipped", () => {
		expect(checksSummary(pr({ checks: [check({ name: "a", status: "skipped" }), check({ name: "b", status: "passed" })] })).headline).toBe("passed");
	});

	it("says no checks were reported for an empty list instead of claiming a pass", () => {
		expect(checksSummary(pr({ checks: [] })).headline).toBe("none");
		expect(checksSummary(pr({ checks: [], ci: { autoInjectCI: true, state: "pending", failingChecks: [] } })).headline).toBe("running");
	});
});

describe("older daemons without the full check list", () => {
	it("falls back to the failing checks and marks the list partial", () => {
		const fallback = pr({
			ci: {
				autoInjectCI: true,
				state: "failing",
				failingChecks: [
					{ name: "CI / test", status: "failed", conclusion: "failure", url: "https://github.com/acme/app/runs/9" },
					{ name: "CI / e2e", status: "cancelled", conclusion: "cancelled" },
				],
			},
		});
		const set = prCheckSet(fallback);
		expect(set.partial).toBe(true);
		expect(set.checks).toEqual([
			{ name: "CI / test", status: "failed", conclusion: "failure", required: false, url: "https://github.com/acme/app/runs/9" },
			{ name: "CI / e2e", status: "cancelled", conclusion: "cancelled", required: false, url: undefined },
		]);
		const summary = checksSummary(fallback);
		expect(summary.headline).toBe("failing");
		expect(summary.counts.failing).toBe(2);
		expect(groupChecks(set.checks).map((group) => group.key)).toEqual(["failing"]);
	});

	it("takes the CI rollup for the headline when no failing check is named", () => {
		const state = (value: MergeBoxPR["ci"]["state"]) => checksSummary(pr({ ci: { autoInjectCI: true, state: value, failingChecks: [] } })).headline;
		expect(state("failing")).toBe("failing");
		expect(state("pending")).toBe("running");
		expect(state("passing")).toBe("passed");
		expect(state("unknown")).toBe("unknown");
	});
});

describe("check timing", () => {
	const now = Date.parse("2026-10-26T10:05:00Z");

	it("says how long a failed check ran before failing", () => {
		expect(checkTiming(githubLikeChecks()[0]!, now)).toEqual({ kind: "failedAfter", durationMs: 6_000 });
	});

	it("says how long a passed check took and when a skipped one was skipped", () => {
		const checks = githubLikeChecks();
		expect(checkTiming(checks.at(-1)!, now)).toEqual({ kind: "passedIn", durationMs: 72_000 });
		expect(checkTiming(checks[1]!, now)).toEqual({ kind: "skippedAt", at: "2026-10-26T08:00:00Z" });
	});

	it("counts a running check from its start, and a queued one has no time yet", () => {
		expect(checkTiming(check({ name: "a", status: "in_progress", startedAt: "2026-10-26T10:02:00Z" }), now)).toEqual({ kind: "runningFor", durationMs: 180_000 });
		expect(checkTiming(check({ name: "a", status: "in_progress" }), now)).toEqual({ kind: "running" });
		expect(checkTiming(check({ name: "a", status: "queued", startedAt: "2026-10-26T10:02:00Z" }), now)).toEqual({ kind: "queued" });
	});

	it("falls back to when it settled, then to the bare status", () => {
		expect(checkTiming(check({ name: "a", status: "failed", completedAt: "2026-10-26T10:00:00Z" }), now)).toEqual({ kind: "failedAt", at: "2026-10-26T10:00:00Z" });
		expect(checkTiming(check({ name: "a", status: "cancelled" }), now)).toEqual({ kind: "cancelled" });
		expect(checkTiming(check({ name: "a", status: "neutral", startedAt: "2026-10-26T10:00:00Z", completedAt: "2026-10-26T10:00:30Z" }), now)).toEqual({ kind: "completedIn", durationMs: 30_000 });
		expect(checkTiming(check({ name: "a", status: "passed", startedAt: "not a date" }), now)).toEqual({ kind: "passed" });
	});

	it("formats durations to the second under an hour", () => {
		expect(formatCheckDuration(0)).toBe("1s");
		expect(formatCheckDuration(6_400)).toBe("6s");
		expect(formatCheckDuration(180_000)).toBe("3m");
		expect(formatCheckDuration(72_000)).toBe("1m 12s");
		expect(formatCheckDuration(3_600_000)).toBe("1h");
		expect(formatCheckDuration(7_500_000)).toBe("2h 5m");
	});
});

describe("merge state", () => {
	it("is ready when the Merge button would merge", () => {
		expect(mergeVerdict(pr())).toEqual({ kind: "ready" });
	});

	it("reads a branch behind its base as stale from GitHub's BEHIND", () => {
		const behind = pr({ mergeStateStatus: "BEHIND", mergeability: { state: "blocked", reasons: [], prUrl: "", conflictFiles: [] } });
		expect(prIsStale(behind)).toBe(true);
		expect(mergeVerdict(behind)).toEqual({ kind: "behind" });
	});

	it("reads a branch behind its base as stale from the daemon's behind_base reason", () => {
		const behind = pr({ mergeability: { state: "blocked", reasons: ["behind_base"], prUrl: "", conflictFiles: [] } });
		expect(prIsStale(behind)).toBe(true);
		expect(mergeVerdict(behind)).toEqual({ kind: "behind" });
		expect(prIsStale(pr())).toBe(false);
	});

	it("puts conflicts ahead of a stale branch and lists the conflicting files", () => {
		const files = [{ path: "backend/go.mod" }, { path: "frontend/package.json", url: "https://github.com/acme/app/pull/12/conflicts" }];
		const conflicting = pr({ mergeStateStatus: "BEHIND", mergeability: { state: "conflicting", reasons: ["behind_base"], prUrl: "", conflictFiles: files } });
		expect(mergeVerdict(conflicting)).toEqual({ kind: "conflicts", files });
		expect(mergeVerdict(pr({ mergeStateStatus: "DIRTY", mergeability: { state: "unknown", reasons: [], prUrl: "" } }))).toEqual({ kind: "conflicts", files: [] });
	});

	it("is blocked by failing checks and by reviews, naming each", () => {
		const failing = pr({ ci: { autoInjectCI: true, state: "failing", failingChecks: [] }, mergeability: { state: "blocked", reasons: ["ci_failing"], prUrl: "", conflictFiles: [] } });
		expect(mergeVerdict(failing)).toEqual({ kind: "blocked", checks: true, review: undefined });
		const review = pr({ review: { decision: "review_required", hasUnresolvedHumanComments: false, unresolvedBy: [] }, mergeability: { state: "blocked", reasons: [], prUrl: "", conflictFiles: [] } });
		expect(mergeVerdict(review)).toEqual({ kind: "blocked", checks: false, review: "required" });
		expect(mergeVerdict({ ...failing, review: { decision: "changes_requested", hasUnresolvedHumanComments: true, unresolvedBy: [] } })).toEqual({ kind: "blocked", checks: true, review: "changes_requested" });
	});

	it("is still checking while checks run, and a draft or settled PR says so", () => {
		expect(mergeVerdict(pr({ ci: { autoInjectCI: true, state: "pending", failingChecks: [] } }))).toEqual({ kind: "checking" });
		expect(mergeVerdict(pr({ state: "draft" }))).toEqual({ kind: "draft" });
		expect(mergeVerdict(pr({ mergeStateStatus: "DRAFT" }))).toEqual({ kind: "draft" });
		expect(mergeVerdict(pr({ state: "merged", stateChangedAt: "2026-10-26T09:00:00Z" }))).toEqual({ kind: "merged", at: "2026-10-26T09:00:00Z" });
		expect(mergeVerdict(pr({ state: "closed" }))).toEqual({ kind: "closed", at: undefined });
	});

	it("does not guess when GitHub access is gone", () => {
		expect(mergeVerdict(pr({ mergeability: { state: "unknown", reasons: ["github_access_lost"], prUrl: "" } }))).toEqual({ kind: "unavailable", reason: "access" });
	});
});

describe("tints", () => {
	const failing = pr({ number: 1, checks: githubLikeChecks(), ci: { autoInjectCI: true, state: "failing", failingChecks: [] } });
	const conflicting = pr({ number: 2, mergeability: { state: "conflicting", reasons: [], prUrl: "", conflictFiles: [] } });
	const running = pr({ number: 3, checks: [check({ name: "a", status: "in_progress" })], ci: { autoInjectCI: true, state: "pending", failingChecks: [] } });

	it("tints the tab for failing CI first, then conflicts, then running checks, on open PRs only", () => {
		expect(pullRequestsTabTone([pr(), running, conflicting, failing])).toBe("failing");
		expect(pullRequestsTabTone([running, conflicting])).toBe("conflicts");
		expect(pullRequestsTabTone([running, pr()])).toBe("running");
		expect(pullRequestsTabTone([pr(), { ...failing, state: "merged" }])).toBe("idle");
	});

	it("tints each PR glyph by what it most needs", () => {
		expect(prGlyphTone(failing)).toBe("failing");
		expect(prGlyphTone(conflicting)).toBe("attention");
		expect(prGlyphTone(pr({ mergeability: { state: "blocked", reasons: ["behind_base"], prUrl: "", conflictFiles: [] } }))).toBe("attention");
		expect(prGlyphTone(running)).toBe("running");
		expect(prGlyphTone(pr())).toBe("ready");
		expect(prGlyphTone(pr({ state: "merged" }))).toBe("merged");
		expect(prGlyphTone(pr({ state: "draft" }))).toBe("draft");
	});
});

describe("drafts for the agent", () => {
	it("names the failing check by workflow and job, with its run link", () => {
		const failed = githubLikeChecks()[0]!;
		expect(checkLabel(failed)).toBe("CI / test");
		expect(checkLabel(check({ name: "build", workflow: "build", status: "passed" }))).toBe("build");
		expect(fixCheckPrompt(failed, pr())).toBe(
			'Fix the failing check "CI / test" on PR #12: https://github.com/acme/app/actions/runs/1/job/2',
		);
		expect(fixCheckPrompt(check({ name: "lint", status: "failed" }), pr())).toBe(
			'Fix the failing check "lint" on PR #12: https://github.com/acme/app/pull/12/checks',
		);
	});

	it("asks for a rebase onto the target branch for conflicts and a stale branch", () => {
		expect(fixMergePrompt("conflicts", pr())).toBe("Resolve the merge conflicts on PR #12 with main: https://github.com/acme/app/pull/12");
		expect(fixMergePrompt("behind", pr())).toBe("Update PR #12 with the latest main: https://github.com/acme/app/pull/12");
	});
});
