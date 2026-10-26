import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import {
	EMPTY_AGENT_CONTROL,
	MAIN_AGENT_CONTROL_KEY,
	type AgentBoardControls,
	type AgentControlState,
} from "../../hooks/useAgentBoardControls";
import type { MainAgentSummary, SubagentCard } from "../../lib/subagent-board";
import { TooltipProvider } from "../ui/tooltip";
import { SubagentBoard } from "./SubagentBoard";

function card(overrides: Partial<SubagentCard> & Pick<SubagentCard, "id">): SubagentCard {
	return {
		providerItemId: `provider-${overrides.id}`,
		turnId: "turn-1",
		title: "Audit the auth flow",
		lane: "working",
		outcome: "working",
		startedAt: new Date(Date.now() - 90_000).toISOString(),
		updatedAt: new Date().toISOString(),
		notes: [],
		decisions: [],
		toolCount: 3,
		inbox: [],
		...overrides,
	};
}

function controls(
	overrides: Partial<AgentBoardControls> = {},
	states: Record<string, Partial<AgentControlState>> = {},
): AgentBoardControls {
	return {
		states: new Map(Object.entries(states).map(([key, state]) => [key, { ...EMPTY_AGENT_CONTROL, ...state }])),
		steerMain: vi.fn(async () => true),
		messageSubagent: vi.fn(async () => true),
		stopSubagent: vi.fn(async () => {}),
		...overrides,
	};
}

const working = card({
	id: "a",
	subagentType: "Explore",
	nowLine: "Reading the auth module",
	prompt: "Read the auth module and report risks.",
	notes: [
		{ kind: "status", text: "Reading the auth module", at: "2026-10-26T10:00:01.000Z" },
		{ kind: "decision", text: "Start with login, because it is the entry point", at: "2026-10-26T10:00:02.000Z" },
	],
});
const blocked = card({ id: "b", title: "Clean the build", lane: "needs_you", outcome: "needs_you", blockedOn: "Run rm -rf build" });
const done = card({
	id: "c",
	title: "Map the repo",
	lane: "done",
	outcome: "completed",
	durationMs: 42_000,
	nowLine: "Findings",
	finalReport: "The refresh token never rotates.",
});
const failed = card({ id: "d", title: "Run the e2e suite", lane: "done", outcome: "failed", turnId: "turn-0" });

const mainWorking: MainAgentSummary = {
	state: "working",
	turnId: "turn-1",
	startedAt: new Date(Date.now() - 125_000).toISOString(),
	nowLine: "Wiring the board into the chat",
	nowSource: "status",
	notes: [
		{ kind: "status", text: "Reading the board code", at: "2026-10-26T10:00:01.000Z" },
		{ kind: "decision", text: "Reuse the card timeline, since it already emphasizes decisions", at: "2026-10-26T10:00:02.000Z" },
		{ kind: "status", text: "Wiring the board into the chat", at: "2026-10-26T10:00:03.000Z" },
	],
	decisions: [{ kind: "decision", text: "Reuse the card timeline, since it already emphasizes decisions", at: "2026-10-26T10:00:02.000Z" }],
	sent: [],
};
const mainIdle: MainAgentSummary = { state: "idle", turnId: "turn-1", notes: [], decisions: [], sent: [] };

function renderBoard(ui: ReactElement) {
	return render(<TooltipProvider>{ui}</TooltipProvider>);
}

describe("SubagentBoard", () => {
	it("explains an empty board", () => {
		renderBoard(<SubagentBoard cards={[]} mode="chat" />);
		expect(screen.getByRole("status")).toHaveTextContent("No subagents yet");
	});

	it("says the board needs chat mode in a terminal session", () => {
		renderBoard(<SubagentBoard cards={[working]} main={mainWorking} mode="tui" />);
		expect(screen.getByRole("status")).toHaveTextContent("The agents board needs chat mode.");
		expect(screen.queryByTestId("subagent-card")).not.toBeInTheDocument();
		expect(screen.queryByTestId("main-agent-card")).not.toBeInTheDocument();
	});

	it("pins the main agent above the lanes with its now line and latest decision", async () => {
		const user = userEvent.setup();
		const onStopTurn = vi.fn();
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[working]} main={mainWorking} mode="chat" onStopTurn={onStopTurn} />);
		const main = screen.getByRole("article", { name: "Main agent" });
		// Before the lanes in reading order.
		expect(main.compareDocumentPosition(screen.getAllByTestId("subagent-lane")[0]!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		expect(main).toHaveAttribute("data-state", "working");
		expect(within(main).getByText("Working")).toHaveClass("sr-only");
		expect(within(main).getByTestId("main-agent-now")).toHaveTextContent("Wiring the board into the chat");
		expect(within(main).getByTestId("main-agent-decision")).toHaveTextContent("Reuse the card timeline, since it already emphasizes decisions");
		expect(within(main).getByText("2m")).toBeInTheDocument();

		await user.click(within(main).getByRole("button", { name: "Stop turn" }));
		expect(onStopTurn).toHaveBeenCalledTimes(1);

		const toggle = within(main).getByRole("button", { name: /Main agent/ });
		await user.click(toggle);
		expect(toggle).toHaveAttribute("aria-expanded", "true");
		const updates = within(main).getByRole("region", { name: "Updates" });
		expect(within(updates).getAllByRole("listitem").map((item) => item.dataset.noteKind)).toEqual(["status", "decision", "status"]);
		fireEvent.keyDown(toggle, { key: "Escape" });
		expect(toggle).toHaveAttribute("aria-expanded", "false");
	});

	it("shows the main agent before any subagent starts, without a stop when no turn runs", () => {
		renderBoard(<SubagentBoard activeTurnId={undefined} cards={[]} main={mainIdle} mode="chat" onStopTurn={vi.fn()} />);
		const main = screen.getByRole("article", { name: "Main agent" });
		expect(main).toHaveAttribute("data-state", "idle");
		expect(within(main).getByText("Idle")).toHaveClass("sr-only");
		expect(within(main).queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();
		expect(screen.getByRole("status")).toHaveTextContent("No subagents yet");
		expect(screen.queryByTestId("subagent-lane")).not.toBeInTheDocument();
	});

	it("says what the main agent is waiting for", () => {
		renderBoard(<SubagentBoard cards={[]} main={{ ...mainWorking, state: "needs_you", blockedOn: "Edit README.md" }} mode="chat" />);
		const main = screen.getByRole("article", { name: "Main agent" });
		expect(main).toHaveAttribute("data-state", "needs_you");
		expect(within(main).getByTestId("main-agent-now")).toHaveTextContent("Waiting for you: Edit README.md");
	});

	it("places cards in working, needs you and done lanes with counts", () => {
		renderBoard(<SubagentBoard cards={[working, blocked, done, failed]} mode="chat" />);
		const lanes = screen.getAllByTestId("subagent-lane");
		expect(lanes.map((lane) => lane.getAttribute("aria-label"))).toEqual(["Working", "Needs you", "Done"]);
		const [workingLane, needsLane, doneLane] = lanes;
		expect(within(workingLane!).getByText("Audit the auth flow")).toBeInTheDocument();
		expect(within(workingLane!).getByText("Explore")).toBeInTheDocument();
		expect(within(workingLane!).getByTestId("subagent-now")).toHaveTextContent("Reading the auth module");
		expect(within(workingLane!).getByText("1m")).toBeInTheDocument();
		expect(within(workingLane!).getByText("3 tools")).toBeInTheDocument();
		expect(within(needsLane!).getByTestId("subagent-now")).toHaveTextContent("Waiting for you: Run rm -rf build");
		expect(within(doneLane!).getAllByTestId("subagent-card")).toHaveLength(2);
		expect(within(doneLane!).getByText("2")).toBeInTheDocument();
		expect(within(doneLane!).getByText("42s")).toBeInTheDocument();
		expect(within(doneLane!).getByText("Failed")).toBeInTheDocument();
	});

	it("expands a card into its decisions, task and final report", async () => {
		const user = userEvent.setup();
		renderBoard(<SubagentBoard cards={[working, done]} mode="chat" />);
		const toggle = screen.getByRole("button", { name: /Audit the auth flow/ });
		expect(toggle).toHaveAttribute("aria-expanded", "false");
		await user.click(toggle);
		expect(toggle).toHaveAttribute("aria-expanded", "true");
		const updates = screen.getByRole("region", { name: "Updates" });
		const decision = within(updates).getByText("Start with login, because it is the entry point").closest("li");
		expect(decision).toHaveAttribute("data-note-kind", "decision");
		expect(within(decision!).getByText("Decision")).toBeInTheDocument();
		expect(screen.queryByText("Read the auth module and report risks.")).not.toBeInTheDocument();
		await user.click(screen.getByRole("button", { name: "Task" }));
		expect(screen.getByText("Read the auth module and report risks.")).toBeInTheDocument();

		await user.click(screen.getByRole("button", { name: /Map the repo/ }));
		expect(screen.getByRole("region", { name: "Final report" })).toHaveTextContent("The refresh token never rotates.");
	});

	it("asks the main agent about a subagent and stops the main turn", async () => {
		const user = userEvent.setup();
		const onAskMainAgent = vi.fn();
		const onStopTurn = vi.fn();
		renderBoard(
			<SubagentBoard activeTurnId="turn-1" cards={[working, done, failed]} mode="chat" onAskMainAgent={onAskMainAgent} onStopTurn={onStopTurn} />,
		);
		const workingCard = screen.getAllByTestId("subagent-card").find((node) => node.dataset.outcome === "working")!;
		await user.click(within(workingCard).getByRole("button", { name: "Ask main agent" }));
		expect(onAskMainAgent).toHaveBeenCalledWith(working);
		await user.click(within(workingCard).getByRole("button", { name: "Stop turn" }));
		expect(onStopTurn).toHaveBeenCalledTimes(1);
		// A finished subagent has nothing left to stop.
		for (const node of screen.getAllByTestId("subagent-card").filter((entry) => entry.dataset.lane === "done")) {
			expect(within(node).queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();
			expect(within(node).getByRole("button", { name: "Ask main agent" })).toBeInTheDocument();
		}
	});

	it("offers no stop for a subagent of another turn", () => {
		renderBoard(<SubagentBoard activeTurnId="turn-2" cards={[working]} mode="chat" onStopTurn={vi.fn()} />);
		expect(screen.queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();
	});

	it("keeps Escape on the board from reaching the chat, folding open cards", async () => {
		const user = userEvent.setup();
		const outer = vi.fn();
		render(
			<TooltipProvider>
				<div onKeyDown={outer}>
					<SubagentBoard cards={[working]} mode="chat" />
				</div>
			</TooltipProvider>,
		);
		const toggle = screen.getByRole("button", { name: /Audit the auth flow/ });
		await user.click(toggle);
		expect(toggle).toHaveAttribute("aria-expanded", "true");
		fireEvent.keyDown(toggle, { key: "Escape" });
		expect(toggle).toHaveAttribute("aria-expanded", "false");
		expect(outer).not.toHaveBeenCalled();
	});
});

describe("SubagentBoard stepping in", () => {
	const inTooltips = (ui: ReactElement) => <TooltipProvider>{ui}</TooltipProvider>;

	it("steers the running main turn from its card", async () => {
		const user = userEvent.setup();
		const board = controls();
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[]} controls={board} main={mainWorking} mode="chat" />);
		const main = screen.getByRole("article", { name: "Main agent" });
		await user.click(within(main).getByRole("button", { name: "Steer main agent" }));
		expect(within(main).getByRole("button", { name: /Main agent/ })).toHaveAttribute("aria-expanded", "true");
		const field = within(main).getByRole("textbox", { name: "Steer main agent" });
		expect(field).toHaveFocus();
		await user.type(field, "Keep the old route working{Enter}");
		expect(board.steerMain).toHaveBeenCalledWith("Keep the old route working");
		expect(field).toHaveValue("");
	});

	it("offers no steering while the main agent is idle", async () => {
		const user = userEvent.setup();
		renderBoard(<SubagentBoard activeTurnId={undefined} cards={[]} controls={controls()} main={mainIdle} mode="chat" />);
		const main = screen.getByRole("article", { name: "Main agent" });
		expect(within(main).queryByRole("button", { name: "Steer main agent" })).not.toBeInTheDocument();
		await user.click(within(main).getByRole("button", { name: /Main agent/ }));
		expect(within(main).queryByRole("textbox")).not.toBeInTheDocument();
	});

	it("messages a running subagent by its tool use id, and gives back words that did not go through", async () => {
		const user = userEvent.setup();
		const board = controls({ messageSubagent: vi.fn(async () => false) });
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[working]} controls={board} mode="chat" />);
		const subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		await user.click(within(subagent).getByRole("button", { name: "Message subagent" }));
		const field = within(subagent).getByRole("textbox", { name: "Message subagent" });
		expect(field).toHaveFocus();
		// A blank line sends nothing.
		await user.type(field, "   {Enter}");
		expect(board.messageSubagent).not.toHaveBeenCalled();
		await user.type(field, "Skip the OAuth adapter{Enter}");
		expect(board.messageSubagent).toHaveBeenCalledWith("provider-a", "Skip the OAuth adapter");
		expect(field).toHaveValue("Skip the OAuth adapter");
	});

	it("clears on Escape, then hands focus back to the card, stopping and folding nothing", async () => {
		const user = userEvent.setup();
		const outer = vi.fn();
		const onStopTurn = vi.fn();
		const board = controls();
		render(inTooltips(
			<div onKeyDown={outer}>
				<SubagentBoard activeTurnId="turn-1" cards={[working]} controls={board} mode="chat" onStopTurn={onStopTurn} />
			</div>,
		));
		const subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		await user.click(within(subagent).getByRole("button", { name: "Message subagent" }));
		const field = within(subagent).getByRole("textbox", { name: "Message subagent" });
		await user.type(field, "half a thought");
		await user.keyboard("{Escape}");
		expect(field).toHaveValue("");
		expect(field).toHaveFocus();
		await user.keyboard("{Escape}");
		const toggle = within(subagent).getByRole("button", { name: /Audit the auth flow/ });
		expect(toggle).toHaveFocus();
		expect(toggle).toHaveAttribute("aria-expanded", "true");
		// Typing reaches the chat as usual; Escape never does, so it cannot stop the turn.
		expect(outer.mock.calls.some(([event]) => (event as { key: string }).key === "Escape")).toBe(false);
		expect(onStopTurn).not.toHaveBeenCalled();
		expect(board.stopSubagent).not.toHaveBeenCalled();
		expect(board.messageSubagent).not.toHaveBeenCalled();
	});

	it("shows sent messages by delivery state after the lines written before them, an echoed send once", async () => {
		const user = userEvent.setup();
		const echoed = card({
			...working,
			inbox: [
				{ id: "m-1", text: "Use the v2 client", state: "delivered", at: "2026-10-26T10:00:05.000Z" },
				{ id: "m-2", text: "Also check refresh tokens", state: "dropped", at: "2026-10-26T10:00:06.000Z" },
			],
		});
		const board = controls({}, {
			"provider-a": {
				pending: [
					{ id: "m-1", text: "Use the v2 client", state: "queued", at: "2026-10-26T10:00:05.000Z" },
					{ id: "local:1", text: "And the logout path", state: "queued", at: "2026-10-26T10:00:07.000Z" },
				],
			},
		});
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[echoed]} controls={board} mode="chat" />);
		await user.click(screen.getByRole("button", { name: /Audit the auth flow/ }));
		const updates = screen.getByRole("region", { name: "Updates" });
		expect(within(updates).getAllByRole("listitem").map((item) => item.dataset.noteKind ?? item.dataset.sentState)).toEqual([
			"status",
			"decision",
			"delivered",
			"dropped",
			"queued",
		]);
		const [delivered, dropped, queued] = within(updates).getAllByTestId("sent-message");
		expect(delivered).toHaveTextContent("You:Use the v2 client· delivered");
		expect(dropped).toHaveTextContent("Also check refresh tokens· not delivered — it finished first");
		expect(queued).toHaveTextContent("And the logout path· waiting for its next step");
	});

	it("stops only the subagent, shows it stopping, then settles it as stopped", async () => {
		const user = userEvent.setup();
		const onStopTurn = vi.fn();
		const board = controls();
		const view = renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[working]} controls={board} mode="chat" onStopTurn={onStopTurn} />);
		let subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		expect(within(subagent).queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();
		await user.click(within(subagent).getByRole("button", { name: "Stop subagent" }));
		expect(board.stopSubagent).toHaveBeenCalledWith("provider-a");
		expect(onStopTurn).not.toHaveBeenCalled();

		const stopping = controls({}, { "provider-a": { stopping: true, stopped: true } });
		view.rerender(inTooltips(<SubagentBoard activeTurnId="turn-1" cards={[working]} controls={stopping} mode="chat" />));
		expect(within(subagent).getByRole("status")).toHaveTextContent("Stopping…");
		expect(within(subagent).getByRole("button", { name: "Stop subagent" })).toBeDisabled();
		expect(within(subagent).getByRole("button", { name: "Message subagent" })).toBeDisabled();

		// Its call settles however it settles; the stop is why it ended.
		view.rerender(inTooltips(
			<SubagentBoard activeTurnId="turn-1" cards={[{ ...working, lane: "done", outcome: "completed" }]} controls={stopping} mode="chat" />,
		));
		subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		expect(subagent).toHaveAttribute("data-lane", "done");
		expect(subagent).toHaveAttribute("data-outcome", "stopped");
		expect(within(subagent).getByText("Stopped")).toHaveClass("sr-only");
		expect(within(subagent).queryByRole("status")).not.toBeInTheDocument();
	});

	it("offers neither a field nor a stop on a finished subagent, keeping Ask main agent", async () => {
		const user = userEvent.setup();
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[done]} controls={controls()} mode="chat" onAskMainAgent={vi.fn()} onStopTurn={vi.fn()} />);
		const finished = screen.getByRole("article", { name: "Map the repo" });
		for (const name of ["Message subagent", "Stop subagent", "Stop turn"]) {
			expect(within(finished).queryByRole("button", { name })).not.toBeInTheDocument();
		}
		expect(within(finished).getByRole("button", { name: "Ask main agent" })).toBeInTheDocument();
		await user.click(within(finished).getByRole("button", { name: /Map the repo/ }));
		expect(within(finished).queryByRole("textbox")).not.toBeInTheDocument();
	});

	it("says on the card why a send or a stop did not go through", async () => {
		const user = userEvent.setup();
		const board = controls({}, {
			"provider-a": { sendError: { kind: "message", message: "This session cannot message subagents" }, stopError: { kind: "finished" } },
			[MAIN_AGENT_CONTROL_KEY]: { sendError: { kind: "message" } },
		});
		renderBoard(<SubagentBoard activeTurnId="turn-1" cards={[working]} controls={board} main={mainWorking} mode="chat" />);
		const subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		expect(within(subagent).getByRole("alert")).toHaveTextContent("This subagent already finished");
		await user.click(within(subagent).getByRole("button", { name: /Audit the auth flow/ }));
		const field = within(subagent).getByRole("textbox", { name: "Message subagent" });
		expect(field).toHaveAttribute("aria-invalid", "true");
		expect(field).toHaveAccessibleDescription("This session cannot message subagents");

		const main = screen.getByRole("article", { name: "Main agent" });
		await user.click(within(main).getByRole("button", { name: /Main agent/ }));
		expect(within(main).getByRole("alert")).toHaveTextContent("That didn’t go through. Try again.");
	});
});
