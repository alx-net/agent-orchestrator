/**
 * The agents board's view model: one card per Claude Code subagent (an Agent
 * or Task tool call) in a chat, plus a summary of the main agent itself, derived
 * from the conversation's raw items.
 *
 * Built from `snapshot.items`, never from the timeline's readable subset: the
 * Agent/Task row arrives as a `reasoning` activity (the ACP bridge files the
 * tool under "think"), and the timeline drops reasoning, so a board fed from
 * the readable items would never see a parent.
 *
 * Everything here is optional on the wire. The newer progress fields
 * (`progressSummary`, `progressAt`, `lastToolName`, `progressToolUses`,
 * `subagentType`) refine a card when present and are never required.
 *
 * Runs on every streaming tick, so it is incremental: a narration row is parsed
 * once per appended chunk rather than from the top, and a card whose inputs did
 * not change keeps its identity so memoized card views skip the render.
 */

import { useMemo, useRef } from "react";
import {
	activityPlan,
	type ConversationActivity,
	type ConversationItem,
	type ConversationMessage,
	type ConversationTurn,
} from "../types/conversation";

export type SubagentLane = "working" | "needs_you" | "done";
/** `blocked`: it finished, and its report says it is blocked or needs an answer. */
export type SubagentOutcome = "working" | "needs_you" | "completed" | "blocked" | "failed" | "stopped";

/** How an agent's turn, or a subagent's call, ended. */
export type AgentOutcomeKind = "done" | "blocked" | "needs_input" | "failed" | "stopped";

/**
 * How an agent finished, read in this order: an `Outcome:` line it wrote, a
 * line that starts with `BLOCKED`, then how its turn or call ended.
 */
export interface AgentOutcome {
	kind: AgentOutcomeKind;
	/** What it finished with, is blocked on, or failed with. */
	text?: string;
	at: string;
	source: "outcome_line" | "blocked_line" | "settled";
}

/**
 * A `Status:` or `Decision:` line the agent wrote, or how it finished. An
 * outcome is always the newest entry.
 */
export interface SubagentNote {
	kind: "status" | "decision" | "outcome";
	text: string;
	/** When the narration block carrying the line started. Lines in one block share it. */
	at: string;
	/** Set on an outcome entry. */
	outcome?: AgentOutcomeKind;
}

export interface SubagentCard {
	/** The Agent/Task activity's id. */
	id: string;
	providerItemId: string;
	turnId?: string;
	/** Absent when the provider gave the call no description; the view names it. */
	title?: string;
	subagentType?: string;
	lane: SubagentLane;
	outcome: SubagentOutcome;
	startedAt: string;
	/** The latest moment the subagent was observed doing something. */
	updatedAt: string;
	/** Settled cards only: start to last observed activity. A live card counts from startedAt. */
	durationMs?: number;
	/** The single derived "what it is doing now" line. */
	nowLine?: string;
	nowSource?: "status" | "progress" | "tool" | "report";
	/** What a subagent that needs you is waiting on. */
	blockedOn?: string;
	/** How a settled subagent finished; absent while it runs. */
	result?: AgentOutcome;
	/** Status and decision lines, in the order they were written, then its outcome. */
	notes: SubagentNote[];
	decisions: SubagentNote[];
	toolCount: number;
	lastToolName?: string;
	/** The task the main agent gave it. */
	prompt?: string;
	/** The subagent's final report when it completed, or its error when it failed. */
	finalReport?: string;
	/** What the person sent it from the board, as its inbox reports their delivery. */
	inbox: SentMessage[];
}

/**
 * Where a message sent from the board stands. A subagent reads its inbox at its
 * next tool call, so a message waits until then and is dropped if it finishes
 * first; a steer reaches the main turn once the daemon accepts it.
 */
export type SentMessageState = "sending" | "queued" | "delivered" | "dropped";

/** A message the person sent from the board, shown among the card's updates. */
export interface SentMessage {
	/** The inbox entry's id for a subagent, the steer's client message id for the main agent. */
	id: string;
	text: string;
	state: SentMessageState;
	at: string;
}

/** One row of a card's updates: a line the agent wrote, or a message the person sent. */
export type BoardTimelineEntry =
	| { kind: "note"; note: SubagentNote }
	| { kind: "sent"; message: SentMessage };

export interface SubagentLanes {
	working: SubagentCard[];
	needs_you: SubagentCard[];
	done: SubagentCard[];
}

/**
 * `working` while a turn runs; once it settles, its outcome: `needs_you` when it
 * is blocked or asks a question, else `completed`, `failed` or `stopped`. `idle`
 * before any turn ran.
 */
export type MainAgentState = "working" | "needs_you" | "idle" | "completed" | "failed" | "stopped";

/** The main agent's own card: a now line and timeline read from its assistant messages. */
export interface MainAgentSummary {
	state: MainAgentState;
	/** The running turn, else the latest one that ran. */
	turnId?: string;
	/** When the running turn started; absent while no turn runs. */
	startedAt?: string;
	nowLine?: string;
	/** `outcome`: the now line is the settled turn's outcome text. */
	nowSource?: "status" | "plan" | "tool" | "outcome";
	/** What the main agent is waiting on when a pending request needs you. */
	blockedOn?: string;
	/** How the latest turn ended; absent while one runs or before any ran. */
	result?: AgentOutcome;
	/**
	 * Status and decision lines from the turn's assistant messages, in order, then
	 * the turn's outcome. A turn that wrote none shows the newest earlier turn's lines.
	 */
	notes: SubagentNote[];
	decisions: SubagentNote[];
	/** What the person steered the running turn with. */
	sent: SentMessage[];
}

export interface SubagentBoardModel {
	cards: SubagentCard[];
	main: MainAgentSummary;
}

const SUBAGENT_TOOLS = new Set(["Agent", "Task"]);

/**
 * The main agent's Agent/Task call that started a subagent. Filed as `mcp_tool`
 * by current daemons and as `reasoning` by older ones, so the kind is not read.
 */
export function isSubagentParent(item: ConversationItem): item is ConversationActivity {
	if (item.kind !== "activity" || !item.providerItemId) return false;
	const detail = item.detail;
	return Boolean(detail?.providerToolName && SUBAGENT_TOOLS.has(detail.providerToolName) && !detail.parentProviderItemId);
}

/**
 * The subagent's own prose ("Subagent response"). It is marked nested like its
 * parent but names no tool and points at the Agent call it belongs to. Its
 * thinking is marked the same way, and is not narration.
 */
function isNarration(activity: ConversationActivity): boolean {
	const detail = activity.detail;
	return activity.activityKind !== "reasoning" && detail?.nestedAgent === true &&
		Boolean(detail.parentProviderItemId) && !detail.providerToolName;
}

function narrationText(activity: ConversationActivity): string {
	const detail = activity.detail as Record<string, unknown> | undefined;
	for (const key of ["progress", "text", "streamedText"]) {
		const value = detail?.[key];
		if (typeof value === "string") return value;
	}
	return "";
}

type ParsedLine = { kind: "status" | "decision"; text: string };

// Line-start label, case-insensitive. Tolerates the markdown the model tends to
// wrap it in ("- **Status:** …", "**Decision**: …").
const NOTE_LINE = /^(?:[-*+•]\s+|>\s*)?(?:\*\*|__)?(status|decision)(?:\*\*|__)?\s*:\s*(?:\*\*|__)?\s*(.*?)\s*(?:\*\*|__)?\s*$/i;

/** One narration line as a status or decision note, or nothing. */
export function parseSubagentNoteLine(line: string): ParsedLine | undefined {
	const match = NOTE_LINE.exec(line.trim());
	if (!match) return undefined;
	const text = match[2]?.trim();
	if (!text) return undefined;
	return { kind: match[1]!.toLowerCase() === "decision" ? "decision" : "status", text };
}

// `Outcome: done — <summary>`, `Outcome: blocked - <on what>`, `Outcome: failed: <why>`,
// `Outcome: needs input — <question>`: case-insensitive, with the same markdown tolerance.
const OUTCOME_LINE = /^(?:[-*+•]\s+|>\s*)?(?:\*\*|__)?outcome(?:\*\*|__)?\s*:\s*(?:\*\*|__)?\s*(needs[\s_-]*input|done|blocked|failed)\b(?:\*\*|__)?\s*(?:[—–:-]+\s*)?(.*?)\s*(?:\*\*|__)?\s*$/i;
// The person's own convention: "BLOCKED on <what>; recommendation: …; default if no answer: …".
// Upper case only, so prose that starts with "Blocked" is not read as one.
const BLOCKED_LINE = /^(?:[-*+•]\s+|>\s*)?(?:\*\*|__)?BLOCKED\b(?:\*\*|__)?\s*[:—–-]?\s*(.*)$/;
const OUTCOME_TEXT_MAX = 140;

/** An `Outcome:` line as its kind and text, or nothing. */
export function parseOutcomeLine(line: string): { kind: Exclude<AgentOutcomeKind, "stopped">; text?: string } | undefined {
	const match = OUTCOME_LINE.exec(line.trim());
	if (!match) return undefined;
	const word = match[1]!.toLowerCase();
	return { kind: word.startsWith("needs") ? "needs_input" : (word as "done" | "blocked" | "failed"), text: clip(match[2]) };
}

/** What a `BLOCKED` line is blocked on, up to its first `;`: empty when it names nothing, absent when it is no such line. */
function parseBlockedLine(line: string): string | undefined {
	const match = BLOCKED_LINE.exec(line.trim());
	if (!match) return undefined;
	return clip(match[1]!.split(";")[0]!.replace(/\*\*|__/g, "")) ?? "";
}

function clip(value: string | undefined): string | undefined {
	const trimmed = value?.replace(/\s+/g, " ").trim();
	if (!trimmed) return undefined;
	return trimmed.length > OUTCOME_TEXT_MAX ? `${trimmed.slice(0, OUTCOME_TEXT_MAX - 1).trimEnd()}…` : trimmed;
}

/** The newest `Outcome:` line in the texts, read from the last line of the last text back. */
function explicitOutcome(texts: readonly string[]): ReturnType<typeof parseOutcomeLine> {
	for (let index = texts.length - 1; index >= 0; index -= 1) {
		const lines = texts[index]!.split("\n");
		for (let line = lines.length - 1; line >= 0; line -= 1) {
			const parsed = parseOutcomeLine(lines[line]!);
			if (parsed) return parsed;
		}
	}
	return undefined;
}

/** The last `BLOCKED` line of a text. */
function blockedOutcome(value: string | undefined): string | undefined {
	if (!value) return undefined;
	const lines = value.split("\n");
	for (let index = lines.length - 1; index >= 0; index -= 1) {
		const blocked = parseBlockedLine(lines[index]!);
		if (blocked !== undefined) return blocked;
	}
	return undefined;
}

function plainMarkdown(line: string): string {
	return line
		.replace(/^\s*(?:#{1,6}\s+|[-*+•]\s+|\d+[.)]\s+|>\s*)/, "")
		.replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
		.replace(/\*\*|__|`/g, "")
		.trim();
}

/** The first sentence of a message's prose, past code fences, tables, rules and note lines; a heading only when nothing follows it. */
export function firstSentence(value: string | undefined): string | undefined {
	if (!value) return undefined;
	let heading: string | undefined;
	let fenced = false;
	for (const raw of value.split("\n")) {
		const line = raw.trim();
		if (line.startsWith("```")) {
			fenced = !fenced;
			continue;
		}
		if (fenced || !line || line.startsWith("|") || /^(?:-{3,}|\*{3,}|_{3,})$/.test(line) || parseSubagentNoteLine(line)) continue;
		const plain = plainMarkdown(line);
		if (!plain) continue;
		if (/^#{1,6}\s/.test(line)) {
			heading ??= plain;
			continue;
		}
		return clip(/^(.+?[.!?])(?=\s|$)/.exec(plain)?.[1] ?? plain);
	}
	return clip(heading);
}

/**
 * How a settled turn ended: an `Outcome:` line in its messages, else a `BLOCKED`
 * line in its final message, else its state, with the final message's first
 * sentence when it completed.
 */
function turnOutcome(turn: ConversationTurn, said: readonly ConversationMessage[]): AgentOutcome {
	const final = said.at(-1);
	const at = turn.completedAt ?? final?.createdAt ?? turn.startedAt ?? turn.requestedAt;
	const explicit = explicitOutcome(said.map((message) => message.text));
	if (explicit) return { ...explicit, at, source: "outcome_line" };
	const blocked = blockedOutcome(final?.text);
	if (blocked !== undefined) return { kind: "blocked", text: blocked || undefined, at, source: "blocked_line" };
	if (turn.state === "failed") return { kind: "failed", text: clip(firstLine(turn.errorMessage)), at, source: "settled" };
	if (turn.state === "interrupted" || turn.state === "cancelled") return { kind: "stopped", at, source: "settled" };
	return { kind: "done", text: firstSentence(final?.text), at, source: "settled" };
}

/** A finished subagent's report, read for an `Outcome:` line, then a `BLOCKED` line. */
function reportOutcome(report: string | undefined): Pick<AgentOutcome, "kind" | "text" | "source"> | undefined {
	if (!report) return undefined;
	const explicit = explicitOutcome([report]);
	if (explicit) return { ...explicit, source: "outcome_line" };
	const blocked = blockedOutcome(report);
	return blocked === undefined ? undefined : { kind: "blocked", text: blocked || undefined, source: "blocked_line" };
}

const MAIN_STATE_BY_OUTCOME = {
	done: "completed",
	blocked: "needs_you",
	needs_input: "needs_you",
	failed: "failed",
	stopped: "stopped",
} as const satisfies Record<AgentOutcomeKind, MainAgentState>;

function outcomeNote(outcome: AgentOutcome): SubagentNote {
	return { kind: "outcome", outcome: outcome.kind, text: outcome.text ?? "", at: outcome.at };
}

interface NarrationParse {
	text: string;
	/** Index just past the last newline already parsed. */
	completeEnd: number;
	complete: ParsedLine[];
	/** The trailing line, which may still be streaming. */
	tail?: ParsedLine;
}

function parseNarration(previous: NarrationParse | undefined, text: string): NarrationParse {
	if (previous?.text === text) return previous;
	// Streaming only appends; a settle can rewrite the block, which reparses it.
	const appended = previous !== undefined && text.length > previous.text.length && text.startsWith(previous.text);
	let start = appended ? previous.completeEnd : 0;
	let complete = appended ? previous.complete : [];
	let added: ParsedLine[] | undefined;
	let newline = text.indexOf("\n", start);
	while (newline !== -1) {
		const note = parseSubagentNoteLine(text.slice(start, newline));
		if (note) (added ??= []).push(note);
		start = newline + 1;
		newline = text.indexOf("\n", start);
	}
	if (added) complete = [...complete, ...added];
	return { text, completeEnd: start, complete, tail: parseSubagentNoteLine(text.slice(start)) };
}

function text(value: unknown): string | undefined {
	return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function record(value: unknown): Record<string, unknown> | undefined {
	return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
}

function timeOf(iso: string | undefined): number {
	const parsed = iso ? Date.parse(iso) : Number.NaN;
	return Number.isFinite(parsed) ? parsed : 0;
}

/** Unix milliseconds, tolerating a provider that reports seconds. */
function unixMs(value: unknown): number | undefined {
	if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) return undefined;
	return value < 1e12 ? value * 1000 : value;
}

/** Text from ACP tool content: `[{type:"content",content:{type:"text",text}}]` or bare text blocks. */
function contentText(value: unknown): string | undefined {
	if (!Array.isArray(value)) return undefined;
	const parts: string[] = [];
	for (const entry of value) {
		if (typeof entry === "string") {
			parts.push(entry);
			continue;
		}
		const block = record(entry);
		const inner = record(block?.content) ?? block;
		if (inner?.type === "text" && typeof inner.text === "string") parts.push(inner.text);
	}
	const joined = parts.join("\n\n").trim();
	return joined || undefined;
}

// The raw Agent tool result ends with a model-directed trailer the bridge strips
// from structured content but not from the raw output fallback.
function withoutAgentTrailer(value: string): string {
	return value
		.replace(/\s*<usage>[\s\S]*?<\/usage>\s*$/, "")
		.replace(/\n+agentId:[^\n]*\s*$/, "")
		.trim();
}

function reportOf(activity: ConversationActivity, prompt: string | undefined): string | undefined {
	const detail = activity.detail;
	const same = (candidate: string) => prompt !== undefined && candidate.trim() === prompt.trim();
	const fromContent = contentText(detail?.content);
	if (fromContent && !same(fromContent)) return withoutAgentTrailer(fromContent) || undefined;
	const output = text(detail?.output);
	if (!output) return undefined;
	let parsed: unknown;
	if (output.startsWith("[")) {
		try {
			parsed = JSON.parse(output);
		} catch {
			parsed = undefined;
		}
	}
	const fromOutput = parsed === undefined ? output : contentText(parsed);
	return fromOutput && !same(fromOutput) ? withoutAgentTrailer(fromOutput) || undefined : undefined;
}

function firstLine(value: string | undefined): string | undefined {
	if (!value) return undefined;
	for (const line of value.split("\n")) {
		const cleaned = line.replace(/^\s*(?:#{1,6}\s+|[-*+]\s+|>\s*)/, "").replace(/\*\*|__/g, "").trim();
		if (cleaned) return cleaned;
	}
	return undefined;
}

function toolSignature(activity: ConversationActivity): string | undefined {
	const input = activity.detail?.input;
	if (input === undefined) return undefined;
	try {
		return `${activity.detail?.providerToolName ?? ""}\u0000${JSON.stringify(input)}`;
	} catch {
		return undefined;
	}
}

function isPendingInteraction(activity: ConversationActivity): boolean {
	return (activity.activityKind === "approval" || activity.activityKind === "user_input") && activity.status === "pending";
}

function interactionLabel(activity: ConversationActivity): string | undefined {
	return text(activity.summary) ?? text(activity.detail?.message);
}

/** Guidance a person typed into the running turn, not a steer from another AO session. */
function isHumanSteer(activity: ConversationActivity): boolean {
	const detail = activity.detail;
	return detail?.event === "steer" && !detail.senderSessionId && (detail.origin ?? "human") === "human";
}

function steerMessage(activity: ConversationActivity): SentMessage {
	return {
		id: activity.detail?.clientMessageId ?? activity.id,
		text: activity.detail?.text ?? activity.summary,
		state: "delivered",
		at: activity.createdAt,
	};
}

const INBOX_STATES: ReadonlySet<unknown> = new Set(["queued", "delivered", "dropped"]);

/** The subagent's inbox from its Agent/Task row, skipping any entry it cannot show. */
function inboxOf(value: unknown, fallbackAt: string): SentMessage[] {
	if (!Array.isArray(value)) return [];
	const messages: SentMessage[] = [];
	for (const entry of value) {
		const row = record(entry);
		if (!row || typeof row.id !== "string" || !row.id || typeof row.text !== "string" || !INBOX_STATES.has(row.state)) continue;
		const ms = unixMs(row.at);
		messages.push({
			id: row.id,
			text: row.text,
			state: row.state as SentMessageState,
			at: ms === undefined ? fallbackAt : new Date(ms).toISOString(),
		});
	}
	return messages;
}

/** Changes whenever an inbox entry arrives or moves on, for the card cache. */
function inboxSignature(value: unknown): string {
	if (!Array.isArray(value)) return "";
	return value.map((entry) => {
		const row = record(entry);
		return `${String(row?.id)}:${String(row?.state)}`;
	}).join(",");
}

const LIVE: ReadonlySet<string> = new Set(["running", "pending"]);
// What Claude Code's Agent tool answers for a run_in_background launch.
const LAUNCH_NOTICE = /^\s*async agent launched/i;
const TURN_LIVE: ReadonlySet<string> = new Set(["running", "queued"]);
// The main agent's own tool calls, for its now line.
const TOOL_KINDS: ReadonlySet<string> = new Set(["command", "file_change", "mcp_tool"]);

export interface SubagentBoardDeriver {
	derive(items: readonly ConversationItem[], turns: readonly ConversationTurn[]): SubagentBoardModel;
}

/**
 * The turn the main agent's card describes: the running one, else the latest
 * that ran. A queued turn has not started and a rolled-back one left history.
 */
function mainTurn(turns: readonly ConversationTurn[]): ConversationTurn | undefined {
	const running = turns.find((turn) => turn.state === "running");
	if (running) return running;
	let latest: ConversationTurn | undefined;
	let latestMs = Number.NEGATIVE_INFINITY;
	for (const turn of turns) {
		if (turn.state === "queued" || turn.rolledBack || (turn.state === "cancelled" && !turn.startedAt)) continue;
		const ms = timeOf(turn.startedAt ?? turn.requestedAt);
		if (ms >= latestMs) {
			latest = turn;
			latestMs = ms;
		}
	}
	return latest;
}

/** What one pass collects for the main agent's card. */
interface MainInputs {
	turn?: ConversationTurn;
	/** Assistant messages by turn; turns in the order they first spoke. */
	said: ReadonlyMap<string, ConversationMessage[]>;
	/** Top-level pending approvals and questions. */
	pending: readonly ConversationActivity[];
	/** Signatures of the pending requests a subagent raised. */
	claims: ReadonlySet<string>;
	/** The turn's newest running top-level tool call. */
	runningTool?: ConversationActivity;
	/** A running AskUserQuestion call, which waits on the person. */
	asking?: ConversationActivity;
	planRow?: ConversationActivity;
	/** The time of the turn's newest row. */
	newestMs: number;
	/** The person's own steers into the turn. */
	steers: readonly ConversationActivity[];
}

interface MessageParse {
	revision: number;
	length: number;
	parse: NarrationParse;
}

/**
 * A deriver keeps the parse and card caches for one surface. Entries for rows
 * that left the snapshot are dropped on the next pass.
 */
export function createSubagentBoardDeriver(): SubagentBoardDeriver {
	let narrations = new Map<string, NarrationParse>();
	let cards = new Map<string, { key: string; card: SubagentCard; claims: readonly string[] }>();
	let messages = new Map<string, MessageParse>();
	let main: { key: string; summary: MainAgentSummary } | undefined;
	// When the in-progress plan step was first seen. A plan update adds no row of
	// its own, so a step that changes while its turn is watched takes the time of
	// the turn's newest row; one already set when the turn arrives is as old as the plan.
	let planStep: { turnId: string; text: string; ms: number } | undefined;
	let watchedTurnId: string | undefined;
	// A settled turn's outcome, read again only when the turn or what it said changes.
	let settled: { key: string; outcome: AgentOutcome } | undefined;

	function outcomeOf(turn: ConversationTurn, said: readonly ConversationMessage[]): AgentOutcome {
		const key = [turn.id, turn.state, turn.completedAt, turn.errorMessage, ...said.map((message) => `${message.id}:${message.revision}:${message.text.length}`)].join("\u0000");
		if (settled?.key !== key) settled = { key, outcome: turnOutcome(turn, said) };
		return settled.outcome;
	}

	// Status and decision lines of one turn's messages, parsed once per streamed chunk.
	function notesOf(said: readonly ConversationMessage[] | undefined, nextMessages: Map<string, MessageParse>) {
		const notes: SubagentNote[] = [];
		for (const message of said ?? []) {
			const cached = messages.get(message.id);
			const entry = cached?.revision === message.revision && cached.length === message.text.length
				? cached
				: { revision: message.revision, length: message.text.length, parse: parseNarration(cached?.parse, message.text) };
			nextMessages.set(message.id, entry);
			const { complete, tail } = entry.parse;
			for (const line of tail ? [...complete, tail] : complete) notes.push({ kind: line.kind, text: line.text, at: message.createdAt });
		}
		// Equal keys mean equal notes, and every turn without notes has the same ones.
		const key = notes.length === 0
			? ""
			: (said ?? []).map((message) => `${message.id}:${message.revision}:${message.text.length}`).join(",");
		return { notes, key };
	}

	function summarize(inputs: MainInputs): MainAgentSummary {
		const { turn } = inputs;
		const running = turn?.state === "running";
		// Only a settled turn has an outcome; a new running turn is working again.
		const result = turn && !running ? outcomeOf(turn, inputs.said.get(turn.id) ?? []) : undefined;
		// A request that matches no subagent's call is the main agent's own.
		let blocked = false;
		let blockedOn: string | undefined;
		for (const request of inputs.pending) {
			const signature = toolSignature(request);
			if (signature && inputs.claims.has(signature)) continue;
			blocked = true;
			blockedOn ??= interactionLabel(request);
		}
		if (running && inputs.asking) {
			blocked = true;
			blockedOn ??= text(inputs.asking.summary);
		}

		const nextMessages = new Map<string, MessageParse>();
		const current = notesOf(turn ? inputs.said.get(turn.id) : undefined, nextMessages);
		let shown = current;
		if (shown.notes.length === 0) {
			const earlier = [...inputs.said.keys()];
			for (let index = earlier.length - 1; index >= 0; index -= 1) {
				if (earlier[index] === turn?.id) continue;
				const candidate = notesOf(inputs.said.get(earlier[index]!), nextMessages);
				if (candidate.notes.length === 0) continue;
				shown = candidate;
				break;
			}
		}
		messages = nextMessages;

		let nowLine: string | undefined;
		let nowSource: MainAgentSummary["nowSource"];
		if (running) {
			const plan = turn.plan?.steps.length ? turn.plan : inputs.planRow ? activityPlan(inputs.planRow) : undefined;
			const step = text(plan?.steps.find((entry) => entry.status === "in_progress")?.text);
			if (!step) planStep = undefined;
			else if (planStep?.turnId !== turn.id || planStep.text !== step) {
				const ms = watchedTurnId === turn.id
					? inputs.newestMs
					: timeOf(inputs.planRow?.createdAt ?? turn.startedAt ?? turn.requestedAt);
				planStep = { turnId: turn.id, text: step, ms };
			}
			// The most recent of its plan step, what it said, and what it is running.
			// A tie goes to the plan step: one stamped with the newest row changed after it.
			let bestMs = Number.NEGATIVE_INFINITY;
			const consider = (candidate: string | undefined, ms: number, source: NonNullable<MainAgentSummary["nowSource"]>) => {
				if (!candidate || ms <= bestMs) return;
				bestMs = ms;
				nowLine = candidate;
				nowSource = source;
			};
			if (planStep) consider(planStep.text, planStep.ms, "plan");
			const status = current.notes.findLast((note) => note.kind === "status");
			if (status) consider(status.text, timeOf(status.at), "status");
			if (inputs.runningTool) consider(text(inputs.runningTool.summary), timeOf(inputs.runningTool.createdAt), "tool");
		} else {
			planStep = undefined;
			const status = shown.notes.findLast((note) => note.kind === "status");
			// A pending request outranks how the turn ended: the card says what it waits for.
			if (result && !blocked) {
				nowLine = result.text;
				nowSource = "outcome";
			} else if (status) {
				nowLine = status.text;
				nowSource = "status";
			}
		}
		watchedTurnId = turn?.id;

		const state: MainAgentState = blocked ? "needs_you" : running ? "working" : result ? MAIN_STATE_BY_OUTCOME[result.kind] : "idle";
		const startedAt = running ? turn.startedAt ?? turn.requestedAt : undefined;
		if (!blocked) blockedOn = undefined;
		const steersKey = inputs.steers.map((steer) => `${steer.id}:${steer.revision}`).join(",");
		const resultKey = result ? [result.kind, result.text, result.at, result.source].join(":") : "";
		// Unchanged inputs keep the summary's identity, so the memoized card skips the render.
		const key = [state, turn?.id, startedAt, nowLine, nowSource, blockedOn, shown.key, steersKey, resultKey].join("\u0000");
		if (main?.key === key) return main.summary;
		const summary: MainAgentSummary = {
			state,
			turnId: turn?.id,
			startedAt,
			nowLine,
			nowSource,
			blockedOn,
			result,
			notes: result ? [...shown.notes, outcomeNote(result)] : shown.notes,
			decisions: shown.notes.filter((note) => note.kind === "decision"),
			sent: inputs.steers.map(steerMessage),
		};
		main = { key, summary };
		return summary;
	}

	return {
		derive(items, turns) {
			const turn = mainTurn(turns);
			const parents: ConversationActivity[] = [];
			const children = new Map<string, ConversationActivity[]>();
			const pending: ConversationActivity[] = [];
			const said = new Map<string, ConversationMessage[]>();
			let runningTool: ConversationActivity | undefined;
			let asking: ConversationActivity | undefined;
			let planRow: ConversationActivity | undefined;
			const steers: ConversationActivity[] = [];
			let newestMs = 0;
			for (const item of items) {
				const inTurn = turn !== undefined && item.turnId === turn.id;
				if (inTurn) newestMs = Math.max(newestMs, timeOf(item.createdAt));
				if (item.kind === "message") {
					if (item.role !== "assistant" || !item.turnId) continue;
					const list = said.get(item.turnId);
					if (list) list.push(item);
					else said.set(item.turnId, [item]);
					continue;
				}
				// Annotated so a false result does not narrow `item` away below.
				const parentOfSubagent: boolean = isSubagentParent(item);
				if (parentOfSubagent) parents.push(item);
				const parentId = item.detail?.parentProviderItemId;
				if (parentId) {
					const list = children.get(parentId);
					if (list) list.push(item);
					else children.set(parentId, [item]);
				} else if (isPendingInteraction(item)) {
					pending.push(item);
				} else if (inTurn && !parentOfSubagent) {
					if (item.activityKind === "plan") planRow = item;
					else if (isHumanSteer(item)) steers.push(item);
					else if (TOOL_KINDS.has(item.activityKind) && LIVE.has(item.status)) {
						runningTool = item;
						if (item.detail?.providerToolName === "AskUserQuestion") asking = item;
					}
				}
			}

			// Signatures of the pending requests a subagent raised; the rest are the main agent's.
			const claims = new Set<string>();
			const mainAgent = () => summarize({ turn, said, pending, claims, runningTool, asking, planRow, newestMs, steers });
			if (parents.length === 0) {
				narrations = new Map();
				cards = new Map();
				return { cards: [], main: mainAgent() };
			}

			const turnStates = new Map(turns.map((entry) => [entry.id, entry.state]));
			// A permission request does not name the subagent that raised it. It carries
			// the tool's input, which matches the subagent's pending call exactly.
			const pendingBySignature = new Map<string, string>();
			for (const request of pending) {
				const signature = toolSignature(request);
				if (signature) pendingBySignature.set(signature, interactionLabel(request) ?? "");
			}
			const pendingKey = pending.map((request) => `${request.id}:${request.revision}`).join(",");

			const nextNarrations = new Map<string, NarrationParse>();
			const nextCards = new Map<string, { key: string; card: SubagentCard; claims: readonly string[] }>();
			const result: SubagentCard[] = [];
			for (const parent of parents) {
				const kids = children.get(parent.providerItemId!) ?? [];
				const turnState = parent.turnId ? turnStates.get(parent.turnId) : undefined;
				let key = `${parent.revision}:${parent.status}|${turnState ?? ""}|${pendingKey}|${inboxSignature(parent.detail?.subagentInbox)}`;
				for (const kid of kids) key += `|${kid.id}:${kid.revision}:${kid.status}`;
				const cached = cards.get(parent.id);
				if (cached?.key === key) {
					for (const kid of kids) {
						const parse = narrations.get(kid.id);
						if (parse) nextNarrations.set(kid.id, parse);
					}
					nextCards.set(parent.id, cached);
					result.push(cached.card);
					for (const claim of cached.claims) claims.add(claim);
					continue;
				}
				const cardClaims: string[] = [];
				const card = buildCard(parent, kids, turnState, pendingBySignature, (kid) => {
					const parse = parseNarration(narrations.get(kid.id), narrationText(kid));
					nextNarrations.set(kid.id, parse);
					return parse;
				}, cardClaims);
				nextCards.set(parent.id, { key, card, claims: cardClaims });
				result.push(card);
				for (const claim of cardClaims) claims.add(claim);
			}
			narrations = nextNarrations;
			cards = nextCards;
			return { cards: result, main: mainAgent() };
		},
	};
}

function buildCard(
	parent: ConversationActivity,
	kids: readonly ConversationActivity[],
	turnState: string | undefined,
	pendingBySignature: ReadonlyMap<string, string>,
	parse: (narration: ConversationActivity) => NarrationParse,
	/** Receives the signature of each pending request this subagent raised. */
	claims: string[],
): SubagentCard {
	const detail = parent.detail ?? {};
	const input = record(detail.input);
	const prompt = text(input?.prompt);
	const startedMs = timeOf(parent.createdAt);
	let latestMs = startedMs;
	let toolCount = 0;
	let lastToolName: string | undefined;
	let runningTool: ConversationActivity | undefined;
	let liveChild = false;
	let blockedOn: string | undefined;
	let blocked = false;
	let latestStatus: { text: string; ms: number } | undefined;
	const notes: SubagentNote[] = [];

	for (const kid of kids) {
		// Raw thinking is not shown, and is not a tool call.
		if (kid.activityKind === "reasoning") continue;
		latestMs = Math.max(latestMs, timeOf(kid.createdAt));
		if (isNarration(kid)) {
			const parsed = parse(kid);
			const lines = parsed.tail ? [...parsed.complete, parsed.tail] : parsed.complete;
			for (const line of lines) {
				notes.push({ kind: line.kind, text: line.text, at: kid.createdAt });
				if (line.kind === "status") latestStatus = { text: line.text, ms: timeOf(kid.createdAt) };
			}
			continue;
		}
		if (kid.activityKind === "approval" || kid.activityKind === "user_input") {
			if (kid.status === "pending") {
				blocked = true;
				blockedOn ??= interactionLabel(kid);
			}
			continue;
		}
		if (kid.activityKind === "usage" || kid.activityKind === "system") continue;
		toolCount += 1;
		lastToolName = text(kid.detail?.providerToolName) ?? lastToolName;
		if (!LIVE.has(kid.status)) continue;
		liveChild = true;
		runningTool = kid;
		if (kid.detail?.providerToolName === "AskUserQuestion") {
			blocked = true;
			blockedOn ??= text(kid.summary);
		}
		const signature = toolSignature(kid);
		if (signature && pendingBySignature.has(signature)) {
			blocked = true;
			blockedOn ??= text(pendingBySignature.get(signature)) ?? text(kid.summary);
			claims.push(signature);
		}
	}

	const progressMs = unixMs(detail.progressAt);
	if (progressMs) latestMs = Math.max(latestMs, progressMs);
	const turnLive = turnState === undefined || TURN_LIVE.has(turnState);
	const parentLive = LIVE.has(parent.status);
	// The Agent call itself can be waiting on permission to start.
	const parentSignature = parentLive ? toolSignature(parent) : undefined;
	if (parentSignature && pendingBySignature.has(parentSignature)) {
		blocked = true;
		blockedOn ??= text(pendingBySignature.get(parentSignature));
		claims.push(parentSignature);
	}
	const parentSettledOk = parent.status === "completed" || parent.status === "resolved" || parent.status === "recovered";
	// A backgrounded subagent's call completes as soon as it launches; its work and
	// progress keep arriving afterwards, for as long as the main turn runs.
	const launchedOnly = LAUNCH_NOTICE.test(contentText(detail.content) ?? text(detail.output) ?? "");
	const background = input?.run_in_background === true || launchedOnly;
	let outcome: SubagentOutcome;
	if (turnLive && (parentLive || (parentSettledOk && (liveChild || background)))) outcome = blocked ? "needs_you" : "working";
	else if (parentLive) outcome = "stopped";
	else if (parent.status === "completed" || parent.status === "resolved") outcome = "completed";
	else if (parent.status === "failed") outcome = turnState === "interrupted" || turnState === "cancelled" ? "stopped" : "failed";
	else outcome = "stopped";

	const report = !launchedOnly && (outcome === "completed" || outcome === "failed" || parent.status === "recovered")
		? reportOf(parent, prompt)
		: undefined;
	if (parent.status === "recovered" && outcome === "stopped" && report) outcome = "completed";
	const lane: SubagentLane = outcome === "working" ? "working" : outcome === "needs_you" ? "needs_you" : "done";

	// A settled subagent's outcome: its report's Outcome: or BLOCKED line, else how
	// its call ended. It stays in Done either way; only its glyph and line change.
	let result: AgentOutcome | undefined;
	if (lane === "done") {
		const at = new Date(latestMs || Date.now()).toISOString();
		const read = outcome === "stopped" ? undefined : reportOutcome(report);
		if (read) {
			if (read.kind === "blocked" || read.kind === "needs_input") outcome = "blocked";
			else if (read.kind === "failed") outcome = "failed";
			result = { ...read, at };
		} else {
			const kind: AgentOutcomeKind = outcome === "completed" ? "done" : outcome === "failed" ? "failed" : "stopped";
			result = { kind, text: kind === "stopped" ? undefined : clip(firstLine(report)), at, source: "settled" };
		}
		notes.push(outcomeNote(result));
	}

	let nowLine: string | undefined;
	let nowSource: SubagentCard["nowSource"];
	if (lane === "done") {
		const reportLine = result?.source === "settled" ? firstLine(report) : result?.text;
		if (reportLine) {
			nowLine = reportLine;
			nowSource = "report";
		} else if (latestStatus) {
			nowLine = latestStatus.text;
			nowSource = "status";
		}
	} else {
		// The most recent of what it said, what Claude summarized, and what it is running.
		let bestMs = Number.NEGATIVE_INFINITY;
		const consider = (candidate: string | undefined, ms: number, source: NonNullable<SubagentCard["nowSource"]>) => {
			if (!candidate || ms <= bestMs) return;
			bestMs = ms;
			nowLine = candidate;
			nowSource = source;
		};
		if (latestStatus) consider(latestStatus.text, latestStatus.ms, "status");
		consider(text(detail.progressSummary), progressMs ?? 0, "progress");
		if (runningTool) consider(text(runningTool.summary), timeOf(runningTool.createdAt), "tool");
	}

	const reportedDuration = typeof detail.durationMs === "number" && detail.durationMs > 0 ? detail.durationMs : undefined;
	const observedDuration = latestMs > startedMs ? latestMs - startedMs : undefined;
	return {
		id: parent.id,
		providerItemId: parent.providerItemId!,
		turnId: parent.turnId,
		title: text(detail.providerTitle) ?? text(input?.description) ?? genericTitle(parent.summary),
		subagentType: text(detail.subagentType) ?? text(input?.subagent_type),
		lane,
		outcome,
		startedAt: parent.createdAt,
		updatedAt: new Date(latestMs || Date.now()).toISOString(),
		durationMs: lane === "done" ? reportedDuration ?? observedDuration : undefined,
		nowLine,
		nowSource,
		blockedOn: lane === "needs_you" ? blockedOn : undefined,
		result,
		notes,
		decisions: notes.filter((note) => note.kind === "decision"),
		toolCount: Math.max(toolCount, typeof detail.progressToolUses === "number" ? detail.progressToolUses : 0),
		lastToolName: text(detail.lastToolName) ?? lastToolName,
		prompt,
		finalReport: report,
		inbox: inboxOf(detail.subagentInbox, parent.createdAt),
	};
}

// The bridge names an undescribed call after its tool; that is no title.
function genericTitle(summary: string): string | undefined {
	const value = text(summary);
	return value && !["Agent", "Task", "Agent tool"].includes(value) ? value : undefined;
}

/** Cards by lane. Live lanes keep start order so cards never jump while streaming; done shows the newest first. */
export function subagentLanes(cards: readonly SubagentCard[]): SubagentLanes {
	const lanes: SubagentLanes = { working: [], needs_you: [], done: [] };
	for (const card of cards) lanes[card.lane].push(card);
	lanes.done.reverse();
	return lanes;
}

/** Subagents still running, the count shown on the board's tab. */
export function activeSubagentCount(cards: readonly SubagentCard[]): { running: number; needsYou: number } {
	let running = 0;
	let needsYou = 0;
	for (const card of cards) {
		if (card.lane === "done") continue;
		running += 1;
		if (card.lane === "needs_you") needsYou += 1;
	}
	return { running, needsYou };
}

/**
 * A card's sent messages with the sends the conversation has not echoed yet. A
 * send the echo already carries, matched by id, is shown once, as the echo.
 */
export function withPendingMessages(echoed: readonly SentMessage[], pending: readonly SentMessage[]): readonly SentMessage[] {
	if (pending.length === 0) return echoed;
	const ids = new Set(echoed.map((message) => message.id));
	const unechoed = pending.filter((message) => !ids.has(message.id));
	return unechoed.length === 0 ? echoed : [...echoed, ...unechoed];
}

/**
 * Status and decision lines with the person's messages placed by when they were
 * sent. A line carries its narration block's start time, so a message lands
 * after every block that started before it.
 */
export function boardTimeline(notes: readonly SubagentNote[], sent: readonly SentMessage[]): BoardTimelineEntry[] {
	const entries: BoardTimelineEntry[] = [];
	const messages = [...sent].sort((a, b) => timeOf(a.at) - timeOf(b.at));
	let next = 0;
	for (const note of notes) {
		const ms = timeOf(note.at);
		while (next < messages.length && timeOf(messages[next]!.at) < ms) entries.push({ kind: "sent", message: messages[next++]! });
		entries.push({ kind: "note", note });
	}
	while (next < messages.length) entries.push({ kind: "sent", message: messages[next++]! });
	return entries;
}

/**
 * A subagent the person stopped settles as stopped, however its call ended: the
 * stop is why it ended. Live and untouched cards keep their identity.
 */
export function withStopRequests(cards: readonly SubagentCard[], stopped: ReadonlySet<string>): readonly SubagentCard[] {
	if (stopped.size === 0) return cards;
	let changed = false;
	const result = cards.map((card) => {
		if (card.lane !== "done" || card.outcome === "stopped" || !stopped.has(card.providerItemId)) return card;
		changed = true;
		const result: AgentOutcome = { kind: "stopped", at: card.result?.at ?? card.updatedAt, source: "settled" };
		return {
			...card,
			outcome: "stopped" as const,
			result,
			notes: [...card.notes.filter((note) => note.kind !== "outcome"), outcomeNote(result)],
		};
	});
	return changed ? result : cards;
}

/** The draft "Ask main agent" starts the composer with. */
export function subagentAskPrefix(title: string): string {
	return `About the subagent "${title}": `;
}

/** The board for one surface, memoized on the snapshot's items and turns. */
export function useSubagentBoard(items: readonly ConversationItem[], turns: readonly ConversationTurn[]): SubagentBoardModel {
	const deriver = useRef<SubagentBoardDeriver | null>(null);
	deriver.current ??= createSubagentBoardDeriver();
	return useMemo(() => deriver.current!.derive(items, turns), [items, turns]);
}
