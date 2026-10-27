/**
 * One shared set of live agent summaries for every surface that shows them: the
 * sessions board's cards, the Agents page, and anything else that reads
 * {@link useAgentSummary}.
 *
 * Surfaces ask for the sessions they show; the provider loads the union once,
 * one hidden loader per session, so the same conversation is never subscribed to
 * or derived twice. Only local, live chat sessions load, the most recently
 * active {@link OVERVIEW_SESSION_CAP} of those asked for, and nothing loads while
 * no surface asks. Each loader derives its session with the agents board's own
 * deriver ({@link useSubagentBoard}), which keeps an unchanged summary by
 * identity, so a reader re-renders only when its own session changed.
 */

import {
	createContext,
	memo,
	useCallback,
	useContext,
	useLayoutEffect,
	useMemo,
	useState,
	useSyncExternalStore,
	type ReactNode,
} from "react";
import { useConversation } from "../hooks/useConversation";
import { useStableArray } from "../hooks/useStable";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { OVERVIEW_SESSION_CAP, overviewSessionsToLoad, type SessionAgentsDetail } from "../lib/agents-overview";
import { useSubagentBoard } from "../lib/subagent-board";
import type { ConversationItem, ConversationTurn } from "../types/conversation";

type Listener = () => void;

const NO_ITEMS: readonly ConversationItem[] = [];
const NO_TURNS: readonly ConversationTurn[] = [];
const NO_IDS: readonly string[] = [];
const NO_DETAILS: ReadonlyMap<string, SessionAgentsDetail> = new Map();
const noSubscription = () => () => {};

function sameDetail(a: SessionAgentsDetail | undefined, b: SessionAgentsDetail | undefined): boolean {
	if (a === b) return true;
	if (!a || !b || a.status !== b.status) return false;
	return a.status !== "ready" || a.model === (b as typeof a).model;
}

/** The summaries, who reads which, and which sessions are asked for. */
export interface AgentSummaryStore {
	get(sessionId: string): SessionAgentsDetail | undefined;
	/** Every summary, a new map whenever one changes. */
	all(): ReadonlyMap<string, SessionAgentsDetail>;
	set(sessionId: string, detail: SessionAgentsDetail | undefined): void;
	subscribe(sessionId: string, listener: Listener): () => void;
	subscribeAll(listener: Listener): () => void;
	/** Asks for these sessions until the returned release is called. */
	demand(sessionIds: readonly string[]): () => void;
	/** Every session asked for, the same array until that changes. */
	wanted(): readonly string[];
	subscribeWanted(listener: Listener): () => void;
}

export function createAgentSummaryStore(): AgentSummaryStore {
	let details: ReadonlyMap<string, SessionAgentsDetail> = new Map();
	const sessionListeners = new Map<string, Set<Listener>>();
	const allListeners = new Set<Listener>();
	const demands = new Map<number, readonly string[]>();
	const wantedListeners = new Set<Listener>();
	let nextDemand = 0;
	let wanted: readonly string[] = NO_IDS;

	const listen = (set: Set<Listener>, listener: Listener) => {
		set.add(listener);
		return () => {
			set.delete(listener);
		};
	};
	const recomputeWanted = () => {
		const union = new Set<string>();
		for (const ids of demands.values()) for (const id of ids) union.add(id);
		const next = [...union];
		if (next.length === wanted.length && next.every((id, index) => id === wanted[index])) return;
		wanted = next.length === 0 ? NO_IDS : next;
		for (const listener of [...wantedListeners]) listener();
	};

	return {
		get: (sessionId) => details.get(sessionId),
		all: () => details,
		set(sessionId, detail) {
			if (sameDetail(details.get(sessionId), detail)) return;
			const next = new Map(details);
			if (detail) next.set(sessionId, detail);
			else next.delete(sessionId);
			details = next;
			for (const listener of [...(sessionListeners.get(sessionId) ?? [])]) listener();
			for (const listener of [...allListeners]) listener();
		},
		subscribe(sessionId, listener) {
			const set = sessionListeners.get(sessionId) ?? new Set<Listener>();
			sessionListeners.set(sessionId, set);
			set.add(listener);
			return () => {
				set.delete(listener);
				if (set.size === 0 && sessionListeners.get(sessionId) === set) sessionListeners.delete(sessionId);
			};
		},
		subscribeAll: (listener) => listen(allListeners, listener),
		demand(sessionIds) {
			const id = nextDemand++;
			demands.set(id, sessionIds);
			recomputeWanted();
			return () => {
				if (!demands.delete(id)) return;
				recomputeWanted();
			};
		},
		wanted: () => wanted,
		subscribeWanted: (listener) => listen(wantedListeners, listener),
	};
}

const AgentSummariesContext = createContext<AgentSummaryStore | null>(null);

/**
 * Mounted once in the shell. Without it, every reader sees no summary and asks
 * for nothing, so a surface rendered on its own (a test, a remote board) still works.
 */
export function AgentSummariesProvider({ children, store: given }: { children: ReactNode; store?: AgentSummaryStore }) {
	const [created] = useState(createAgentSummaryStore);
	const store = given ?? created;
	const wanted = useSyncExternalStore(store.subscribeWanted, store.wanted);
	const workspaces = useWorkspaceQuery().data;
	const loaded = useStableArray(
		useMemo(
			() => (wanted.length === 0 ? NO_IDS : overviewSessionsToLoad(workspaces ?? [], OVERVIEW_SESSION_CAP, new Set(wanted))),
			[wanted, workspaces],
		),
	);
	return (
		<AgentSummariesContext.Provider value={store}>
			{loaded.map((sessionId) => <SessionAgentsSource key={sessionId} sessionId={sessionId} store={store} />)}
			{children}
		</AgentSummariesContext.Provider>
	);
}

/**
 * Loads one chat session's conversation and derives its agents. Layout effects,
 * so a conversation already cached reaches its readers before the first paint.
 */
const SessionAgentsSource = memo(function SessionAgentsSource({ sessionId, store }: { sessionId: string; store: AgentSummaryStore }) {
	const conversation = useConversation(sessionId);
	const snapshot = conversation.snapshot;
	const model = useSubagentBoard(snapshot?.items ?? NO_ITEMS, snapshot?.turns ?? NO_TURNS);
	const status: SessionAgentsDetail["status"] = snapshot ? "ready" : conversation.unavailable ? "unavailable" : conversation.error ? "error" : "loading";
	useLayoutEffect(() => {
		store.set(sessionId, status === "ready" ? { status, model } : { status });
	}, [model, sessionId, status, store]);
	useLayoutEffect(() => () => store.set(sessionId, undefined), [sessionId, store]);
	return null;
});

/** Asks for these sessions' summaries while the caller is mounted. Pass a stable array. */
export function useAgentSummaryDemand(sessionIds: readonly string[]): void {
	const store = useContext(AgentSummariesContext);
	useLayoutEffect(() => {
		if (!store || sessionIds.length === 0) return;
		return store.demand(sessionIds);
	}, [sessionIds, store]);
}

/** One session's summary, asked for while the caller is mounted; re-renders only when it changes. */
export function useAgentSummary(sessionId: string | undefined): SessionAgentsDetail | undefined {
	const store = useContext(AgentSummariesContext);
	const ids = useMemo(() => (sessionId ? [sessionId] : NO_IDS), [sessionId]);
	useAgentSummaryDemand(ids);
	const subscribe = useCallback(
		(listener: Listener) => (store && sessionId ? store.subscribe(sessionId, listener) : () => {}),
		[sessionId, store],
	);
	const read = useCallback(() => (store && sessionId ? store.get(sessionId) : undefined), [sessionId, store]);
	return useSyncExternalStore(subscribe, read);
}

/** Every loaded summary, for a surface that shows many sessions at once. */
export function useAgentSummaries(): ReadonlyMap<string, SessionAgentsDetail> {
	const store = useContext(AgentSummariesContext);
	return useSyncExternalStore(store ? store.subscribeAll : noSubscription, store ? store.all : () => NO_DETAILS);
}
