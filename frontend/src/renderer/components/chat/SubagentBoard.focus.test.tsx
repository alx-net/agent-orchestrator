import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { EMPTY_AGENT_CONTROL, MAIN_AGENT_CONTROL_KEY, type AgentBoardControls } from "../../hooks/useAgentBoardControls";
import type { MainAgentSummary, SubagentCard, SubagentNote } from "../../lib/subagent-board";
import { TooltipProvider } from "../ui/tooltip";
import { SubagentBoard } from "./SubagentBoard";

// The session streams a new snapshot every ~150 ms while agents work; each one
// derives fresh card and summary objects. A field the person is typing in must
// survive all of them.

const startedAt = new Date(Date.now() - 60_000).toISOString();
const note = (index: number): SubagentNote => ({ kind: "status", text: `Step ${index}`, at: new Date(Date.now() + index).toISOString() });

function snapshot(index: number, lane: SubagentCard["lane"] = "working") {
	const notes = Array.from({ length: index + 1 }, (_, n) => note(n));
	const card: SubagentCard = {
		id: "a",
		providerItemId: "provider-a",
		turnId: "turn-1",
		title: "Audit the auth flow",
		lane,
		outcome: lane === "needs_you" ? "needs_you" : "working",
		startedAt,
		updatedAt: new Date().toISOString(),
		nowLine: `Step ${index}`,
		notes,
		decisions: [],
		toolCount: index,
		inbox: [],
	};
	const main: MainAgentSummary = {
		state: "working",
		turnId: "turn-1",
		startedAt,
		nowLine: `Main step ${index}`,
		nowSource: "status",
		notes,
		decisions: [],
		sent: [],
	};
	// The controls hook hands out a new object whenever any card's state moves.
	const controls: AgentBoardControls = {
		states: new Map([[MAIN_AGENT_CONTROL_KEY, { ...EMPTY_AGENT_CONTROL }]]),
		steerMain: vi.fn(async () => true),
		messageSubagent: vi.fn(async () => true),
		stopSubagent: vi.fn(async () => {}),
	};
	return { cards: [card], main, controls };
}

function board(index: number, lane?: SubagentCard["lane"]) {
	const { cards, main, controls } = snapshot(index, lane);
	return (
		<TooltipProvider>
			<SubagentBoard activeTurnId="turn-1" cards={cards} controls={controls} main={main} mode="chat" />
		</TooltipProvider>
	);
}

describe("SubagentBoard keeps the field the person is typing in", () => {
	it("keeps the steer and message fields focused, with every keystroke, while updates stream in", async () => {
		const user = userEvent.setup();
		const { rerender } = render(board(0));
		const main = screen.getByRole("article", { name: "Main agent" });
		await user.click(within(main).getByRole("button", { name: "Steer main agent" }));
		const steer = within(main).getByRole("textbox", { name: "Steer main agent" });
		expect(steer).toHaveFocus();

		let typed = "";
		for (let index = 1; index <= 6; index += 1) {
			await user.keyboard(`s${index}`);
			typed += `s${index}`;
			rerender(board(index));
			expect(steer).toHaveFocus();
			expect(steer).toHaveValue(typed);
		}

		const subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		await user.click(within(subagent).getByRole("button", { name: "Message subagent" }));
		const message = within(subagent).getByRole("textbox", { name: "Message subagent" });
		expect(message).toHaveFocus();
		typed = "";
		for (let index = 7; index <= 12; index += 1) {
			await user.keyboard(`m${index}`);
			typed += `m${index}`;
			rerender(board(index));
			expect(message).toHaveFocus();
			expect(message).toHaveValue(typed);
		}
	});

	it("keeps a subagent's message and caret when the card changes lane mid-sentence", async () => {
		const user = userEvent.setup();
		const { rerender } = render(board(0));
		const subagent = screen.getByRole("article", { name: "Audit the auth flow" });
		await user.click(within(subagent).getByRole("button", { name: "Message subagent" }));
		await user.keyboard("Use the staging ");

		// It asks for a permission: the card moves to Needs you, then back to Working.
		for (const [index, lane] of [[1, "needs_you"], [2, "working"]] as const) {
			rerender(board(index, lane));
			const field = screen.getByRole("textbox", { name: "Message subagent" });
			expect(field).toHaveFocus();
			expect(field).toHaveValue("Use the staging ");
		}
		await user.keyboard("database");
		expect(screen.getByRole("textbox", { name: "Message subagent" })).toHaveValue("Use the staging database");
	});
});
