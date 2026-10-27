/**
 * Every agent in one view: projects, then orchestrator sessions with the worker
 * sessions they spawned nested under them, then each session's subagents, each
 * row with what it is doing now and its latest updates, newest first.
 *
 * The rows read what the agents board derives for a session (`useSubagentBoard`);
 * nothing here parses a conversation. The shared `AgentSummariesProvider` loads
 * the most recently active chat sessions, with the same loaders the board's
 * cards read, and they stay live through the same event invalidation the
 * session's own chat uses.
 *
 * A live chat session can be stepped into from its row, with the same controls
 * as its Agents tab ({@link useSessionAgentActions}): its running turn steered or
 * stopped, a turn that ended answered with a new message, and each running
 * subagent messaged or stopped on its own.
 *
 * A folded row keeps one height whatever it says, so streaming updates never
 * move the rows around it; an opened one scrolls inside a fixed height.
 */

import { createContext, memo, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Bot, ChevronRight, CornerUpRight, Folder, MessageSquare, Square } from "lucide-react";
import { ScrollArea } from "radix-ui";
import { EMPTY_AGENT_CONTROL, MAIN_AGENT_CONTROL_KEY, type AgentControlError, type AgentControlState } from "../hooks/useAgentBoardControls";
import { useSessionAgentActions } from "../hooks/useSessionAgentActions";
import { useStableArray } from "../hooks/useStable";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import {
	buildAgentsOverview,
	overviewSessionsToLoad,
	type AgentsOverview as AgentsOverviewModel,
	type OverviewDetail,
	type OverviewProject,
	type OverviewSessionNode,
} from "../lib/agents-overview";
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { hidesShellTopbar } from "../lib/platform";
import {
	boardTimeline,
	withPendingMessages,
	withStopRequests,
	type BoardTimelineEntry,
	type MainAgentState,
	type MainAgentSummary,
	type SentMessage,
	type SubagentCard,
	type SubagentNote,
} from "../lib/subagent-board";
import { cn } from "../lib/utils";
import { isOrchestratorSession, type WorkspaceSession } from "../types/workspace";
import { useAgentSummaries, useAgentSummaryDemand } from "./AgentSummariesProvider";
import { formatDuration, formatTime } from "./chat/ChatTimelineItems";
import {
	AgentMessageField,
	CardAction,
	errorText,
	mainNowLine,
	NotesTimeline,
	SENT_STATE_KEYS,
	StatusGlyph,
	subagentNowLine,
	useFieldFocus,
} from "./chat/SubagentBoard";
import { NAV_ROW_HIGHLIGHT_HOST_CLASS, NavRowHighlight } from "./NavRowHighlight";
import { topbarProjectLabelClass } from "./TopbarButton";

/** Inline updates per row, each one line; the full timeline opens below it. */
const INLINE_UPDATES = 3;

/** The overview route: asks for what it shows, then hands it to {@link AgentsOverviewView}. */
export function AgentsOverview() {
	const workspaceQuery = useWorkspaceQuery();
	const workspaces = workspaceQuery.data;
	const navigateToSession = useNavigateToSession();
	useAgentSummaryDemand(useStableArray(useMemo(() => overviewSessionsToLoad(workspaces ?? []), [workspaces])));
	const details = useAgentSummaries();
	const overview = useMemo(() => buildAgentsOverview(workspaces ?? [], details), [details, workspaces]);
	const openSession = useCallback((session: WorkspaceSession) => navigateToSession(session.workspaceId, session.id), [navigateToSession]);
	return (
		<AgentsOverviewView
			error={workspaceQuery.error ? workspaceQuery.error.message : undefined}
			isLoading={workspaceQuery.isLoading}
			onOpenSession={openSession}
			overview={overview}
		/>
	);
}

export interface AgentsOverviewViewProps {
	overview: AgentsOverviewModel;
	isLoading?: boolean;
	error?: string;
	onOpenSession: (session: WorkspaceSession) => void;
}

/** One page-wide clock, so live rows count up without a timer of their own. */
const NowContext = createContext(0);

function Clock({ children, live }: { children: ReactNode; live: boolean }) {
	const [now, setNow] = useState(() => Date.now());
	useEffect(() => {
		setNow(Date.now());
		const timer = window.setInterval(() => setNow(Date.now()), live ? 1_000 : 30_000);
		return () => window.clearInterval(timer);
	}, [live]);
	return <NowContext.Provider value={now}>{children}</NowContext.Provider>;
}

export function AgentsOverviewView({ error, isLoading = false, onOpenSession, overview }: AgentsOverviewViewProps) {
	const { t } = useTranslation();
	const showPageTitle = hidesShellTopbar();
	const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
	const toggle = useCallback((id: string) => {
		setExpanded((current) => {
			const next = new Set(current);
			if (!next.delete(id)) next.add(id);
			return next;
		});
	}, []);
	const hasSessions = overview.projects.length > 0;
	return (
		<div className="flex min-h-0 flex-1 flex-col bg-background" data-testid="agents-overview">
			<header className="workspace-topbar-container center-panel-titlebar flex h-toolbar shrink-0 items-center justify-between gap-3 border-b border-border-strong pr-[calc((var(--size-toolbar)-var(--size-topbar-control))/2)]">
				<h1 className={showPageTitle ? cn(topbarProjectLabelClass, "inline-flex items-center gap-1.5") : "sr-only"}>
					{showPageTitle ? <Bot aria-hidden="true" className="size-icon-md" /> : null}
					{t("agentsOverview.title")}
				</h1>
				{overview.liveSessions > 0 ? (
					<p className="flex shrink-0 items-center gap-3 text-sm tabular-nums text-muted-foreground" data-testid="agents-overview-summary">
						{overview.needsYou > 0 ? (
							<span className="inline-flex items-center gap-1.5 text-foreground">
								<span aria-hidden="true" className="size-2 rounded-full bg-status-needs-you" />
								{t("agentsOverview.needsYou", { count: overview.needsYou })}
							</span>
						) : null}
						<span className="inline-flex items-center gap-1.5">
							<span aria-hidden="true" className="size-2 rounded-full bg-status-working" />
							{t("agentsOverview.working", { count: overview.working })}
						</span>
					</p>
				) : null}
			</header>
			<ScrollArea.Root asChild scrollHideDelay={500} type="hover">
				<main className="relative min-h-0 flex-1 overflow-hidden">
					<ScrollArea.Viewport className="h-full w-full overscroll-contain focus-visible:outline-none">
						<div className="mx-auto w-full max-w-5xl p-4">
							{isLoading ? (
								<p className="px-3 py-8 text-center text-sm text-muted-foreground" role="status">{t("agentsOverview.loading")}</p>
							) : error ? (
								<p className="px-3 py-8 text-center text-sm text-destructive" role="alert">{error}</p>
							) : (
								<Clock live={overview.working > 0}>
									{overview.liveSessions === 0 ? (
										<div className="flex flex-col items-center gap-1.5 px-3 py-16 text-center" data-testid="agents-overview-empty" role="status">
											<p className="text-heading-sm font-medium text-foreground">{t("agentsOverview.empty.title")}</p>
											<p className="text-sm text-muted-foreground">{t("agentsOverview.empty.description")}</p>
										</div>
									) : null}
									{hasSessions ? (
										<div className="flex flex-col gap-6">
											{overview.projects.map((project) => (
												<ProjectSection expanded={expanded} key={project.id} onOpenSession={onOpenSession} onToggle={toggle} project={project} />
											))}
										</div>
									) : null}
								</Clock>
							)}
						</div>
					</ScrollArea.Viewport>
					<ScrollArea.Scrollbar className="z-10 flex w-2 touch-none select-none p-0.5" orientation="vertical">
						<ScrollArea.Thumb className="relative flex-1 rounded-full bg-[color-mix(in_srgb,var(--color-scrollbar)_42%,transparent)] hover:bg-[color-mix(in_srgb,var(--color-scrollbar)_56%,transparent)]" />
					</ScrollArea.Scrollbar>
				</main>
			</ScrollArea.Root>
		</div>
	);
}

interface TreeProps {
	expanded: ReadonlySet<string>;
	onOpenSession: (session: WorkspaceSession) => void;
	onToggle: (id: string) => void;
}

function ProjectSection({ project, ...tree }: TreeProps & { project: OverviewProject }) {
	const { t } = useTranslation();
	const finishedKey = `finished:${project.id}`;
	const finishedOpen = tree.expanded.has(finishedKey);
	const finishedId = useId();
	return (
		<section aria-label={project.name} data-project-id={project.id} data-testid="agents-overview-project">
			<h2 className="flex h-8 items-center gap-2 px-3 text-sm font-medium text-muted-foreground">
				<Folder aria-hidden="true" className="size-icon-md shrink-0" />
				<span className="truncate text-foreground">{project.name}</span>
				<span className="tabular-nums text-passive">{project.sessions.length > 0 ? countTree(project.sessions) : null}</span>
			</h2>
			{project.sessions.length > 0 ? (
				<ul className="divide-y divide-border">
					{project.sessions.map((node) => <SessionBranch key={node.session.id} node={node} {...tree} />)}
				</ul>
			) : null}
			{project.finished.length > 0 ? (
				<div className={cn(project.sessions.length > 0 && "border-t border-border")}>
					<button
						aria-controls={finishedOpen ? finishedId : undefined}
						aria-expanded={finishedOpen}
						className={cn("group/nav-row relative flex h-9 w-full items-center gap-2 rounded-lg px-3 text-left text-sm text-muted-foreground outline-none hover:text-foreground", NAV_ROW_HIGHLIGHT_HOST_CLASS)}
						onClick={() => tree.onToggle(finishedKey)}
						type="button"
					>
						<NavRowHighlight />
						<ChevronRight aria-hidden="true" className={cn("relative z-[1] size-3.5 shrink-0 transition-transform motion-reduce:transition-none", finishedOpen && "rotate-90")} />
						<span className="relative z-[1]">{t("agentsOverview.finished")}</span>
						<span className="relative z-[1] tabular-nums text-passive">{countTree(project.finished)}</span>
					</button>
					{finishedOpen ? (
						<ul className="divide-y divide-border border-t border-border" id={finishedId}>
							{project.finished.map((node) => <SessionBranch key={node.session.id} node={node} {...tree} />)}
						</ul>
					) : null}
				</div>
			) : null}
		</section>
	);
}

function countTree(nodes: readonly OverviewSessionNode[]): number {
	let count = 0;
	for (const node of nodes) count += 1 + countTree(node.children);
	return count;
}

const subagentKey = (card: SubagentCard) => `subagent:${card.id}`;
const NO_SENT: readonly SentMessage[] = [];

/** A session, its subagents one level in, then the sessions it spawned. */
function SessionBranch({ node, ...tree }: TreeProps & { node: OverviewSessionNode }) {
	const sessionOpen = tree.expanded.has(node.session.id);
	const nested = node.subagents.length > 0 || node.children.length > 0;
	// Only a chat session the shared summaries hold has actions; any other row opens its session.
	const actions = useSessionAgentActions(node.detail === "chat" ? node.session.id : undefined);
	const { states } = actions;
	// A subagent the person stopped reads as stopped once it settles.
	const subagents = useMemo(() => {
		const stopped = new Set<string>();
		for (const [key, state] of states) if (state.stopped && key !== MAIN_AGENT_CONTROL_KEY) stopped.add(key);
		return withStopRequests(node.subagents, stopped);
	}, [node.subagents, states]);
	const { onOpenSession } = tree;
	const { session } = node;
	const openSession = useCallback(() => onOpenSession(session), [onOpenSession, session]);
	return (
		<li data-session-id={node.session.id} data-testid="agents-overview-branch">
			<SessionRow
				control={states.get(MAIN_AGENT_CONTROL_KEY) ?? EMPTY_AGENT_CONTROL}
				detail={node.detail}
				expanded={sessionOpen}
				main={node.main}
				mainMessage={actions.mainMessage}
				onOpenSession={openSession}
				onSend={actions.steerMain ?? actions.sendMain}
				onStop={actions.stopMain}
				onToggle={tree.onToggle}
				session={node.session}
				state={node.state}
			/>
			{nested ? (
				<ul className="ms-8 divide-y divide-border border-t border-border">
					{subagents.map((card) => {
						const live = card.lane !== "done";
						return (
							<li data-testid="agents-overview-subagent-item" key={card.id}>
								<SubagentRow
									card={card}
									control={states.get(card.providerItemId) ?? EMPTY_AGENT_CONTROL}
									expanded={tree.expanded.has(subagentKey(card))}
									onMessage={live ? actions.messageSubagent : undefined}
									onOpenSession={openSession}
									onStop={live ? actions.stopSubagent : undefined}
									onToggle={tree.onToggle}
								/>
							</li>
						);
					})}
					{node.children.map((child) => <SessionBranch key={child.session.id} node={child} {...tree} />)}
				</ul>
			) : null}
		</li>
	);
}

// A click anywhere in the row does what its title does; the title stays the keyboard target.
const ROW_CLASS = cn("group/nav-row relative flex min-w-0 cursor-pointer items-start gap-2.5 rounded-lg px-3 py-2.5", NAV_ROW_HIGHLIGHT_HOST_CLASS);
/** Shared with the PRs page's session rows. */
export const ROW_TITLE_CLASS = "min-w-0 truncate rounded-sm text-left leading-tight tracking-tight text-foreground outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent/50";
// Under the row, aligned with its text past the disclosure and the glyph.
const ROW_BELOW_CLASS = "ps-18 pe-3 pb-3";
// An opened row scrolls its updates inside one height rather than growing with them.
const OPEN_TIMELINE_CLASS = "board-scrollbar h-80 overflow-y-auto";

/** A row's chevron that opens its detail below; shared with the PRs page's rows. */
export function DisclosureButton({
	controls,
	expanded,
	label,
	onClick,
	ref,
}: {
	controls: string;
	expanded: boolean;
	label: string;
	onClick: () => void;
	ref?: RefObject<HTMLButtonElement | null>;
}) {
	return (
		<button
			aria-controls={expanded ? controls : undefined}
			aria-expanded={expanded}
			aria-label={label}
			className="relative z-[1] -my-0.5 grid size-6 shrink-0 place-items-center rounded-md text-muted-foreground outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-accent/50"
			onClick={(event) => {
				// The row's own click opens the session.
				event.stopPropagation();
				onClick();
			}}
			ref={ref}
			type="button"
		>
			<ChevronRight aria-hidden="true" className={cn("size-3.5 transition-transform motion-reduce:transition-none", expanded && "rotate-90")} />
		</button>
	);
}

/**
 * A row's actions, shown on hover or focus. Every row holds the slot at one
 * width, whatever it offers right now or whether it offers anything, so the
 * time column lines up and nothing moves when the actions show or change.
 * Clicking an action does not open or fold the row.
 */
function ActionSlot({ children, open }: { children?: ReactNode; open: boolean }) {
	return (
		<span
			className="relative z-[1] -my-1 flex w-[3.125rem] shrink-0 items-center justify-end gap-0.5 self-center opacity-0 transition-opacity duration-[120ms] group-hover/nav-row:opacity-100 group-focus-within/nav-row:opacity-100 data-[open]:opacity-100 motion-reduce:transition-none"
			data-open={open ? "" : undefined}
			data-testid="agents-overview-actions"
			onClick={(event) => {
				if (event.target instanceof Element && event.target.closest("button")) event.stopPropagation();
			}}
		>
			{children}
		</span>
	);
}

/**
 * A row's message field: opened from its action and focused once it shows,
 * closed by Escape, which hands focus back to the row. Hidden while nothing can
 * be sent.
 */
function useRowComposer(available: boolean, returnFocus: RefObject<HTMLButtonElement | null>) {
	const [composing, setComposing] = useState(false);
	const open = composing && available;
	const fieldRef = useRef<HTMLInputElement>(null);
	const focusField = useFieldFocus(open, fieldRef);
	const begin = useCallback(() => {
		focusField();
		setComposing(true);
	}, [focusField]);
	const close = useCallback(() => {
		setComposing(false);
		returnFocus.current?.focus();
	}, [returnFocus]);
	return { open, fieldRef, begin, close };
}

/** What went wrong, in words; an agent that is not running is restarted by opening its session. */
function controlErrorText(t: TFunction, error: AgentControlError | undefined): string | undefined {
	return error?.kind === "not_running" ? t("agentsOverview.notRunning") : errorText(t, error);
}

/** Opens the session, where its agent can be restarted. */
function OpenSessionLink({ onOpen }: { onOpen: () => void }) {
	const { t } = useTranslation();
	return (
		<button
			className="rounded-sm font-medium text-foreground underline underline-offset-2 outline-none hover:no-underline focus-visible:outline-2 focus-visible:outline-accent/50"
			onClick={onOpen}
			type="button"
		>
			{t("agentsOverview.openSessionLink")}
		</button>
	);
}

/**
 * Under a row: its message field while it is open, and why a stop did not go
 * through. Nothing here opens or folds the row.
 */
function RowControls({
	composer,
	control,
	disabled,
	label,
	onOpenSession,
	onSend,
}: {
	composer: ReturnType<typeof useRowComposer>;
	control: AgentControlState;
	disabled: boolean;
	label: string;
	onOpenSession: () => void;
	onSend?: (text: string) => Promise<boolean>;
}) {
	const { t } = useTranslation();
	const stopError = controlErrorText(t, control.stopError);
	if (!(composer.open && onSend) && !stopError) return null;
	return (
		<div className={cn(ROW_BELOW_CLASS, "flex flex-col gap-1.5")} data-testid="agents-overview-controls">
			{composer.open && onSend ? (
				<AgentMessageField
					closeOnEscape
					disabled={disabled}
					error={controlErrorText(t, control.sendError)}
					errorAction={control.sendError?.kind === "not_running" ? <OpenSessionLink onOpen={onOpenSession} /> : undefined}
					fieldRef={composer.fieldRef}
					label={label}
					onCancel={composer.close}
					onSend={onSend}
					sending={control.sending}
				/>
			) : null}
			{stopError ? (
				<p className="text-sm text-destructive" role="alert">
					{stopError}
					{control.stopError?.kind === "not_running" ? <> <OpenSessionLink onOpen={onOpenSession} /></> : null}
				</p>
			) : null}
		</div>
	);
}

/** What the agent is doing now, in two lines that are kept even while it says less. */
function NowLine({ children, strong }: { children: ReactNode; strong: boolean }) {
	return (
		<p
			className={cn("line-clamp-2 h-[2lh] text-sm leading-snug", strong ? "text-foreground" : "text-muted-foreground")}
			data-testid="agents-overview-now"
		>
			{children}
		</p>
	);
}

const SessionRow = memo(function SessionRow({
	control,
	detail,
	expanded,
	main,
	mainMessage,
	onOpenSession,
	onSend,
	onStop,
	onToggle,
	session,
	state,
}: {
	control: AgentControlState;
	detail: OverviewDetail;
	expanded: boolean;
	main?: MainAgentSummary;
	/** What a message does: steer the running turn, or start a new one once it ended. */
	mainMessage?: "steer" | "send";
	onOpenSession: () => void;
	/** Resolves true once the message went out. */
	onSend?: (text: string) => Promise<boolean>;
	/** Interrupts the running turn. */
	onStop?: () => Promise<void>;
	onToggle: (id: string) => void;
	session: WorkspaceSession;
	state: MainAgentState;
}) {
	const { t } = useTranslation();
	const timelineId = useId();
	const titleRef = useRef<HTMLButtonElement>(null);
	const composer = useRowComposer(Boolean(onSend), titleRef);
	const nowLine = main ? mainNowLine(t, main) : session.displayStatus;
	const kind = isOrchestratorSession(session) ? t("agentsOverview.kind.orchestrator") : t("agentsOverview.kind.worker");
	// What was sent shows at once, until the conversation echoes it.
	const sent = useMemo(() => withPendingMessages(main?.sent ?? NO_SENT, control.pending), [control.pending, main?.sent]);
	const updates = useMemo(() => latestUpdates(main?.notes, sent, main?.nowSource === "status" ? main.nowLine : undefined), [main, sent]);
	const messageLabel = mainMessage === "steer" ? t("subagents.steerMain") : t("subagents.messageMain");
	const stopping = control.stopping;
	return (
		<>
			<div className={ROW_CLASS} data-detail={detail} data-state={state} data-testid="agents-overview-session" onClick={onOpenSession}>
				<NavRowHighlight />
				<DisclosureButton
					controls={timelineId}
					expanded={expanded}
					label={t("agentsOverview.updatesFor", { title: session.title })}
					onClick={() => onToggle(session.id)}
				/>
				<span className="relative z-[1]"><StatusGlyph outcome={state} /></span>
				<div className="relative z-[1] flex min-w-0 flex-1 flex-col gap-0.5">
					<div className="flex min-w-0 items-baseline gap-2">
						{/* Its click bubbles to the row, which opens the session. The global
						    `font: inherit` on buttons outranks a utility, so a span sets the type. */}
						<button aria-label={t("agentsOverview.openSession", { title: session.title })} className={ROW_TITLE_CLASS} ref={titleRef} title={session.title} type="button">
							<span className="text-subtitle font-semibold">{session.title}</span>
						</button>
						<span className="shrink-0 text-xs text-muted-foreground">{kind}</span>
						{stopping ? <span className="shrink-0 text-xs text-muted-foreground" role="status">{t("subagents.stopping")}</span> : null}
						<span className="ms-auto shrink-0 text-xs tabular-nums text-passive">
							{main?.startedAt ? <LiveDuration startedAt={main.startedAt} /> : <RelativeTime at={activityAt(session)} />}
						</span>
						<ActionSlot open={composer.open}>
							{onSend ? (
								<CardAction disabled={stopping} label={messageLabel} onClick={composer.begin}>
									{mainMessage === "steer"
										? <CornerUpRight aria-hidden="true" className="size-3.5" />
										: <MessageSquare aria-hidden="true" className="size-3.5" />}
								</CardAction>
							) : null}
							{onStop ? (
								<CardAction destructive disabled={stopping} hint={t("subagents.stopTurnHint")} label={t("subagents.stopTurn")} onClick={() => void onStop()}>
									<Square aria-hidden="true" className="size-3" />
								</CardAction>
							) : null}
						</ActionSlot>
					</div>
					<NowLine strong={state === "needs_you"}>{nowLine}</NowLine>
					{/* The open timeline below holds every update. */}
					{expanded ? null : <InlineUpdates entries={updates} note={<DetailNote detail={detail} />} />}
				</div>
			</div>
			<RowControls composer={composer} control={control} disabled={stopping} label={messageLabel} onOpenSession={onOpenSession} onSend={onSend} />
			{expanded ? (
				<div className={ROW_BELOW_CLASS} id={timelineId}>
					{main ? <NotesTimeline className={OPEN_TIMELINE_CLASS} notes={main.notes} sent={sent} /> : <DetailNote detail={detail} fallback />}
				</div>
			) : null}
		</>
	);
});

const SubagentRow = memo(function SubagentRow({
	card,
	control,
	expanded,
	onMessage,
	onOpenSession,
	onStop,
	onToggle,
}: {
	card: SubagentCard;
	control: AgentControlState;
	expanded: boolean;
	/** Present while the subagent runs and the daemon can reach it. */
	onMessage?: (toolUseId: string, text: string) => Promise<boolean>;
	onOpenSession: () => void;
	/** Stops this subagent only. */
	onStop?: (toolUseId: string) => Promise<void>;
	onToggle: (id: string) => void;
}) {
	const { t } = useTranslation();
	const timelineId = useId();
	const disclosureRef = useRef<HTMLButtonElement>(null);
	const composer = useRowComposer(Boolean(onMessage), disclosureRef);
	const title = card.title ?? t("subagents.untitled");
	const nowLine = subagentNowLine(t, card);
	const sent = useMemo(() => withPendingMessages(card.inbox, control.pending), [card.inbox, control.pending]);
	const updates = useMemo(() => latestUpdates(card.notes, sent, card.nowSource === "status" ? card.nowLine : undefined), [card, sent]);
	const stopping = card.lane !== "done" && control.stopping;
	const toggle = () => onToggle(subagentKey(card));
	const send = useCallback((text: string) => onMessage ? onMessage(card.providerItemId, text) : Promise.resolve(false), [card.providerItemId, onMessage]);
	return (
		<>
			<div className={ROW_CLASS} data-lane={card.lane} data-outcome={card.outcome} data-testid="agents-overview-subagent" onClick={toggle}>
				<NavRowHighlight />
				<DisclosureButton controls={timelineId} expanded={expanded} label={t("agentsOverview.updatesFor", { title })} onClick={toggle} ref={disclosureRef} />
				<span className="relative z-[1]"><StatusGlyph outcome={card.outcome} /></span>
				<div className="relative z-[1] flex min-w-0 flex-1 flex-col gap-0.5">
					<div className="flex min-w-0 items-baseline gap-2">
						{/* The disclosure is its keyboard control; the title is text. */}
						<span className={cn(ROW_TITLE_CLASS, "text-base font-semibold")} title={title}>
							{title}
						</span>
						<span className="shrink-0 text-xs text-muted-foreground">{card.subagentType ?? t("agentsOverview.kind.subagent")}</span>
						{stopping ? <span className="shrink-0 text-xs text-muted-foreground" role="status">{t("subagents.stopping")}</span> : null}
						<span className="ms-auto shrink-0 text-xs tabular-nums text-passive">
							{card.lane === "done"
								? card.durationMs !== undefined ? formatDuration(card.durationMs) : null
								: <LiveDuration startedAt={card.startedAt} />}
						</span>
						<ActionSlot open={composer.open}>
							{onMessage ? (
								<CardAction disabled={stopping} label={t("subagents.message")} onClick={composer.begin}>
									<MessageSquare aria-hidden="true" className="size-3.5" />
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
							) : null}
						</ActionSlot>
					</div>
					<NowLine strong={card.lane === "needs_you" || card.outcome === "blocked"}>{nowLine}</NowLine>
					{expanded ? null : <InlineUpdates entries={updates} />}
				</div>
			</div>
			<RowControls
				composer={composer}
				control={control}
				disabled={stopping}
				label={t("subagents.message")}
				onOpenSession={onOpenSession}
				onSend={onMessage ? send : undefined}
			/>
			{expanded ? (
				<div className={ROW_BELOW_CLASS} id={timelineId}>
					<NotesTimeline className={OPEN_TIMELINE_CLASS} notes={card.notes} sent={sent} />
				</div>
			) : null}
		</>
	);
});

type InlineUpdate = { entry: BoardTimelineEntry; order: number };
const NO_UPDATES: readonly InlineUpdate[] = [];

/**
 * The newest lines worth showing under the now line, newest first: decisions,
 * statuses and what the person sent, not an outcome or a turn's divider, nor
 * the status the now line already repeats. Each keeps its place from the oldest
 * as its key, so a new line re-keys nothing.
 */
function latestUpdates(
	notes: readonly SubagentNote[] | undefined,
	sent: readonly SentMessage[],
	nowLine: string | undefined,
	limit = INLINE_UPDATES,
): readonly InlineUpdate[] {
	if (!notes?.length && sent.length === 0) return NO_UPDATES;
	const entries = boardTimeline(notes ?? [], sent);
	const shown: InlineUpdate[] = [];
	let skippedNow = false;
	for (let index = entries.length - 1; index >= 0 && shown.length < limit; index -= 1) {
		const entry = entries[index]!;
		if (entry.kind === "note") {
			const { note } = entry;
			if (note.kind === "outcome" || note.kind === "turn") continue;
			if (!skippedNow && note.kind === "status" && note.text === nowLine) {
				skippedNow = true;
				continue;
			}
		}
		shown.push({ entry, order: index });
	}
	return shown;
}

// One line per update; three of them, and their gaps, are always reserved.
const UPDATE_LINE_CLASS = "flex h-[1lh] min-w-0 items-baseline gap-2 overflow-hidden";

/**
 * The latest updates, one line each, in a block that keeps its height however
 * many there are. `note` says why there are none, when that is worth saying.
 */
function InlineUpdates({ entries, note }: { entries: readonly InlineUpdate[]; note?: ReactNode }) {
	const { t } = useTranslation();
	return (
		<div className="mt-1 h-[calc(3lh+0.25rem)] min-w-0 overflow-hidden text-sm leading-snug">
			{entries.length > 0 ? (
				<ol aria-label={t("subagents.timeline")} className="flex min-w-0 flex-col gap-0.5" data-testid="agents-overview-updates">
					{entries.map(({ entry, order }) => {
						if (entry.kind === "sent") {
							const { message } = entry;
							return (
								<li className={UPDATE_LINE_CLASS} data-sent-state={message.state} data-testid="sent-message" key={`sent:${message.id}`}>
									<time className="shrink-0 text-xs tabular-nums text-passive" dateTime={message.at}>{formatTime(message.at)}</time>
									<span className="min-w-0 truncate text-foreground" title={message.text}>
										<span className="mr-1.5 text-xs text-muted-foreground">{t("subagents.you")}</span>
										{message.text}
									</span>
									<span className={cn("shrink-0 text-xs", message.state === "dropped" ? "text-muted-foreground" : "text-passive")}>
										<span aria-hidden="true">· </span>
										{t(SENT_STATE_KEYS[message.state])}
									</span>
								</li>
							);
						}
						const { note: update } = entry;
						return (
							<li className={UPDATE_LINE_CLASS} data-note-kind={update.kind} key={`note:${order}`}>
								<time className="shrink-0 text-xs tabular-nums text-passive" dateTime={update.at}>{formatTime(update.at)}</time>
								<span className={cn("min-w-0 truncate", update.kind === "decision" ? "font-medium text-foreground" : "text-muted-foreground")} title={update.text}>
									{update.kind === "decision" ? <span className="mr-1.5 text-xs font-normal text-muted-foreground">{t("subagents.decision")}</span> : null}
									{update.text}
								</span>
							</li>
						);
					})}
				</ol>
			) : note}
		</div>
	);
}

/** Why a row has no agent detail, when that is worth saying. */
function DetailNote({ detail, fallback = false }: { detail: OverviewDetail; fallback?: boolean }) {
	const { t } = useTranslation();
	const text = detail === "tui"
		? t("agentsOverview.needsChat")
		: detail === "loading"
			? t("agentsOverview.updatesLoading")
			: detail === "unavailable"
				? t("agentsOverview.updatesUnavailable")
				: fallback ? t("subagents.noUpdates") : undefined;
	if (!text) return null;
	return <p className="text-xs text-passive" data-detail-note={detail}>{text}</p>;
}

function activityAt(session: WorkspaceSession): string {
	return session.activity?.lastActivityAt || session.updatedAt;
}

function LiveDuration({ startedAt }: { startedAt: string }) {
	const now = useContext(NowContext);
	const started = Date.parse(startedAt);
	if (!Number.isFinite(started)) return null;
	return <span>{formatDuration(Math.max(0, now - started))}</span>;
}

function RelativeTime({ at }: { at: string }) {
	const now = useContext(NowContext);
	return <time dateTime={at}>{formatTimeCompact(at, { now: now || Date.now() })}</time>;
}
