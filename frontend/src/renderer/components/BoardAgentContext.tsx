/**
 * A chat session's live agent context on its board card: the main agent's latest
 * decision, then what it is doing now (or how its turn ended) right above the
 * card's status line, and how many subagents are running among the card's
 * metadata. It refines the card's one status line rather than competing with
 * it: plain text, no chip, and color only where an outcome means something.
 *
 * Each part reads the session's summary itself, so a streaming agent redraws
 * these lines and not the memoized card around them.
 */

import { memo, useMemo } from "react";
import { useTranslation } from "react-i18next";
import { activeSubagentCount, type SubagentBoardModel } from "../lib/subagent-board";
import { cn } from "../lib/utils";
import { useAgentSummary } from "./AgentSummariesProvider";
import { mainNowLine } from "./chat/SubagentBoard";

/** Reads, and asks for, one session's summary; renders nothing until it is derived. */
export const BoardAgentContext = memo(function BoardAgentContext({ sessionId }: { sessionId: string }) {
	const detail = useAgentSummary(sessionId);
	return detail?.status === "ready" ? <BoardAgentContextView model={detail.model} /> : null;
});

/** The running subagents, for the card's metadata; nothing while none run. */
export const BoardAgentSubagents = memo(function BoardAgentSubagents({ sessionId }: { sessionId: string }) {
	const detail = useAgentSummary(sessionId);
	return detail?.status === "ready" ? <BoardAgentSubagentsView model={detail.model} /> : null;
});

export function BoardAgentContextView({ model }: { model: SubagentBoardModel }) {
	const { t } = useTranslation();
	const { main } = model;
	const now = mainNowLine(t, main, true);
	const decision = main.decisions.at(-1);
	if (!now && !decision) return null;
	return (
		<div className="flex min-w-0 flex-col gap-0.5 text-caption leading-snug" data-state={main.state} data-testid="board-agent-context">
			{decision ? (
				<p className="line-clamp-2 break-words font-medium text-foreground" data-testid="board-agent-decision" title={decision.text}>
					{/* A real space, so the label and the decision are read as two words. */}
					<span className="font-normal text-muted-foreground">{t("subagents.decision")}</span> {decision.text}
				</p>
			) : null}
			{now ? (
				<p
					className={cn(
						"line-clamp-2 break-words",
						main.state === "needs_you" ? "text-foreground" : "text-muted-foreground",
						// A live now line changes with every tool call; two lines held for it
						// keep the cards below from jumping as it wraps and unwraps.
						main.state === "working" && "min-h-[2lh]",
					)}
					data-testid="board-agent-now"
					title={main.nowLine}
				>
					{now}
				</p>
			) : null}
		</div>
	);
}

export function BoardAgentSubagentsView({ model }: { model: SubagentBoardModel }) {
	const { t } = useTranslation();
	const running = useMemo(() => activeSubagentCount(model.cards).running, [model.cards]);
	return running > 0 ? <span data-testid="board-agent-subagents">{t("subagents.count", { count: running })}</span> : null;
}
