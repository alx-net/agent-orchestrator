import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { buildAgentsOverview, type SessionAgentsDetail } from "../lib/agents-overview";
import type { MainAgentSummary, SubagentCard } from "../lib/subagent-board";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";
import { AgentsOverviewView } from "./AgentsOverview";

function session(id: string, overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "app",
		workspaceName: "agent-orchestrator",
		title: id,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		activity: { state: "idle", lastActivityAt: new Date(Date.now() - 5 * 60_000).toISOString() },
		createdAt: "2026-10-26T09:00:00.000Z",
		updatedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
		prs: [],
		...overrides,
	};
}

function workspace(sessions: WorkspaceSession[]): WorkspaceSummary {
	return { id: "app", name: "agent-orchestrator", path: "/repos/app", sessions };
}

const at = (second: number) => `2026-10-26T10:00:${String(second).padStart(2, "0")}.000Z`;

const orchestratorMain: MainAgentSummary = {
	state: "working",
	turnId: "turn-1",
	startedAt: new Date(Date.now() - 125_000).toISOString(),
	nowLine: "Dispatching the pause work",
	nowSource: "status",
	notes: [
		{ kind: "turn", text: "Add pausing to sessions", at: at(0), fromYou: true },
		{ kind: "status", text: "Reading the issue", at: at(1) },
		{ kind: "decision", text: "Split the work into an API and a UI worker", at: at(2) },
		{ kind: "turn", text: "Go ahead", at: at(3), fromYou: true },
		{ kind: "status", text: "Spawned both workers", at: at(3) },
		{ kind: "status", text: "Dispatching the pause work", at: at(4) },
	],
	decisions: [{ kind: "decision", text: "Split the work into an API and a UI worker", at: at(2) }],
	sent: [],
};

const blockedMain: MainAgentSummary = {
	state: "needs_you",
	turnId: "turn-1",
	nowLine: "on you turning on 2FA in Verda",
	nowSource: "outcome",
	result: { kind: "blocked", text: "on you turning on 2FA in Verda", at: at(9), source: "blocked_line" },
	notes: [{ kind: "outcome", outcome: "blocked", text: "on you turning on 2FA in Verda", at: at(9) }],
	decisions: [],
	sent: [],
};

const audit: SubagentCard = {
	id: "audit",
	providerItemId: "toolu-audit",
	turnId: "turn-1",
	title: "Audit the auth flow",
	subagentType: "Explore",
	lane: "working",
	outcome: "working",
	startedAt: new Date(Date.now() - 30_000).toISOString(),
	updatedAt: new Date().toISOString(),
	nowLine: "Reading the token validator",
	nowSource: "status",
	notes: [
		{ kind: "decision", text: "Start from the middleware", at: at(5) },
		{ kind: "status", text: "Reading the token validator", at: at(6) },
	],
	decisions: [{ kind: "decision", text: "Start from the middleware", at: at(5) }],
	toolCount: 2,
	inbox: [],
};

function overview(details: Map<string, SessionAgentsDetail> = new Map(), sessions?: WorkspaceSession[]) {
	return buildAgentsOverview([workspace(sessions ?? [
		session("orch", { kind: "orchestrator", title: "Add pausing" }),
		session("api", { parentSessionId: "orch", title: "Pause API", createdAt: "2026-10-26T09:10:00.000Z" }),
		session("ui", { parentSessionId: "orch", title: "Pause UI", mode: "tui", createdAt: "2026-10-26T09:20:00.000Z" }),
		session("solo", { title: "Rotate secrets", createdAt: "2026-10-26T08:00:00.000Z" }),
		session("old", { title: "Old cleanup", isTerminated: true, status: "terminated" }),
	])], details);
}

const loaded = () => new Map<string, SessionAgentsDetail>([
	["orch", { status: "ready", model: { main: orchestratorMain, cards: [] } }],
	["api", { status: "ready", model: { main: { ...orchestratorMain, nowLine: "Writing the handler", notes: [], decisions: [] }, cards: [audit] } }],
	["solo", { status: "ready", model: { main: blockedMain, cards: [] } }],
]);

const rowOf = (title: string) => {
	const row = screen.getByRole("button", { name: `Open ${title}` }).closest<HTMLElement>("[data-testid='agents-overview-session']");
	if (!row) throw new Error(`no row for ${title}`);
	return row;
};

describe("AgentsOverviewView", () => {
	it("renders one tree per project: orchestrators with their workers nested, then each session's subagents", () => {
		render(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(loaded())} />);
		const project = screen.getByRole("region", { name: "agent-orchestrator" });
		const branches = within(project).getAllByTestId("agents-overview-branch");
		// The blocked session needs you, so it sorts first; the orchestrator holds both workers.
		const top = branches.filter((branch) => branch.parentElement?.parentElement === project);
		expect(top.map((branch) => branch.dataset.sessionId)).toEqual(["solo", "orch"]);
		const orchestrator = top[1]!;
		expect(within(orchestrator).getAllByTestId("agents-overview-branch").map((branch) => branch.dataset.sessionId)).toEqual(["ui", "api"]);

		const orch = rowOf("Add pausing");
		expect(orch).toHaveAttribute("data-state", "working");
		expect(within(orch).getByText("Orchestrator")).toBeInTheDocument();
		expect(within(orch).getByTestId("agents-overview-now")).toHaveTextContent("Dispatching the pause work");
		expect(within(orch).getByText("2m")).toBeInTheDocument();
		// The latest updates, without repeating the now line or a turn's divider, decisions emphasized.
		const updates = within(orch).getByTestId("agents-overview-updates");
		expect(within(updates).getAllByRole("listitem").map((item) => item.textContent)).toEqual([
			expect.stringContaining("Reading the issue"),
			expect.stringContaining("Split the work into an API and a UI worker"),
			expect.stringContaining("Spawned both workers"),
		]);
		expect(within(updates).getByText("Split the work into an API and a UI worker")).toHaveClass("font-medium");

		expect(within(rowOf("Pause API")).getByText("Worker")).toBeInTheDocument();
		const subagent = screen.getByTestId("agents-overview-subagent");
		expect(subagent).toHaveAttribute("data-outcome", "working");
		expect(subagent).toHaveTextContent("Audit the auth flow");
		expect(subagent).toHaveTextContent("Explore");
		expect(within(subagent).getByTestId("agents-overview-now")).toHaveTextContent("Reading the token validator");
		// Subagents sit one level inside their session's branch.
		const apiBranch = within(orchestrator).getAllByTestId("agents-overview-branch").find((branch) => branch.dataset.sessionId === "api")!;
		expect(apiBranch).toContainElement(subagent);
	});

	it("names how a session ended, and a blocked one needs you", () => {
		render(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(loaded())} />);
		const solo = rowOf("Rotate secrets");
		expect(solo).toHaveAttribute("data-state", "needs_you");
		expect(within(solo).getByText("Needs you")).toHaveClass("sr-only");
		const now = within(solo).getByTestId("agents-overview-now");
		expect(within(now).getByTestId("agent-outcome")).toHaveTextContent("Blocked");
		expect(now).toHaveTextContent("Blocked on you turning on 2FA in Verda");
		expect(screen.getByTestId("agents-overview-summary")).toHaveTextContent("1 needs you");
	});

	it("opens a session from its row, and expands its full timeline from the disclosure", async () => {
		const user = userEvent.setup();
		const onOpenSession = vi.fn();
		render(<AgentsOverviewView onOpenSession={onOpenSession} overview={overview(loaded())} />);

		await user.click(screen.getByRole("button", { name: "Open Add pausing" }));
		expect(onOpenSession).toHaveBeenCalledWith(expect.objectContaining({ id: "orch" }));
		await user.click(within(rowOf("Pause API")).getByTestId("agents-overview-now"));
		expect(onOpenSession).toHaveBeenLastCalledWith(expect.objectContaining({ id: "api" }));

		const disclosure = screen.getByRole("button", { name: "Updates for Add pausing" });
		expect(disclosure).toHaveAttribute("aria-expanded", "false");
		await user.click(disclosure);
		expect(onOpenSession).toHaveBeenCalledTimes(2);
		expect(disclosure).toHaveAttribute("aria-expanded", "true");
		const timeline = document.getElementById(disclosure.getAttribute("aria-controls")!)!;
		expect(within(timeline).getAllByRole("listitem").map((item) => item.dataset.noteKind)).toEqual(["turn", "status", "decision", "turn", "status", "status"]);

		// A subagent row expands rather than navigating.
		const subagent = screen.getByTestId("agents-overview-subagent");
		await user.click(within(subagent).getByText("Audit the auth flow"));
		expect(onOpenSession).toHaveBeenCalledTimes(2);
		expect(within(subagent).getByRole("button", { name: "Updates for Audit the auth flow" })).toHaveAttribute("aria-expanded", "true");
		expect(screen.getAllByRole("region", { name: "Updates" }).length).toBe(2);
	});

	it("says a terminal session needs chat mode for subagent detail, and a loading one is loading", () => {
		const details = loaded();
		details.set("orch", { status: "loading" });
		render(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(details)} />);
		const ui = rowOf("Pause UI");
		expect(ui).toHaveAttribute("data-detail", "tui");
		expect(ui).toHaveTextContent("Subagent detail needs chat mode");
		expect(rowOf("Add pausing")).toHaveTextContent("Loading updates…");
	});

	it("folds finished sessions into a collapsed group per project", async () => {
		const user = userEvent.setup();
		render(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(loaded())} />);
		const finished = screen.getByRole("button", { name: /Finished/ });
		expect(finished).toHaveAttribute("aria-expanded", "false");
		expect(screen.queryByRole("button", { name: "Open Old cleanup" })).not.toBeInTheDocument();
		await user.click(finished);
		expect(rowOf("Old cleanup")).toHaveAttribute("data-state", "stopped");
	});

	it("explains an empty overview, and shows loading and failure in place", () => {
		const { rerender } = render(<AgentsOverviewView onOpenSession={vi.fn()} overview={buildAgentsOverview([], new Map())} />);
		expect(screen.getByTestId("agents-overview-empty")).toHaveTextContent("No agents running");
		expect(screen.queryByTestId("agents-overview-summary")).not.toBeInTheDocument();

		// Only finished sessions: still empty, with the history one click away.
		rerender(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(new Map(), [session("old", { isTerminated: true, status: "terminated" })])} />);
		expect(screen.getByTestId("agents-overview-empty")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: /Finished/ })).toBeInTheDocument();

		rerender(<AgentsOverviewView isLoading onOpenSession={vi.fn()} overview={buildAgentsOverview([], new Map())} />);
		expect(screen.getByRole("status")).toHaveTextContent("Loading agents…");

		rerender(<AgentsOverviewView error="daemon unreachable" onOpenSession={vi.fn()} overview={buildAgentsOverview([], new Map())} />);
		expect(screen.getByRole("alert")).toHaveTextContent("daemon unreachable");
	});
});
