/**
 * Every agent in one view: projects, then orchestrator sessions with the worker
 * sessions they spawned nested under them, then each session's subagents, each
 * row with what it is doing now and its latest updates.
 *
 * The rows read what the agents board derives for a session
 * ({@link useSubagentBoard}); nothing here parses a conversation. Conversations
 * load for the most recently active chat sessions only, and stay live through
 * the same event invalidation the session's own chat uses.
 */

import { createContext, memo, useCallback, useContext, useEffect, useId, useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Bot, ChevronRight, Folder } from "lucide-react";
import { ScrollArea } from "radix-ui";
import { useConversation } from "../hooks/useConversation";
import { useStableArray } from "../hooks/useStable";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import {
	buildAgentsOverview,
	overviewSessionsToLoad,
	type AgentsOverview as AgentsOverviewModel,
	type OverviewDetail,
	type OverviewProject,
	type OverviewSessionNode,
	type SessionAgentsDetail,
} from "../lib/agents-overview";
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { hidesShellTopbar } from "../lib/platform";
import { useSubagentBoard, type MainAgentState, type MainAgentSummary, type SubagentCard, type SubagentNote } from "../lib/subagent-board";
import { cn } from "../lib/utils";
import type { ConversationItem, ConversationTurn } from "../types/conversation";
import { isOrchestratorSession, type WorkspaceSession } from "../types/workspace";
import { formatDuration, formatTime } from "./chat/ChatTimelineItems";
import { mainNowLine, NotesTimeline, StatusGlyph, subagentNowLine } from "./chat/SubagentBoard";
import { NAV_ROW_HIGHLIGHT_HOST_CLASS, NavRowHighlight } from "./NavRowHighlight";
import { topbarProjectLabelClass } from "./TopbarButton";

const NO_ITEMS: readonly ConversationItem[] = [];
const NO_TURNS: readonly ConversationTurn[] = [];
/** Inline updates per row; the full timeline opens below it. */
const SESSION_UPDATES = 3;
const SUBAGENT_UPDATES = 2;

type DetailListener = (sessionId: string, detail: SessionAgentsDetail | undefined) => void;

function sameDetail(a: SessionAgentsDetail | undefined, b: SessionAgentsDetail | undefined): boolean {
	if (a === b) return true;
	if (!a || !b || a.status !== b.status) return false;
	return a.status !== "ready" || a.model === (b as typeof a).model;
}

/**
 * Loads one chat session's conversation and derives its agents with the board's
 * own deriver, which keeps unchanged cards and summaries by identity.
 */
const SessionAgentsSource = memo(function SessionAgentsSource({ onDetail, sessionId }: { onDetail: DetailListener; sessionId: string }) {
	const conversation = useConversation(sessionId);
	const snapshot = conversation.snapshot;
	const model = useSubagentBoard(snapshot?.items ?? NO_ITEMS, snapshot?.turns ?? NO_TURNS);
	const status: SessionAgentsDetail["status"] = snapshot ? "ready" : conversation.unavailable ? "unavailable" : conversation.error ? "error" : "loading";
	useEffect(() => {
		onDetail(sessionId, status === "ready" ? { status, model } : { status });
	}, [model, onDetail, sessionId, status]);
	useEffect(() => () => onDetail(sessionId, undefined), [onDetail, sessionId]);
	return null;
});

/** The overview route: loads what it shows, then hands it to {@link AgentsOverviewView}. */
export function AgentsOverview() {
	const workspaceQuery = useWorkspaceQuery();
	const workspaces = workspaceQuery.data;
	const navigateToSession = useNavigateToSession();
	const loaded = useStableArray(useMemo(() => overviewSessionsToLoad(workspaces ?? []), [workspaces]));
	const [details, setDetails] = useState<ReadonlyMap<string, SessionAgentsDetail>>(() => new Map());
	const onDetail = useCallback<DetailListener>((sessionId, detail) => {
		setDetails((current) => {
			if (sameDetail(current.get(sessionId), detail)) return current;
			const next = new Map(current);
			if (detail) next.set(sessionId, detail);
			else next.delete(sessionId);
			return next;
		});
	}, []);
	const overview = useMemo(() => buildAgentsOverview(workspaces ?? [], details), [details, workspaces]);
	const openSession = useCallback((session: WorkspaceSession) => navigateToSession(session.workspaceId, session.id), [navigateToSession]);
	return (
		<>
			{loaded.map((sessionId) => <SessionAgentsSource key={sessionId} onDetail={onDetail} sessionId={sessionId} />)}
			<AgentsOverviewView
				error={workspaceQuery.error ? workspaceQuery.error.message : undefined}
				isLoading={workspaceQuery.isLoading}
				onOpenSession={openSession}
				overview={overview}
			/>
		</>
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

/** A session, its subagents one level in, then the sessions it spawned. */
function SessionBranch({ node, ...tree }: TreeProps & { node: OverviewSessionNode }) {
	const sessionOpen = tree.expanded.has(node.session.id);
	const nested = node.subagents.length > 0 || node.children.length > 0;
	return (
		<li data-session-id={node.session.id} data-testid="agents-overview-branch">
			<SessionRow
				detail={node.detail}
				expanded={sessionOpen}
				main={node.main}
				onOpenSession={tree.onOpenSession}
				onToggle={tree.onToggle}
				session={node.session}
				state={node.state}
			/>
			{nested ? (
				<ul className="ms-8 divide-y divide-border border-t border-border">
					{node.subagents.map((card) => (
						<li data-testid="agents-overview-subagent-item" key={card.id}>
							<SubagentRow card={card} expanded={tree.expanded.has(subagentKey(card))} onToggle={tree.onToggle} />
						</li>
					))}
					{node.children.map((child) => <SessionBranch key={child.session.id} node={child} {...tree} />)}
				</ul>
			) : null}
		</li>
	);
}

// A click anywhere in the row does what its title does; the title stays the keyboard target.
const ROW_CLASS = cn("group/nav-row relative flex min-w-0 cursor-pointer items-start gap-2.5 rounded-lg px-3 py-2.5", NAV_ROW_HIGHLIGHT_HOST_CLASS);
const ROW_TITLE_CLASS = "min-w-0 truncate rounded-sm text-left leading-tight tracking-tight text-foreground outline-none focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent/50";

function DisclosureButton({ controls, expanded, label, onClick }: { controls: string; expanded: boolean; label: string; onClick: () => void }) {
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
			type="button"
		>
			<ChevronRight aria-hidden="true" className={cn("size-3.5 transition-transform motion-reduce:transition-none", expanded && "rotate-90")} />
		</button>
	);
}

const SessionRow = memo(function SessionRow({
	detail,
	expanded,
	main,
	onOpenSession,
	onToggle,
	session,
	state,
}: {
	detail: OverviewDetail;
	expanded: boolean;
	main?: MainAgentSummary;
	onOpenSession: (session: WorkspaceSession) => void;
	onToggle: (id: string) => void;
	session: WorkspaceSession;
	state: MainAgentState;
}) {
	const { t } = useTranslation();
	const timelineId = useId();
	const nowLine = main ? mainNowLine(t, main) : session.displayStatus;
	const kind = isOrchestratorSession(session) ? t("agentsOverview.kind.orchestrator") : t("agentsOverview.kind.worker");
	const updates = useMemo(() => latestUpdates(main?.notes, main?.nowSource === "status" ? main.nowLine : undefined, SESSION_UPDATES), [main]);
	return (
		<>
			<div className={ROW_CLASS} data-detail={detail} data-state={state} data-testid="agents-overview-session" onClick={() => onOpenSession(session)}>
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
						<button aria-label={t("agentsOverview.openSession", { title: session.title })} className={ROW_TITLE_CLASS} title={session.title} type="button">
							<span className="text-subtitle font-semibold">{session.title}</span>
						</button>
						<span className="shrink-0 text-xs text-muted-foreground">{kind}</span>
						<span className="ms-auto shrink-0 text-xs tabular-nums text-passive">
							{main?.startedAt ? <LiveDuration startedAt={main.startedAt} /> : <RelativeTime at={activityAt(session)} />}
						</span>
					</div>
					{nowLine ? (
						<p
							className={cn("line-clamp-2 text-sm leading-snug", state === "needs_you" ? "text-foreground" : "text-muted-foreground")}
							data-testid="agents-overview-now"
						>
							{nowLine}
						</p>
					) : null}
					{/* The open timeline below holds every update. */}
					{expanded ? null : <InlineUpdates notes={updates} />}
					<DetailNote detail={detail} />
				</div>
			</div>
			{expanded ? (
				<div className="ps-18 pe-3 pb-3" id={timelineId}>
					{main ? <NotesTimeline className="board-scrollbar max-h-80 overflow-y-auto" notes={main.notes} /> : <DetailNote detail={detail} fallback />}
				</div>
			) : null}
		</>
	);
});

const SubagentRow = memo(function SubagentRow({ card, expanded, onToggle }: { card: SubagentCard; expanded: boolean; onToggle: (id: string) => void }) {
	const { t } = useTranslation();
	const timelineId = useId();
	const title = card.title ?? t("subagents.untitled");
	const nowLine = subagentNowLine(t, card);
	const updates = useMemo(() => latestUpdates(card.notes, card.nowSource === "status" ? card.nowLine : undefined, SUBAGENT_UPDATES), [card]);
	const toggle = () => onToggle(subagentKey(card));
	return (
		<>
			<div className={ROW_CLASS} data-lane={card.lane} data-outcome={card.outcome} data-testid="agents-overview-subagent" onClick={toggle}>
				<NavRowHighlight />
				<DisclosureButton controls={timelineId} expanded={expanded} label={t("agentsOverview.updatesFor", { title })} onClick={toggle} />
				<span className="relative z-[1]"><StatusGlyph outcome={card.outcome} /></span>
				<div className="relative z-[1] flex min-w-0 flex-1 flex-col gap-0.5">
					<div className="flex min-w-0 items-baseline gap-2">
						{/* The disclosure is its keyboard control; the title is text. */}
						<span className={cn(ROW_TITLE_CLASS, "text-base font-semibold")} title={title}>
							{title}
						</span>
						<span className="shrink-0 text-xs text-muted-foreground">{card.subagentType ?? t("agentsOverview.kind.subagent")}</span>
						<span className="ms-auto shrink-0 text-xs tabular-nums text-passive">
							{card.lane === "done"
								? card.durationMs !== undefined ? formatDuration(card.durationMs) : null
								: <LiveDuration startedAt={card.startedAt} />}
						</span>
					</div>
					{nowLine ? (
						<p
							className={cn("line-clamp-2 text-sm leading-snug", card.lane === "needs_you" || card.outcome === "blocked" ? "text-foreground" : "text-muted-foreground")}
							data-testid="agents-overview-now"
						>
							{nowLine}
						</p>
					) : null}
					{expanded ? null : <InlineUpdates notes={updates} />}
				</div>
			</div>
			{expanded ? (
				<div className="ps-18 pe-3 pb-3" id={timelineId}>
					<NotesTimeline className="board-scrollbar max-h-80 overflow-y-auto" notes={card.notes} sent={card.inbox} />
				</div>
			) : null}
		</>
	);
});

/**
 * The newest lines worth showing under the now line: decisions and statuses, not
 * an outcome or a turn's divider, nor the status the now line already repeats.
 */
function latestUpdates(notes: readonly SubagentNote[] | undefined, nowLine: string | undefined, limit: number): SubagentNote[] {
	if (!notes?.length) return [];
	const shown: SubagentNote[] = [];
	let skippedNow = false;
	for (let index = notes.length - 1; index >= 0 && shown.length < limit; index -= 1) {
		const note = notes[index]!;
		if (note.kind === "outcome" || note.kind === "turn") continue;
		if (!skippedNow && note.kind === "status" && note.text === nowLine) {
			skippedNow = true;
			continue;
		}
		shown.push(note);
	}
	return shown.reverse();
}

function InlineUpdates({ notes }: { notes: readonly SubagentNote[] }) {
	const { t } = useTranslation();
	if (notes.length === 0) return null;
	return (
		<ol aria-label={t("subagents.timeline")} className="mt-1 flex min-w-0 flex-col gap-0.5" data-testid="agents-overview-updates">
			{notes.map((note, index) => (
				<li className="flex min-w-0 items-baseline gap-2 text-sm leading-snug" data-note-kind={note.kind} key={`${index}:${note.at}`}>
					<time className="shrink-0 text-xs tabular-nums text-passive" dateTime={note.at}>{formatTime(note.at)}</time>
					<span className={cn("min-w-0 truncate", note.kind === "decision" ? "font-medium text-foreground" : "text-muted-foreground")} title={note.text}>
						{note.kind === "decision" ? <span className="mr-1.5 text-xs font-normal text-muted-foreground">{t("subagents.decision")}</span> : null}
						{note.text}
					</span>
				</li>
			))}
		</ol>
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
