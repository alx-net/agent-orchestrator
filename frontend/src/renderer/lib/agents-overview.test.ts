import { describe, expect, it } from "vitest";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";
import {
	buildAgentsOverview,
	overviewSessionsToLoad,
	sessionLevelState,
	workingSessionCount,
	type OverviewSessionNode,
	type SessionAgentsDetail,
} from "./agents-overview";
import type { MainAgentSummary, SubagentBoardModel, SubagentCard } from "./subagent-board";

function session(id: string, overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "proj",
		workspaceName: "Project",
		title: id,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		activity: { state: "idle", lastActivityAt: "2026-10-26T10:00:00.000Z" },
		createdAt: "2026-10-26T09:00:00.000Z",
		updatedAt: "2026-10-26T10:00:00.000Z",
		prs: [],
		...overrides,
	};
}

function workspace(id: string, sessions: WorkspaceSession[], name = id): WorkspaceSummary {
	return { id, name, path: `/repos/${id}`, sessions: sessions.map((entry) => ({ ...entry, workspaceId: id, workspaceName: name })) };
}

function main(overrides: Partial<MainAgentSummary> = {}): MainAgentSummary {
	return { state: "idle", turnId: "turn-1", notes: [], decisions: [], sent: [], ...overrides };
}

function card(id: string, overrides: Partial<SubagentCard> = {}): SubagentCard {
	return {
		id,
		providerItemId: `toolu-${id}`,
		turnId: "turn-1",
		title: id,
		lane: "working",
		outcome: "working",
		startedAt: "2026-10-26T10:00:00.000Z",
		updatedAt: "2026-10-26T10:00:00.000Z",
		notes: [],
		decisions: [],
		toolCount: 0,
		inbox: [],
		...overrides,
	};
}

function ready(summary: MainAgentSummary, cards: SubagentCard[] = []): SessionAgentsDetail {
	const model: SubagentBoardModel = { main: summary, cards };
	return { status: "ready", model };
}

const ids = (nodes: readonly OverviewSessionNode[]) => nodes.map((node) => node.session.id);

describe("buildAgentsOverview", () => {
	it("groups by project and nests workers under the orchestrator that spawned them", () => {
		const overview = buildAgentsOverview([
			workspace("app", [
				session("orch", { kind: "orchestrator" }),
				session("w1", { parentSessionId: "orch", createdAt: "2026-10-26T09:10:00.000Z" }),
				session("w2", { parentSessionId: "orch", createdAt: "2026-10-26T09:20:00.000Z" }),
				session("solo"),
			]),
			workspace("empty", []),
			workspace("docs", [session("d1")]),
		], new Map());

		expect(overview.projects.map((project) => project.id)).toEqual(["app", "docs"]);
		const [app] = overview.projects;
		// Orchestrators first, then newest first.
		expect(ids(app!.sessions)).toEqual(["orch", "solo"]);
		expect(ids(app!.sessions[0]!.children)).toEqual(["w2", "w1"]);
		expect(overview.liveSessions).toBe(5);
	});

	it("keeps a worker at the top when its parent is missing, elsewhere, finished or circular", () => {
		const overview = buildAgentsOverview([
			workspace("app", [
				session("orphan", { parentSessionId: "gone" }),
				session("self", { parentSessionId: "self" }),
				session("a", { parentSessionId: "b" }),
				session("b", { parentSessionId: "a" }),
				session("dead-orch", { kind: "orchestrator", isTerminated: true, status: "terminated" }),
				session("left-behind", { parentSessionId: "dead-orch" }),
				session("cross", { parentSessionId: "other-orch" }),
			]),
			workspace("other", [session("other-orch", { kind: "orchestrator" })]),
		], new Map());

		const app = overview.projects[0]!;
		expect(ids(app.sessions).sort()).toEqual(["a", "b", "cross", "left-behind", "orphan", "self"]);
		expect(app.sessions.every((node) => node.children.length === 0)).toBe(true);
		expect(ids(app.finished)).toEqual(["dead-orch"]);
		expect(overview.projects[1]!.sessions[0]!.children).toEqual([]);
	});

	it("nests deeper chains, and finished sessions among themselves", () => {
		const overview = buildAgentsOverview([
			workspace("app", [
				session("orch", { kind: "orchestrator" }),
				session("lead", { parentSessionId: "orch" }),
				session("helper", { parentSessionId: "lead" }),
				session("old-orch", { kind: "orchestrator", isTerminated: true, status: "terminated", updatedAt: "2026-10-25T10:00:00.000Z" }),
				session("old-worker", { parentSessionId: "old-orch", isTerminated: true, status: "merged", updatedAt: "2026-10-25T11:00:00.000Z" }),
				session("newer", { isTerminated: true, status: "terminated", updatedAt: "2026-10-26T08:00:00.000Z" }),
			]),
		], new Map());
		const app = overview.projects[0]!;
		expect(ids(app.sessions[0]!.children)).toEqual(["lead"]);
		expect(ids(app.sessions[0]!.children[0]!.children)).toEqual(["helper"]);
		// Most recently updated first; nested workers stay under their orchestrator.
		expect(ids(app.finished)).toEqual(["newer", "old-orch"]);
		expect(app.finished[1]!.children[0]).toMatchObject({ state: "completed", detail: "none" });
		expect(app.finished[0]).toMatchObject({ state: "stopped" });
	});

	it("sorts sessions that need you first, carrying a nested worker's need up to its orchestrator", () => {
		const details = new Map<string, SessionAgentsDetail>([
			["w-blocked", ready(main({ state: "needs_you" }))],
			["solo-sub", ready(main({ state: "working" }), [card("asks", { lane: "needs_you", outcome: "needs_you" })])],
		]);
		const overview = buildAgentsOverview([
			workspace("app", [
				session("orch-a", { kind: "orchestrator", createdAt: "2026-10-26T09:50:00.000Z" }),
				session("orch-b", { kind: "orchestrator", createdAt: "2026-10-26T09:00:00.000Z" }),
				session("w-fine", { parentSessionId: "orch-b", createdAt: "2026-10-26T09:59:00.000Z" }),
				session("w-blocked", { parentSessionId: "orch-b", createdAt: "2026-10-26T09:01:00.000Z" }),
				session("solo-sub", { createdAt: "2026-10-26T08:00:00.000Z" }),
				session("solo", { createdAt: "2026-10-26T09:58:00.000Z" }),
			]),
		], details);
		const app = overview.projects[0]!;
		expect(ids(app.sessions)).toEqual(["orch-b", "solo-sub", "orch-a", "solo"]);
		expect(app.sessions[0]!.needsYou).toBe(true);
		expect(ids(app.sessions[0]!.children)).toEqual(["w-blocked", "w-fine"]);
		expect(overview.needsYou).toBe(2);
	});

	it("uses the derived agents for a loaded chat session and the session's own status otherwise", () => {
		const done = card("done-now", { lane: "done", outcome: "completed" });
		const doneEarlier = card("done-earlier", { lane: "done", outcome: "completed", turnId: "turn-0" });
		const details = new Map<string, SessionAgentsDetail>([
			["chat", ready(main({ state: "working", turnId: "turn-1" }), [done, card("live"), doneEarlier, card("asks", { lane: "needs_you", outcome: "needs_you" })])],
			["loading", { status: "loading" }],
			["broken", { status: "error" }],
		]);
		const overview = buildAgentsOverview([
			workspace("app", [
				session("chat"),
				session("loading"),
				session("broken"),
				session("terminal", { mode: "tui", activity: { state: "waiting_input", lastActivityAt: "2026-10-26T10:00:00.000Z" } }),
				session("unread"),
			]),
		], details);
		const byId = new Map(overview.projects[0]!.sessions.map((node) => [node.session.id, node]));
		// Needs you, working, then the main turn's finished ones; an older turn's are history.
		expect(byId.get("chat")).toMatchObject({ state: "working", detail: "chat" });
		expect(byId.get("chat")!.subagents.map((entry) => entry.id)).toEqual(["asks", "live", "done-now"]);
		expect(byId.get("loading")).toMatchObject({ detail: "loading", state: "idle", subagents: [] });
		expect(byId.get("broken")).toMatchObject({ detail: "unavailable" });
		expect(byId.get("terminal")).toMatchObject({ detail: "tui", state: "needs_you" });
		expect(byId.get("unread")).toMatchObject({ detail: "none" });
		// The main agent and one subagent work; one subagent and the terminal session need you.
		expect(overview).toMatchObject({ working: 2, needsYou: 2 });
	});

	it("reads an outcome as the session's state", () => {
		const details = new Map<string, SessionAgentsDetail>([
			["blocked", ready(main({ state: "needs_you", nowSource: "outcome", result: { kind: "blocked", text: "on 2FA", at: "2026-10-26T10:00:00.000Z", source: "blocked_line" } }))],
			["failed", ready(main({ state: "failed" }))],
		]);
		const overview = buildAgentsOverview([workspace("app", [session("failed"), session("blocked")])], details);
		expect(overview.projects[0]!.sessions.map((node) => [node.session.id, node.state, node.needsYou])).toEqual([
			["blocked", "needs_you", true],
			["failed", "failed", false],
		]);
	});
});

describe("overviewSessionsToLoad", () => {
	it("loads live local chat sessions, most recently active first, up to the cap", () => {
		const at = (minute: number) => `2026-10-26T10:${String(minute).padStart(2, "0")}:00.000Z`;
		const workspaces = [
			workspace("app", [
				session("old", { updatedAt: at(1), activity: { state: "idle", lastActivityAt: at(1) } }),
				session("new", { updatedAt: at(2), activity: { state: "active", lastActivityAt: at(30) } }),
				session("tui", { mode: "tui", updatedAt: at(40) }),
				session("dead", { isTerminated: true, status: "terminated", updatedAt: at(50) }),
				session("remote", { hostId: "host-1", updatedAt: at(51) }),
				session("cloud", { cloud: { orgId: "org" }, updatedAt: at(52) }),
				session("talked", { updatedAt: at(3), lastInteractionAt: at(20) }),
			]),
		];
		expect(overviewSessionsToLoad(workspaces)).toEqual(["new", "talked", "old"]);
		expect(overviewSessionsToLoad(workspaces, 2)).toEqual(["new", "talked"]);
		// Only the sessions asked for, still readable ones only, in the same order.
		expect(overviewSessionsToLoad(workspaces, 30, new Set(["old", "tui", "new", "missing"]))).toEqual(["new", "old"]);
		expect(overviewSessionsToLoad(workspaces, 1, new Set(["old", "talked"]))).toEqual(["talked"]);
	});
});

describe("sessionLevelState", () => {
	it("maps the daemon's facts onto one status slot", () => {
		expect(sessionLevelState(session("a", { activity: { state: "active", lastActivityAt: "" } }))).toBe("working");
		expect(sessionLevelState(session("a", { activity: { state: "blocked", lastActivityAt: "" } }))).toBe("needs_you");
		expect(sessionLevelState(session("a", { status: "needs_input" }))).toBe("needs_you");
		expect(sessionLevelState(session("a", { activity: { state: "exited", lastActivityAt: "" } }))).toBe("stopped");
		expect(sessionLevelState(session("a", { provisionState: "failed" }))).toBe("failed");
		expect(sessionLevelState(session("a", { isTerminated: true, status: "merged" }))).toBe("completed");
		expect(sessionLevelState(session("a", { isTerminated: true, status: "terminated" }))).toBe("stopped");
		expect(sessionLevelState(session("a"))).toBe("idle");
	});

	it("counts live local sessions whose agent works, for the sidebar", () => {
		const active = { state: "active" as const, lastActivityAt: "" };
		expect(workingSessionCount([
			workspace("app", [
				session("a", { activity: active }),
				session("b", { activity: active, mode: "tui" }),
				session("c"),
				session("d", { activity: active, isTerminated: true, status: "terminated" }),
				session("e", { activity: active, hostId: "remote" }),
			]),
		])).toBe(2);
	});
});
