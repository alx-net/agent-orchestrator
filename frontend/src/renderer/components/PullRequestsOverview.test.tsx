/**
 * The PRs page: every live session's open and draft PRs grouped by project and
 * session, one fixed-height row each, opening to the shared merge box.
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { getMock, postMock, workspaceQueryMock, navigateMock } = vi.hoisted(() => ({
	getMock: vi.fn(),
	postMock: vi.fn(),
	workspaceQueryMock: vi.fn(),
	navigateMock: vi.fn(),
}));

vi.mock("../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/api-client")>(),
	apiClient: { GET: getMock, POST: postMock },
}));
vi.mock("../lib/host-clients", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/host-clients")>(),
	clientForSessionHost: () => ({ GET: getMock, POST: postMock }),
	clientForHost: () => ({ GET: getMock, POST: postMock }),
}));
vi.mock("../hooks/useCloudCp", () => ({
	useCloudCp: () => ({ client: { mergePullRequest: vi.fn() }, ready: false, baseUrl: "", userId: "" }),
}));
vi.mock("../hooks/useWorkspaceQuery", async (importOriginal) => ({
	...await importOriginal<typeof import("../hooks/useWorkspaceQuery")>(),
	useWorkspaceQuery: () => workspaceQueryMock(),
}));
vi.mock("../lib/navigate-to-session", () => ({ useNavigateToSession: () => navigateMock }));

import { PullRequestsOverview, PullRequestsOverviewView } from "./PullRequestsOverview";
import { TooltipProvider } from "./ui/tooltip";
import { buildPRsOverview, type SessionPRsLoad } from "../lib/prs-overview";
import type { MergeBoxPR, PRCheck } from "../lib/pr-merge-box";
import type { PullRequestFacts, WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
const failingRun = "https://github.com/acme/app/actions/runs/1/job/2";

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
		failingChecks: pr.ci.failingChecks,
		reviewComments: false,
		updatedAt: pr.updatedAt,
	};
}

/** A failing required check and checks still running, on a branch behind main. */
const failing = summary(41, {
	title: "Add pausing to sessions",
	ci: { autoInjectCI: true, state: "failing", failingChecks: [{ name: "test", status: "failed", conclusion: "failure", url: failingRun }] },
	mergeability: { state: "blocked", reasons: ["ci_failing", "behind_base"], prUrl: "https://github.com/acme/app/pull/41", conflictFiles: [] },
	mergeStateStatus: "BEHIND",
	review: { decision: "changes_requested", hasUnresolvedHumanComments: false, unresolvedBy: [] },
	checks: [
		{ name: "test", workflow: "CI", status: "failed", required: true, startedAt: minutesAgo(10), completedAt: new Date(Date.parse(minutesAgo(10)) + 6_000).toISOString(), url: failingRun },
		...Array.from({ length: 3 }, (_, index): PRCheck => ({ name: `e2e-${index}`, workflow: "CI", status: "in_progress", required: false, startedAt: minutesAgo(3) })),
		...Array.from({ length: 6 }, (_, index): PRCheck => ({ name: `lint-${index}`, workflow: "CI", status: "passed", required: false, startedAt: minutesAgo(10), completedAt: minutesAgo(9) })),
	],
});
/** Green, approved and up to date. */
const ready = summary(42, {
	title: "Document the pause route",
	review: { decision: "approved", hasUnresolvedHumanComments: false, unresolvedBy: [] },
	checks: [{ name: "lint", workflow: "CI", status: "passed", required: true, startedAt: minutesAgo(5), completedAt: minutesAgo(4) }],
	mergeStateStatus: "CLEAN",
});
/** Only checks running. */
const running = summary(43, {
	title: "Tune the reaper",
	ci: { autoInjectCI: true, state: "pending", failingChecks: [] },
	mergeability: { state: "unknown", reasons: [], prUrl: "https://github.com/acme/app/pull/43", conflictFiles: [] },
	checks: [
		{ name: "unit", workflow: "CI", status: "in_progress", required: true, startedAt: minutesAgo(1) },
		{ name: "lint", workflow: "CI", status: "passed", required: true, startedAt: minutesAgo(2), completedAt: minutesAgo(1) },
	],
});

function session(id: string, prs: MergeBoxPR[], overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "app",
		workspaceName: "app",
		title: id,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		branch: `session/${id}`,
		createdAt: minutesAgo(60),
		updatedAt: minutesAgo(1),
		prs: prs.map(fact),
		...overrides,
	};
}

function workspace(id: string, sessions: WorkspaceSession[]): WorkspaceSummary {
	return { id, name: id, path: `/repos/${id}`, sessions };
}

const pausing = session("pausing", [failing], { title: "Ship session pausing", createdAt: minutesAgo(120), activity: { state: "active", lastActivityAt: minutesAgo(0) } });
const docs = session("docs", [ready], { title: "Document pausing", createdAt: minutesAgo(30) });
const reaper = session("reaper", [running], { title: "Reaper tuning", workspaceId: "ops", workspaceName: "ops" });

function loaded(entries: [WorkspaceSession, MergeBoxPR[]][]): Map<string, SessionPRsLoad> {
	return new Map(entries.map(([entry, prs]) => [entry.id, { summaries: prs, pending: false, failed: false }]));
}

function wrap(ui: ReactNode) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	return render(
		<QueryClientProvider client={client}>
			<TooltipProvider>{ui}</TooltipProvider>
		</QueryClientProvider>,
	);
}

function renderView(workspaces: WorkspaceSummary[], loads = new Map<string, SessionPRsLoad>(), onOpenSession = vi.fn()) {
	wrap(<PullRequestsOverviewView onOpenSession={onOpenSession} overview={buildPRsOverview(workspaces, loads)} />);
	return { onOpenSession };
}

const sessionGroup = (id: string) => screen.getAllByTestId("prs-overview-session").find((entry) => entry.getAttribute("data-session-id") === id)!;
const prRow = (number: number) => screen.getAllByTestId("prs-overview-pr").find((entry) => entry.getAttribute("data-pr-number") === String(number))!;

beforeEach(() => {
	getMock.mockReset();
	postMock.mockReset();
	navigateMock.mockReset();
	workspaceQueryMock.mockReset();
});

describe("PullRequestsOverviewView", () => {
	it("groups PRs by project and session, failing first, each row saying its checks, merge state and review", () => {
		renderView(
			[workspace("app", [docs, pausing]), workspace("ops", [reaper])],
			loaded([[pausing, [failing]], [docs, [ready]], [reaper, [running]]]),
		);

		const projects = screen.getAllByTestId("prs-overview-project");
		expect(projects.map((project) => project.getAttribute("data-project-id"))).toEqual(["app", "ops"]);
		expect(within(projects[0]!).getAllByTestId("prs-overview-session").map((entry) => entry.getAttribute("data-session-id"))).toEqual(["pausing", "docs"]);
		expect(screen.getByTestId("prs-overview-summary")).toHaveTextContent("1 failing2 running3 open");

		// The session row: its working spinner, title and branch, and it opens the session.
		const pausingRow = within(sessionGroup("pausing")).getByTestId("prs-overview-session-row");
		expect(pausingRow.querySelector("[data-working='true']")).not.toBeNull();
		expect(pausingRow).toHaveTextContent("session/pausing");
		expect(within(pausingRow).getByRole("button", { name: "Open Ship session pausing" })).toBeInTheDocument();

		const failingRow = prRow(41);
		expect(within(failingRow).getByRole("link", { name: /^#41\s*Add pausing to sessions$/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/41");
		expect(within(failingRow).getByTestId("prs-overview-review")).toHaveTextContent("Changes requested");
		expect(within(failingRow).getByTestId("prs-overview-checks")).toHaveTextContent("Some checks were not successful · 1 failing, 3 in progress, 6 successful");
		expect(within(failingRow).getByTestId("prs-overview-merge-state")).toHaveTextContent("Out of date with main");
		expect(within(failingRow).getByTestId("prs-overview-observed-at")).toHaveTextContent("Updated 2m ago");
		expect(within(failingRow).getByTestId("pr-glyph")).toHaveAttribute("data-tone", "failing");
		// The failure leads; the spinner sits by what is still running.
		expect(within(failingRow).getByTestId("prs-overview-checks")).toHaveAttribute("data-running", "true");
		expect(within(failingRow).getByTestId("prs-overview-checks-spinner")).toBeInTheDocument();
		expect(within(failingRow).queryByRole("button", { name: "Merge PR #41" })).not.toBeInTheDocument();

		const runningRow = prRow(43);
		expect(within(runningRow).getByTestId("prs-overview-checks")).toHaveTextContent("Checks are running · 1 in progress, 1 successful");
		expect(within(runningRow).getByTestId("prs-overview-checks-spinner")).toBeInTheDocument();

		const readyRow = prRow(42);
		expect(within(readyRow).getByTestId("prs-overview-checks")).toHaveTextContent("All checks have passed · 1 successful");
		expect(within(readyRow).getByTestId("prs-overview-merge-state")).toHaveTextContent("Ready to merge");
		expect(within(readyRow).getByRole("button", { name: "Merge PR #42" })).toBeInTheDocument();
		expect(within(readyRow).queryByTestId("prs-overview-checks-spinner")).not.toBeInTheDocument();

		// Every row holds one fixed height, whatever its checks say.
		for (const number of [41, 42, 43]) expect(within(prRow(number)).getByTestId("prs-overview-pr-row")).toHaveClass("h-14");
	});

	it("opens a row to the shared merge box in a fixed-height area, and the session row opens the session", async () => {
		const user = userEvent.setup();
		const { onOpenSession } = renderView([workspace("app", [pausing])], loaded([[pausing, [failing]]]));

		const toggle = within(prRow(41)).getByRole("button", { name: "Details for PR #41" });
		expect(toggle).toHaveAttribute("aria-expanded", "false");
		expect(screen.queryByTestId("pr-merge-box-body")).not.toBeInTheDocument();
		await user.click(toggle);
		expect(toggle).toHaveAttribute("aria-expanded", "true");

		const box = screen.getByRole("region", { name: "Checks and merge state of PR #41" });
		expect(box).toHaveClass("h-72", "overflow-y-auto");
		expect(within(box).getByTestId("pr-checks-counts")).toHaveTextContent("1 failing, 3 in progress, 6 successful");
		expect(within(box).getAllByTestId("pr-check-group").map((group) => group.getAttribute("data-group"))).toEqual(["failing", "running", "passed"]);
		const failingCheck = within(within(box).getByRole("region", { name: "1 failing check" })).getByTestId("pr-check");
		expect(failingCheck).toHaveTextContent("CI / test");
		expect(failingCheck).toHaveTextContent("Failing after 6s");
		expect(failingCheck).toHaveTextContent("Required");
		expect(within(box).getByTestId("pr-merge-state")).toHaveTextContent("Branch is out of date with main");
		// No composer here to take a "Fix this" draft.
		expect(within(box).queryByRole("button", { name: /^Fix this/ })).not.toBeInTheDocument();

		// A click on the row (not its link) closes it again.
		await user.click(within(prRow(41)).getByTestId("prs-overview-merge-state"));
		expect(screen.queryByTestId("pr-merge-box-body")).not.toBeInTheDocument();

		await user.click(within(sessionGroup("pausing")).getByRole("button", { name: "Open Ship session pausing" }));
		expect(onOpenSession).toHaveBeenCalledWith(pausing);
	});

	it("merges a ready PR from its row", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue({ data: { state: "merged", stateChangedAt: new Date().toISOString() }, error: undefined });
		renderView([workspace("app", [docs])], loaded([[docs, [ready]]]));

		await user.click(within(prRow(42)).getByRole("button", { name: "Merge PR #42" }));
		await waitFor(() => expect(postMock).toHaveBeenCalledWith("/api/v1/prs/{id}/merge", {
			params: { path: { id: "42" } },
			body: { prUrl: ready.url, expectedHeadSha: "sha-42" },
		}));
	});

	it("says there are no open pull requests, and keeps the last day's merged ones behind a collapsed group", async () => {
		const user = userEvent.setup();
		const merged = summary(40, { title: "Add the paused_at column", state: "merged", stateChangedAt: minutesAgo(90) });
		renderView([workspace("app", [session("old", [merged])])], loaded([[session("old", [merged]), [merged]]]));

		expect(screen.getByTestId("prs-overview-empty")).toHaveTextContent("No open pull requests");
		expect(screen.queryByTestId("prs-overview-pr")).not.toBeInTheDocument();
		expect(screen.queryByTestId("prs-overview-summary")).not.toBeInTheDocument();
		const group = screen.getByRole("button", { name: /Recently merged/ });
		expect(group).toHaveAttribute("aria-expanded", "false");
		expect(screen.queryByTestId("prs-overview-merged")).not.toBeInTheDocument();
		await user.click(group);
		const row = screen.getByTestId("prs-overview-merged");
		expect(within(row).getByRole("link", { name: /^#40\s*Add the paused_at column$/ })).toHaveAttribute("href", merged.htmlUrl);
		expect(row).toHaveTextContent("Merged 1h ago");
	});

	it("says when a session's PRs could not be refreshed and offers a retry", async () => {
		const user = userEvent.setup();
		const onRetry = vi.fn();
		const onOpenSession = vi.fn();
		wrap(
			<PullRequestsOverviewView
				onOpenSession={onOpenSession}
				onRetry={onRetry}
				overview={buildPRsOverview([workspace("app", [docs])], new Map([[docs.id, { summaries: [ready], pending: false, failed: true }]]))}
			/>,
		);
		const alert = within(sessionGroup("docs")).getByRole("alert");
		expect(alert).toHaveTextContent("Couldn’t refresh");
		await user.click(within(alert).getByRole("button", { name: "Retry" }));
		expect(onRetry).toHaveBeenCalledWith(docs);
		expect(onOpenSession).not.toHaveBeenCalled();
		// The last known state still shows.
		expect(within(prRow(42)).getByTestId("prs-overview-merge-state")).toHaveTextContent("Ready to merge");
	});

	it("shows loading and error states for the session list", () => {
		const { unmount } = wrap(<PullRequestsOverviewView isLoading onOpenSession={vi.fn()} overview={buildPRsOverview([], new Map())} />);
		expect(screen.getByRole("status")).toHaveTextContent("Loading pull requests…");
		unmount();
		wrap(<PullRequestsOverviewView error="daemon unavailable" onOpenSession={vi.fn()} overview={buildPRsOverview([], new Map())} />);
		expect(screen.getByRole("alert")).toHaveTextContent("daemon unavailable");
	});
});

describe("PullRequestsOverview", () => {
	it("reads summaries only for live sessions with an open or draft PR, and shows their facts meanwhile", async () => {
		const mergedOnly = session("merged-only", [summary(39, { state: "merged", stateChangedAt: minutesAgo(600) })]);
		const ended = session("ended", [summary(38)], { isTerminated: true, status: "terminated" });
		const idle = session("idle", []);
		workspaceQueryMock.mockReturnValue({ data: [workspace("app", [pausing, docs, mergedOnly, ended, idle])], isLoading: false, error: null });
		getMock.mockImplementation((_path: string, { params }: { params: { path: { sessionId: string } } }) => {
			const prs = params.path.sessionId === "pausing" ? [failing] : [ready];
			return Promise.resolve({ data: { prs, linkedPrs: [] } });
		});
		wrap(<PullRequestsOverview />);

		// The facts paint first; the summaries fill in the checks.
		expect(prRow(41)).toBeInTheDocument();
		await waitFor(() => expect(within(prRow(41)).getByTestId("prs-overview-checks")).toHaveTextContent("1 failing, 3 in progress, 6 successful"));
		const read = getMock.mock.calls.map(([, options]) => (options as { params: { path: { sessionId: string } } }).params.path.sessionId).sort();
		expect(read).toEqual(["docs", "pausing"]);
		expect(screen.getAllByTestId("prs-overview-session").map((entry) => entry.getAttribute("data-session-id"))).toEqual(["pausing", "docs"]);
	});

	it("opens a session through the session navigation", async () => {
		const user = userEvent.setup();
		workspaceQueryMock.mockReturnValue({ data: [workspace("app", [docs])], isLoading: false, error: null });
		getMock.mockResolvedValue({ data: { prs: [ready], linkedPrs: [] } });
		wrap(<PullRequestsOverview />);
		await user.click(within(sessionGroup("docs")).getByTestId("prs-overview-session-row"));
		expect(navigateMock).toHaveBeenCalledWith("app", "docs");
	});
});
