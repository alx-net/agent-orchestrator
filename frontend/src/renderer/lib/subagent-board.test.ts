import { describe, expect, it } from "vitest";
import type {
	ActivityStatus,
	ConversationActivity,
	ConversationItem,
	ConversationMessage,
	ConversationPlan,
	ConversationTurn,
	TurnState,
} from "../types/conversation";
import {
	activeSubagentCount,
	boardTimeline,
	createSubagentBoardDeriver,
	parseSubagentNoteLine,
	subagentAskPrefix,
	subagentLanes,
	withPendingMessages,
	withStopRequests,
	type MainAgentSummary,
	type SentMessage,
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
	return createSubagentBoardDeriver().derive(items, turns).cards;
}

function said(id: string, text: string, overrides: Partial<ConversationMessage> = {}): ConversationMessage {
	sequence += 1;
	return {
		kind: "message",
		id,
		turnId: "turn-1",
		sequence,
		revision: 1,
		role: "assistant",
		origin: "provider",
		text,
		streaming: false,
		createdAt: new Date(Date.parse("2026-10-26T10:00:00.000Z") + sequence * 1000).toISOString(),
		...overrides,
	};
}

function mainOf(items: ConversationItem[], turns: ConversationTurn[] = [turn()]): MainAgentSummary {
	return createSubagentBoardDeriver().derive(items, turns).main;
}

function planned(step: string): ConversationPlan {
	return { steps: [{ text: "Read the code", status: "completed" }, { text: step, status: "in_progress" }, { text: "Ship", status: "pending" }] };
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
		const before = deriver.derive([agent, first], turns).cards[0]!;
		expect(before.notes.map((note) => note.text)).toEqual(["Reading", "Use"]);
		expect(deriver.derive([agent, first], turns).cards[0]).toBe(before);

		const grown = { ...first, revision: 2, detail: { ...first.detail, progress: "Status: Reading\nDecision: Use the cookie\nStatus: Testing" } };
		const after = deriver.derive([agent, grown], turns).cards[0]!;
		expect(after).not.toBe(before);
		expect(after.notes.map((note) => [note.kind, note.text])).toEqual([
			["status", "Reading"],
			["decision", "Use the cookie"],
			["status", "Testing"],
		]);
		expect(after.nowLine).toBe("Testing");

		// A settle can rewrite the block; the parse starts over.
		const settled = { ...first, revision: 3, status: "completed" as const, detail: { ...first.detail, progress: "Decision: Ship it" } };
		expect(deriver.derive([agent, settled], turns).cards[0]!.notes.map((note) => note.text)).toEqual(["Ship it"]);
	});

	it("returns nothing when the conversation has no subagents", () => {
		expect(derive([tool("t1", "ls", "completed")])).toEqual([]);
	});
});

describe("main agent summary", () => {
	const topLevelTool = (id: string, summary: string, status: ActivityStatus = "running") =>
		activity({ id, providerItemId: id, activityKind: "command", status, summary, detail: { providerToolName: "Bash" } as ConversationActivity["detail"] });

	it("reads its status and decision lines from its own assistant messages", () => {
		const message = said("m1", "Looking at the board.\nStatus: Reading the board code\nDecision: Reuse the line parser, because it is incremental\n");
		const main = mainOf([
			said("u1", "Status: not the agent", { role: "user", origin: "human" }),
			message,
			parent(),
			narration("n1", "Status: A subagent line\n"),
		]);
		expect(main).toMatchObject({
			state: "working",
			turnId: "turn-1",
			startedAt: "2026-10-26T10:00:00.000Z",
			nowLine: "Reading the board code",
			nowSource: "status",
		});
		expect(main.notes).toEqual([
			{ kind: "status", text: "Reading the board code", at: message.createdAt },
			{ kind: "decision", text: "Reuse the line parser, because it is incremental", at: message.createdAt },
		]);
		expect(main.decisions.map((note) => note.text)).toEqual(["Reuse the line parser, because it is incremental"]);
	});

	it("shows the newest of its plan step, its status line and its running tool", () => {
		const deriver = createSubagentBoardDeriver();
		const withPlan = (step: string): ConversationTurn[] => [{ ...turn(), plan: planned(step) }];
		const status = said("m1", "Status: Reading the board code\n");
		// A step already in progress when the turn arrives is as old as the turn.
		expect(deriver.derive([status], withPlan("Write the derivation")).main).toMatchObject({
			nowLine: "Reading the board code",
			nowSource: "status",
		});

		const npm = topLevelTool("npm", "npm test");
		// Newer, but a subagent's tool and the agent's thinking are not what the main agent runs.
		const nested = tool("nested", "rg token", "running");
		const thinking = activity({ id: "think", providerItemId: "think", activityKind: "reasoning", summary: "Thinking" });
		expect(deriver.derive([status, npm, nested, thinking], withPlan("Write the derivation")).main).toMatchObject({
			nowLine: "npm test",
			nowSource: "tool",
		});

		// The step moves on while the turn is watched, after everything already shown.
		const finished = { ...npm, revision: 2, status: "completed" as const };
		expect(deriver.derive([status, finished, nested, thinking], withPlan("Update the e2e spec")).main).toMatchObject({
			nowLine: "Update the e2e spec",
			nowSource: "plan",
		});

		const next = said("m2", "Status: Running the e2e spec");
		expect(deriver.derive([status, finished, nested, thinking, next], withPlan("Update the e2e spec")).main).toMatchObject({
			nowLine: "Running the e2e spec",
			nowSource: "status",
		});
	});

	it("needs you only for a request that no subagent raised", () => {
		const input = { command: "rm -rf build" };
		const subagentAsks = [
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
		];
		const board = createSubagentBoardDeriver().derive(subagentAsks, [turn()]);
		expect(board.cards[0]!.lane).toBe("needs_you");
		expect(board.main).toMatchObject({ state: "working", blockedOn: undefined });

		const own = activity({
			id: "own",
			providerItemId: "request-2",
			activityKind: "approval",
			status: "pending",
			summary: "Edit README.md",
			detail: { providerToolName: "Edit", input: { file_path: "README.md" } } as ConversationActivity["detail"],
		});
		expect(mainOf([...subagentAsks, own])).toMatchObject({ state: "needs_you", blockedOn: "Edit README.md" });

		// With no subagent at all, and a question carrying no tool input to match.
		const question = activity({ id: "question", providerItemId: "question", activityKind: "user_input", status: "pending", summary: "Which branch?" });
		expect(mainOf([question])).toMatchObject({ state: "needs_you", blockedOn: "Which branch?" });
		expect(mainOf([{ ...question, status: "resolved" as const }])).toMatchObject({ state: "working", blockedOn: undefined });
	});

	it("is idle between turns and falls back to the previous turn's lines", () => {
		const earlier: ConversationTurn = { id: "turn-0", state: "completed", requestedAt: "2026-10-26T09:00:00.000Z" };
		const items = [
			said("m0", "Decision: Add a migration, since the column is new\nStatus: Wrote the migration\n", { turnId: "turn-0" }),
			said("m1", "Here is the answer."),
		];
		const idle = mainOf(items, [earlier, turn("completed")]);
		expect(idle).toMatchObject({ state: "idle", turnId: "turn-1", startedAt: undefined, nowLine: "Wrote the migration", nowSource: "status" });
		expect(idle.notes.map((note) => [note.kind, note.text, note.at])).toEqual([
			["decision", "Add a migration, since the column is new", items[0]!.createdAt],
			["status", "Wrote the migration", items[0]!.createdAt],
		]);
		expect(idle.decisions).toHaveLength(1);

		// A turn with lines of its own shows only those.
		const own = mainOf([...items, said("m2", "Status: Answering\n")], [earlier, turn("completed")]);
		expect(own.notes.map((note) => note.text)).toEqual(["Answering"]);

		// A new turn that has said nothing keeps the last lines, but not as its now line.
		const next = mainOf(items, [earlier, turn("completed"), { id: "turn-2", state: "running", requestedAt: "2026-10-26T11:00:00.000Z" }]);
		expect(next).toMatchObject({ state: "working", turnId: "turn-2", startedAt: "2026-10-26T11:00:00.000Z", nowLine: undefined });
		expect(next.notes.map((note) => note.text)).toEqual(["Add a migration, since the column is new", "Wrote the migration"]);
	});

	it("parses streamed messages incrementally and keeps an unchanged summary", () => {
		const deriver = createSubagentBoardDeriver();
		const turns = [turn()];
		const streaming = said("m1", "Status: Reading\nDecision: Use", { streaming: true });
		const before = deriver.derive([streaming], turns).main;
		expect(before.notes.map((note) => note.text)).toEqual(["Reading", "Use"]);
		expect(deriver.derive([streaming], turns).main).toBe(before);

		const grown = { ...streaming, revision: 2, text: `${streaming.text} the cookie\nStatus: Testing` };
		const after = deriver.derive([grown], turns).main;
		expect(after).not.toBe(before);
		expect(after.notes.map((note) => [note.kind, note.text])).toEqual([
			["status", "Reading"],
			["decision", "Use the cookie"],
			["status", "Testing"],
		]);
		expect(after.nowLine).toBe("Testing");
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

describe("messages sent from the board", () => {
	const at = (second: number) => `2026-10-26T10:00:${String(second).padStart(2, "0")}.000Z`;
	const sent = (id: string, second: number, state: SentMessage["state"] = "queued"): SentMessage => ({ id, text: id, state, at: at(second) });

	it("reads the subagent's inbox from its Agent row, skipping entries it cannot show", () => {
		const deriver = createSubagentBoardDeriver();
		const turns = [turn()];
		const sentAt = Date.parse(at(30));
		const card = deriver.derive([parent({}, {
			subagentInbox: [
				{ id: "m-1", text: "Use the v2 client", state: "queued", at: sentAt },
				{ id: "m-2", text: "Skip OAuth", state: "delivered", at: sentAt / 1000, deliveredAt: sentAt + 2000 },
				{ id: "m-3", text: "Too late", state: "dropped", at: sentAt },
				{ id: "", text: "no id", state: "queued", at: sentAt },
				{ id: "m-4", text: "unknown state", state: "lost", at: sentAt },
				"not an entry",
			],
		})], turns).cards[0]!;
		expect(card.inbox).toEqual([
			{ id: "m-1", text: "Use the v2 client", state: "queued", at: at(30) },
			{ id: "m-2", text: "Skip OAuth", state: "delivered", at: at(30) },
			{ id: "m-3", text: "Too late", state: "dropped", at: at(30) },
		]);
		// A delivery moves the card on even when the row's revision did not.
		const delivered = deriver.derive([parent({}, {
			subagentInbox: [{ id: "m-1", text: "Use the v2 client", state: "delivered", at: sentAt }],
		})], turns).cards[0]!;
		expect(delivered).not.toBe(card);
		expect(delivered.inbox.map((message) => message.state)).toEqual(["delivered"]);
		expect(derive([parent()])[0]!.inbox).toEqual([]);
	});

	it("lists the person's own steers into the running turn as delivered", () => {
		const steer = (id: string, detail: Record<string, unknown>, turnId = "turn-1") => activity({
			id,
			turnId,
			activityKind: "system",
			status: "completed",
			summary: "Steer",
			detail: { event: "steer", text: `guidance ${id}`, ...detail } as ConversationActivity["detail"],
		});
		const own = steer("s1", { clientMessageId: "client-1" });
		const main = mainOf([
			own,
			steer("s2", {}),
			steer("s3", { senderSessionId: "ao-7" }),
			steer("s4", { origin: "automation" }),
			steer("s5", {}, "turn-0"),
		]);
		expect(main.sent).toEqual([
			{ id: "client-1", text: "guidance s1", state: "delivered", at: own.createdAt },
			expect.objectContaining({ id: "s2", text: "guidance s2" }),
		]);
	});

	it("shows a send once the conversation echoes it, matched by id", () => {
		const echoed = [sent("m-1", 5, "delivered")];
		expect(withPendingMessages(echoed, [])).toBe(echoed);
		expect(withPendingMessages(echoed, [sent("m-1", 5)])).toBe(echoed);
		expect(withPendingMessages(echoed, [sent("m-1", 5), sent("local:2", 6)]).map((message) => message.id)).toEqual(["m-1", "local:2"]);
	});

	it("places sent messages after the lines whose block started before them", () => {
		const notes = [
			{ kind: "status" as const, text: "first block", at: at(1) },
			{ kind: "decision" as const, text: "first block too", at: at(1) },
			{ kind: "status" as const, text: "second block", at: at(9) },
		];
		const entries = boardTimeline(notes, [sent("late", 12), sent("early", 4), sent("same", 9)]);
		expect(entries.map((entry) => entry.kind === "note" ? entry.note.text : entry.message.id)).toEqual([
			"first block",
			"first block too",
			"early",
			"second block",
			"same",
			"late",
		]);
		expect(boardTimeline([], [sent("only", 3)]).map((entry) => entry.kind)).toEqual(["sent"]);
	});

	it("settles a subagent the person stopped as stopped, leaving the rest alone", () => {
		const live = { id: "a", providerItemId: "toolu-a", lane: "working", outcome: "working" } as SubagentCard;
		const finished = { id: "b", providerItemId: "toolu-b", lane: "done", outcome: "completed" } as SubagentCard;
		const failed = { id: "c", providerItemId: "toolu-c", lane: "done", outcome: "failed" } as SubagentCard;
		const cards = [live, finished, failed];
		expect(withStopRequests(cards, new Set())).toBe(cards);
		expect(withStopRequests(cards, new Set(["toolu-a"]))).toBe(cards);
		const result = withStopRequests(cards, new Set(["toolu-a", "toolu-b"]));
		expect(result.map((card) => card.outcome)).toEqual(["working", "stopped", "failed"]);
		expect(result[0]).toBe(live);
		expect(result[2]).toBe(failed);
	});
});

describe("subagentAskPrefix", () => {
	it("names the subagent for the main agent", () => {
		expect(subagentAskPrefix("Audit the auth flow")).toBe('About the subagent "Audit the auth flow": ');
	});
});
