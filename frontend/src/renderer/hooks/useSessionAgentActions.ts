/**
 * Stepping in on one session's agents from a surface that does not have its
 * chat open, such as the Agents page.
 *
 * Everything it needs comes from the shared {@link AgentSummariesProvider},
 * which already holds the session's conversation: its derived agents and what
 * the conversation allows. The requests are the conversation's own commands
 * (steer, send, interrupt) and the subagent routes, through the same
 * {@link useAgentBoardControls} the session's Agents tab uses.
 *
 * Only a local, live chat session the provider loaded has any actions; a
 * terminal, cloud or remote session is never loaded there, so it has none.
 */

import { useMemo } from "react";
import { useAgentSummary } from "../components/AgentSummariesProvider";
import type { SubagentCard } from "../lib/subagent-board";
import type { ChatSteerOutcome } from "../types/conversation";
import { useAgentBoardControls, type AgentBoardControls } from "./useAgentBoardControls";
import { useConversationCommands } from "./useConversation";
import { useLatestCallback } from "./useStable";

const NO_CARDS: readonly SubagentCard[] = [];

export interface SessionAgentActions extends AgentBoardControls {
	/**
	 * What a message to the main agent does: steer the running turn, or start a
	 * new one once it ended. Absent where it cannot be sent at all.
	 */
	mainMessage?: "steer" | "send";
}

export function useSessionAgentActions(sessionId: string | undefined): SessionAgentActions {
	const detail = useAgentSummary(sessionId);
	const ready = detail?.status === "ready" && detail.control ? detail : undefined;
	const control = ready?.control;
	const commands = useConversationCommands(ready ? sessionId : undefined);
	// The commands are rebuilt every render; the controls keep one identity per role.
	const steer = useLatestCallback((text: string, attachments: undefined, clientMessageId: string): Promise<ChatSteerOutcome> =>
		commands.steer(text, attachments, clientMessageId));
	const send = useLatestCallback((text: string, clientMessageId: string) => commands.send({ text, clientMessageId }));
	const interrupt = useLatestCallback(() => commands.interruptAsync());
	const running = control?.runningTurnId !== undefined;
	const controls = useAgentBoardControls({
		sessionId: control?.subagents ? sessionId : undefined,
		// Never queued: while a turn runs a message steers it or is not offered.
		steer: running && control?.steer ? steer : undefined,
		send: control && !running ? send : undefined,
		interrupt: running ? interrupt : undefined,
		cards: ready?.model.cards ?? NO_CARDS,
		main: ready?.model.main,
		runningTurnId: control?.runningTurnId,
	});
	const mainMessage = controls.steerMain ? "steer" : controls.sendMain ? "send" : undefined;
	return useMemo(() => ({ ...controls, mainMessage }), [controls, mainMessage]);
}
