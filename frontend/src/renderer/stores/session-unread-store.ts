import { useLayoutEffect } from "react";
import { create } from "zustand";
import { isAgentActivityWorking } from "@aoagents/product-ui";
import type { WorkspaceSummary } from "../types/workspace";

export const SESSION_UNREAD_STORAGE_KEY = "ao.sidebar.unread-sessions";

function readStoredUnread(): Record<string, true> {
	try {
		const saved: unknown = JSON.parse(window.localStorage.getItem(SESSION_UNREAD_STORAGE_KEY) ?? "null");
		if (Array.isArray(saved)) {
			return Object.fromEntries(saved.filter((id): id is string => typeof id === "string").map((id) => [id, true] as const));
		}
	} catch {}
	return {};
}

function storeUnread(unread: Record<string, true>) {
	try {
		window.localStorage.setItem(SESSION_UNREAD_STORAGE_KEY, JSON.stringify(Object.keys(unread)));
	} catch {}
}

type SessionUnreadState = {
	/** Sessions whose agent finished a turn the user has not opened since. */
	unread: Record<string, true>;
	/** Whether each session's agent was working at the last snapshot; unset until one arrives. */
	working: Record<string, boolean>;
	observe: (workspaces: readonly WorkspaceSummary[], openSessionId: string | undefined) => void;
	markSeen: (sessionId: string) => void;
};

/**
 * A session whose agent stops working while the user is looking elsewhere is
 * unread until they open it, so the sidebar can show which agents finished.
 * Only a working → stopped change counts: the first snapshot after a launch
 * marks nothing, and the open session is never marked.
 */
export const useSessionUnreadStore = create<SessionUnreadState>((set, get) => ({
	unread: readStoredUnread(),
	working: {},
	observe: (workspaces, openSessionId) => {
		const { unread, working } = get();
		const nextWorking: Record<string, boolean> = {};
		let nextUnread = unread;
		const live = new Set<string>();
		for (const workspace of workspaces) {
			for (const session of workspace.sessions) {
				if (session.hostId) continue;
				live.add(session.id);
				const isWorking = isAgentActivityWorking(session.activity);
				nextWorking[session.id] = isWorking;
				const finished = working[session.id] === true && !isWorking;
				if (finished && session.id !== openSessionId && !nextUnread[session.id]) {
					nextUnread = { ...nextUnread, [session.id]: true };
				}
			}
		}
		if (openSessionId && nextUnread[openSessionId]) {
			nextUnread = { ...nextUnread };
			delete nextUnread[openSessionId];
		}
		// Forget sessions that are gone, once a snapshot has listed any.
		if (live.size > 0 && Object.keys(nextUnread).some((id) => !live.has(id))) {
			nextUnread = Object.fromEntries(Object.keys(nextUnread).filter((id) => live.has(id)).map((id) => [id, true] as const));
		}
		if (nextUnread !== unread) storeUnread(nextUnread);
		set({ working: nextWorking, unread: nextUnread });
	},
	markSeen: (sessionId) => {
		const { unread } = get();
		if (!unread[sessionId]) return;
		const next = { ...unread };
		delete next[sessionId];
		storeUnread(next);
		set({ unread: next });
	},
}));

/** Feeds each workspace snapshot to the tracker. */
export function useSessionUnreadTracking(workspaces: readonly WorkspaceSummary[], openSessionId: string | undefined) {
	const observe = useSessionUnreadStore((state) => state.observe);
	// Before paint, so the session the user just opened never flashes bold.
	useLayoutEffect(() => observe(workspaces, openSessionId), [observe, workspaces, openSessionId]);
}
