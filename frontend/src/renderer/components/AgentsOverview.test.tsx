import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render as renderUi, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { buildAgentsOverview, type SessionAgentsDetail, type SessionControlFacts } from "../lib/agents-overview";
import type { MainAgentSummary, SubagentCard } from "../lib/subagent-board";
import type { ConversationSnapshot } from "../types/conversation";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const { conversationCalls, conversationsMock, postMock, workspaceQueryMock } = vi.hoisted(() => ({
	conversationCalls: new Set<string>(),
	conversationsMock: new Map<string, unknown>(),
	postMock: vi.fn(),
	workspaceQueryMock: vi.fn(),
}));

// Only the route component loads anything; the view tests below pass a built
// overview. The conversation's commands stay real, so a row's actions reach the routes.
vi.mock("../hooks/useConversation", async (importOriginal) => ({
	...await importOriginal<typeof import("../hooks/useConversation")>(),
	useConversation: (sessionId: string) => {
		conversationCalls.add(sessionId);
		const snapshot = conversationsMock.get(sessionId);
		return { snapshot, isLoading: !snapshot, hasOlder: false, isLoadingOlder: false, loadOlder: () => {} };
	},
}));
vi.mock("../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/api-client")>(),
	apiClient: { POST: postMock },
}));
vi.mock("../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => workspaceQueryMock() }));
vi.mock("../lib/navigate-to-session", () => ({ useNavigateToSession: () => vi.fn() }));

import { AgentsOverview, AgentsOverviewView } from "./AgentsOverview";
import { AgentSummariesProvider, createAgentSummaryStore } from "./AgentSummariesProvider";
import { TooltipProvider } from "./ui/tooltip";

function queryClient() {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	vi.spyOn(client, "invalidateQueries").mockResolvedValue();
	return client;
}

/** The app's own providers around a view: a query cache and tooltips. */
function render(ui: ReactElement) {
	const client = queryClient();
	const wrap = (next: ReactElement) => (
		<QueryClientProvider client={client}>
			<TooltipProvider>{next}</TooltipProvider>
		</QueryClientProvider>
	);
	const view = renderUi(wrap(ui));
	return { ...view, rerender: (next: ReactElement) => view.rerender(wrap(next)) };
}

beforeEach(() => {
	postMock.mockReset();
	// No workspaces: a test's own summaries stand, with no loader to replace them.
	workspaceQueryMock.mockReturnValue({ data: [], isLoading: false, error: null });
});

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
		// The latest updates newest first, without repeating the now line or a turn's divider, decisions emphasized.
		const updates = within(orch).getByTestId("agents-overview-updates");
		expect(within(updates).getAllByRole("listitem").map((item) => item.textContent)).toEqual([
			expect.stringContaining("Spawned both workers"),
			expect.stringContaining("Split the work into an API and a UI worker"),
			expect.stringContaining("Reading the issue"),
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
		// Newest first, each turn's lines above the message that began it.
		expect(within(timeline).getAllByRole("listitem").map((item) => item.dataset.noteKind)).toEqual(["status", "status", "turn", "decision", "status", "turn"]);
		// It scrolls inside one height instead of growing with its updates.
		expect(within(timeline).getByRole("list")).toHaveClass("h-80", "overflow-y-auto");

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

describe("AgentsOverview", () => {
	it("reads its sessions' agents from the shared summaries, loading each chat session once and no terminal one", () => {
		workspaceQueryMock.mockReturnValue({ data: [workspace([
			session("api", { title: "Pause API" }),
			session("ui", { title: "Pause UI", mode: "tui" }),
		])], isLoading: false, error: null });
		conversationsMock.set("api", {
			turns: [{ id: "turn-1", state: "running", requestedAt: at(0) }],
			items: [{ kind: "message", id: "said", turnId: "turn-1", sequence: 1, revision: 1, role: "assistant", origin: "provider", text: "Status: Writing the pause handler\n", streaming: false, createdAt: at(1) }],
		} as unknown as ConversationSnapshot);
		render(<AgentSummariesProvider><AgentsOverview /></AgentSummariesProvider>);
		expect(within(rowOf("Pause API")).getByTestId("agents-overview-now")).toHaveTextContent("Writing the pause handler");
		expect(rowOf("Pause UI")).toHaveAttribute("data-detail", "tui");
		expect([...conversationCalls]).toEqual(["api"]);
	});
});

describe("AgentsOverviewView steady rows", () => {
	it("keeps a folded row's height whatever it says: one title line, two now lines, three update lines", () => {
		render(<AgentsOverviewView onOpenSession={vi.fn()} overview={overview(loaded())} />);
		for (const row of [rowOf("Add pausing"), rowOf("Rotate secrets"), rowOf("Pause UI"), screen.getByTestId("agents-overview-subagent")]) {
			// The now line keeps two lines even when it says less, or nothing.
			expect(within(row).getByTestId("agents-overview-now")).toHaveClass("line-clamp-2", "h-[2lh]");
			// Three one-line updates are always reserved, however many there are.
			const block = row.querySelector(".h-\\[calc\\(3lh\\+0\\.25rem\\)\\]");
			expect(block).not.toBeNull();
			for (const line of within(block as HTMLElement).queryAllByRole("listitem")) expect(line).toHaveClass("h-[1lh]", "overflow-hidden");
		}
		expect(within(rowOf("Rotate secrets")).queryByTestId("agents-overview-updates")).not.toBeInTheDocument();
		// A terminal session says why it has none in the same room.
		expect(within(rowOf("Pause UI")).getByText("Subagent detail needs chat mode")).toBeInTheDocument();
	});
});

const STEER = "/api/v1/sessions/{sessionId}/conversation/steer";
const MESSAGES = "/api/v1/sessions/{sessionId}/conversation/messages";
const INTERRUPT = "/api/v1/sessions/{sessionId}/conversation/interrupt";
const SUBAGENT_MESSAGES = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/messages";
const SUBAGENT_STOP = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop";

const RUNNING: SessionControlFacts = { runningTurnId: "turn-1", steer: true, subagents: true };
const ENDED: SessionControlFacts = { steer: true, subagents: true };
const apiMain: MainAgentSummary = { ...orchestratorMain, nowLine: "Writing the handler", notes: [], decisions: [] };

/** The sessions as the shared summaries hold them, with what each conversation allows. */
const controlled = () => new Map<string, SessionAgentsDetail>([
	["orch", { status: "ready", model: { main: orchestratorMain, cards: [{ ...audit, id: "orch-audit", providerItemId: "toolu-orch", title: "Survey the workers" }] }, control: { ...RUNNING, subagents: false } }],
	["api", { status: "ready", model: { main: apiMain, cards: [audit] }, control: RUNNING }],
	["solo", { status: "ready", model: { main: blockedMain, cards: [] }, control: ENDED }],
]);

function renderControlled(details = controlled()) {
	const onOpenSession = vi.fn();
	const store = createAgentSummaryStore();
	for (const [id, detail] of details) store.set(id, detail);
	const ui = () => (
		<AgentSummariesProvider store={store}>
			<AgentsOverviewView onOpenSession={onOpenSession} overview={overview(details)} />
		</AgentSummariesProvider>
	);
	const view = render(ui());
	return {
		onOpenSession,
		/** A new summary arrives, as the conversation's echo would bring it. */
		update(sessionId: string, detail: SessionAgentsDetail) {
			details.set(sessionId, detail);
			act(() => store.set(sessionId, detail));
			view.rerender(ui());
		},
	};
}

const accepted = (data: unknown) => ({ data, error: undefined, response: { status: 202 } });
const refused = (status: number, code: string, message: string) => ({ data: undefined, error: { error: "refused", code, message }, response: { status } });
const bodyOf = (call: number) => (postMock.mock.calls[call]![1] as { body: { text: string; clientMessageId: string } }).body;

describe("AgentsOverviewView stepping in", () => {
	it("steers a running session from its row: Enter sends into the turn, the message shows at once, Escape closes", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue(accepted({ providerTurnId: "provider-1" }));
		const view = renderControlled();
		const row = rowOf("Pause API");
		// One slot, shown on hover or focus, that holds its width whatever it offers.
		const slot = within(row).getByTestId("agents-overview-actions");
		expect(slot).toHaveClass("opacity-0", "group-hover/nav-row:opacity-100", "group-focus-within/nav-row:opacity-100", "w-[3.125rem]");
		expect(within(slot).getByRole("button", { name: "Stop turn" })).toBeInTheDocument();

		await user.click(within(slot).getByRole("button", { name: "Steer main agent" }));
		const field = screen.getByRole("textbox", { name: "Steer main agent" });
		expect(field).toHaveFocus();
		expect(slot).toHaveAttribute("data-open");
		await user.type(field, "Keep the v1 route working{Enter}");
		expect(postMock).toHaveBeenCalledWith(STEER, expect.objectContaining({ params: { path: { sessionId: "api" } } }));
		expect(bodyOf(0)).toMatchObject({ text: "Keep the v1 route working", clientMessageId: expect.any(String) });
		expect(postMock).not.toHaveBeenCalledWith(MESSAGES, expect.anything());
		expect(view.onOpenSession).not.toHaveBeenCalled();
		expect(field).toHaveValue("");

		// Shown at once, newest first, in the row's own log.
		const sent = await within(rowOf("Pause API")).findByTestId("sent-message");
		expect(sent).toHaveTextContent("You:Keep the v1 route working· delivered");
		expect(within(within(rowOf("Pause API")).getByTestId("agents-overview-updates")).getAllByRole("listitem")[0]).toBe(sent);

		// The steer's own row carries the same id, and replaces the local entry.
		const clientMessageId = bodyOf(0).clientMessageId;
		view.update("api", { status: "ready", model: { main: { ...apiMain, sent: [{ id: clientMessageId, text: "Keep the v1 route working", state: "delivered", at: new Date().toISOString() }] }, cards: [audit] }, control: RUNNING });
		expect(within(rowOf("Pause API")).getAllByTestId("sent-message")).toHaveLength(1);

		// Escape closes the field, draft and all, and hands focus back to the row.
		await user.type(screen.getByRole("textbox", { name: "Steer main agent" }), "half a thought");
		await user.keyboard("{Escape}");
		expect(screen.queryByRole("textbox", { name: "Steer main agent" })).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: "Open Pause API" })).toHaveFocus();
		expect(postMock).toHaveBeenCalledTimes(1);
	});

	it("answers a session that waits for you with a new message, held in its log until the conversation echoes it", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue(accepted({ turnId: "turn-2", state: "running" }));
		const view = renderControlled();
		const row = rowOf("Rotate secrets");
		// Its turn ended, so there is nothing to stop.
		expect(within(row).queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();

		await user.click(within(row).getByRole("button", { name: "Message main agent" }));
		await user.type(screen.getByRole("textbox", { name: "Message main agent" }), "Use the shared authenticator{Enter}");
		expect(postMock).toHaveBeenCalledWith(MESSAGES, expect.objectContaining({ params: { path: { sessionId: "solo" } } }));
		const { clientMessageId, text } = bodyOf(0);
		expect(text).toBe("Use the shared authenticator");
		expect(postMock).not.toHaveBeenCalledWith(STEER, expect.anything());
		expect(await within(rowOf("Rotate secrets")).findByTestId("sent-message")).toHaveTextContent("You:Use the shared authenticator");

		// The message opens the next turn under the same id; the row now steers that turn.
		view.update("solo", {
			status: "ready",
			model: {
				main: { ...orchestratorMain, turnId: "turn-2", notes: [{ kind: "turn", text, at: new Date().toISOString(), fromYou: true, clientMessageId }] },
				cards: [],
			},
			control: { ...RUNNING, runningTurnId: "turn-2" },
		});
		expect(within(rowOf("Rotate secrets")).queryByTestId("sent-message")).not.toBeInTheDocument();
		expect(screen.getByRole("textbox", { name: "Steer main agent" })).toBeInTheDocument();
		expect(within(rowOf("Rotate secrets")).getByRole("button", { name: "Stop turn" })).toBeInTheDocument();
	});

	it("stops a running turn from its row without opening the session", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue(accepted(undefined));
		const view = renderControlled();
		await user.click(within(rowOf("Pause API")).getByRole("button", { name: "Stop turn" }));
		expect(postMock).toHaveBeenCalledWith(INTERRUPT, { params: { path: { sessionId: "api" } } });
		expect(await within(rowOf("Pause API")).findByRole("status")).toHaveTextContent("Stopping…");
		expect(within(rowOf("Pause API")).getByRole("button", { name: "Stop turn" })).toBeDisabled();
		expect(view.onOpenSession).not.toHaveBeenCalled();
	});

	it("messages and stops one running subagent from its row, and says when it already finished", async () => {
		const user = userEvent.setup();
		postMock
			.mockResolvedValueOnce(accepted({ id: "inbox-1", state: "queued" }))
			.mockResolvedValueOnce(refused(409, "CHAT_SUBAGENT_NOT_RUNNING", "that subagent is not running"));
		renderControlled();
		const subagent = () => within(screen.getAllByTestId("agents-overview-branch").find((branch) => branch.dataset.sessionId === "api")!).getByTestId("agents-overview-subagent");

		await user.click(within(subagent()).getByRole("button", { name: "Message subagent" }));
		await user.type(screen.getByRole("textbox", { name: "Message subagent" }), "Use the v2 client{Enter}");
		expect(postMock).toHaveBeenCalledWith(SUBAGENT_MESSAGES, { params: { path: { sessionId: "api", toolUseId: "toolu-audit" } }, body: { text: "Use the v2 client" } });
		expect(await within(subagent()).findByTestId("sent-message")).toHaveTextContent("You:Use the v2 client· waiting for its next step");
		// Its actions neither fold nor open it.
		expect(within(subagent()).getByRole("button", { name: "Updates for Audit the auth flow" })).toHaveAttribute("aria-expanded", "false");

		await user.click(within(subagent()).getByRole("button", { name: "Stop subagent" }));
		expect(postMock).toHaveBeenLastCalledWith(SUBAGENT_STOP, { params: { path: { sessionId: "api", toolUseId: "toolu-audit" } } });
		expect(postMock).not.toHaveBeenCalledWith(INTERRUPT, expect.anything());
		const alerts = await screen.findAllByRole("alert");
		expect(alerts.map((alert) => alert.textContent)).toContain("This subagent already finished");
	});

	it("says when the agent is not running, with a way to open its session", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue(refused(409, "CHAT_CONTROLLER_NOT_READY", "the agent controller for this session is not running"));
		const view = renderControlled();
		await user.click(within(rowOf("Pause API")).getByRole("button", { name: "Steer main agent" }));
		const field = screen.getByRole("textbox", { name: "Steer main agent" });
		await user.type(field, "Use the cookie{Enter}");
		const alert = await screen.findByRole("alert");
		expect(alert).toHaveTextContent("The agent isn’t running; open the session to restart it. Open");
		// The words come back, and nothing pretends it was sent.
		expect(field).toHaveValue("Use the cookie");
		expect(within(rowOf("Pause API")).queryByTestId("sent-message")).not.toBeInTheDocument();
		await user.click(within(alert).getByRole("button", { name: "Open" }));
		expect(view.onOpenSession).toHaveBeenCalledWith(expect.objectContaining({ id: "api" }));
	});

	it("offers nothing where nothing can be done from here, in a slot every row holds so the times line up", async () => {
		const user = userEvent.setup();
		const view = renderControlled();
		// A terminal session keeps only opening it, from anywhere in the row, its empty slot included.
		const slot = within(rowOf("Pause UI")).getByTestId("agents-overview-actions");
		expect(slot).toHaveClass("w-[3.125rem]");
		expect(within(slot).queryAllByRole("button")).toEqual([]);
		await user.click(slot);
		expect(view.onOpenSession).toHaveBeenCalledWith(expect.objectContaining({ id: "ui" }));
		// Subagents the daemon cannot reach one by one have no controls of their own.
		const orchestrator = screen.getAllByTestId("agents-overview-branch").find((branch) => branch.dataset.sessionId === "orch")!;
		const survey = within(orchestrator).getAllByTestId("agents-overview-subagent").find((row) => row.textContent?.includes("Survey the workers"))!;
		expect(within(survey).getAllByRole("button").map((button) => button.getAttribute("aria-label"))).toEqual(["Updates for Survey the workers"]);
	});

	it("has no controls for a session the shared summaries hold without its conversation's facts", () => {
		renderControlled(loaded());
		for (const slot of screen.getAllByTestId("agents-overview-actions")) expect(within(slot).queryAllByRole("button")).toEqual([]);
	});
});
