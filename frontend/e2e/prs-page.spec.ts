import { mkdirSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// The global PRs page: every live session's open PRs grouped by project and
// session, one fixed-height row per PR with its review, one checks line (with a
// spinner while checks run), its merge state and when it was last read. A row
// opens to the same merge box as the session's Pull requests tab, and Merge
// works from the row. The sidebar's PRs count is tinted for the failing CI.

const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();
const failingRun = "https://github.com/acme/app/actions/runs/101/job/202";

type Check = {
	name: string;
	workflow?: string;
	status: string;
	required: boolean;
	startedAt?: string;
	completedAt?: string;
	url?: string;
};

function pr(number: number, fields: Record<string, unknown>) {
	const htmlUrl = `https://github.com/acme/app/pull/${number}`;
	return {
		url: htmlUrl,
		htmlUrl,
		number,
		state: "open",
		provider: "github",
		repo: "acme/app",
		author: "ada",
		targetBranch: "main",
		headSha: `sha-${number}`,
		changedFiles: 3,
		additions: 40,
		deletions: 4,
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: htmlUrl, conflictFiles: [] },
		updatedAt: at(90),
		observedAt: at(90),
		ciObservedAt: at(90),
		...fields,
	};
}

/** A failing required check, three checks still running, six passed, and the branch behind main. */
const failing = pr(41, {
	title: "Add pausing to sessions",
	sourceBranch: "feat/session-pause",
	ci: { autoInjectCI: true, state: "failing", failingChecks: [{ name: "CI / test", status: "failed", conclusion: "failure", url: failingRun }] },
	mergeability: { state: "blocked", reasons: ["ci_failing", "behind_base"], prUrl: "https://github.com/acme/app/pull/41", conflictFiles: [] },
	review: { decision: "changes_requested", hasUnresolvedHumanComments: false, unresolvedBy: [] },
	mergeStateStatus: "BEHIND",
	checks: [
		{ name: "test", workflow: "CI", status: "failed", required: true, startedAt: at(306), completedAt: at(300), url: failingRun },
		...["e2e-chromium", "e2e-webkit", "integration"].map(
			(name): Check => ({ name, workflow: "CI", status: "in_progress", required: name === "integration", startedAt: at(180), url: `https://github.com/acme/app/actions/runs/101/job/${name}` }),
		),
		...["lint", "typecheck", "unit", "build", "go-vet", "codeql"].map(
			(name, index): Check => ({ name, workflow: "CI", status: "passed", required: index < 2, startedAt: at(372), completedAt: at(300), url: `https://github.com/acme/app/actions/runs/101/job/${name}` }),
		),
	],
});

/** Green, approved and up to date with main. */
const ready = pr(42, {
	title: "Document the pause route",
	sourceBranch: "docs/pause-route",
	review: { decision: "approved", hasUnresolvedHumanComments: false, unresolvedBy: [] },
	mergeStateStatus: "CLEAN",
	checks: [
		{ name: "lint", workflow: "CI", status: "passed", required: true, startedAt: at(250), completedAt: at(210), url: "https://github.com/acme/app/actions/runs/104/job/lint" },
		{ name: "docs", workflow: "Docs", status: "passed", required: false, startedAt: at(250), completedAt: at(190), url: "https://github.com/acme/app/actions/runs/105/job/docs" },
	],
});

function facts(summary: ReturnType<typeof pr>) {
	return {
		url: summary.url,
		number: summary.number,
		state: summary.state,
		ci: (summary.ci as { state: string }).state,
		review: (summary.review as { decision: string }).decision,
		mergeability: (summary.mergeability as { state: string }).state,
		failingChecks: (summary.ci as { failingChecks: unknown[] }).failingChecks,
		reviewComments: false,
		updatedAt: summary.updatedAt,
	};
}

/** What the fake daemon has done to the PRs, and which sessions' summaries the page read. */
interface PullRequestState {
	merged: boolean;
	reads: string[];
}

async function installPullRequests(page: Page, state: PullRequestState) {
	await installFakeAgent(page, {
		workers: [
			{ id: "pr-pausing", title: "Ship session pausing", mode: "chat", provider: "claude-code", status: "ci_failed", activity: "active", createdAt: at(3_600), prs: [failing].map(facts) },
			{ id: "pr-docs", title: "Document pausing", mode: "chat", provider: "claude-code", status: "mergeable", activity: "idle", createdAt: at(1_800), prs: [ready].map(facts) },
			{ id: "no-pr", title: "Spike the scheduler", mode: "chat", provider: "claude-code", activity: "idle", createdAt: at(600) },
		],
	});
	await page.route("**/api/v1/sessions/**", async (route) => {
		const path = new URL(route.request().url()).pathname;
		const sessionId = path.split("/")[4] ?? "";
		if (path.endsWith("/pr")) {
			state.reads.push(sessionId);
			if (sessionId === "pr-pausing") return route.fulfill({ json: { prs: [failing], linkedPrs: [] } });
			if (sessionId === "pr-docs") {
				return route.fulfill({ json: { prs: [state.merged ? { ...ready, state: "merged", stateChangedAt: at(1) } : ready], linkedPrs: [] } });
			}
			return route.fulfill({ json: { prs: [], linkedPrs: [] } });
		}
		if (path.endsWith("/conversation") && route.request().method() === "GET") {
			return route.fulfill({
				json: {
					conversationId: `conversation-${sessionId}`, sessionId, harness: "claude-code", mode: "chat", controller: "ready",
					latestSequence: 0, oldestSequence: 0, hasMoreBefore: false, turns: [], messages: [], activities: [], settings: {},
				},
			});
		}
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
}

async function capture(page: Page, name: string) {
	const directory = process.env.AO_PRS_EVIDENCE_DIR;
	if (!directory) return;
	mkdirSync(directory, { recursive: true });
	await page.screenshot({ path: `${directory}/${name}.png` });
}

test("the PRs page shows every open PR with its CI, grouped by session, and merges from a row @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	const state: PullRequestState = { merged: false, reads: [] };
	await installPullRequests(page, state);
	await page.goto("/#/");

	// After Agents, counting both open PRs, tinted for the failing CI.
	const agents = page.getByRole("button", { name: "Agents", exact: true });
	const entry = page.getByRole("button", { name: "PRs", exact: true });
	await expect(entry).toBeVisible();
	await expect(entry).toContainText("2");
	await expect(entry.locator("[data-tone]")).toHaveAttribute("data-tone", "failing");
	expect((await agents.boundingBox())!.y).toBeLessThan((await entry.boundingBox())!.y);
	await entry.click();
	await expect(page).toHaveURL(/#\/prs$/);

	const overview = page.getByTestId("prs-overview");
	const project = overview.getByRole("region", { name: "fake-proj" });
	await expect(project).toBeVisible();
	const sessions = project.getByTestId("prs-overview-session");
	// Only the sessions with an open PR, the failing one first.
	await expect(sessions).toHaveCount(2);
	await expect(sessions.nth(0)).toHaveAttribute("data-session-id", "pr-pausing");
	await expect(sessions.nth(1)).toHaveAttribute("data-session-id", "pr-docs");
	expect([...new Set(state.reads)].sort()).toEqual(["pr-docs", "pr-pausing"]);
	await expect(overview.getByTestId("prs-overview-summary")).toHaveText("1 failing1 running2 open");

	// The working session shows its spinner in the status slot.
	await expect(sessions.nth(0).getByTestId("prs-overview-session-row").locator("[data-working='true']")).toBeVisible();
	await expect(sessions.nth(0).getByTestId("prs-overview-session-row")).toContainText("session/pr-pausing");

	const row = (number: number) => overview.locator(`[data-testid='prs-overview-pr'][data-pr-number='${number}']`);
	await expect(row(41).getByTestId("prs-overview-checks")).toHaveText("Some checks were not successful · 1 failing, 3 in progress, 6 successful");
	// The failure leads the line; a spinner sits by the checks still running.
	await expect(row(41).getByTestId("prs-overview-checks-spinner")).toBeVisible();
	await expect(row(41).getByTestId("prs-overview-merge-state")).toHaveText("Out of date with main");
	await expect(row(41).getByTestId("prs-overview-review")).toHaveText("Changes requested");
	await expect(row(41).getByRole("link", { name: /#41\s*Add pausing to sessions/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/41");
	await expect(row(42).getByTestId("prs-overview-checks")).toHaveText("All checks have passed · 2 successful");
	await expect(row(42).getByTestId("prs-overview-merge-state")).toHaveText("Ready to merge");
	await expect(row(42).getByRole("button", { name: "Merge PR #42" })).toBeVisible();
	await expect(row(41).getByRole("button", { name: "Merge PR #41" })).toHaveCount(0);

	// Every PR row is the same fixed height, whatever its checks say.
	const heights = await overview.getByTestId("prs-overview-pr-row").evaluateAll((rows) => rows.map((element) => element.getBoundingClientRect().height));
	expect(heights).toEqual([56, 56]);
	await capture(page, "prs-page-1-overview");

	// Opening a row shows the shared merge box in a fixed-height scroll area.
	const disclosure = row(41).getByRole("button", { name: "Details for PR #41" });
	await disclosure.click();
	await expect(disclosure).toHaveAttribute("aria-expanded", "true");
	await expect.poll(() => disclosure.locator("svg").evaluate((element) => getComputedStyle(element).rotate)).toBe("90deg");
	const box = overview.getByRole("region", { name: "Checks and merge state of PR #41" });
	await expect(box).toBeVisible();
	expect((await box.boundingBox())!.height).toBe(288);
	// The row above did not move or grow.
	expect(await overview.getByTestId("prs-overview-pr-row").first().evaluate((element) => element.getBoundingClientRect().height)).toBe(56);
	await expect(box.getByTestId("pr-checks-counts")).toHaveText("1 failing, 3 in progress, 6 successful");
	const failingGroup = box.getByRole("region", { name: "1 failing check" });
	await expect(failingGroup.getByTestId("pr-check")).toContainText("CI / test");
	await expect(failingGroup.getByTestId("pr-check")).toContainText("Failing after 6s");
	await expect(failingGroup.getByTestId("pr-check")).toContainText("Required");
	await expect(box.getByRole("region", { name: "3 in progress checks" }).getByTestId("pr-check")).toHaveCount(3);
	// "Running for" counts up while the checks run.
	const runningTiming = box.getByRole("region", { name: "3 in progress checks" }).getByTestId("pr-check-timing").first();
	await expect(runningTiming).toContainText(/Running for 3m/);
	const firstTiming = await runningTiming.textContent();
	await expect.poll(() => runningTiming.textContent(), { timeout: 5_000 }).not.toBe(firstTiming);
	await expect(box.getByRole("button", { name: "6 successful checks" })).toHaveAttribute("aria-expanded", "false");
	await expect(box.getByTestId("pr-merge-state")).toContainText("Branch is out of date with main");
	await capture(page, "prs-page-2-expanded");

	// Merge the ready PR from its row. The browser harness runs on preview data,
	// so the shared merge action skips its POST (unit-tested) and re-reads the
	// session's PRs, which the fake daemon now reports merged.
	state.merged = true;
	await row(42).getByRole("button", { name: "Merge PR #42" }).click();
	await expect(row(42)).toHaveCount(0);
	await expect(sessions).toHaveCount(1);
	const merged = overview.getByRole("button", { name: /Recently merged/ });
	await expect(merged).toHaveAttribute("aria-expanded", "false");
	await merged.click();
	const mergedRow = overview.getByTestId("prs-overview-merged");
	await expect(mergedRow).toHaveCount(1);
	await expect(mergedRow).toContainText("Document the pause route");
	await expect(mergedRow).toContainText("Document pausing");
	await capture(page, "prs-page-3-merged");

	await page.emulateMedia({ colorScheme: "dark" });
	await capture(page, "prs-page-4-dark");

	// A session row opens that session.
	await sessions.nth(0).getByRole("button", { name: "Open Ship session pausing" }).click();
	await expect(page).toHaveURL(/#\/projects\/fake-proj\/sessions\/pr-pausing$/);
});

test("the PRs page says when no session has an open pull request @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	await installFakeAgent(page, { workers: [{ id: "no-pr", title: "Spike the scheduler", mode: "chat", provider: "claude-code", activity: "idle" }] });
	await page.goto("/#/prs");
	await expect(page.getByTestId("prs-overview-empty")).toContainText("No open pull requests");
	await expect(page.getByRole("button", { name: "PRs", exact: true })).toHaveText("PRs");
	await capture(page, "prs-page-5-empty");
});
