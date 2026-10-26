import { describe, expect, it } from "vitest";
import type { ActivityStatus, ConversationActivity, ConversationItem, ConversationTurn, TurnState } from "../types/conversation";
import {
	activeSubagentCount,
	createSubagentBoardDeriver,
	parseSubagentNoteLine,
	subagentAskPrefix,
	subagentLanes,
	type SubagentCard,
} from "./subagent-board";

let sequence = 0;

function activity(overrides: Partial<ConversationActivity> & Pick<ConversationActivity, "id">): ConversationActivity {
	sequence += 1;
	return {
		kind: "activity",
		turnId: "turn-1",
		sequence,
		revision: 1,
		activityKind: "mcp_tool",
		status: "running",
		summary: "",
		createdAt: new Date(Date.parse("2026-10-26T10:00:00.000Z") + sequence * 1000).toISOString(),
		...overrides,
	};
}

function parent(overrides: Partial<ConversationActivity> = {}, detail: Record<string, unknown> = {}): ConversationActivity {
	return activity({
		id: "agent-row",
		providerItemId: "agent-1",
		summary: "Audit the auth flow",
		...overrides,
		detail: {
			nestedAgent: true,
			providerToolName: "Agent",
			input: { description: "Audit the auth flow", prompt: "Read the auth module and report risks.", subagent_type: "Explore" },
			...detail,
		} as ConversationActivity["detail"],
	});
}

function narration(id: string, progress: string, overrides: Partial<ConversationActivity> = {}): ConversationActivity {
	return activity({
		id,
		providerItemId: `msg-${id}`,
		summary: "Subagent response",
		...overrides,
		detail: { parentProviderItemId: "agent-1", nestedAgent: true, progress } as ConversationActivity["detail"],
	});
}

function tool(id: string, summary: string, status: ActivityStatus, detail: Record<string, unknown> = {}): ConversationActivity {
	return activity({
		id,
		providerItemId: `tool-${id}`,
		activityKind: "command",
		status,
		summary,
		detail: { parentProviderItemId: "agent-1", providerToolName: "Bash", ...detail } as ConversationActivity["detail"],
	});
}

function turn(state: TurnState = "running"): ConversationTurn {
	return { id: "turn-1", state, requestedAt: "2026-10-26T10:00:00.000Z" };
}

function derive(items: ConversationItem[], turns: ConversationTurn[] = [turn()]): SubagentCard[] {
	return createSubagentBoardDeriver().derive(items, turns);
}

describe("parseSubagentNoteLine", () => {
	it("reads status and decision lines at the start of a line, case-insensitively", () => {
		expect(parseSubagentNoteLine("Status: Reading the auth module")).toEqual({ kind: "status", text: "Reading the auth module" });
		expect(parseSubagentNoteLine("  decision:  Use the session cookie, because tokens expire  ")).toEqual({
			kind: "decision",
			text: "Use the session cookie, because tokens expire",
		});
		expect(parseSubagentNoteLine("STATUS: done")).toEqual({ kind: "status", text: "done" });
	});

	it("tolerates the markdown the model wraps labels in", () => {
		expect(parseSubagentNoteLine("**Status:** Running tests")).toEqual({ kind: "status", text: "Running tests" });
		expect(parseSubagentNoteLine("- **Decision**: Skip the flaky suite")).toEqual({ kind: "decision", text: "Skip the flaky suite" });
	});

	it("ignores lines that only mention a label or carry no text", () => {
		expect(parseSubagentNoteLine("The status: unknown")).toBeUndefined();
		expect(parseSubagentNoteLine("Statuses: many")).toBeUndefined();
		expect(parseSubagentNoteLine("Status:")).toBeUndefined();
		expect(parseSubagentNoteLine("Just prose.")).toBeUndefined();
	});
});

describe("createSubagentBoardDeriver", () => {
	it("builds a working card from the Agent call, its narration and its tools", () => {
		const [card] = derive([
			parent(),
			narration("n1", "Looking around.\nStatus: Reading the auth module\nDecision: Start with login, because it is the entry point\n"),
			tool("t1", "rg session", "completed"),
		]);
		expect(card).toMatchObject({
			title: "Audit the auth flow",
			subagentType: "Explore",
			lane: "working",
			outcome: "working",
			toolCount: 1,
			lastToolName: "Bash",
			prompt: "Read the auth module and report risks.",
			nowLine: "Reading the auth module",
			nowSource: "status",
		});
		expect(card!.notes.map((note) => [note.kind, note.text])).toEqual([
			["status", "Reading the auth module"],
			["decision", "Start with login, because it is the entry point"],
		]);
		expect(card!.decisions).toHaveLength(1);
		expect(card!.finalReport).toBeUndefined();
	});

	it("accepts a parent stored as reasoning by an older daemon", () => {
		const cards = derive([parent({ activityKind: "reasoning" })]);
		expect(cards).toHaveLength(1);
		expect(cards[0]!.lane).toBe("working");
	});

	it("does not mistake a narration, a nested call or subagent thinking for a parent", () => {
		const cards = derive([
			parent(),
			narration("n1", "Status: working"),
			activity({ id: "nested", providerItemId: "agent-2", detail: { providerToolName: "Agent", nestedAgent: true, parentProviderItemId: "agent-1" } }),
			activity({ id: "think", providerItemId: "thought-1", activityKind: "reasoning", detail: { parentProviderItemId: "agent-1", nestedAgent: true, text: "Status: secret plan" } }),
		]);
		expect(cards).toHaveLength(1);
		// The nested call is one of its tool uses; the thinking is neither a tool nor a note.
		expect(cards[0]!.toolCount).toBe(1);
		expect(cards[0]!.notes.map((note) => note.text)).toEqual(["working"]);
	});

	it("shows the most recent of status, progress summary and running tool", () => {
		const base = [parent(), narration("n1", "Status: Planning the audit")];
		const running = tool("t1", "npm test", "running");
		expect(derive([...base, running])[0]).toMatchObject({ nowLine: "npm test", nowSource: "tool" });

		const later = Date.parse(running.createdAt) + 60_000;
		const withProgress = [parent({}, { progressSummary: "Checking token refresh", progressAt: later }), base[1]!, running];
		expect(derive(withProgress)[0]).toMatchObject({ nowLine: "Checking token refresh", nowSource: "progress" });

		const afterTool = narration("n2", "Status: Writing the report");
		expect(derive([...base, running, afterTool])[0]).toMatchObject({ nowLine: "Writing the report", nowSource: "status" });
	});

	it("works with none of the newer progress fields and prefers them when present", () => {
		const plain = derive([parent({}, { input: { description: "Map the repo" } }), tool("t1", "ls", "completed"), tool("t2", "cat", "completed")])[0]!;
		expect(plain.subagentType).toBeUndefined();
		expect(plain.toolCount).toBe(2);
		expect(plain.nowLine).toBeUndefined();

		const enriched = derive([
			parent({}, { subagentType: "code-reviewer", progressToolUses: 9, lastToolName: "Grep", progressSummary: "Reviewing diffs", progressAt: Date.parse("2026-10-26T11:00:00.000Z") }),
			tool("t1", "ls", "completed"),
		])[0]!;
		expect(enriched).toMatchObject({ subagentType: "code-reviewer", toolCount: 9, lastToolName: "Grep", nowLine: "Reviewing diffs" });
	});

	it("moves a subagent blocked on a permission request to needs you", () => {
		const input = { command: "rm -rf build" };
		const cards = derive([
			parent(),
			tool("t1", "rm -rf build", "pending", { input }),
			activity({
				id: "approval",
				providerItemId: "request-1",
				activityKind: "approval",
				status: "pending",
				summary: "Run rm -rf build",
				detail: { providerToolName: "Bash", input } as ConversationActivity["detail"],
			}),
		]);
		expect(cards[0]).toMatchObject({ lane: "needs_you", outcome: "needs_you", blockedOn: "Run rm -rf build" });
		expect(activeSubagentCount(cards)).toEqual({ running: 1, needsYou: 1 });
	});

	it("does not flag a subagent for the main agent's own permission request", () => {
		const cards = derive([
			parent(),
			tool("t1", "npm test", "running", { input: { command: "npm test" } }),
			activity({
				id: "approval",
				providerItemId: "request-1",
				activityKind: "approval",
				status: "pending",
				summary: "Edit README.md",
				detail: { providerToolName: "Edit", input: { file_path: "README.md" } } as ConversationActivity["detail"],
			}),
		]);
		expect(cards[0]!.lane).toBe("working");
	});

	it("settles a completed subagent with its final report", () => {
		const [card] = derive([
			parent({ status: "completed" }, { content: [{ type: "content", content: { type: "text", text: "## Findings\nThe refresh token never rotates." } }] }),
			narration("n1", "Status: Reading\n"),
			tool("t1", "rg token", "completed"),
		]);
		expect(card).toMatchObject({
			lane: "done",
			outcome: "completed",
			finalReport: "## Findings\nThe refresh token never rotates.",
			nowLine: "Findings",
			nowSource: "report",
		});
		expect(card!.durationMs).toBeGreaterThan(0);
	});

	it("reads the report from raw output and drops the model-directed trailer", () => {
		const output = JSON.stringify([{ type: "text", text: "All clear.\nagentId: a1 (use SendMessage to continue)\n<usage>total_tokens: 10</usage>" }]);
		const [card] = derive([parent({ status: "completed" }, { output })]);
		expect(card!.finalReport).toBe("All clear.");
	});

	it("never presents the prompt as the report", () => {
		const prompt = "Read the auth module and report risks.";
		const [card] = derive([parent({ status: "completed" }, { content: [{ type: "content", content: { type: "text", text: prompt } }] })]);
		expect(card!.finalReport).toBeUndefined();
	});

	it("tells a failure from a stop", () => {
		expect(derive([parent({ status: "failed" })])[0]!.outcome).toBe("failed");
		expect(derive([parent({ status: "failed" })], [turn("interrupted")])[0]!.outcome).toBe("stopped");
		expect(derive([parent({ status: "cancelled" })])[0]!.outcome).toBe("stopped");
		// Still marked running, but its turn is over.
		expect(derive([parent()], [turn("completed")])[0]).toMatchObject({ lane: "done", outcome: "stopped" });
	});

	it("keeps a backgrounded subagent working after its call returns", () => {
		const launched = [{ type: "content", content: { type: "text", text: "Async agent launched successfully." } }];
		const viaNotice = derive([parent({ status: "completed" }, { content: launched, progressAt: Date.parse("2026-10-26T11:00:00.000Z") })])[0]!;
		expect(viaNotice).toMatchObject({ lane: "working" });
		expect(viaNotice.finalReport).toBeUndefined();

		const viaFlag = derive([parent({ status: "completed" }, { input: { description: "Watch CI", run_in_background: true } })])[0]!;
		expect(viaFlag.lane).toBe("working");

		const viaChild = derive([parent({ status: "completed" }), tool("t1", "sleep 60", "running")])[0]!;
		expect(viaChild.lane).toBe("working");

		expect(derive([parent({ status: "completed" }, { content: launched })], [turn("completed")])[0]!.lane).toBe("done");
	});

	it("parses streamed narration incrementally and reuses unchanged cards", () => {
		const deriver = createSubagentBoardDeriver();
		const agent = parent();
		const first = narration("n1", "Status: Reading\nDecision: Use");
		const turns = [turn()];
		const before = deriver.derive([agent, first], turns)[0]!;
		expect(before.notes.map((note) => note.text)).toEqual(["Reading", "Use"]);
		expect(deriver.derive([agent, first], turns)[0]).toBe(before);

		const grown = { ...first, revision: 2, detail: { ...first.detail, progress: "Status: Reading\nDecision: Use the cookie\nStatus: Testing" } };
		const after = deriver.derive([agent, grown], turns)[0]!;
		expect(after).not.toBe(before);
		expect(after.notes.map((note) => [note.kind, note.text])).toEqual([
			["status", "Reading"],
			["decision", "Use the cookie"],
			["status", "Testing"],
		]);
		expect(after.nowLine).toBe("Testing");

		// A settle can rewrite the block; the parse starts over.
		const settled = { ...first, revision: 3, status: "completed" as const, detail: { ...first.detail, progress: "Decision: Ship it" } };
		expect(deriver.derive([agent, settled], turns)[0]!.notes.map((note) => note.text)).toEqual(["Ship it"]);
	});

	it("returns nothing when the conversation has no subagents", () => {
		expect(derive([tool("t1", "ls", "completed")])).toEqual([]);
	});
});

describe("subagentLanes", () => {
	it("groups cards, keeping live lanes in start order and done newest first", () => {
		const cards = [
			{ id: "a", lane: "done" },
			{ id: "b", lane: "working" },
			{ id: "c", lane: "done" },
			{ id: "d", lane: "needs_you" },
			{ id: "e", lane: "working" },
		] as SubagentCard[];
		const lanes = subagentLanes(cards);
		expect(lanes.working.map((card) => card.id)).toEqual(["b", "e"]);
		expect(lanes.needs_you.map((card) => card.id)).toEqual(["d"]);
		expect(lanes.done.map((card) => card.id)).toEqual(["c", "a"]);
	});
});

describe("subagentAskPrefix", () => {
	it("names the subagent for the main agent", () => {
		expect(subagentAskPrefix("Audit the auth flow")).toBe('About the subagent "Audit the auth flow": ');
	});
});
