/**
 * Stepping in from the agents board: steering the main agent's running turn,
 * messaging a running subagent, and stopping one subagent.
 *
 * The main agent is steered through the conversation's own steer command, so a
 * message lands in the turn already running and is never queued behind it. A
 * subagent has an inbox on the daemon instead: it reads a message at its next
 * tool call, and the parent Agent row reports each message's delivery.
 *
 * What the board sent but the conversation has not echoed yet is held here,
 * above the board, so it survives switching back to the chat tab.
 */

import { QueryClientContext } from "@tanstack/react-query";
import { useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { components } from "../../api/schema";
import { apiErrorMessage } from "../lib/api-client";
import { clientForSessionHost } from "../lib/host-clients";
import type { SentMessage, SubagentCard } from "../lib/subagent-board";
import type { ChatSteerOutcome } from "../types/conversation";
import { conversationQueryKey, refreshRemoteConversation } from "./useConversation";

export type SubagentMessageResponse = components["schemas"]["SendSubagentMessageResponse"];

// The session's own daemon client, with its base URL and error handling.
function subagentClient(hostId?: string) {
	return clientForSessionHost(hostId);
}

/** A refused subagent request, with the daemon's status and error envelope. */
export class SubagentRequestError extends Error {
	constructor(readonly status: number, readonly envelope: unknown) {
		super(apiErrorMessage(envelope, `Request failed (${status})`));
		this.name = "SubagentRequestError";
	}
}

/** Queues a message for a running subagent; it reads it at its next tool call. */
export async function sendSubagentMessage(
	sessionId: string,
	toolUseId: string,
	text: string,
	hostId?: string,
): Promise<SubagentMessageResponse> {
	const { data, error, response } = await subagentClient(hostId).POST(
		"/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/messages",
		{ params: { path: { sessionId, toolUseId } }, body: { text } },
	);
	if (error !== undefined || !data) throw new SubagentRequestError(response.status, error);
	return data;
}

/** Stops one running subagent; the main agent's turn keeps going. */
export async function stopSubagent(sessionId: string, toolUseId: string, hostId?: string): Promise<void> {
	const { error, response } = await subagentClient(hostId).POST(
		"/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop",
		{ params: { path: { sessionId, toolUseId } } },
	);
	if (error !== undefined) throw new SubagentRequestError(response.status, error);
}

/** Why a board action did not go through: the subagent had finished, or the daemon's own words. */
export type AgentControlError = { kind: "finished" } | { kind: "message"; message?: string };

/** One card's board actions in flight and their outcome. */
export interface AgentControlState {
	/** Sends the conversation has not echoed yet, oldest first. */
	pending: readonly SentMessage[];
	sending: boolean;
	sendError?: AgentControlError;
	/** From the click until the card settles; the daemon may still refuse. */
	stopping: boolean;
	/** The daemon accepted the stop. */
	stopped: boolean;
	stopError?: AgentControlError;
}

export const EMPTY_AGENT_CONTROL: AgentControlState = { pending: [], sending: false, stopping: false, stopped: false };

/** The main agent's key in {@link AgentBoardControls.states}; a subagent's is its tool use id. */
export const MAIN_AGENT_CONTROL_KEY = "\u0000main-agent";

export interface AgentBoardControls {
	states: ReadonlyMap<string, AgentControlState>;
	/** Steers the running main turn. Absent while none runs or it cannot be steered. Resolves true once sent. */
	steerMain?: (text: string) => Promise<boolean>;
	/** Absent where the daemon cannot reach subagents. Resolves true once queued. */
	messageSubagent?: (toolUseId: string, text: string) => Promise<boolean>;
	stopSubagent?: (toolUseId: string) => Promise<void>;
}

function describe(error: unknown): AgentControlError {
	if (error instanceof SubagentRequestError) {
		if (error.status === 409) return { kind: "finished" };
		return { kind: "message", message: apiErrorMessage(error.envelope, "") || undefined };
	}
	return { kind: "message", message: apiErrorMessage(error, "") || undefined };
}

export function useAgentBoardControls({
	sessionId,
	hostId,
	steer,
	cards,
	mainSent,
}: {
	/** The daemon session the subagents run in; absent where the board cannot reach them. */
	sessionId?: string;
	hostId?: string;
	/** The conversation's steer command, present only while a turn runs that can be steered. */
	steer?: (text: string, attachments: undefined, clientMessageId: string) => Promise<ChatSteerOutcome | void>;
	cards: readonly SubagentCard[];
	mainSent: readonly SentMessage[];
}): AgentBoardControls {
	// Read without requiring a provider: a chat rendered outside the app has no cache to refresh.
	const queryClient = useContext(QueryClientContext);
	const [states, setStates] = useState<ReadonlyMap<string, AgentControlState>>(() => new Map());
	const update = useCallback((key: string, change: (state: AgentControlState) => AgentControlState) => {
		setStates((current) => new Map(current).set(key, change(current.get(key) ?? EMPTY_AGENT_CONTROL)));
	}, []);

	// Another session's sends and stops are not this one's.
	const owner = `${hostId ?? ""}\u0000${sessionId ?? ""}`;
	const ownerRef = useRef(owner);
	useEffect(() => {
		if (ownerRef.current === owner) return;
		ownerRef.current = owner;
		setStates(new Map());
	}, [owner]);

	// A send the conversation echoed is shown from the echo from then on.
	useEffect(() => {
		setStates((current) => {
			let next: Map<string, AgentControlState> | undefined;
			for (const [key, state] of current) {
				if (state.pending.length === 0) continue;
				const echoed = key === MAIN_AGENT_CONTROL_KEY ? mainSent : cards.find((card) => card.providerItemId === key)?.inbox;
				if (!echoed?.length) continue;
				const ids = new Set(echoed.map((message) => message.id));
				const pending = state.pending.filter((message) => !ids.has(message.id));
				if (pending.length !== state.pending.length) (next ??= new Map(current)).set(key, { ...state, pending });
			}
			return next ?? current;
		});
	}, [cards, mainSent]);

	// The daemon's change feed carries the echo; a refetch makes it arrive without waiting on it.
	const refresh = useCallback(() => {
		if (!sessionId || !queryClient) return;
		const done = hostId
			? refreshRemoteConversation(queryClient, sessionId, hostId)
			: queryClient.invalidateQueries({ queryKey: conversationQueryKey(sessionId) });
		void done.catch(() => {});
	}, [hostId, queryClient, sessionId]);

	const steerMain = useCallback(async (text: string) => {
		if (!steer) return false;
		const clientMessageId = crypto.randomUUID();
		const sent: SentMessage = { id: clientMessageId, text, state: "sending", at: new Date().toISOString() };
		const drop = (sendError: AgentControlError) => update(MAIN_AGENT_CONTROL_KEY, (state) => ({
			...state,
			sending: false,
			sendError,
			pending: state.pending.filter((message) => message.id !== clientMessageId),
		}));
		update(MAIN_AGENT_CONTROL_KEY, (state) => ({ ...state, sending: true, sendError: undefined, pending: [...state.pending, sent] }));
		try {
			const outcome = await steer(text, undefined, clientMessageId);
			if (outcome?.status === "not-accepted") {
				drop({ kind: "message", message: outcome.reason });
				return false;
			}
		} catch (error) {
			drop(describe(error));
			return false;
		}
		update(MAIN_AGENT_CONTROL_KEY, (state) => ({
			...state,
			sending: false,
			pending: state.pending.map((message) => message.id === clientMessageId ? { ...message, state: "delivered" } : message),
		}));
		return true;
	}, [steer, update]);

	const messageSubagent = useCallback(async (toolUseId: string, text: string) => {
		if (!sessionId) return false;
		// Until the daemon answers with the inbox id, the send is known by a local one.
		const localId = `local:${crypto.randomUUID()}`;
		const sent: SentMessage = { id: localId, text, state: "queued", at: new Date().toISOString() };
		update(toolUseId, (state) => ({ ...state, sending: true, sendError: undefined, pending: [...state.pending, sent] }));
		try {
			const queued = await sendSubagentMessage(sessionId, toolUseId, text, hostId);
			update(toolUseId, (state) => ({
				...state,
				sending: false,
				pending: state.pending.map((message) => message.id === localId ? { ...message, id: queued.id || localId } : message),
			}));
			refresh();
			return true;
		} catch (error) {
			update(toolUseId, (state) => ({
				...state,
				sending: false,
				sendError: describe(error),
				pending: state.pending.filter((message) => message.id !== localId),
			}));
			refresh();
			return false;
		}
	}, [hostId, refresh, sessionId, update]);

	const stop = useCallback(async (toolUseId: string) => {
		if (!sessionId) return;
		update(toolUseId, (state) => ({ ...state, stopping: true, stopError: undefined }));
		try {
			await stopSubagent(sessionId, toolUseId, hostId);
			update(toolUseId, (state) => ({ ...state, stopped: true }));
			refresh();
		} catch (error) {
			update(toolUseId, (state) => ({ ...state, stopping: false, stopError: describe(error) }));
			// A refusal usually means the board is behind: the subagent already finished.
			refresh();
		}
	}, [hostId, refresh, sessionId, update]);

	return useMemo(() => ({
		states,
		steerMain: steer ? steerMain : undefined,
		messageSubagent: sessionId ? messageSubagent : undefined,
		stopSubagent: sessionId ? stop : undefined,
	}), [messageSubagent, sessionId, states, steer, steerMain, stop]);
}
