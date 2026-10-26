/**
 * The subagent board's view model: one card per Claude Code subagent (an Agent
 * or Task tool call) in a chat, derived from the conversation's raw items.
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
import type { ConversationActivity, ConversationItem, ConversationTurn } from "../types/conversation";

export type SubagentLane = "working" | "needs_you" | "done";
export type SubagentOutcome = "working" | "needs_you" | "completed" | "failed" | "stopped";

/** A `Status:` or `Decision:` line the subagent wrote in its narration. */
export interface SubagentNote {
	kind: "status" | "decision";
	text: string;
	/** When the narration block carrying the line started. Lines in one block share it. */
	at: string;
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
	/** Status and decision lines, in the order they were written. */
	notes: SubagentNote[];
	decisions: SubagentNote[];
	toolCount: number;
	lastToolName?: string;
	/** The task the main agent gave it. */
	prompt?: string;
	/** The subagent's final report when it completed, or its error when it failed. */
	finalReport?: string;
}

export interface SubagentLanes {
	working: SubagentCard[];
	needs_you: SubagentCard[];
	done: SubagentCard[];
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

type ParsedLine = { kind: SubagentNote["kind"]; text: string };

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

const LIVE: ReadonlySet<string> = new Set(["running", "pending"]);
// What Claude Code's Agent tool answers for a run_in_background launch.
const LAUNCH_NOTICE = /^\s*async agent launched/i;
const TURN_LIVE: ReadonlySet<string> = new Set(["running", "queued"]);

export interface SubagentBoardDeriver {
	derive(items: readonly ConversationItem[], turns: readonly ConversationTurn[]): SubagentCard[];
}

/**
 * A deriver keeps the parse and card caches for one surface. Entries for rows
 * that left the snapshot are dropped on the next pass.
 */
export function createSubagentBoardDeriver(): SubagentBoardDeriver {
	let narrations = new Map<string, NarrationParse>();
	let cards = new Map<string, { key: string; card: SubagentCard }>();

	return {
		derive(items, turns) {
			const parents: ConversationActivity[] = [];
			const children = new Map<string, ConversationActivity[]>();
			const pending: ConversationActivity[] = [];
			for (const item of items) {
				if (item.kind !== "activity") continue;
				if (isSubagentParent(item)) parents.push(item);
				const parentId = item.detail?.parentProviderItemId;
				if (parentId) {
					const list = children.get(parentId);
					if (list) list.push(item);
					else children.set(parentId, [item]);
				} else if (isPendingInteraction(item)) {
					pending.push(item);
				}
			}
			if (parents.length === 0) {
				narrations = new Map();
				cards = new Map();
				return [];
			}

			const turnStates = new Map(turns.map((turn) => [turn.id, turn.state]));
			// A permission request does not name the subagent that raised it. It carries
			// the tool's input, which matches the subagent's pending call exactly.
			const pendingBySignature = new Map<string, string>();
			for (const request of pending) {
				const signature = toolSignature(request);
				if (signature) pendingBySignature.set(signature, interactionLabel(request) ?? "");
			}
			const pendingKey = pending.map((request) => `${request.id}:${request.revision}`).join(",");

			const nextNarrations = new Map<string, NarrationParse>();
			const nextCards = new Map<string, { key: string; card: SubagentCard }>();
			const result: SubagentCard[] = [];
			for (const parent of parents) {
				const kids = children.get(parent.providerItemId!) ?? [];
				const turnState = parent.turnId ? turnStates.get(parent.turnId) : undefined;
				let key = `${parent.revision}:${parent.status}|${turnState ?? ""}|${pendingKey}`;
				for (const kid of kids) key += `|${kid.id}:${kid.revision}:${kid.status}`;
				const cached = cards.get(parent.id);
				if (cached?.key === key) {
					for (const kid of kids) {
						const parse = narrations.get(kid.id);
						if (parse) nextNarrations.set(kid.id, parse);
					}
					nextCards.set(parent.id, cached);
					result.push(cached.card);
					continue;
				}
				const card = buildCard(parent, kids, turnState, pendingBySignature, (kid) => {
					const parse = parseNarration(narrations.get(kid.id), narrationText(kid));
					nextNarrations.set(kid.id, parse);
					return parse;
				});
				nextCards.set(parent.id, { key, card });
				result.push(card);
			}
			narrations = nextNarrations;
			cards = nextCards;
			return result;
		},
	};
}

function buildCard(
	parent: ConversationActivity,
	kids: readonly ConversationActivity[],
	turnState: string | undefined,
	pendingBySignature: ReadonlyMap<string, string>,
	parse: (narration: ConversationActivity) => NarrationParse,
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

	let nowLine: string | undefined;
	let nowSource: SubagentCard["nowSource"];
	if (lane === "done") {
		const reportLine = firstLine(report);
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
		notes,
		decisions: notes.filter((note) => note.kind === "decision"),
		toolCount: Math.max(toolCount, typeof detail.progressToolUses === "number" ? detail.progressToolUses : 0),
		lastToolName: text(detail.lastToolName) ?? lastToolName,
		prompt,
		finalReport: report,
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

/** The draft "Ask main agent" starts the composer with. */
export function subagentAskPrefix(title: string): string {
	return `About the subagent "${title}": `;
}

/** Cards for one surface, memoized on the snapshot's items and turns. */
export function useSubagentCards(items: readonly ConversationItem[], turns: readonly ConversationTurn[]): SubagentCard[] {
	const deriver = useRef<SubagentBoardDeriver | null>(null);
	deriver.current ??= createSubagentBoardDeriver();
	return useMemo(() => deriver.current!.derive(items, turns), [items, turns]);
}
