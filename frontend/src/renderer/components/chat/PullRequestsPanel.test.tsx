/**
 * The chat's Pull requests tab body: one merge box per session PR with its
 * checks grouped like GitHub's merge box, whether the branch is stale, and the
 * shared Merge action.
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../../lib/api-client")>(),
	apiClient: { GET: getMock, POST: postMock },
}));
vi.mock("../../lib/host-clients", async (importOriginal) => ({
	...await importOriginal<typeof import("../../lib/host-clients")>(),
	clientForSessionHost: () => ({ GET: getMock, POST: postMock }),
	clientForHost: () => ({ GET: getMock, POST: postMock }),
}));
vi.mock("../../hooks/useCloudCp", () => ({
	useCloudCp: () => ({ client: { mergePullRequest: vi.fn() }, ready: false, baseUrl: "", userId: "" }),
}));

import { PullRequestsPanel } from "./PullRequestsPanel";
import { TooltipProvider } from "../ui/tooltip";
import { sessionScmSummaryQueryKey } from "../../hooks/useSessionScmSummary";
import type { MergeBoxPR, PRCheck } from "../../lib/pr-merge-box";
import type { PullRequestFacts, WorkspaceSession } from "../../types/workspace";

const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

function summary(number: number, overrides: Partial<MergeBoxPR> = {}): MergeBoxPR {
	const htmlUrl = `https://github.com/acme/app/pull/${number}`;
	return {
		url: htmlUrl,
		htmlUrl,
		number,
		title: `PR ${number}`,
		state: "open",
		provider: "github",
		repo: "acme/app",
		author: "ada",
		sourceBranch: `feat/${number}`,
		targetBranch: "main",
		headSha: `sha-${number}`,
		additions: 12,
		deletions: 3,
		changedFiles: 2,
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: htmlUrl, conflictFiles: [] },
		updatedAt: minutesAgo(2),
		observedAt: minutesAgo(2),
		...overrides,
	};
}

function fact(pr: MergeBoxPR): PullRequestFacts {
	return {
		url: pr.url,
		number: pr.number,
		state: pr.state,
		ci: pr.ci.state,
		review: pr.review.decision,
		mergeability: pr.mergeability.state,
		reviewComments: false,
		updatedAt: pr.updatedAt,
	};
}

const failingRun = "https://github.com/acme/app/actions/runs/1/job/2";

/** One failing required check, eight skipped, six passed, and the branch behind main. */
const stale = summary(41, {
	title: "Add pausing to sessions",
	ci: { autoInjectCI: true, state: "failing", failingChecks: [{ name: "test", status: "failed", conclusion: "failure", url: failingRun }] },
	mergeability: { state: "blocked", reasons: ["ci_failing", "behind_base"], prUrl: "https://github.com/acme/app/pull/41", conflictFiles: [] },
	mergeStateStatus: "BEHIND",
	checks: [
		{ name: "test", workflow: "CI", status: "failed", required: true, startedAt: minutesAgo(10), completedAt: new Date(Date.parse(minutesAgo(10)) + 6_000).toISOString(), url: failingRun },
		...Array.from({ length: 8 }, (_, index): PRCheck => ({ name: `deploy-${index}`, workflow: "Release", status: "skipped", required: false, completedAt: minutesAgo(120) })),
		...Array.from({ length: 6 }, (_, index): PRCheck => ({ name: `lint-${index}`, workflow: "CI", status: "passed", required: index === 0, startedAt: minutesAgo(10), completedAt: minutesAgo(9) })),
	],
});
const clean = summary(42, {
	title: "Document the pause route",
	checks: [{ name: "lint", workflow: "CI", status: "passed", required: true, startedAt: minutesAgo(5), completedAt: minutesAgo(4) }],
	mergeStateStatus: "CLEAN",
});
const merged = summary(40, { title: "Add the paused_at column", state: "merged", stateChangedAt: minutesAgo(60) });

function sessionWith(prs: MergeBoxPR[]): WorkspaceSession {
	return {
		id: "sess-prs",
		workspaceId: "project-1",
		workspaceName: "app",
		title: "Pausing",
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "ci_failed",
		updatedAt: minutesAgo(1),
		prs: prs.map(fact),
	};
}

function renderPanel(ui: ReactNode, seed?: MergeBoxPR[]) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	if (seed) client.setQueryData(sessionScmSummaryQueryKey("sess-prs"), { prs: seed, linkedPrs: [] });
	return render(
		<QueryClientProvider client={client}>
			<TooltipProvider>{ui}</TooltipProvider>
		</QueryClientProvider>,
	);
}

const box = (name: RegExp) => screen.getByRole("article", { name });

beforeEach(() => {
	getMock.mockReset();
	postMock.mockReset();
});

describe("PullRequestsPanel", () => {
	it("stacks one merge box per PR, open ones first and merged ones folded at the bottom", () => {
		const prs = [merged, clean, stale];
		getMock.mockResolvedValue({ data: { prs, linkedPrs: [] } });
		renderPanel(<PullRequestsPanel session={sessionWith(prs)} />, prs);

		const boxes = screen.getAllByTestId("pr-merge-box");
		expect(boxes.map((entry) => entry.getAttribute("data-pr-number"))).toEqual(["41", "42", "40"]);
		expect(within(box(/#41/)).getByTestId("pr-branches")).toHaveTextContent("feat/41 → main");
		expect(within(box(/#41/)).getByTestId("pr-observed-at")).toHaveTextContent("Updated 2m ago");
		// The merged PR is history: its header only, until opened.
		const history = box(/#40/);
		expect(within(history).getByRole("button", { name: "Details for PR #40" })).toHaveAttribute("aria-expanded", "false");
		expect(within(history).queryByTestId("pr-checks-summary")).not.toBeInTheDocument();
	});

	it("says where the checks are and groups them, failing open and skipped or passed folded", async () => {
		const user = userEvent.setup();
		getMock.mockResolvedValue({ data: { prs: [stale], linkedPrs: [] } });
		renderPanel(<PullRequestsPanel session={sessionWith([stale])} />, [stale]);
		const staleBox = box(/#41/);

		const headline = within(staleBox).getByTestId("pr-checks-summary");
		expect(headline).toHaveTextContent("Some checks were not successful");
		expect(within(headline).getByTestId("pr-checks-counts")).toHaveTextContent("1 failing, 8 skipped, 6 successful");

		const groups = within(staleBox).getAllByTestId("pr-check-group");
		expect(groups.map((group) => group.getAttribute("data-group"))).toEqual(["failing", "skipped", "passed"]);
		const failing = within(staleBox).getByRole("region", { name: "1 failing check" });
		expect(within(failing).getByRole("button", { name: "1 failing check" })).toHaveAttribute("aria-expanded", "true");
		const row = within(failing).getByTestId("pr-check");
		expect(row).toHaveTextContent("CI / test");
		expect(row).toHaveTextContent("Failing after 6s");
		expect(row).toHaveTextContent("Required");
		expect(within(row).getByRole("link", { name: "Open CI / test" })).toHaveAttribute("href", failingRun);

		const skipped = within(staleBox).getByRole("region", { name: "8 skipped checks" });
		const skippedToggle = within(skipped).getByRole("button", { name: "8 skipped checks" });
		expect(skippedToggle).toHaveAttribute("aria-expanded", "false");
		expect(within(skipped).queryAllByTestId("pr-check")).toHaveLength(0);
		await user.click(skippedToggle);
		expect(within(skipped).getAllByTestId("pr-check")).toHaveLength(8);
		expect(within(skipped).getAllByTestId("pr-check")[0]).toHaveTextContent("Skipped 2h ago");

		const passed = within(staleBox).getByRole("region", { name: "6 successful checks" });
		await user.click(within(passed).getByRole("button", { name: "6 successful checks" }));
		expect(within(passed).getAllByTestId("pr-check")[0]).toHaveTextContent("Passed in 1m");
	});

	it("calls a branch behind its base out of date, and only the clean PR offers Merge", async () => {
		const user = userEvent.setup();
		const prs = [stale, clean];
		getMock.mockResolvedValue({ data: { prs, linkedPrs: [] } });
		postMock.mockResolvedValue({ data: { state: "merged", stateChangedAt: new Date().toISOString() }, error: undefined });
		renderPanel(<PullRequestsPanel session={sessionWith(prs)} />, prs);

		const staleState = within(box(/#41/)).getByTestId("pr-merge-state");
		expect(staleState).toHaveAttribute("data-verdict", "behind");
		expect(staleState).toHaveTextContent("Branch is out of date with main");
		expect(within(box(/#41/)).queryByRole("button", { name: "Merge PR #41" })).not.toBeInTheDocument();

		const cleanBox = box(/#42/);
		expect(within(cleanBox).getByTestId("pr-checks-summary")).toHaveTextContent("All checks have passed");
		expect(within(cleanBox).getByTestId("pr-merge-state")).toHaveTextContent("Ready to merge");
		await user.click(within(cleanBox).getByRole("button", { name: "Merge PR #42" }));
		await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/prs/{id}/merge", {
			params: { path: { id: "42" } },
			body: { prUrl: clean.url, expectedHeadSha: "sha-42" },
		}));
	});

	it("drafts a fix for a failing check and for a stale branch", async () => {
		const user = userEvent.setup();
		const onFix = vi.fn();
		getMock.mockResolvedValue({ data: { prs: [stale], linkedPrs: [] } });
		renderPanel(<PullRequestsPanel onFix={onFix} session={sessionWith([stale])} />, [stale]);

		await user.click(within(box(/#41/)).getByRole("button", { name: "Fix this: CI / test" }));
		expect(onFix).toHaveBeenLastCalledWith(`Fix the failing check "CI / test" on PR #41: ${failingRun}`);
		await user.click(within(within(box(/#41/)).getByTestId("pr-merge-state")).getByRole("button", { name: "Fix this" }));
		expect(onFix).toHaveBeenLastCalledWith("Update PR #41 with the latest main: https://github.com/acme/app/pull/41");
	});

	it("offers no fix while the agent cannot take new work", () => {
		getMock.mockResolvedValue({ data: { prs: [stale], linkedPrs: [] } });
		renderPanel(<PullRequestsPanel session={sessionWith([stale])} />, [stale]);
		expect(screen.queryByRole("button", { name: /^Fix this/ })).not.toBeInTheDocument();
	});

	it("falls back to the failing checks when the daemon sends no check list", () => {
		const older: MergeBoxPR = { ...stale, checks: undefined, mergeStateStatus: undefined };
		getMock.mockResolvedValue({ data: { prs: [older], linkedPrs: [] } });
		renderPanel(<PullRequestsPanel session={sessionWith([older])} />, [older]);
		const olderBox = box(/#41/);
		expect(within(olderBox).getByTestId("pr-checks-summary")).toHaveTextContent("Some checks were not successful");
		expect(within(olderBox).getByTestId("pr-checks-summary")).toHaveTextContent("Only failing checks are listed");
		expect(within(olderBox).getAllByTestId("pr-check")).toHaveLength(1);
		// behind_base alone still says the branch is stale.
		expect(within(olderBox).getByTestId("pr-merge-state")).toHaveTextContent("Branch is out of date with main");
	});

	it("shows the session's PR facts as loading until the summary arrives", () => {
		getMock.mockReturnValue(new Promise(() => {}));
		renderPanel(<PullRequestsPanel session={sessionWith([stale])} />);
		const loadingBox = screen.getByTestId("pr-merge-box");
		expect(within(loadingBox).getByRole("status")).toHaveTextContent("Loading check details…");
		expect(within(loadingBox).getByTestId("pr-checks-summary")).toHaveTextContent("Some checks were not successful");
	});

	it("says when the summary could not be refreshed and offers a retry", async () => {
		getMock.mockResolvedValue({ data: undefined, error: { message: "daemon unavailable" } });
		renderPanel(<PullRequestsPanel session={sessionWith([stale])} />);
		const alert = await screen.findByRole("alert", {}, { timeout: 4_000 });
		expect(alert).toHaveTextContent("Couldn’t refresh pull request status");
		expect(within(alert).getByRole("button", { name: "Retry" })).toBeInTheDocument();
		expect(screen.getByTestId("pr-merge-box")).toBeInTheDocument();
	});
});
