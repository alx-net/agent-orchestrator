/**
 * The global agents overview's view model: every session grouped by project,
 * worker sessions nested under the session that spawned them, and each
 * session's subagents under it.
 *
 * Nothing here reads a conversation. A chat session's main agent and subagents
 * arrive already derived by the agents board's own deriver (see
 * {@link createSubagentBoardDeriver}); a session without one falls back to the
 * status the daemon derived for it.
 */

import {
	isOrchestratorSession,
	sessionIsActive,
	type WorkspaceSession,
	type WorkspaceSummary,
} from "../types/workspace";
import { subagentLanes, type MainAgentState, type MainAgentSummary, type SubagentBoardModel, type SubagentCard } from "./subagent-board";

/** How many chat sessions the overview loads conversations for, most recently active first. */
export const OVERVIEW_SESSION_CAP = 30;

/** What the overview has for one session's conversation. */
export type SessionAgentsDetail =
	| { status: "loading" }
	| { status: "error" }
	/** The session has no chat conversation to read right now. */
	| { status: "unavailable" }
	| { status: "ready"; model: SubagentBoardModel };

/**
 * Where a row's agent detail comes from: the derived conversation, a terminal
 * session that has none, one still loading or failing, or a session the
 * overview does not read (finished, remote, or past the cap).
 */
export type OverviewDetail = "chat" | "tui" | "loading" | "unavailable" | "none";

export interface OverviewSessionNode {
	session: WorkspaceSession;
	state: MainAgentState;
	detail: OverviewDetail;
	/** The main agent, when its conversation is derived. */
	main?: MainAgentSummary;
	/** Needs you first, then working in start order, then the main turn's finished ones, newest first. */
	subagents: readonly SubagentCard[];
	/** Sessions it spawned, in the same order as their siblings. */
	children: OverviewSessionNode[];
	/** It, one of its subagents, or a session under it needs you. */
	needsYou: boolean;
}

export interface OverviewProject {
	id: string;
	name: string;
	/** Live sessions with no live parent in the project. */
	sessions: OverviewSessionNode[];
	/** Terminated sessions, most recently updated first. */
	finished: OverviewSessionNode[];
}

export interface AgentsOverview {
	projects: OverviewProject[];
	/** Live sessions across every project. */
	liveSessions: number;
	/** Working main agents and subagents. */
	working: number;
	/** Main agents and subagents waiting on the person. */
	needsYou: number;
}

function timeOf(value: string | undefined): number {
	const parsed = value ? Date.parse(value) : Number.NaN;
	return Number.isFinite(parsed) ? parsed : 0;
}

/** The latest moment the daemon saw the session do anything. */
export function sessionActivityTime(session: WorkspaceSession): number {
	return Math.max(timeOf(session.activity?.lastActivityAt), timeOf(session.updatedAt), timeOf(session.lastInteractionAt));
}

/** A local, live chat session, whose conversation the overview can read. */
function readable(session: WorkspaceSession): boolean {
	return session.mode === "chat" && !session.hostId && !session.cloud && sessionIsActive(session);
}

/**
 * The chat sessions whose conversations are loaded, most recently active first:
 * every readable one, or only those `wanted` names.
 */
export function overviewSessionsToLoad(
	workspaces: readonly WorkspaceSummary[],
	cap = OVERVIEW_SESSION_CAP,
	wanted?: ReadonlySet<string>,
): string[] {
	const candidates: WorkspaceSession[] = [];
	for (const workspace of workspaces) {
		for (const session of workspace.sessions) if (readable(session) && (!wanted || wanted.has(session.id))) candidates.push(session);
	}
	candidates.sort((a, b) => sessionActivityTime(b) - sessionActivityTime(a) || a.id.localeCompare(b.id));
	return candidates.slice(0, cap).map((session) => session.id);
}

/** The session's state from what the daemon derived, for a session whose conversation is not read. */
export function sessionLevelState(session: WorkspaceSession): MainAgentState {
	if (!sessionIsActive(session)) return session.status === "merged" || session.scmStatus === "merged" ? "completed" : "stopped";
	if (session.provisionState === "failed") return "failed";
	const activity = session.activity?.state;
	if (activity === "active") return "working";
	if (activity === "waiting_input" || activity === "blocked" || session.status === "needs_input") return "needs_you";
	if (activity === "exited") return "stopped";
	return "idle";
}

/** Live sessions whose agent is working, for the sidebar's count. */
export function workingSessionCount(workspaces: readonly WorkspaceSummary[]): number {
	let count = 0;
	for (const workspace of workspaces) {
		for (const session of workspace.sessions) if (!session.hostId && sessionIsActive(session) && sessionLevelState(session) === "working") count += 1;
	}
	return count;
}

const DETAIL_BY_STATUS = { loading: "loading", error: "unavailable", unavailable: "unavailable", ready: "chat" } as const;

/**
 * The subagents a row shows: every live one, and the finished ones of the main
 * agent's current or latest turn. Older finished ones are that session's history.
 */
function shownSubagents(model: SubagentBoardModel): SubagentCard[] {
	const lanes = subagentLanes(model.cards);
	const turnId = model.main.turnId;
	return [...lanes.needs_you, ...lanes.working, ...lanes.done.filter((card) => card.turnId === undefined || card.turnId === turnId)];
}

function nodeOf(session: WorkspaceSession, detail: SessionAgentsDetail | undefined): OverviewSessionNode {
	const base = { session, children: [], needsYou: false };
	if (detail?.status === "ready") {
		const subagents = shownSubagents(detail.model);
		return { ...base, state: detail.model.main.state, detail: "chat", main: detail.model.main, subagents };
	}
	const state = sessionLevelState(session);
	if (!sessionIsActive(session)) return { ...base, state, detail: "none", subagents: [] };
	if (session.mode !== "chat") return { ...base, state, detail: "tui", subagents: [] };
	return { ...base, state, detail: detail ? DETAIL_BY_STATUS[detail.status] : "none", subagents: [] };
}

/** Nests each node under its parent when the parent is among `nodes` and no cycle runs through it. */
function nest(nodes: readonly OverviewSessionNode[]): OverviewSessionNode[] {
	const byId = new Map(nodes.map((node) => [node.session.id, node]));
	const parentOf = (node: OverviewSessionNode) => {
		const id = node.session.parentSessionId;
		return id && id !== node.session.id ? byId.get(id) : undefined;
	};
	// A parent chain that comes back to the node is a cycle; the node stays at the top.
	const cyclic = (node: OverviewSessionNode) => {
		const seen = new Set<string>([node.session.id]);
		for (let parent = parentOf(node); parent; parent = parentOf(parent)) {
			if (seen.has(parent.session.id)) return true;
			seen.add(parent.session.id);
		}
		return false;
	};
	const roots: OverviewSessionNode[] = [];
	for (const node of nodes) {
		const parent = parentOf(node);
		if (parent && !cyclic(node)) parent.children.push(node);
		else roots.push(node);
	}
	return roots;
}

/** Marks every node that needs you, or holds one that does, bottom up. */
function markNeedsYou(node: OverviewSessionNode): boolean {
	let needsYou = node.state === "needs_you" || node.subagents.some((card) => card.lane === "needs_you");
	for (const child of node.children) if (markNeedsYou(child)) needsYou = true;
	node.needsYou = needsYou;
	return needsYou;
}

/**
 * Needs you first, then orchestrators, then newest first. Nothing else moves a
 * row, so a session does not jump around while its agents stream.
 */
function compareLive(a: OverviewSessionNode, b: OverviewSessionNode): number {
	return Number(b.needsYou) - Number(a.needsYou) ||
		Number(isOrchestratorSession(b.session)) - Number(isOrchestratorSession(a.session)) ||
		timeOf(b.session.createdAt) - timeOf(a.session.createdAt) ||
		a.session.id.localeCompare(b.session.id);
}

function compareFinished(a: OverviewSessionNode, b: OverviewSessionNode): number {
	return timeOf(b.session.updatedAt) - timeOf(a.session.updatedAt) || a.session.id.localeCompare(b.session.id);
}

function sortTree(nodes: OverviewSessionNode[], compare: (a: OverviewSessionNode, b: OverviewSessionNode) => number): OverviewSessionNode[] {
	for (const node of nodes) sortTree(node.children, compare);
	return nodes.sort(compare);
}

/**
 * Sessions and their derived agents as one tree per project. Live sessions nest
 * under a live parent in the same project; a parent that is missing, finished
 * or elsewhere leaves its worker at the top. Finished sessions nest the same
 * way among themselves.
 */
export function buildAgentsOverview(
	workspaces: readonly WorkspaceSummary[],
	details: ReadonlyMap<string, SessionAgentsDetail>,
): AgentsOverview {
	const projects: OverviewProject[] = [];
	let liveSessions = 0;
	let working = 0;
	let needsYou = 0;
	for (const workspace of workspaces) {
		if (workspace.sessions.length === 0) continue;
		const live: OverviewSessionNode[] = [];
		const finished: OverviewSessionNode[] = [];
		for (const session of workspace.sessions) {
			const node = nodeOf(session, details.get(session.id));
			if (!sessionIsActive(session)) {
				finished.push(node);
				continue;
			}
			live.push(node);
			liveSessions += 1;
			if (node.state === "working") working += 1;
			if (node.state === "needs_you") needsYou += 1;
			for (const card of node.subagents) {
				if (card.lane === "working") working += 1;
				else if (card.lane === "needs_you") needsYou += 1;
			}
		}
		const roots = nest(live);
		for (const root of roots) markNeedsYou(root);
		projects.push({
			id: workspace.id,
			name: workspace.name,
			sessions: sortTree(roots, compareLive),
			finished: sortTree(nest(finished), compareFinished),
		});
	}
	return { projects, liveSessions, working, needsYou };
}
