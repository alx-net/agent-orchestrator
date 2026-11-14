/**
 * Every open pull request of every live session in one view: projects, then the
 * sessions with an open or draft PR, then one fixed-height row per PR with its
 * review, one checks line, its merge state and when it was last read. A row
 * opens to the same merge box the session's Pull requests tab shows.
 *
 * Rows show the sessions' PR facts at once, then the full summary of each
 * session the page reads (`prSessionsToLoad`): the same query and key the Pull
 * requests tab uses, so the daemon's events keep both live. Everything a row
 * says is derived in lib/prs-overview.ts and lib/pr-merge-box.ts.
 */

import { Fragment, memo, useCallback, useId, useMemo, useState, type MouseEvent } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ChevronRight, CircleCheck, CircleDashed, CircleX, Folder, GitPullRequest, LoaderCircle, TriangleAlert } from "lucide-react";
import { ScrollArea } from "radix-ui";
import { useQueries, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { useStableArray } from "../hooks/useStable";
import { usePullRequestMerge } from "../hooks/usePullRequestMerge";
import {
	sessionScmSummaryQueryKey,
	sessionScmSummaryQueryOptions,
	type SessionPRReference,
	type SessionPRSummary,
} from "../hooks/useSessionScmSummary";
import { useWorkspaceQuery } from "../hooks/useWorkspaceQuery";
import { formatTimeCompact } from "../lib/format-time";
import { useNavigateToSession } from "../lib/navigate-to-session";
import { prBrowserUrl } from "../lib/pr-display";
import {
	checkCountParts,
	checksSummary,
	mergeVerdict,
	prGlyphTone,
	prObservedAt,
	type ChecksSummary,
	type MergeBoxPR,
	type MergeVerdict,
} from "../lib/pr-merge-box";
import {
	buildPRsOverview,
	prChecksRunning,
	prSessionsToLoad,
	type MergedPR,
	type PRsDetail,
	type PRsOverview,
	type PRsOverviewProject,
	type PRsOverviewSession,
	type SessionPRsLoad,
} from "../lib/prs-overview";
import { hidesShellTopbar } from "../lib/platform";
import { cn } from "../lib/utils";
import type { MessageKey } from "../i18n";
import type { WorkspaceSession } from "../types/workspace";
import { DisclosureButton, ROW_TITLE_CLASS } from "./AgentsOverview";
import { checkCountsText, COUNT_KEYS, HEADLINE_KEYS, PRGlyph, PullRequestMergeBoxBody, REVIEW_LABEL_KEYS, useNow } from "./chat/PullRequestsPanel";
import { NAV_ROW_HIGHLIGHT_HOST_CLASS, NavRowHighlight } from "./NavRowHighlight";
import { ProductExternalLink } from "./ProductExternalLink";
import { PullRequestMergeButton } from "./PullRequestMergeButton";
import { SessionStatusDot } from "./SessionStatusDot";
import { topbarProjectLabelClass } from "./TopbarButton";
import { Button } from "./ui/button";

type SummaryData = { prs: SessionPRSummary[]; linkedPrs: SessionPRReference[] };

/** Only what a row reads, so a refetch that changes nothing re-renders nothing. */
function combineLoads(results: UseQueryResult<SummaryData>[]): SessionPRsLoad[] {
	return results.map((result) => ({ summaries: result.data?.prs, pending: result.isPending, failed: result.isError }));
}

/** The PRs route: reads what it shows, then hands it to {@link PullRequestsOverviewView}. */
export function PullRequestsOverview() {
	const workspaceQuery = useWorkspaceQuery();
	const workspaces = workspaceQuery.data;
	const navigateToSession = useNavigateToSession();
	const queryClient = useQueryClient();
	const toLoad = useStableArray(useMemo(() => prSessionsToLoad(workspaces ?? []), [workspaces]));
	const results = useQueries({ queries: toLoad.map((sessionId) => sessionScmSummaryQueryOptions(sessionId)), combine: combineLoads });
	const loads = useMemo(() => new Map(toLoad.map((sessionId, index) => [sessionId, results[index]!] as const)), [results, toLoad]);
	const overview = useMemo(() => buildPRsOverview(workspaces ?? [], loads), [loads, workspaces]);
	const openSession = useCallback((session: WorkspaceSession) => navigateToSession(session.workspaceId, session.id), [navigateToSession]);
	const retry = useCallback(
		(session: WorkspaceSession) => void queryClient.refetchQueries({ queryKey: sessionScmSummaryQueryKey(session.id) }),
		[queryClient],
	);
	return (
		<PullRequestsOverviewView
			error={workspaceQuery.error ? workspaceQuery.error.message : undefined}
			isLoading={workspaceQuery.isLoading}
			onOpenSession={openSession}
			onRetry={retry}
			overview={overview}
		/>
	);
}

export interface PullRequestsOverviewViewProps {
	overview: PRsOverview;
	isLoading?: boolean;
	error?: string;
	onOpenSession: (session: WorkspaceSession) => void;
	/** Reads a session's PR summary again after it failed. */
	onRetry?: (session: WorkspaceSession) => void;
}

/** Relative times move on the minute; "Running for" in an open merge box every second. */
const RELATIVE_TICK_MS = 30_000;

export function PullRequestsOverviewView({ error, isLoading = false, onOpenSession, onRetry, overview }: PullRequestsOverviewViewProps) {
	const { t } = useTranslation();
	const showPageTitle = hidesShellTopbar();
	const [expanded, setExpanded] = useState<ReadonlySet<string>>(() => new Set());
	const [mergedOpen, setMergedOpen] = useState(false);
	const mergedId = useId();
	const toggle = useCallback((id: string) => {
		setExpanded((current) => {
			const next = new Set(current);
			if (!next.delete(id)) next.add(id);
			return next;
		});
	}, []);
	// Two clocks: closed rows re-render on the slow one only.
	const now = useNow(overview.running > 0, RELATIVE_TICK_MS);
	const relativeNow = useNow(false, RELATIVE_TICK_MS);
	return (
		<div className="flex min-h-0 flex-1 flex-col bg-background" data-testid="prs-overview">
			<header className="workspace-topbar-container center-panel-titlebar flex h-toolbar shrink-0 items-center justify-between gap-3 border-b border-border-strong pr-[calc((var(--size-toolbar)-var(--size-topbar-control))/2)]">
				<h1 className={showPageTitle ? cn(topbarProjectLabelClass, "inline-flex items-center gap-1.5") : "sr-only"}>
					{showPageTitle ? <GitPullRequest aria-hidden="true" className="size-icon-md" /> : null}
					{t("prsOverview.title")}
				</h1>
				{overview.open > 0 ? (
					<p className="flex shrink-0 items-center gap-3 text-sm tabular-nums text-muted-foreground" data-testid="prs-overview-summary">
						{overview.failing > 0 ? (
							<span className="inline-flex items-center gap-1.5 text-foreground">
								<span aria-hidden="true" className="size-2 rounded-full bg-status-exited" />
								{t("prsOverview.summary.failing", { count: overview.failing })}
							</span>
						) : null}
						{overview.running > 0 ? (
							<span className="inline-flex items-center gap-1.5">
								<span aria-hidden="true" className="size-2 rounded-full bg-status-validating" />
								{t("prsOverview.summary.running", { count: overview.running })}
							</span>
						) : null}
						<span>{t("prsOverview.summary.open", { count: overview.open })}</span>
					</p>
				) : null}
			</header>
			<ScrollArea.Root asChild scrollHideDelay={500} type="hover">
				<main className="relative min-h-0 flex-1 overflow-hidden">
					<ScrollArea.Viewport className="h-full w-full overscroll-contain focus-visible:outline-none">
						<div className="mx-auto w-full max-w-5xl p-4">
							{isLoading ? (
								<p className="px-3 py-8 text-center text-sm text-muted-foreground" role="status">{t("prsOverview.loading")}</p>
							) : error ? (
								<p className="px-3 py-8 text-center text-sm text-destructive" role="alert">{error}</p>
							) : (
								<div className="flex flex-col gap-6">
									{overview.open === 0 ? (
										<div className="flex flex-col items-center gap-1.5 px-3 py-16 text-center" data-testid="prs-overview-empty" role="status">
											<p className="text-heading-sm font-medium text-foreground">{t("prsOverview.empty.title")}</p>
											<p className="text-sm text-muted-foreground">{t("prsOverview.empty.description")}</p>
										</div>
									) : null}
									{overview.projects.map((project) => (
										<ProjectSection
											expanded={expanded}
											key={project.id}
											now={now}
											onOpenSession={onOpenSession}
											onRetry={onRetry}
											onToggle={toggle}
											project={project}
											relativeNow={relativeNow}
										/>
									))}
									{overview.recentlyMerged.length > 0 ? (
										<section aria-label={t("prsOverview.recentlyMerged")} data-testid="prs-overview-merged-group">
											<button
												aria-controls={mergedOpen ? mergedId : undefined}
												aria-expanded={mergedOpen}
												className={cn("group/nav-row relative flex h-9 w-full items-center gap-2 rounded-lg px-3 text-left text-sm text-muted-foreground outline-none hover:text-foreground", NAV_ROW_HIGHLIGHT_HOST_CLASS)}
												onClick={() => setMergedOpen((open) => !open)}
												type="button"
											>
												<NavRowHighlight />
												<ChevronRight aria-hidden="true" className={cn("relative z-[1] size-3.5 shrink-0 transition-transform motion-reduce:transition-none", mergedOpen && "rotate-90")} />
												<span className="relative z-[1]">{t("prsOverview.recentlyMerged")}</span>
												<span className="relative z-[1] tabular-nums text-passive">{overview.recentlyMerged.length}</span>
											</button>
											{mergedOpen ? (
												<ul className="divide-y divide-border border-t border-border" id={mergedId}>
													{overview.recentlyMerged.map((merged) => (
														<MergedRow key={`${merged.session.id}:${prKey(merged.pr)}`} merged={merged} now={relativeNow} onOpenSession={onOpenSession} />
													))}
												</ul>
											) : null}
										</section>
									) : null}
								</div>
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

function prKey(pr: MergeBoxPR): string {
	return pr.url || pr.htmlUrl || String(pr.number);
}

interface ListProps {
	expanded: ReadonlySet<string>;
	now: number;
	relativeNow: number;
	onOpenSession: (session: WorkspaceSession) => void;
	onRetry?: (session: WorkspaceSession) => void;
	onToggle: (id: string) => void;
}

function ProjectSection({ project, ...list }: ListProps & { project: PRsOverviewProject }) {
	return (
		<section aria-label={project.name} data-project-id={project.id} data-testid="prs-overview-project">
			<h2 className="flex h-8 items-center gap-2 px-3 text-sm font-medium text-muted-foreground">
				<Folder aria-hidden="true" className="size-icon-md shrink-0" />
				<span className="truncate text-foreground">{project.name}</span>
				<span className="tabular-nums text-passive">{project.prCount}</span>
			</h2>
			<ul className="divide-y divide-border">
				{project.sessions.map((entry) => <SessionGroup entry={entry} key={entry.session.id} {...list} />)}
			</ul>
		</section>
	);
}

/** A session's row, then one row per open or draft PR under it. */
function SessionGroup({ entry, expanded, now, onOpenSession, onRetry, onToggle, relativeNow }: ListProps & { entry: PRsOverviewSession }) {
	const { t } = useTranslation();
	const { session, detail } = entry;
	const branch = session.branchNaming ? undefined : session.branch;
	return (
		<li data-detail={detail} data-session-id={session.id} data-testid="prs-overview-session" data-tier={entry.tier}>
			{/* A click anywhere in the row opens the session; the title stays the keyboard target. */}
			<div
				className={cn("group/nav-row relative flex h-9 min-w-0 cursor-pointer items-center gap-2 rounded-lg px-3", NAV_ROW_HIGHLIGHT_HOST_CLASS)}
				data-testid="prs-overview-session-row"
				onClick={() => onOpenSession(session)}
			>
				<NavRowHighlight />
				<SessionStatusDot session={session} />
				<button aria-label={t("prsOverview.openSession", { title: session.title })} className={cn("relative z-[1]", ROW_TITLE_CLASS)} title={session.title} type="button">
					<span className="text-sm font-medium">{session.title}</span>
				</button>
				{branch ? <span className="relative z-[1] min-w-0 shrink truncate font-mono text-2xs text-muted-foreground" title={branch}>{branch}</span> : null}
				{detail === "failed" ? <RefreshFailed onRetry={onRetry ? () => onRetry(session) : undefined} /> : null}
			</div>
			<ul className="ms-6 divide-y divide-border border-t border-border">
				{entry.prs.map((pr) => {
					const key = `${session.id}:${prKey(pr)}`;
					const open = expanded.has(key);
					return (
						<PullRequestRow
							detail={detail}
							expanded={open}
							id={key}
							key={key}
							// Only an open merge box counts checks up by the second.
							now={open ? now : relativeNow}
							onToggle={onToggle}
							pr={pr}
							session={session}
						/>
					);
				})}
			</ul>
		</li>
	);
}

function RefreshFailed({ onRetry }: { onRetry?: () => void }) {
	const { t } = useTranslation();
	return (
		<span className="relative z-[1] ms-auto flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground" data-testid="prs-overview-refresh-failed" role="alert">
			<TriangleAlert aria-hidden="true" className="size-3.5 shrink-0 text-status-needs-you" />
			{t("prsOverview.refreshFailed")}
			{onRetry ? (
				<Button
					className="h-6 px-2 text-xs"
					onClick={(event) => {
						event.stopPropagation();
						onRetry();
					}}
					size="sm"
					type="button"
					variant="ghost"
				>
					{t("prMergeBox.retry")}
				</Button>
			) : null}
		</span>
	);
}

/** A click on the row's link or a button does only that; anywhere else opens or closes the merge box. */
function fromControl(event: MouseEvent<HTMLElement>): boolean {
	return event.target instanceof Element && event.target.closest("a, button") !== null;
}

/**
 * One PR in a fixed 56px row, two truncated lines, so checks finishing or a
 * Merge button appearing never moves the rows below it.
 */
const PullRequestRow = memo(function PullRequestRow({
	detail,
	expanded,
	id,
	now,
	onToggle,
	pr,
	session,
}: {
	detail: PRsDetail;
	expanded: boolean;
	id: string;
	now: number;
	onToggle: (id: string) => void;
	pr: MergeBoxPR;
	session: WorkspaceSession;
}) {
	const { t } = useTranslation();
	const bodyId = useId();
	const merge = usePullRequestMerge({ cloudOrgId: session.cloud?.orgId, hostId: session.hostId, pr, sessionId: session.id });
	const summary = checksSummary(pr);
	const verdict = mergeVerdict(pr);
	const href = prBrowserUrl(pr);
	const title = pr.title || href;
	const reviewKey = REVIEW_LABEL_KEYS[pr.review.decision];
	const observedAt = prObservedAt(pr);
	const toggle = () => onToggle(id);
	return (
		<li data-expanded={expanded} data-pr-number={pr.number} data-state={pr.state} data-testid="prs-overview-pr">
			<div
				className={cn("group/nav-row relative flex h-14 min-w-0 cursor-pointer items-center gap-2.5 rounded-lg px-3", NAV_ROW_HIGHLIGHT_HOST_CLASS)}
				data-testid="prs-overview-pr-row"
				onClick={(event) => {
					if (!fromControl(event)) toggle();
				}}
			>
				<NavRowHighlight />
				<DisclosureButton controls={bodyId} expanded={expanded} label={t("prMergeBox.toggleDetails", { number: pr.number })} onClick={toggle} />
				<span className="relative z-[1]"><PRGlyph className="mt-0" state={pr.state} tone={prGlyphTone(pr)} /></span>
				<div className="relative z-[1] flex min-w-0 flex-1 flex-col justify-center gap-1">
					<div className="flex min-w-0 items-baseline gap-2">
						<ProductExternalLink
							className="min-w-0 truncate rounded-sm text-sm font-medium leading-tight tracking-tight text-foreground underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
							href={href}
						>
							<span className="mr-1.5 tabular-nums text-muted-foreground">#{pr.number}</span>
							{title}
						</ProductExternalLink>
						{reviewKey ? <span className="shrink-0 text-xs text-muted-foreground" data-testid="prs-overview-review">{t(reviewKey)}</span> : null}
						{observedAt ? (
							<time className="ms-auto shrink-0 text-xs tabular-nums text-passive" dateTime={observedAt} data-testid="prs-overview-observed-at">
								{t("prMergeBox.updated", { time: formatTimeCompact(observedAt, { now }) })}
							</time>
						) : null}
					</div>
					<div className="flex min-w-0 items-center gap-1.5 overflow-hidden whitespace-nowrap text-xs text-muted-foreground">
						<ChecksLine summary={summary} />
						<span aria-hidden="true" className="shrink-0 text-passive">·</span>
						<span className="shrink-0" data-testid="prs-overview-merge-state" data-verdict={verdict.kind}>{mergeStateText(t, verdict, pr)}</span>
					</div>
				</div>
				{/* Buttons inherit their size (the global `font: inherit` outranks a utility), so the slot sets it. */}
				{merge.canMerge ? (
					<div className="relative z-[1] flex shrink-0 items-center text-xs">
						<PullRequestMergeButton merge={merge} number={pr.number} />
					</div>
				) : null}
			</div>
			{expanded ? (
				<div
					aria-label={t("prsOverview.mergeBox", { number: pr.number })}
					className="board-scrollbar ms-8 me-3 mb-3 h-72 overflow-y-auto rounded-lg bg-surface"
					data-testid="prs-overview-merge-box"
					id={bodyId}
					role="region"
				>
					<PullRequestMergeBoxBody detailsLoaded={detail !== "loading"} mergeError={merge.error} now={now} pr={pr} />
				</div>
			) : null}
		</li>
	);
});

const SPINNER_CLASS = "shrink-0 animate-spin text-status-validating motion-reduce:animate-none";

/**
 * The merge box's checks headline and counts on one line: "Checks are running ·
 * 3 in progress, 6 successful". The spinner leads while checks run; when some
 * already failed, the failure leads and the spinner sits by the running count.
 */
function ChecksLine({ summary }: { summary: ChecksSummary }) {
	const { t } = useTranslation();
	const { headline } = summary;
	const parts = checkCountParts(summary.counts);
	const headlineText = t(HEADLINE_KEYS[headline]);
	const counts = checkCountsText(t, summary);
	return (
		<span
			className="flex min-w-0 shrink items-center gap-1.5"
			data-headline={headline}
			data-running={prChecksRunning(summary) || undefined}
			data-testid="prs-overview-checks"
			title={counts ? `${headlineText} · ${counts}` : headlineText}
		>
			{headline === "failing" ? (
				<CircleX aria-hidden="true" className="size-3.5 shrink-0 text-status-exited" />
			) : headline === "running" ? (
				<LoaderCircle aria-hidden="true" className={cn("size-3.5", SPINNER_CLASS)} data-testid="prs-overview-checks-spinner" />
			) : headline === "passed" ? (
				<CircleCheck aria-hidden="true" className="size-3.5 shrink-0 text-status-ready" />
			) : (
				<CircleDashed aria-hidden="true" className="size-3.5 shrink-0 text-passive" />
			)}
			<span className="truncate">
				<span className={cn(headline === "failing" && "text-foreground")}>{headlineText}</span>
				{parts.length > 0 ? <span aria-hidden="true" className="text-passive"> · </span> : null}
				{parts.map((part, index) => (
					<Fragment key={part.key}>
						{index > 0 ? t("prMergeBox.count.separator") : null}
						{part.key === "running" && headline !== "running" ? (
							<LoaderCircle aria-hidden="true" className={cn("me-1 inline-block size-3 align-[-2px]", SPINNER_CLASS)} data-testid="prs-overview-checks-spinner" />
						) : null}
						{t(COUNT_KEYS[part.key], { count: part.count })}
					</Fragment>
				))}
			</span>
		</span>
	);
}

const MERGE_STATE_KEYS = {
	ready: "pr.card.readyToMerge",
	conflicts: "prsOverview.merge.conflicts",
	blocked: "prsOverview.merge.blocked",
	checking: "pr.merge.checkingReadiness",
	draft: "prMergeBox.state.draft",
	unavailable: "pr.merge.accessLost",
	merged: "prMergeBox.merge.merged",
	closed: "prMergeBox.merge.closed",
} as const satisfies Record<Exclude<MergeVerdict["kind"], "behind">, MessageKey>;

/** The merge box's verdict in a word or three. */
function mergeStateText(t: TFunction, verdict: MergeVerdict, pr: MergeBoxPR): string {
	if (verdict.kind === "behind") return t("prsOverview.merge.behind", { target: pr.targetBranch || t("prMergeBox.baseBranch") });
	return t(MERGE_STATE_KEYS[verdict.kind]);
}

function MergedRow({ merged, now, onOpenSession }: { merged: MergedPR; now: number; onOpenSession: (session: WorkspaceSession) => void }) {
	const { t } = useTranslation();
	const { pr, session } = merged;
	const href = prBrowserUrl(pr);
	return (
		<li className="flex h-9 min-w-0 items-center gap-2.5 px-3 text-sm" data-pr-number={pr.number} data-testid="prs-overview-merged">
			<PRGlyph className="mt-0" state="merged" tone="merged" />
			<ProductExternalLink
				className="min-w-0 truncate rounded-sm text-foreground underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
				href={href}
			>
				<span className="mr-1.5 tabular-nums text-muted-foreground">#{pr.number}</span>
				{pr.title || href}
			</ProductExternalLink>
			<button
				aria-label={t("prsOverview.openSession", { title: session.title })}
				className="min-w-0 shrink truncate rounded-sm text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:outline-2 focus-visible:outline-accent/50"
				onClick={() => onOpenSession(session)}
				title={session.title}
				type="button"
			>
				{session.title}
			</button>
			<time className="ms-auto shrink-0 text-xs tabular-nums text-passive" dateTime={merged.mergedAt}>
				{t("prMergeBox.merge.mergedAt", { time: formatTimeCompact(merged.mergedAt, { now }) })}
			</time>
		</li>
	);
}
