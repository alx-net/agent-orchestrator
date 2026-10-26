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
	firstSentence,
	parseOutcomeLine,
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

	it("is idle before any turn, settles on the turn's outcome and falls back to the previous turn's lines", () => {
		expect(mainOf([], [])).toMatchObject({ state: "idle", turnId: undefined, nowLine: undefined, result: undefined });

		const earlier: ConversationTurn = { id: "turn-0", state: "completed", requestedAt: "2026-10-26T09:00:00.000Z" };
		const items = [
			said("m0", "Decision: Add a migration, since the column is new\nStatus: Wrote the migration\n", { turnId: "turn-0" }),
			said("m1", "Here is the answer."),
		];
		const settled = mainOf(items, [earlier, turn("completed")]);
		expect(settled).toMatchObject({ state: "completed", turnId: "turn-1", startedAt: undefined, nowLine: "Here is the answer.", nowSource: "outcome" });
		expect(settled.notes.map((note) => [note.kind, note.text, note.at])).toEqual([
			["decision", "Add a migration, since the column is new", items[0]!.createdAt],
			["status", "Wrote the migration", items[0]!.createdAt],
			["outcome", "Here is the answer.", items[1]!.createdAt],
		]);
		expect(settled.decisions).toHaveLength(1);

		// A turn with lines of its own shows only those, then its outcome.
		const own = mainOf([...items, said("m2", "Status: Answering\n")], [earlier, turn("completed")]);
		expect(own.notes.map((note) => note.kind === "outcome" ? note.outcome : note.text)).toEqual(["Answering", "done"]);

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

describe("outcomes", () => {
	const settledTurn = (state: TurnState, extra: Partial<ConversationTurn> = {}): ConversationTurn => ({
		...turn(state),
		startedAt: "2026-10-26T10:00:00.000Z",
		completedAt: "2026-10-26T10:05:00.000Z",
		...extra,
	});

	it("reads an Outcome: line with any separator, case-insensitively", () => {
		expect(parseOutcomeLine("Outcome: done — Shipped the pause route")).toEqual({ kind: "done", text: "Shipped the pause route" });
		expect(parseOutcomeLine("outcome: BLOCKED - on the Hetzner 2FA prompt")).toEqual({ kind: "blocked", text: "on the Hetzner 2FA prompt" });
		expect(parseOutcomeLine("Outcome: failed: the migration does not apply")).toEqual({ kind: "failed", text: "the migration does not apply" });
		expect(parseOutcomeLine("Outcome: needs input — Which region should it deploy to?")).toEqual({ kind: "needs_input", text: "Which region should it deploy to?" });
		expect(parseOutcomeLine("Outcome: Needs-Input: Which branch?")).toEqual({ kind: "needs_input", text: "Which branch?" });
		expect(parseOutcomeLine("- **Outcome:** done – all green")).toEqual({ kind: "done", text: "all green" });
		expect(parseOutcomeLine("Outcome: done")).toEqual({ kind: "done", text: undefined });
		expect(parseOutcomeLine("Outcome: maybe — later")).toBeUndefined();
		expect(parseOutcomeLine("The outcome: done")).toBeUndefined();
	});

	it("takes a settled turn's explicit Outcome: line over every fallback", () => {
		const items = [
			said("m1", "Status: Checking the deploy\nBLOCKED on the old prompt"),
			said("m2", "All set.\nOutcome: done — Deployed to staging; smoke tests pass\nThanks!"),
		];
		const main = mainOf(items, [settledTurn("failed", { errorMessage: "boom" })]);
		expect(main).toMatchObject({
			state: "completed",
			nowLine: "Deployed to staging; smoke tests pass",
			nowSource: "outcome",
			result: { kind: "done", text: "Deployed to staging; smoke tests pass", at: "2026-10-26T10:05:00.000Z", source: "outcome_line" },
		});
		expect(main.notes.at(-1)).toEqual({ kind: "outcome", outcome: "done", text: "Deployed to staging; smoke tests pass", at: "2026-10-26T10:05:00.000Z" });

		expect(mainOf([said("m1", "Outcome: needs input — Which region?")], [settledTurn("completed")])).toMatchObject({
			state: "needs_you",
			nowLine: "Which region?",
			result: { kind: "needs_input" },
		});
		expect(mainOf([said("m1", "Outcome: failed — the API key is revoked")], [settledTurn("completed")])).toMatchObject({
			state: "failed",
			result: { kind: "failed", text: "the API key is revoked" },
		});
	});

	it("falls back to a BLOCKED line in the final message, up to its first semicolon", () => {
		const final = said("m1", [
			"I checked every provider.",
			"BLOCKED on you turning on 2FA in Verda, Resend and Hetzner; recommendation: use the shared authenticator; default if no answer: skip Verda",
		].join("\n"));
		expect(mainOf([final], [settledTurn("completed")])).toMatchObject({
			state: "needs_you",
			nowLine: "on you turning on 2FA in Verda, Resend and Hetzner",
			nowSource: "outcome",
			blockedOn: undefined,
			result: { kind: "blocked", source: "blocked_line" },
		});
		// Only the final message counts, and only an upper-case BLOCKED.
		const earlier = mainOf([said("m0", "BLOCKED on the token"), said("m1", "Got the token, all done.")], [settledTurn("completed")]);
		expect(earlier).toMatchObject({ state: "completed", nowLine: "Got the token, all done." });
		expect(mainOf([said("m1", "Blocked requests are retried.")], [settledTurn("completed")]).state).toBe("completed");
	});

	it("falls back to how the turn ended", () => {
		const items = [said("m1", "## Summary\n\nFixed the **login** redirect. Also cleaned up the tests.\n\nStatus: Wrapping up")];
		expect(mainOf(items, [settledTurn("completed")])).toMatchObject({ state: "completed", nowLine: "Fixed the login redirect.", result: { kind: "done", source: "settled" } });
		expect(mainOf(items, [settledTurn("recovered")]).state).toBe("completed");
		expect(mainOf(items, [settledTurn("failed", { errorMessage: "Usage limit reached\nretry at 5pm" })])).toMatchObject({
			state: "failed",
			nowLine: "Usage limit reached",
			result: { kind: "failed", text: "Usage limit reached" },
		});
		expect(mainOf(items, [settledTurn("interrupted")])).toMatchObject({ state: "stopped", nowLine: undefined, result: { kind: "stopped" } });
		expect(mainOf(items, [settledTurn("cancelled")]).state).toBe("stopped");
		expect(mainOf([], [settledTurn("completed")])).toMatchObject({ state: "completed", nowLine: undefined, result: { kind: "done", text: undefined } });
	});

	it("clips the first sentence and skips fences, tables and note lines", () => {
		expect(firstSentence("```ts\nconst a = 1;\n```\n| a | b |\nStatus: x\nThe build is green now! Next up: docs.")).toBe("The build is green now!");
		expect(firstSentence("# Only a heading")).toBe("Only a heading");
		const long = firstSentence(`${"word ".repeat(60)}end`)!;
		expect(long.length).toBe(140);
		expect(long.endsWith("…")).toBe(true);
	});

	it("is working again once a new turn runs, and a pending request still outranks the outcome", () => {
		const deriver = createSubagentBoardDeriver();
		const blocked = said("m1", "BLOCKED on the deploy key");
		const first = settledTurn("completed");
		expect(deriver.derive([blocked], [first]).main.state).toBe("needs_you");
		const next: ConversationTurn = { id: "turn-2", state: "running", requestedAt: "2026-10-26T11:00:00.000Z", startedAt: "2026-10-26T11:00:00.000Z" };
		const running = deriver.derive([blocked], [first, next]).main;
		expect(running).toMatchObject({ state: "working", turnId: "turn-2", result: undefined });
		expect(running.notes.some((note) => note.kind === "outcome")).toBe(false);

		const question = activity({ id: "question", providerItemId: "question", activityKind: "user_input", status: "pending", summary: "Which branch?" });
		expect(mainOf([said("m1", "Outcome: done — shipped"), question], [first])).toMatchObject({
			state: "needs_you",
			blockedOn: "Which branch?",
			nowSource: undefined,
			result: { kind: "done" },
		});
	});

	it("keeps an unchanged settled summary's identity", () => {
		const deriver = createSubagentBoardDeriver();
		const items = [said("m1", "Outcome: done — shipped")];
		const turns = [settledTurn("completed")];
		const before = deriver.derive(items, turns).main;
		expect(deriver.derive([...items], [...turns]).main).toBe(before);
	});

	it("reads a finished subagent's report for an Outcome: or BLOCKED line, keeping it in Done", () => {
		const report = (text: string) => ({ content: [{ type: "content", content: { type: "text", text } }] });
		const blocked = derive([parent({ status: "completed" }, report("Looked everywhere.\nOutcome: blocked — the staging DB needs a VPN"))])[0]!;
		expect(blocked).toMatchObject({ lane: "done", outcome: "blocked", nowLine: "the staging DB needs a VPN", result: { kind: "blocked", source: "outcome_line" } });
		expect(blocked.notes.at(-1)).toMatchObject({ kind: "outcome", outcome: "blocked", text: "the staging DB needs a VPN" });

		const asks = derive([parent({ status: "completed" }, report("Outcome: needs input — keep v1?"))])[0]!;
		expect(asks).toMatchObject({ lane: "done", outcome: "blocked", result: { kind: "needs_input" } });

		const convention = derive([parent({ status: "completed" }, report("BLOCKED on a missing fixture; default: skip it"))])[0]!;
		expect(convention).toMatchObject({ outcome: "blocked", nowLine: "on a missing fixture", result: { source: "blocked_line" } });

		const failed = derive([parent({ status: "completed" }, report("Outcome: failed — the suite does not compile"))])[0]!;
		expect(failed).toMatchObject({ lane: "done", outcome: "failed", nowLine: "the suite does not compile" });

		const done = derive([parent({ status: "completed" }, report("Outcome: done — 3 risks found"))])[0]!;
		expect(done).toMatchObject({ outcome: "completed", nowLine: "3 risks found", result: { kind: "done", source: "outcome_line" } });

		// Without one, how its call ended.
		const plain = derive([parent({ status: "completed" }, report("## Findings\nThe refresh token never rotates."))])[0]!;
		expect(plain).toMatchObject({ outcome: "completed", nowLine: "Findings", result: { kind: "done", text: "Findings", source: "settled" } });
		expect(derive([parent({ status: "cancelled" })])[0]!.result).toMatchObject({ kind: "stopped", text: undefined });
		expect(derive([parent({ status: "failed" }, { output: "Tool crashed" })])[0]!.result).toMatchObject({ kind: "failed", text: "Tool crashed" });
		// A live subagent has none.
		expect(derive([parent()])[0]!.result).toBeUndefined();
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
		const live = { id: "a", providerItemId: "toolu-a", lane: "working", outcome: "working", notes: [] } as unknown as SubagentCard;
		const done = { kind: "done", text: "All clear", at: "2026-10-26T10:00:05.000Z", source: "settled" } as const;
		const finished = {
			id: "b", providerItemId: "toolu-b", lane: "done", outcome: "completed", result: done,
			notes: [{ kind: "status", text: "Reading", at: "2026-10-26T10:00:01.000Z" }, { kind: "outcome", outcome: "done", text: "All clear", at: done.at }],
		} as unknown as SubagentCard;
		const failed = { id: "c", providerItemId: "toolu-c", lane: "done", outcome: "failed", notes: [] } as unknown as SubagentCard;
		const cards = [live, finished, failed];
		expect(withStopRequests(cards, new Set())).toBe(cards);
		expect(withStopRequests(cards, new Set(["toolu-a"]))).toBe(cards);
		const result = withStopRequests(cards, new Set(["toolu-a", "toolu-b"]));
		expect(result.map((card) => card.outcome)).toEqual(["working", "stopped", "failed"]);
		// Its outcome entry says it was stopped, not what its call reported.
		expect(result[1]!.result).toEqual({ kind: "stopped", at: done.at, source: "settled" });
		expect(result[1]!.notes.map((note) => note.outcome ?? note.text)).toEqual(["Reading", "stopped"]);
		expect(result[0]).toBe(live);
		expect(result[2]).toBe(failed);
	});
});

describe("subagentAskPrefix", () => {
	it("names the subagent for the main agent", () => {
		expect(subagentAskPrefix("Audit the auth flow")).toBe('About the subagent "Audit the auth flow": ');
	});
});
