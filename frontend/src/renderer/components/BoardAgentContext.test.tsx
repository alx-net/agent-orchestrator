import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import { useSyncExternalStore, type ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { MainAgentSummary, SubagentBoardModel, SubagentCard } from "../lib/subagent-board";
import type { ConversationItem, ConversationSnapshot, ConversationTurn } from "../types/conversation";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const { conversations, scmSummaryMock, workspaceQueryMock } = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	const data = new Map<string, unknown>();
	return {
		conversations: {
			get: (id: string) => data.get(id),
			set(id: string, snapshot: unknown) {
				data.set(id, snapshot);
				for (const listener of [...listeners]) listener();
			},
			clear: () => data.clear(),
			subscribe(listener: () => void) {
				listeners.add(listener);
				return () => {
					listeners.delete(listener);
				};
			},
		},
		scmSummaryMock: vi.fn(() => ({ data: undefined })),
		workspaceQueryMock: vi.fn(),
	};
});

vi.mock("../hooks/useConversation", () => ({
	useConversation: (sessionId: string) => {
		const snapshot = useSyncExternalStore(conversations.subscribe, () => conversations.get(sessionId));
		return { snapshot, isLoading: !snapshot, hasOlder: false, isLoadingOlder: false, loadOlder: () => {} };
	},
}));
vi.mock("../hooks/useWorkspaceQuery", () => ({ useWorkspaceQuery: () => workspaceQueryMock() }));
// Called once per card render: a count of how often the card itself redrew.
vi.mock("../hooks/useSessionScmSummary", () => ({ useSessionScmSummary: scmSummaryMock }));
vi.mock("../hooks/useTerminateSession", () => ({
	clearTerminateSessionState: () => {},
	useTerminateSessionState: () => ({ error: null, isPending: false }),
}));
vi.mock("../lib/bridge", () => ({ aoBridge: { clipboard: { writeText: vi.fn() } } }));

import { AgentSummariesProvider } from "./AgentSummariesProvider";
import { BoardAgentContextView, BoardAgentSubagentsView } from "./BoardAgentContext";
import { BoardSessionCardAdapter } from "./SessionsBoardAdapters";
import { TooltipProvider } from "./ui/tooltip";

const at = (second: number) => `2026-10-26T10:00:${String(second).padStart(2, "0")}.000Z`;

function main(overrides: Partial<MainAgentSummary> = {}): MainAgentSummary {
	return { state: "working", turnId: "turn-1", notes: [], decisions: [], sent: [], ...overrides };
}

function subagent(id: string, lane: SubagentCard["lane"]): SubagentCard {
	return {
		id,
		providerItemId: `toolu-${id}`,
		lane,
		outcome: lane === "done" ? "completed" : lane,
		startedAt: at(1),
		updatedAt: at(2),
		notes: [],
		decisions: [],
		toolCount: 0,
		inbox: [],
	};
}

const model = (summary: MainAgentSummary, cards: SubagentCard[] = []): SubagentBoardModel => ({ main: summary, cards });

describe("BoardAgentContextView", () => {
	it("shows the latest decision stronger, then what a working agent is doing, holding two lines for it", () => {
		const working = model(
			main({
				nowLine: "Running the pause e2e spec",
				nowSource: "tool",
				decisions: [
					{ kind: "decision", text: "Reuse the session lock", at: at(1) },
					{ kind: "decision", text: "Keep the slot while paused, as asked", at: at(9) },
				],
			}),
			[subagent("a", "working"), subagent("b", "needs_you"), subagent("c", "done")],
		);
		render(<BoardAgentContextView model={working} />);
		const now = screen.getByTestId("board-agent-now");
		expect(now).toHaveTextContent("Running the pause e2e spec");
		expect(now).toHaveClass("line-clamp-2", "text-muted-foreground", "min-h-[2lh]");
		const decision = screen.getByTestId("board-agent-decision");
		expect(decision).toHaveTextContent("Decision Keep the slot while paused, as asked");
		expect(decision).toHaveClass("line-clamp-2", "font-medium", "text-foreground");
		expect(within(decision).getByText("Decision")).toHaveClass("font-normal", "text-muted-foreground");
		// The now line sits last, next to the card's status line it refines.
		expect(decision.compareDocumentPosition(now) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(screen.queryByTestId("board-agent-subagents")).not.toBeInTheDocument();
	});

	it("counts the running subagents for the card's metadata, and says nothing while none run", () => {
		const { rerender, container } = render(
			<BoardAgentSubagentsView model={model(main(), [subagent("a", "working"), subagent("b", "needs_you"), subagent("c", "done")])} />,
		);
		expect(screen.getByTestId("board-agent-subagents")).toHaveTextContent("2 subagents");
		rerender(<BoardAgentSubagentsView model={model(main(), [subagent("a", "working")])} />);
		expect(screen.getByTestId("board-agent-subagents")).toHaveTextContent("1 subagent");
		rerender(<BoardAgentSubagentsView model={model(main(), [subagent("c", "done")])} />);
		expect(container).toBeEmptyDOMElement();
	});

	it.each([
		["blocked", "Blocked", "text-status-needs-you"],
		["needs_input", "Needs input", "text-status-needs-you"],
		["failed", "Failed", "text-destructive"],
		["done", "Done", "text-status-ready"],
		["stopped", "Stopped", "text-muted-foreground"],
	] as const)("tints how a turn ended (%s) and nothing else", (kind, label, tone) => {
		render(
			<BoardAgentContextView
				model={model(main({
					state: kind === "done" ? "completed" : kind === "failed" ? "failed" : kind === "stopped" ? "stopped" : "needs_you",
					nowLine: "on the staging VPN",
					nowSource: "outcome",
					result: { kind, text: "on the staging VPN", at: at(9), source: "outcome_line" },
				}))}
			/>,
		);
		const now = screen.getByTestId("board-agent-now");
		expect(now).toHaveTextContent(`${label} on the staging VPN`);
		expect(now).not.toHaveClass("min-h-[2lh]");
		expect(within(now).getByTestId("agent-outcome")).toHaveClass("font-medium", tone);
	});

	it("says what the agent waits for, and shows nothing for an idle agent with nothing to say", () => {
		const { rerender, container } = render(
			<BoardAgentContextView model={model(main({ state: "needs_you", blockedOn: "Edit README.md" }))} />,
		);
		expect(screen.getByTestId("board-agent-now")).toHaveTextContent("Waiting for you: Edit README.md");
		expect(screen.getByTestId("board-agent-now")).toHaveClass("text-foreground");
		rerender(<BoardAgentContextView model={model(main({ state: "idle", turnId: undefined }))} />);
		expect(container).toBeEmptyDOMElement();
	});
});

function workerSession(id: string, overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "app",
		workspaceName: "app",
		title: `Task ${id}`,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		kanbanColumn: "building",
		displayStatus: "Working",
		activity: { state: "active", lastActivityAt: at(30) },
		createdAt: at(0),
		updatedAt: at(30),
		prs: [],
		...overrides,
	};
}

let sequence = 0;
const message = (id: string, turnId: string, role: "user" | "assistant", text: string, second: number): ConversationItem => ({
	kind: "message",
	id,
	turnId,
	sequence: ++sequence,
	revision: 1,
	role,
	origin: role === "user" ? "human" : "provider",
	text,
	streaming: false,
	createdAt: at(second),
});

/** Turn 1 decided and asked; the person answered, and turn 2 is under way. */
function twoTurns(extra = ""): ConversationSnapshot {
	const turns: ConversationTurn[] = [
		{ id: "t1", state: "completed", requestedAt: at(0), startedAt: at(0), completedAt: at(10) },
		{ id: "t2", state: "running", requestedAt: at(12), startedAt: at(12) },
	];
	const items: ConversationItem[] = [
		message("u1", "t1", "user", "Pause sessions from the board", 0),
		message("m1", "t1", "assistant", "Status: Reading the lifecycle\nDecision: Pause through the daemon\nOutcome: needs input — Keep the slot?", 5),
		message("u2", "t2", "user", "Yes, keep it", 12),
		message("m2", "t2", "assistant", `Status: Keeping the slot on pause\n${extra}`, 14),
		{
			kind: "activity",
			id: "agent-slots",
			providerItemId: "agent-slots",
			turnId: "t2",
			sequence: ++sequence,
			revision: 1,
			activityKind: "mcp_tool",
			status: "running",
			summary: "Audit the slot allocator",
			createdAt: at(15),
			detail: { nestedAgent: true, providerToolName: "Agent", input: { description: "Audit the slot allocator" } },
		} as ConversationItem,
	];
	return { items, turns } as unknown as ConversationSnapshot;
}

function renderCards(ui: ReactElement) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<AgentSummariesProvider>{ui}</AgentSummariesProvider>
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

describe("board session cards", () => {
	beforeEach(() => {
		conversations.clear();
		scmSummaryMock.mockClear();
	});

	it("shows a chat session's now line and latest decision across turns, and leaves a terminal card as it was", () => {
		const chat = workerSession("chat");
		const tui = workerSession("tui", { mode: "tui" });
		const workspaces: WorkspaceSummary[] = [{ id: "app", name: "app", path: "/repos/app", sessions: [chat, tui] }];
		workspaceQueryMock.mockReturnValue({ data: workspaces });
		conversations.set("chat", twoTurns());
		conversations.set("tui", twoTurns());
		const onOpen = vi.fn();
		renderCards(
			<>
				<BoardSessionCardAdapter onOpen={onOpen} session={chat} />
				<BoardSessionCardAdapter onOpen={onOpen} session={tui} />
			</>,
		);
		const [chatCard, tuiCard] = screen.getAllByTestId("board-session-card");
		const context = within(chatCard!).getByTestId("board-agent-context");
		expect(within(context).getByTestId("board-agent-now")).toHaveTextContent("Keeping the slot on pause");
		// Decided in the first turn, still the latest after the person answered.
		expect(within(context).getByTestId("board-agent-decision")).toHaveTextContent("Pause through the daemon");
		// The footer keeps its one status line, with the running subagents among its metadata.
		expect(within(chatCard!).getAllByTestId("session-status")).toHaveLength(1);
		expect(within(chatCard!).getByTestId("session-status")).toHaveTextContent("Working");
		expect(within(chatCard!).getByTestId("session-card-meta")).toHaveTextContent("1 subagent");
		expect(within(tuiCard!).queryByTestId("session-card-context")).not.toBeInTheDocument();
		expect(within(tuiCard!).queryByTestId("session-card-meta")).not.toBeInTheDocument();
	});

	it("redraws only the context line while the agent streams, not the memoized card", () => {
		const chat = workerSession("chat");
		workspaceQueryMock.mockReturnValue({ data: [{ id: "app", name: "app", path: "/repos/app", sessions: [chat] }] });
		conversations.set("chat", twoTurns());
		renderCards(<BoardSessionCardAdapter onOpen={vi.fn()} session={chat} />);
		const cardRenders = scmSummaryMock.mock.calls.length;

		act(() => conversations.set("chat", twoTurns("Status: Running the e2e spec\nDecision: Skip the flaky retry\n")));
		expect(screen.getByTestId("board-agent-now")).toHaveTextContent("Running the e2e spec");
		expect(screen.getByTestId("board-agent-decision")).toHaveTextContent("Skip the flaky retry");
		expect(scmSummaryMock.mock.calls.length).toBe(cardRenders);
	});
});
