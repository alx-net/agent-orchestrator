/**
 * The session's agents as a board: the main agent pinned on top, then its Claude
 * Code subagents, each with what it was asked to do, what it is doing now, and
 * the decisions it made on the way, so a person can step in without reading the
 * raw trace.
 *
 * Lanes follow the session board: Working, Needs you, Done. Failed and stopped
 * subagents settle into Done with their own glyph rather than a lane of their
 * own: nothing can be done to a finished subagent from here, and the main agent
 * already sees its failure, so a fourth lane would mostly sit empty.
 *
 * A live card can be stepped into: the main agent's running turn is steered
 * directly, never queued, and a running subagent gets a message it reads at its
 * next tool call. Either can be stopped; stopping a subagent leaves the main
 * agent running, stopping the main turn stops every subagent it started.
 *
 * The cards read from {@link SubagentCard} and {@link MainAgentSummary}; nothing
 * here inspects the trace.
 */

import {
	memo,
	useCallback,
	useEffect,
	useId,
	useMemo,
	useRef,
	useState,
	type KeyboardEvent,
	type ReactNode,
	type RefObject,
} from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ArrowUp, Check, ChevronRight, CircleX, CornerUpRight, Loader2, MessageSquare, MessageSquareReply, Square } from "lucide-react";
import { cn } from "../../lib/utils";
import {
	boardTimeline,
	subagentLanes,
	withPendingMessages,
	withStopRequests,
	type MainAgentState,
	type MainAgentSummary,
	type SentMessage,
	type SentMessageState,
	type SubagentCard,
	type SubagentLane,
	type SubagentNote,
	type SubagentOutcome,
} from "../../lib/subagent-board";
import {
	EMPTY_AGENT_CONTROL,
	MAIN_AGENT_CONTROL_KEY,
	type AgentBoardControls,
	type AgentControlError,
	type AgentControlState,
} from "../../hooks/useAgentBoardControls";
import type { SessionMode } from "../../types/conversation";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Tooltip, TooltipContent, TooltipTrigger } from "../ui/tooltip";
import { ChatMarkdown } from "./ChatMarkdown";
import { formatDuration, formatTime } from "./ChatTimelineItems";

// The main agent's key in the board's set of open cards; no activity id looks like it.
const MAIN_CARD_ID = "\u0000main-agent";
const NO_MESSAGES: readonly SentMessage[] = [];

export interface SubagentBoardProps {
	cards: readonly SubagentCard[];
	/** The main agent's card, pinned above the lanes. */
	main?: MainAgentSummary;
	mode: SessionMode;
	/** The main turn in flight: the main card is steered and stopped only while it runs. */
	activeTurnId?: string;
	onAskMainAgent?: (card: SubagentCard) => void;
	/** Interrupts the main turn, and with it every subagent it started. */
	onStopTurn?: () => void;
	/**
	 * Steering the main turn, and messaging or stopping one subagent. Without it a
	 * subagent card falls back to stopping the main turn.
	 */
	controls?: AgentBoardControls;
}

const LANES: { id: SubagentLane; labelKey: "subagents.lane.working" | "subagents.lane.needsYou" | "subagents.lane.done"; dot: string; title: string }[] = [
	{ id: "working", labelKey: "subagents.lane.working", dot: "bg-status-working", title: "text-status-working" },
	{ id: "needs_you", labelKey: "subagents.lane.needsYou", dot: "bg-status-needs-you", title: "text-status-needs-you" },
	{ id: "done", labelKey: "subagents.lane.done", dot: "bg-status-terminated", title: "text-status-terminated-foreground" },
];

export const SubagentBoard = memo(function SubagentBoard({
	cards,
	main,
	mode,
	activeTurnId,
	onAskMainAgent,
	onStopTurn,
	controls,
}: SubagentBoardProps) {
	const { t } = useTranslation();
	const states = controls?.states;
	// A subagent the person stopped reads as stopped once it settles.
	const shown = useMemo(() => {
		const stopped = new Set<string>();
		for (const [key, state] of states ?? []) if (state.stopped) stopped.add(key);
		return withStopRequests(cards, stopped);
	}, [cards, states]);
	const lanes = useMemo(() => subagentLanes(shown), [shown]);
	const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
	const toggle = useCallback((id: string) => {
		setExpanded((current) => {
			const next = new Set(current);
			if (!next.delete(id)) next.add(id);
			return next;
		});
	}, []);
	const open = useCallback((id: string) => {
		setExpanded((current) => current.has(id) ? current : new Set(current).add(id));
	}, []);
	// Escape anywhere in the chat stops the turn. On the board it only folds the
	// open cards: stopping every subagent has to be the deliberate button.
	const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
		if (event.key !== "Escape" || event.defaultPrevented) return;
		event.preventDefault();
		event.stopPropagation();
		if (expanded.size > 0) setExpanded(new Set());
	};

	if (mode !== "chat") return <BoardMessage>{t("subagents.needsChat")}</BoardMessage>;
	if (cards.length === 0 && !main) return <BoardMessage>{t("subagents.empty")}</BoardMessage>;

	const mainRunning = main?.turnId !== undefined && main.turnId === activeTurnId;
	return (
		<section
			aria-label={t("subagents.tab")}
			className="@container/subagents flex h-full min-h-0 flex-col"
			data-testid="subagent-board"
			onKeyDown={onKeyDown}
		>
			{main ? (
				<div className="shrink-0 border-b border-border-strong p-3">
					<MainAgentCardView
						control={states?.get(MAIN_AGENT_CONTROL_KEY) ?? EMPTY_AGENT_CONTROL}
						expanded={expanded.has(MAIN_CARD_ID)}
						onOpen={open}
						onSteer={mainRunning ? controls?.steerMain : undefined}
						onStopTurn={onStopTurn && mainRunning ? onStopTurn : undefined}
						onToggle={toggle}
						summary={main}
					/>
				</div>
			) : null}
			{cards.length === 0 ? (
				<div className="flex min-h-0 flex-1 items-center justify-center px-6 text-center text-sm text-muted-foreground" role="status">
					{t("subagents.empty")}
				</div>
			) : (
				/* One continuous grid with shared dividers; a narrow pane stacks the lanes. */
				<div className="board-scrollbar grid min-h-0 flex-1 grid-cols-1 content-start divide-y divide-border-strong overflow-y-auto @3xl/subagents:grid-cols-3 @3xl/subagents:content-stretch @3xl/subagents:divide-x @3xl/subagents:divide-y-0 @3xl/subagents:overflow-hidden">
					{LANES.map((lane) => {
						const laneCards = lanes[lane.id];
						const label = t(lane.labelKey);
						return (
							<section
								aria-label={label}
								className="flex min-w-0 flex-col @3xl/subagents:min-h-0 @3xl/subagents:overflow-hidden"
								data-lane={lane.id}
								data-testid="subagent-lane"
								key={lane.id}
							>
								<div className="flex h-12 shrink-0 items-center gap-2.5 border-b border-border-strong px-4">
									<span aria-hidden="true" className={cn("size-[var(--size-swatch)] rounded-full", lane.dot)} />
									<span className={cn("text-xs font-medium", lane.title)}>{label}</span>
									<span className="ml-auto text-xs leading-none tabular-nums text-passive">{laneCards.length}</span>
								</div>
								{laneCards.length > 0 ? (
									<div className="board-scrollbar px-3 py-3 @3xl/subagents:min-h-0 @3xl/subagents:flex-1 @3xl/subagents:overflow-y-auto">
										<div className="flex flex-col gap-2.5">
											{laneCards.map((card) => {
												const live = card.lane !== "done";
												return (
													<SubagentCardView
														card={card}
														control={states?.get(card.providerItemId) ?? EMPTY_AGENT_CONTROL}
														expanded={expanded.has(card.id)}
														key={card.id}
														onAsk={onAskMainAgent}
														onMessage={live ? controls?.messageSubagent : undefined}
														onOpen={open}
														onStop={live ? controls?.stopSubagent : undefined}
														onStopTurn={
															!controls?.stopSubagent && onStopTurn && live && (!card.turnId || card.turnId === activeTurnId)
																? onStopTurn
																: undefined
														}
														onToggle={toggle}
													/>
												);
											})}
										</div>
									</div>
								) : null}
							</section>
						);
					})}
				</div>
			)}
		</section>
	);
});

function BoardMessage({ children }: { children: ReactNode }) {
	return (
		<div className="flex h-full min-h-0 items-center justify-center px-6 text-center text-sm text-muted-foreground" data-testid="subagent-board" role="status">
			{children}
		</div>
	);
}

/**
 * Asks for the card's message field: focused at once when the card is open, or as
 * soon as it opens.
 */
function useFieldFocus(expanded: boolean, field: RefObject<HTMLInputElement | null>): () => void {
	const wanted = useRef(false);
	useEffect(() => {
		if (!expanded || !wanted.current) return;
		wanted.current = false;
		field.current?.focus();
	}, [expanded, field]);
	return useCallback(() => {
		if (expanded) field.current?.focus();
		else wanted.current = true;
	}, [expanded, field]);
}

function errorText(t: TFunction, error: AgentControlError | undefined): string | undefined {
	if (!error) return undefined;
	return error.kind === "finished" ? t("subagents.error.finished") : error.message ?? t("subagents.error.failed");
}

const SubagentCardView = memo(function SubagentCardView({
	card,
	control,
	expanded,
	onAsk,
	onMessage,
	onOpen,
	onStop,
	onStopTurn,
	onToggle,
}: {
	card: SubagentCard;
	control: AgentControlState;
	expanded: boolean;
	onAsk?: (card: SubagentCard) => void;
	/** Present while the subagent runs and the daemon can reach it. */
	onMessage?: (toolUseId: string, text: string) => Promise<boolean>;
	onOpen: (id: string) => void;
	/** Stops this subagent only. */
	onStop?: (toolUseId: string) => Promise<void>;
	/** The fallback where a subagent cannot be stopped on its own. */
	onStopTurn?: () => void;
	onToggle: (id: string) => void;
}) {
	const { t } = useTranslation();
	const detailsId = useId();
	const toggleRef = useRef<HTMLButtonElement>(null);
	const fieldRef = useRef<HTMLInputElement>(null);
	const focusField = useFieldFocus(expanded, fieldRef);
	const title = card.title ?? t("subagents.untitled");
	const nowLine = card.lane === "needs_you"
		? card.blockedOn ? t("subagents.waitingFor", { what: card.blockedOn }) : t("subagents.waiting")
		: card.nowLine;
	const stopping = card.lane !== "done" && control.stopping;
	const sent = useMemo(() => withPendingMessages(card.inbox, control.pending), [card.inbox, control.pending]);
	const stopError = errorText(t, control.stopError);
	return (
		<article
			aria-label={title}
			className={cn(
				"group/subagent relative w-full min-w-0 rounded-lg border bg-surface text-left transition-colors duration-[120ms] ease-out hover:bg-interactive-hover focus-within:bg-interactive-hover",
				card.lane === "needs_you" ? "border-status-needs-you" : "border-border",
			)}
			data-lane={card.lane}
			data-outcome={card.outcome}
			data-testid="subagent-card"
		>
			<button
				aria-controls={expanded ? detailsId : undefined}
				aria-expanded={expanded}
				className="flex w-full min-w-0 items-start gap-2.5 rounded-lg px-3.5 pb-2 pt-2.5 text-left outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent/50"
				onClick={() => onToggle(card.id)}
				ref={toggleRef}
				type="button"
			>
				<StatusGlyph outcome={card.outcome} />
				<span className="flex min-w-0 flex-1 flex-col gap-0.5">
					<span className="line-clamp-2 text-sm-md font-semibold leading-tight tracking-tight text-foreground" title={title}>
						{title}
					</span>
					{card.subagentType ? <span className="truncate text-2xs text-muted-foreground">{card.subagentType}</span> : null}
					{nowLine ? (
						<span
							className={cn("mt-1 line-clamp-2 text-xs leading-snug", card.lane === "needs_you" ? "text-foreground" : "text-muted-foreground")}
							data-testid="subagent-now"
						>
							{nowLine}
						</span>
					) : null}
				</span>
				<ChevronRight
					aria-hidden="true"
					className={cn("mt-0.5 size-3 shrink-0 text-muted-foreground/60 transition-transform motion-reduce:transition-none", expanded && "rotate-90")}
				/>
			</button>
			<div className="flex h-8 min-w-0 items-center gap-2 border-t border-border px-3.5 text-2xs text-muted-foreground">
				<Elapsed durationMs={card.durationMs} live={card.lane !== "done"} startedAt={card.startedAt} />
				<span className="tabular-nums">{t("subagents.tools", { count: card.toolCount })}</span>
				{stopping ? <span role="status">{t("subagents.stopping")}</span> : null}
				{/* One action slot, revealed on hover or focus without moving anything. */}
				<span
					className="ml-auto flex items-center gap-0.5 opacity-0 transition-opacity duration-[120ms] group-hover/subagent:opacity-100 group-focus-within/subagent:opacity-100 data-[open]:opacity-100 motion-reduce:transition-none"
					data-open={expanded ? "" : undefined}
				>
					{onMessage ? (
						<CardAction
							disabled={stopping}
							label={t("subagents.message")}
							onClick={() => {
								focusField();
								onOpen(card.id);
							}}
						>
							<MessageSquare aria-hidden="true" className="size-3.5" />
						</CardAction>
					) : null}
					{onAsk ? (
						<CardAction label={t("subagents.ask")} onClick={() => onAsk(card)}>
							<MessageSquareReply aria-hidden="true" className="size-3.5" />
						</CardAction>
					) : null}
					{onStop ? (
						<CardAction
							destructive
							disabled={stopping}
							hint={t("subagents.stopSubagentHint")}
							label={t("subagents.stopSubagent")}
							onClick={() => void onStop(card.providerItemId)}
						>
							<Square aria-hidden="true" className="size-3" />
						</CardAction>
					) : onStopTurn ? (
						<CardAction destructive hint={t("subagents.stopTurnHint")} label={t("subagents.stopTurn")} onClick={onStopTurn}>
							<Square aria-hidden="true" className="size-3" />
						</CardAction>
					) : null}
				</span>
			</div>
			{stopError ? (
				<p className="border-t border-border px-3.5 py-2 text-xs text-destructive" role="alert">{stopError}</p>
			) : null}
			{expanded ? (
				<SubagentDetails
					card={card}
					field={onMessage ? (
						<AgentMessageField
							disabled={stopping}
							error={errorText(t, control.sendError)}
							fieldRef={fieldRef}
							label={t("subagents.message")}
							onCancel={() => toggleRef.current?.focus()}
							onSend={(text) => onMessage(card.providerItemId, text)}
							sending={control.sending}
						/>
					) : null}
					id={detailsId}
					sent={sent}
				/>
			) : null}
		</article>
	);
});

/**
 * The main agent: what it is doing now and its latest decision, with the turn's
 * status and decision lines when opened. Pinned above the lanes, so it is there
 * before any subagent starts.
 */
const MainAgentCardView = memo(function MainAgentCardView({
	control,
	expanded,
	onOpen,
	onSteer,
	onStopTurn,
	onToggle,
	summary,
}: {
	control: AgentControlState;
	expanded: boolean;
	onOpen: (id: string) => void;
	/** Steers the running turn; present only while it runs and can be steered. */
	onSteer?: (text: string) => Promise<boolean>;
	/** Present only while the turn runs. */
	onStopTurn?: () => void;
	onToggle: (id: string) => void;
	summary: MainAgentSummary;
}) {
	const { t } = useTranslation();
	const detailsId = useId();
	const toggleRef = useRef<HTMLButtonElement>(null);
	const fieldRef = useRef<HTMLInputElement>(null);
	const focusField = useFieldFocus(expanded, fieldRef);
	const title = t("subagents.main");
	const needsYou = summary.state === "needs_you";
	const nowLine = needsYou
		? summary.blockedOn ? t("subagents.waitingFor", { what: summary.blockedOn }) : t("subagents.waiting")
		: summary.nowLine;
	const decision = summary.decisions.at(-1);
	const sent = useMemo(() => withPendingMessages(summary.sent, control.pending), [summary.sent, control.pending]);
	return (
		<article
			aria-label={title}
			className={cn(
				"group/subagent relative w-full min-w-0 rounded-lg border bg-surface text-left transition-colors duration-[120ms] ease-out hover:bg-interactive-hover focus-within:bg-interactive-hover",
				needsYou ? "border-status-needs-you" : "border-border",
			)}
			data-state={summary.state}
			data-testid="main-agent-card"
		>
			<button
				aria-controls={expanded ? detailsId : undefined}
				aria-expanded={expanded}
				className="flex w-full min-w-0 items-start gap-2.5 rounded-lg px-3.5 pb-2 pt-2.5 text-left outline-none focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent/50"
				onClick={() => onToggle(MAIN_CARD_ID)}
				ref={toggleRef}
				type="button"
			>
				<StatusGlyph outcome={summary.state} />
				<span className="flex min-w-0 flex-1 flex-col gap-0.5">
					<span className="truncate text-sm-md font-semibold leading-tight tracking-tight text-foreground">{title}</span>
					{nowLine ? (
						<span
							className={cn("mt-1 line-clamp-2 text-xs leading-snug", needsYou ? "text-foreground" : "text-muted-foreground")}
							data-testid="main-agent-now"
						>
							{nowLine}
						</span>
					) : null}
					{decision ? (
						<span className="mt-0.5 line-clamp-2 text-sm font-medium leading-snug text-foreground" data-testid="main-agent-decision">
							<span className="mr-1.5 text-2xs font-normal text-muted-foreground">{t("subagents.decision")}</span>
							{decision.text}
						</span>
					) : null}
				</span>
				<ChevronRight
					aria-hidden="true"
					className={cn("mt-0.5 size-3 shrink-0 text-muted-foreground/60 transition-transform motion-reduce:transition-none", expanded && "rotate-90")}
				/>
			</button>
			{summary.startedAt ? (
				<div className="flex h-8 min-w-0 items-center gap-2 border-t border-border px-3.5 text-2xs text-muted-foreground">
					<Elapsed live separated={false} startedAt={summary.startedAt} />
					<span
						className="ml-auto flex items-center gap-0.5 opacity-0 transition-opacity duration-[120ms] group-hover/subagent:opacity-100 group-focus-within/subagent:opacity-100 data-[open]:opacity-100 motion-reduce:transition-none"
						data-open={expanded ? "" : undefined}
					>
						{onSteer ? (
							<CardAction
								label={t("subagents.steerMain")}
								onClick={() => {
									focusField();
									onOpen(MAIN_CARD_ID);
								}}
							>
								<CornerUpRight aria-hidden="true" className="size-3.5" />
							</CardAction>
						) : null}
						{onStopTurn ? (
							<CardAction destructive hint={t("subagents.stopTurnHint")} label={t("subagents.stopTurn")} onClick={onStopTurn}>
								<Square aria-hidden="true" className="size-3" />
							</CardAction>
						) : null}
					</span>
				</div>
			) : null}
			{expanded ? (
				<div className="flex min-w-0 flex-col gap-3 border-t border-border px-3.5 pb-3 pt-2.5" id={detailsId}>
					<NotesTimeline className="board-scrollbar max-h-64 overflow-y-auto" notes={summary.notes} sent={sent} />
					{onSteer ? (
						<AgentMessageField
							error={errorText(t, control.sendError)}
							fieldRef={fieldRef}
							label={t("subagents.steerMain")}
							onCancel={() => toggleRef.current?.focus()}
							onSend={onSteer}
							sending={control.sending}
						/>
					) : null}
				</div>
			) : null}
		</article>
	);
});

/**
 * One line to step in with. Enter sends; Escape clears the field, then hands focus
 * back to the card, and never stops anything. A send that did not go through
 * puts its words back.
 */
function AgentMessageField({
	disabled = false,
	error,
	fieldRef,
	label,
	onCancel,
	onSend,
	sending,
}: {
	disabled?: boolean;
	error?: string;
	fieldRef: RefObject<HTMLInputElement | null>;
	label: string;
	onCancel: () => void;
	/** Resolves true once the message went out. */
	onSend: (text: string) => Promise<boolean>;
	sending: boolean;
}) {
	const { t } = useTranslation();
	const id = useId();
	const errorId = useId();
	const [draft, setDraft] = useState("");
	const draftRef = useRef(draft);
	draftRef.current = draft;
	// Two Enters before the next render must not send twice.
	const inFlight = useRef(false);
	const blocked = disabled || sending;
	const submit = async () => {
		const text = draftRef.current.trim();
		if (!text || blocked || inFlight.current) return;
		inFlight.current = true;
		draftRef.current = "";
		setDraft("");
		try {
			const sent = await onSend(text);
			if (!sent && !draftRef.current) setDraft(text);
		} finally {
			inFlight.current = false;
		}
	};
	const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
		if (event.key === "Enter" && !event.nativeEvent.isComposing) {
			event.preventDefault();
			void submit();
			return;
		}
		if (event.key !== "Escape") return;
		// The board folds its cards on Escape, and the chat stops the turn; neither here.
		event.preventDefault();
		event.stopPropagation();
		if (draft) setDraft("");
		else onCancel();
	};
	return (
		<div className="min-w-0">
			<Label className="text-caption font-medium text-muted-foreground" htmlFor={id}>{label}</Label>
			{/* The global `font: inherit` on inputs outranks a utility, so the row sets the size. */}
			<div className="mt-1.5 flex items-center gap-1 text-xs">
				<Input
					aria-describedby={error ? errorId : undefined}
					aria-invalid={error ? true : undefined}
					autoComplete="off"
					className="h-control-md flex-1 px-2.5 text-xs focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
					disabled={disabled}
					enterKeyHint="send"
					id={id}
					onChange={(event) => setDraft(event.target.value)}
					onKeyDown={onKeyDown}
					placeholder={t("subagents.messagePlaceholder")}
					ref={fieldRef}
					value={draft}
				/>
				<CardAction disabled={blocked || !draft.trim()} label={t("subagents.send")} onClick={() => void submit()}>
					{sending ? (
						<Loader2 aria-hidden="true" className="size-3.5 animate-spin motion-reduce:animate-none" />
					) : (
						<ArrowUp aria-hidden="true" className="size-3.5" />
					)}
				</CardAction>
			</div>
			{error ? (
				<p className="mt-1.5 text-xs text-destructive" id={errorId} role="alert">{error}</p>
			) : null}
		</div>
	);
}

const OUTCOME_LABEL_KEYS = {
	working: "subagents.state.working",
	needs_you: "subagents.state.needsYou",
	idle: "subagents.state.idle",
	completed: "subagents.state.completed",
	failed: "subagents.state.failed",
	stopped: "subagents.state.stopped",
} as const satisfies Record<SubagentOutcome | MainAgentState, string>;

function StatusGlyph({ outcome }: { outcome: SubagentOutcome | MainAgentState }) {
	const { t } = useTranslation();
	const label = t(OUTCOME_LABEL_KEYS[outcome]);
	return (
		<span className="mt-0.5 flex size-4 shrink-0 items-center justify-center" data-testid="subagent-glyph">
			{outcome === "working" ? (
				<Loader2 aria-hidden="true" className="size-3.5 animate-spin text-status-working motion-reduce:animate-none" />
			) : outcome === "needs_you" ? (
				<span aria-hidden="true" className="size-2 rounded-full bg-status-needs-you" />
			) : outcome === "idle" ? (
				<span aria-hidden="true" className="size-2 rounded-full bg-status-idle" />
			) : outcome === "completed" ? (
				<Check aria-hidden="true" className="size-3.5 text-muted-foreground" />
			) : outcome === "failed" ? (
				<CircleX aria-hidden="true" className="size-3.5 text-destructive" />
			) : (
				<Square aria-hidden="true" className="size-3 text-muted-foreground" />
			)}
			<span className="sr-only">{label}</span>
		</span>
	);
}

function CardAction({
	children,
	destructive = false,
	disabled = false,
	hint,
	label,
	onClick,
}: {
	children: ReactNode;
	destructive?: boolean;
	disabled?: boolean;
	hint?: string;
	label: string;
	onClick: () => void;
}) {
	return (
		<Tooltip>
			<TooltipTrigger asChild>
				<Button
					aria-label={label}
					className={cn(
						"size-6 text-muted-foreground hover:text-foreground focus-visible:outline-2 focus-visible:outline-accent/50",
						destructive && "hover:text-destructive focus-visible:text-destructive",
					)}
					disabled={disabled}
					onClick={onClick}
					size="icon-sm"
					type="button"
					variant="ghost"
				>
					{children}
				</Button>
			</TooltipTrigger>
			<TooltipContent side="bottom">{hint ?? label}</TooltipContent>
		</Tooltip>
	);
}

/** A live card counts from its start; a settled one shows its duration. */
function Elapsed({
	durationMs,
	live,
	separated = true,
	startedAt: started,
}: {
	durationMs?: number;
	live: boolean;
	/** Followed by more metadata, so it ends in a separator. */
	separated?: boolean;
	startedAt: string;
}) {
	const startedAt = Date.parse(started);
	const now = useNow(live, Number.isFinite(startedAt) ? startedAt : undefined);
	const ms = live ? (Number.isFinite(startedAt) ? Math.max(0, now - startedAt) : undefined) : durationMs;
	if (ms === undefined) return null;
	return (
		<>
			<span className="tabular-nums">{formatDuration(ms)}</span>
			{separated ? <span aria-hidden="true" className="text-border-strong">·</span> : null}
		</>
	);
}

/** Wall time for a live card: every second for the first minute, then every fifteen. */
function useNow(active: boolean, since: number | undefined): number {
	const [now, setNow] = useState(() => Date.now());
	const coarse = since !== undefined && now - since >= 60_000;
	useEffect(() => {
		if (!active) return;
		setNow(Date.now());
		const timer = window.setInterval(() => setNow(Date.now()), coarse ? 15_000 : 1_000);
		return () => window.clearInterval(timer);
	}, [active, coarse]);
	return now;
}

const SENT_STATE_KEYS = {
	sending: "subagents.sent.sending",
	queued: "subagents.sent.queued",
	delivered: "subagents.sent.delivered",
	dropped: "subagents.sent.dropped",
} as const satisfies Record<SentMessageState, string>;

/**
 * Status and decision lines in the order they were written, decisions emphasized,
 * with what the person sent placed by when they sent it.
 */
function NotesTimeline({
	className,
	notes,
	sent = NO_MESSAGES,
}: {
	className?: string;
	notes: readonly SubagentNote[];
	sent?: readonly SentMessage[];
}) {
	const { t } = useTranslation();
	const entries = useMemo(() => boardTimeline(notes, sent), [notes, sent]);
	return (
		<section aria-label={t("subagents.timeline")}>
			<h4 className="text-caption font-medium text-muted-foreground">{t("subagents.timeline")}</h4>
			{entries.length > 0 ? (
				<ol className={cn("mt-1.5 flex flex-col gap-1.5", className)}>
					{entries.map((entry, index) => {
						if (entry.kind === "sent") {
							const { message } = entry;
							return (
								<li
									className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2 text-xs leading-snug"
									data-sent-state={message.state}
									data-testid="sent-message"
									key={`sent:${message.id}`}
								>
									<time className="pt-px text-2xs tabular-nums text-passive" dateTime={message.at}>{formatTime(message.at)}</time>
									<span className="min-w-0 break-words text-foreground">
										<span className="mr-1.5 text-2xs text-muted-foreground">{t("subagents.you")}</span>
										{message.text}
										<span className={cn("ml-1.5 text-2xs", message.state === "dropped" ? "text-muted-foreground" : "text-passive")}>
											<span aria-hidden="true">· </span>
											{t(SENT_STATE_KEYS[message.state])}
										</span>
									</span>
								</li>
							);
						}
						const { note } = entry;
						return (
							<li
								className={cn("grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-2 leading-snug", note.kind === "decision" ? "text-sm" : "text-xs")}
								data-note-kind={note.kind}
								key={`note:${index}`}
							>
								<time className="text-2xs tabular-nums text-passive" dateTime={note.at}>{formatTime(note.at)}</time>
								<span className={cn("min-w-0 break-words", note.kind === "decision" ? "font-medium text-foreground" : "text-muted-foreground")}>
									<span className={note.kind === "decision" ? "mr-1.5 text-2xs font-normal text-muted-foreground" : "sr-only"}>
										{note.kind === "decision" ? t("subagents.decision") : t("subagents.status")}
									</span>
									{note.text}
								</span>
							</li>
						);
					})}
				</ol>
			) : (
				<p className="mt-1 text-xs text-passive">{t("subagents.noUpdates")}</p>
			)}
		</section>
	);
}

function SubagentDetails({
	card,
	field,
	id,
	sent,
}: {
	card: SubagentCard;
	/** The message field, while the subagent can still be reached. */
	field?: ReactNode;
	id: string;
	sent: readonly SentMessage[];
}) {
	const { t } = useTranslation();
	const [taskOpen, setTaskOpen] = useState(false);
	const reportLabel = card.outcome === "failed" ? t("subagents.failure") : t("subagents.report");
	return (
		<div className="flex min-w-0 flex-col gap-3 border-t border-border px-3.5 pb-3 pt-2.5" id={id}>
			<NotesTimeline notes={card.notes} sent={sent} />
			{field}
			{card.prompt ? (
				<div className="min-w-0">
					<button
						aria-expanded={taskOpen}
						className="flex items-center gap-1 rounded-sm text-caption font-medium text-muted-foreground outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-accent/50"
						onClick={() => setTaskOpen((open) => !open)}
						type="button"
					>
						<ChevronRight aria-hidden="true" className={cn("size-3 transition-transform motion-reduce:transition-none", taskOpen && "rotate-90")} />
						{t("subagents.task")}
					</button>
					{taskOpen ? (
						<p className="board-scrollbar mt-1.5 max-h-48 overflow-y-auto whitespace-pre-wrap break-words text-xs leading-relaxed text-muted-foreground">
							{card.prompt}
						</p>
					) : null}
				</div>
			) : null}
			{card.lane === "done" && card.finalReport ? (
				<section aria-label={reportLabel} className="min-w-0">
					<h4 className="text-caption font-medium text-muted-foreground">{reportLabel}</h4>
					<div className="board-scrollbar mt-1.5 max-h-80 overflow-y-auto text-xs">
						<ChatMarkdown text={card.finalReport} />
					</div>
				</section>
			) : null}
		</div>
	);
}
