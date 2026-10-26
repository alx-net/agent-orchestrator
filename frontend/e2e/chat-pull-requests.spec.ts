import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// The Pull requests tab of a chat session with two PRs in parallel: one merge
// box each, with the head commit's checks grouped like GitHub's merge box
// (failing and running open, skipped and passed folded), whether the branch
// fell behind its base, and the shared Merge action on the PR that can merge.
// A failing check's "Fix this" drafts the fix in the composer under the tab.

const sessionId = "chat-pull-requests";
const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();
const completedAt = at(600);
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
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: htmlUrl, conflictFiles: [] },
		updatedAt: at(90),
		observedAt: at(90),
		ciObservedAt: at(90),
		...fields,
	};
}

/** One failing required check, eight skipped, six passed, and the branch behind main. */
const stale = pr(41, {
	title: "Add pausing to sessions",
	sourceBranch: "feat/session-pause",
	additions: 182,
	deletions: 14,
	ci: { autoInjectCI: true, state: "failing", failingChecks: [{ name: "CI / test", status: "failed", conclusion: "failure", url: failingRun }] },
	mergeability: { state: "blocked", reasons: ["ci_failing", "behind_base"], prUrl: "https://github.com/acme/app/pull/41", conflictFiles: [] },
	mergeStateStatus: "BEHIND",
	checks: [
		{ name: "test", workflow: "CI", status: "failed", required: true, startedAt: at(306), completedAt: at(300), url: failingRun },
		...["deploy-preview", "deploy-docs", "publish-npm", "publish-image", "release-notes", "notify-slack", "sbom", "provenance"].map(
			(name): Check => ({ name, workflow: "Release", status: "skipped", required: false, completedAt: at(7_200), url: `https://github.com/acme/app/actions/runs/103/job/${name}` }),
		),
		...["lint", "typecheck", "unit", "build", "go-vet", "codeql"].map(
			(name, index): Check => ({ name, workflow: "CI", status: "passed", required: index < 2, startedAt: at(372), completedAt: at(300), url: `https://github.com/acme/app/actions/runs/101/job/${name}` }),
		),
	],
});

/** Green, approved and up to date with main. */
const clean = pr(42, {
	title: "Document the pause route",
	sourceBranch: "docs/pause-route",
	additions: 36,
	deletions: 2,
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
		reviewComments: false,
		updatedAt: summary.updatedAt,
	};
}

function snapshot() {
	const turnId = "turn-pause";
	return {
		conversationId: "conversation-chat-pull-requests",
		sessionId,
		harness: "claude-code",
		mode: "chat",
		controller: "ready",
		latestSequence: 2,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns: [{ id: turnId, state: "completed", providerTurnId: "provider-pause", requestedAt: completedAt, startedAt: completedAt, completedAt }],
		messages: [
			{ kind: "message", id: "user-pause", turnId, sequence: 1, revision: 0, role: "user", origin: "human", text: "Add pausing to sessions and document the route", streaming: false, createdAt: completedAt },
			{ kind: "message", id: "assistant-pause", turnId, sequence: 2, revision: 0, role: "assistant", origin: "provider", text: "Opened #41 for the pause flow and #42 for the docs.", streaming: false, createdAt: completedAt },
		],
		activities: [],
		settings: {},
	};
}

/** What the fake daemon has done to the PRs, and how often the renderer re-read them. */
interface PullRequestState {
	merged: boolean;
	summaryReads: number;
}

async function installPullRequestSession(page: Page, state: PullRequestState = { merged: false, summaryReads: 0 }) {
	await installFakeAgent(page, {
		workers: [{ id: sessionId, title: "Add pausing to sessions", mode: "chat", provider: "claude-code", status: "ci_failed", prs: [stale, clean].map(facts) }],
	});
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		if (path.endsWith("/pr")) {
			state.summaryReads += 1;
			const prs = state.merged ? [stale, { ...clean, state: "merged", stateChangedAt: at(1) }] : [stale, clean];
			return route.fulfill({ json: { prs, linkedPrs: [] } });
		}
		if (path.endsWith("/conversation") && route.request().method() === "GET") return route.fulfill({ json: snapshot() });
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
}

async function capture(page: Page, name: string) {
	const directory = process.env.AO_PR_EVIDENCE_DIR;
	if (directory) await page.screenshot({ path: `${directory}/${name}.png` });
}

test("the Pull requests tab shows a merge box per PR with grouped checks, a stale branch and merge readiness @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	const state: PullRequestState = { merged: false, summaryReads: 0 };
	await installPullRequestSession(page, state);

	// After the Agents tab, with the count tinted for the failing CI.
	const tabs = page.getByRole("tablist", { name: "Chat tabs" }).getByRole("tab");
	const tab = page.getByRole("tab", { name: /Pull requests/ });
	await expect(tab).toBeVisible();
	await expect(tab).toHaveAttribute("data-tone", "failing");
	const labels = await tabs.allTextContents();
	expect(labels.findIndex((label) => label.startsWith("Pull requests"))).toBeGreaterThan(labels.findIndex((label) => label.startsWith("Agents")));
	await tab.click();
	await expect(tab).toHaveAttribute("aria-selected", "true");

	const panel = page.getByTestId("pr-merge-panel");
	await expect(panel.getByTestId("pr-merge-box")).toHaveCount(2);
	const staleBox = panel.getByRole("article", { name: /#41/ });
	const cleanBox = panel.getByRole("article", { name: /#42/ });

	await expect(staleBox.getByTestId("pr-checks-summary")).toContainText("Some checks were not successful");
	await expect(staleBox.getByTestId("pr-checks-counts")).toHaveText("1 failing, 8 skipped, 6 successful");
	const failing = staleBox.getByRole("region", { name: "1 failing check" });
	await expect(failing.getByTestId("pr-check")).toContainText("CI / test");
	await expect(failing.getByTestId("pr-check")).toContainText("Failing after 6s");
	await expect(failing.getByTestId("pr-check")).toContainText("Required");
	await expect(staleBox.getByRole("button", { name: "8 skipped checks" })).toHaveAttribute("aria-expanded", "false");
	await expect(staleBox.getByRole("button", { name: "6 successful checks" })).toHaveAttribute("aria-expanded", "false");
	await expect(staleBox.getByTestId("pr-merge-state")).toContainText("Branch is out of date with main");
	await expect(staleBox.getByRole("button", { name: "Merge PR #41" })).toHaveCount(0);

	await expect(cleanBox.getByTestId("pr-checks-summary")).toContainText("All checks have passed");
	await expect(cleanBox.getByTestId("pr-merge-state")).toContainText("Ready to merge");
	await expect(cleanBox.getByRole("button", { name: "Merge PR #42" })).toBeVisible();
	await capture(page, "pr-merge-box-1-tab");

	await staleBox.getByRole("button", { name: "8 skipped checks" }).click();
	await expect(staleBox.getByRole("region", { name: "8 skipped checks" }).getByTestId("pr-check")).toHaveCount(8);
	await expect(staleBox.getByRole("region", { name: "8 skipped checks" }).getByTestId("pr-check").first()).toContainText("Skipped 2h ago");
	await capture(page, "pr-merge-box-2-skipped-open");

	// Merge the clean PR from its box. The browser harness runs on preview data, so
	// the shared merge action skips its POST (unit-tested) and goes straight to
	// re-reading the session's PRs, which the fake daemon now reports merged.
	const readsBeforeMerge = state.summaryReads;
	state.merged = true;
	await cleanBox.getByRole("button", { name: "Merge PR #42" }).click();
	await expect.poll(() => state.summaryReads).toBeGreaterThan(readsBeforeMerge);
	await expect(cleanBox).toHaveAttribute("data-state", "merged");
	await expect(cleanBox.getByTestId("pr-merge-state")).toContainText("Merged");
	await capture(page, "pr-merge-box-3-merged");
});

test("Fix this on a failing check drafts the fix in the composer and returns to the chat @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	await page.emulateMedia({ colorScheme: "dark" });
	await installPullRequestSession(page);
	await page.getByRole("tab", { name: /Pull requests/ }).click();
	const staleBox = page.getByTestId("pr-merge-panel").getByRole("article", { name: /#41/ });
	await expect(staleBox.getByTestId("pr-checks-counts")).toHaveText("1 failing, 8 skipped, 6 successful");
	await capture(page, "pr-merge-box-1-tab-dark");

	await staleBox.getByRole("button", { name: "Fix this: CI / test" }).click();
	await expect(page.getByTestId("pr-merge-panel")).toHaveCount(0);
	await expect(page.getByRole("tab", { name: /Pull requests/ })).toHaveAttribute("aria-selected", "false");
	await expect(page.getByRole("combobox", { name: "Message the agent" })).toHaveText(
		`Fix the failing check "CI / test" on PR #41: ${failingRun}`,
	);
	await capture(page, "pr-merge-box-4-fix-drafted");
});
