/**
 * The session's pull requests as merge boxes, in the chat's Pull requests tab:
 * one box per PR with where its checks are, whether the branch fell behind its
 * base, and whether it can merge, so a session with several PRs in parallel can
 * be read and shipped without leaving the chat.
 *
 * The tab covers the timeline like the Agents tab and keeps the composer, so a
 * failing check's "Fix this" starts a draft right there. Everything shown is
 * derived in lib/pr-merge-box.ts from the daemon's PR summary; merging reuses the
 * inspector's merge action.
 */

import { memo, useEffect, useId, useMemo, useState, type KeyboardEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import {
	ArrowUpRight,
	Check,
	ChevronDown,
	CircleAlert,
	CircleCheck,
	CircleDashed,
	CircleHelp,
	CircleMinus,
	CircleSlash,
	CircleStop,
	CircleX,
	GitMerge,
	GitPullRequest,
	GitPullRequestClosed,
	GitPullRequestDraft,
	Loader2,
	TriangleAlert,
} from "lucide-react";
import { useSessionScmSummary } from "../../hooks/useSessionScmSummary";
import { usePullRequestMerge } from "../../hooks/usePullRequestMerge";
import { formatTimeCompact } from "../../lib/format-time";
import { prBrowserUrl, prChecksUrl, prNounKeys, sessionPRDisplaySummaries } from "../../lib/pr-display";
import {
	checkCountParts,
	checkGroupOf,
	checkLabel,
	checksSummary,
	checkTiming,
	fixCheckPrompt,
	fixMergePrompt,
	formatCheckDuration,
	groupChecks,
	mergeVerdict,
	prCheckSet,
	prGlyphTone,
	prIsSettled,
	prObservedAt,
	pullRequestsTabTone,
	type CheckGroup,
	type CheckGroupKey,
	type CheckTiming,
	type ChecksHeadline,
	type MergeBoxPR,
	type MergeVerdict,
	type PRCheck,
	type PRCheckStatus,
	type PRGlyphTone,
	type PullRequestsTabTone,
} from "../../lib/pr-merge-box";
import { cn } from "../../lib/utils";
import type { MessageKey, PluralMessageKey } from "../../i18n";
import type { WorkspaceSession } from "../../types/workspace";
import { ProductExternalLink } from "../ProductExternalLink";
import { PullRequestMergeButton } from "../PullRequestMergeButton";
import { Button } from "../ui/button";

/** The session's PRs for the tab and the panel: facts first, the full summary once it lands. */
export function useSessionPullRequests(session: WorkspaceSession, hostId?: string) {
	const query = useSessionScmSummary(
		session.id,
		session.prs.length > 0,
		session.cloud?.orgId,
		session.cloud ? session.autoInjectCI === true : false,
		hostId,
	);
	const summaries = query.data?.prs;
	const prs = useMemo(() => sessionPRDisplaySummaries(session, summaries) as MergeBoxPR[], [session, summaries]);
	return { prs, query };
}

const TAB_TONE_CLASS: Record<PullRequestsTabTone, string> = {
	failing: "text-status-exited",
	conflicts: "text-status-needs-you",
	running: "text-status-validating",
	idle: "text-passive",
};

const TAB_TONE_LABEL: Partial<Record<PullRequestsTabTone, MessageKey>> = {
	failing: "pr.card.checksFailing",
	conflicts: "pr.card.mergeConflict",
	running: "pr.card.checksPending",
};

/** The Pull requests tab beside Agents: the PR count, tinted by what the open PRs most need. */
export function PullRequestsTabButton({
	active,
	hostId,
	onSelect,
	session,
}: {
	active: boolean;
	hostId?: string;
	onSelect: () => void;
	session: WorkspaceSession;
}) {
	const { t } = useTranslation();
	const { prs } = useSessionPullRequests(session, hostId);
	const tone = pullRequestsTabTone(prs);
	const toneLabel = TAB_TONE_LABEL[tone];
	return (
		<button
			aria-current={active ? true : undefined}
			aria-selected={active}
			className={cn(
				"group relative inline-flex shrink-0 self-stretch cursor-pointer items-center gap-1.5 border-r border-border px-3 text-control font-medium leading-none transition-colors focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent/50",
				active
					? "bg-overlay text-foreground after:absolute after:inset-x-0 after:bottom-0 after:h-0.5 after:bg-foreground/80"
					: "text-muted-foreground hover:bg-raised hover:text-foreground",
			)}
			data-testid="chat-pull-requests-tab"
			data-tone={tone}
			onClick={active ? undefined : onSelect}
			role="tab"
			tabIndex={active ? 0 : -1}
			type="button"
		>
			<GitPullRequest aria-hidden="true" className="size-icon-base" />
			<span>{t("prMergeBox.tab")}</span>
			<span aria-hidden="true" className={cn("text-caption tabular-nums", TAB_TONE_CLASS[tone])}>{prs.length}</span>
			<span className="sr-only">{t("prMergeBox.tabCount", { count: prs.length })}</span>
			{toneLabel ? <span className="sr-only">{t(toneLabel)}</span> : null}
		</button>
	);
}

export interface PullRequestsPanelProps {
	session: WorkspaceSession;
	hostId?: string;
	/** Starts a draft in the composer and returns to the chat. Absent while the agent cannot take new work. */
	onFix?: (prompt: string) => void;
}

export const PullRequestsPanel = memo(function PullRequestsPanel({ session, hostId, onFix }: PullRequestsPanelProps) {
	const { t } = useTranslation();
	const { prs, query } = useSessionPullRequests(session, hostId);
	const running = prs.some((pr) => !prIsSettled(pr) && prCheckSet(pr).checks.some((check) => check.status === "in_progress"));
	const now = useNow(running);
	const detailsLoaded = query.data !== undefined;
	// Escape anywhere in the chat stops the turn; reading checks must never do that.
	const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
		if (event.key !== "Escape" || event.defaultPrevented) return;
		event.preventDefault();
		event.stopPropagation();
	};
	return (
		<section
			aria-label={t("prMergeBox.tab")}
			className="flex h-full min-h-0 flex-col"
			data-testid="pr-merge-panel"
			onKeyDown={onKeyDown}
		>
			<div className="board-scrollbar min-h-0 flex-1 overflow-y-auto">
				<div className="mx-auto flex w-full max-w-3xl flex-col gap-3 px-4 py-4">
					{query.isError ? (
						<div className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground" role="alert">
							<TriangleAlert aria-hidden="true" className="size-3.5 shrink-0 text-status-needs-you" />
							<span className="min-w-0 flex-1">{t("prMergeBox.loadFailed")}</span>
							<Button
								className="h-6 shrink-0 px-2 text-xs"
								disabled={query.isFetching}
								onClick={() => void query.refetch()}
								size="sm"
								type="button"
								variant="ghost"
							>
								{t("prMergeBox.retry")}
							</Button>
						</div>
					) : null}
					{prs.length === 0 ? (
						<p className="py-10 text-center text-sm text-muted-foreground" role="status">
							{query.isPending && session.prs.length > 0 ? t("prMergeBox.loading") : t("prMergeBox.empty")}
						</p>
					) : (
						prs.map((pr) => (
							<PullRequestMergeBox
								cloudOrgId={session.cloud?.orgId}
								detailsLoaded={detailsLoaded}
								hostId={hostId}
								key={pr.url || pr.htmlUrl || pr.number}
								now={now}
								onFix={onFix}
								pr={pr}
								sessionId={session.id}
							/>
						))
					)}
				</div>
			</div>
		</section>
	);
});

/** Wall time for running checks' "Running for", ticking only while one runs. */
function useNow(active: boolean): number {
	const [now, setNow] = useState(() => Date.now());
	useEffect(() => {
		if (!active) return;
		setNow(Date.now());
		const timer = window.setInterval(() => setNow(Date.now()), 1_000);
		return () => window.clearInterval(timer);
	}, [active]);
	return now;
}

const STATE_LABEL_KEYS = {
	open: "prMergeBox.state.open",
	draft: "prMergeBox.state.draft",
	merged: "prMergeBox.state.merged",
	closed: "prMergeBox.state.closed",
} as const satisfies Record<MergeBoxPR["state"], MessageKey>;

const REVIEW_LABEL_KEYS: Partial<Record<MergeBoxPR["review"]["decision"], MessageKey>> = {
	approved: "pr.review.approved",
	changes_requested: "pr.review.changesRequested",
	review_required: "pr.review.pending",
};

const PullRequestMergeBox = memo(function PullRequestMergeBox({
	cloudOrgId,
	detailsLoaded,
	hostId,
	now,
	onFix,
	pr,
	sessionId,
}: {
	cloudOrgId?: string;
	/** The full summary arrived; until then only the session's PR facts are known. */
	detailsLoaded: boolean;
	hostId?: string;
	now: number;
	onFix?: (prompt: string) => void;
	pr: MergeBoxPR;
	sessionId: string;
}) {
	const { t } = useTranslation();
	const bodyId = useId();
	const settled = prIsSettled(pr);
	// Merged and closed PRs are history: they fold to their header.
	const [open, setOpen] = useState(!settled);
	const merge = usePullRequestMerge({ cloudOrgId, hostId, pr, sessionId });
	const summary = checksSummary(pr);
	const groups = groupChecks(prCheckSet(pr).checks);
	const verdict = mergeVerdict(pr);
	const href = prBrowserUrl(pr);
	const observedAt = prObservedAt(pr);
	const reviewKey = settled ? undefined : REVIEW_LABEL_KEYS[pr.review.decision];
	const title = pr.title || href;
	return (
		<article
			aria-label={`#${pr.number} ${title}`}
			className="min-w-0 rounded-lg border border-border bg-surface"
			data-pr-number={pr.number}
			data-state={pr.state}
			data-testid="pr-merge-box"
		>
			<div className="flex min-w-0 items-start gap-2.5 px-3.5 py-2.5">
				<PRGlyph state={pr.state} tone={prGlyphTone(pr)} />
				<div className="flex min-w-0 flex-1 flex-col gap-1">
					<ProductExternalLink
						className="min-w-0 text-sm-md font-semibold leading-snug tracking-tight text-foreground underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
						href={href}
					>
						<span className="mr-1.5 font-medium tabular-nums text-muted-foreground">#{pr.number}</span>
						{title}
					</ProductExternalLink>
					<div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
						{pr.sourceBranch ? (
							<span className="min-w-0 max-w-full truncate font-mono text-2xs" data-testid="pr-branches">
								{pr.targetBranch ? `${pr.sourceBranch} → ${pr.targetBranch}` : pr.sourceBranch}
							</span>
						) : null}
						{settled ? <span>{t(STATE_LABEL_KEYS[pr.state])}</span> : null}
						{reviewKey ? <span>{t(reviewKey)}</span> : null}
						{pr.additions > 0 || pr.deletions > 0 ? (
							<span
								className="tabular-nums"
								title={t("prMergeBox.diff", { additions: pr.additions, deletions: pr.deletions })}
							>
								{`+${pr.additions} −${pr.deletions}`}
							</span>
						) : null}
						{observedAt ? (
							<time className="text-passive" dateTime={observedAt} data-testid="pr-observed-at">
								{t("prMergeBox.updated", { time: formatTimeCompact(observedAt) })}
							</time>
						) : null}
					</div>
				</div>
				{/* Buttons inherit their size (the global `font: inherit` outranks a utility), so the slot sets it. */}
				<div className="flex shrink-0 items-center gap-1 text-xs">
					{merge.canMerge ? <PullRequestMergeButton merge={merge} number={pr.number} /> : null}
					<Button
						aria-controls={open ? bodyId : undefined}
						aria-expanded={open}
						aria-label={t("prMergeBox.toggleDetails", { number: pr.number })}
						className="size-6 text-muted-foreground hover:text-foreground"
						onClick={() => setOpen((current) => !current)}
						size="icon-sm"
						type="button"
						variant="ghost"
					>
						<ChevronDown
							aria-hidden="true"
							className={cn("size-3.5 transition-transform motion-reduce:transition-none", !open && "-rotate-90")}
						/>
					</Button>
				</div>
			</div>
			{open ? (
				<div className="divide-y divide-border border-t border-border" id={bodyId}>
					<ChecksSummaryRow detailsLoaded={detailsLoaded} pr={pr} summary={summary} />
					{groups.map((group) => (
						<CheckGroupSection group={group} key={group.key} now={now} onFix={settled ? undefined : onFix} pr={pr} />
					))}
					<MergeStateRow error={merge.error} onFix={onFix} pr={pr} verdict={verdict} />
				</div>
			) : null}
		</article>
	);
});

const PR_GLYPH_CLASS: Record<PRGlyphTone, string> = {
	merged: "text-status-merged",
	closed: "text-muted-foreground",
	draft: "text-passive",
	failing: "text-status-exited",
	attention: "text-status-needs-you",
	running: "text-status-validating",
	ready: "text-status-ready",
	neutral: "text-muted-foreground",
};

function PRGlyph({ state, tone }: { state: MergeBoxPR["state"]; tone: PRGlyphTone }) {
	const { t } = useTranslation();
	const className = cn("size-4", PR_GLYPH_CLASS[tone]);
	return (
		<span className="mt-0.5 flex size-4 shrink-0 items-center justify-center" data-testid="pr-glyph" data-tone={tone}>
			{state === "merged" ? (
				<GitMerge aria-hidden="true" className={className} />
			) : state === "closed" ? (
				<GitPullRequestClosed aria-hidden="true" className={className} />
			) : state === "draft" ? (
				<GitPullRequestDraft aria-hidden="true" className={className} />
			) : (
				<GitPullRequest aria-hidden="true" className={className} />
			)}
			<span className="sr-only">{t(STATE_LABEL_KEYS[state])}</span>
		</span>
	);
}

const HEADLINE_KEYS = {
	failing: "prMergeBox.checks.failing",
	running: "prMergeBox.checks.running",
	passed: "prMergeBox.checks.passed",
	none: "prMergeBox.checks.none",
	unknown: "prMergeBox.checks.unknown",
} as const satisfies Record<ChecksHeadline, MessageKey>;

const COUNT_KEYS = {
	failing: "prMergeBox.count.failing",
	running: "prMergeBox.count.running",
	skipped: "prMergeBox.count.skipped",
	passed: "prMergeBox.count.passed",
} as const satisfies Record<CheckGroupKey, PluralMessageKey>;

function ChecksSummaryRow({
	detailsLoaded,
	pr,
	summary,
}: {
	detailsLoaded: boolean;
	pr: MergeBoxPR;
	summary: ReturnType<typeof checksSummary>;
}) {
	const { t } = useTranslation();
	const counts = checkCountParts(summary.counts)
		.map((part) => t(COUNT_KEYS[part.key], { count: part.count }))
		.join(t("prMergeBox.count.separator"));
	const checksUrl = prChecksUrl(pr);
	return (
		<div className="flex min-w-0 items-start gap-2.5 px-3.5 py-2.5" data-headline={summary.headline} data-testid="pr-checks-summary">
			<HeadlineGlyph headline={summary.headline} />
			<div className="min-w-0 flex-1">
				<p className="text-xs font-medium text-foreground">{t(HEADLINE_KEYS[summary.headline])}</p>
				{counts ? <p className="mt-0.5 text-xs tabular-nums text-muted-foreground" data-testid="pr-checks-counts">{counts}</p> : null}
				{!detailsLoaded ? (
					<p className="mt-0.5 flex items-center gap-1.5 text-2xs text-passive" role="status">
						<Loader2 aria-hidden="true" className="size-3 animate-spin motion-reduce:animate-none" />
						{t("prMergeBox.loadingChecks")}
					</p>
				) : summary.partial && summary.headline !== "none" ? (
					<p className="mt-0.5 text-2xs text-passive">{t("prMergeBox.checks.partial")}</p>
				) : null}
			</div>
			{checksUrl ? (
				<ProductExternalLink
					className="inline-flex shrink-0 items-center gap-0.5 rounded-sm text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
					href={checksUrl}
				>
					{t("prMergeBox.checks.open")}
					<ArrowUpRight aria-hidden="true" className="size-3" />
				</ProductExternalLink>
			) : null}
		</div>
	);
}

function HeadlineGlyph({ headline }: { headline: ChecksHeadline }) {
	return (
		<span aria-hidden="true" className="mt-px flex size-4 shrink-0 items-center justify-center">
			{headline === "failing" ? (
				<CircleX className="size-4 text-status-exited" />
			) : headline === "running" ? (
				<span className="size-2.5 animate-status-pulse rounded-full bg-status-validating motion-reduce:animate-none" />
			) : headline === "passed" ? (
				<CircleCheck className="size-4 text-status-ready" />
			) : (
				<CircleDashed className="size-4 text-passive" />
			)}
		</span>
	);
}

const GROUP_KEYS = {
	failing: "prMergeBox.group.failing",
	running: "prMergeBox.group.running",
	skipped: "prMergeBox.group.skipped",
	passed: "prMergeBox.group.passed",
} as const satisfies Record<CheckGroupKey, PluralMessageKey>;

function CheckGroupSection({
	group,
	now,
	onFix,
	pr,
}: {
	group: CheckGroup;
	now: number;
	onFix?: (prompt: string) => void;
	pr: MergeBoxPR;
}) {
	const { t } = useTranslation();
	const listId = useId();
	const [open, setOpen] = useState(group.defaultOpen);
	const label = t(GROUP_KEYS[group.key], { count: group.checks.length });
	return (
		<section aria-label={label} className="text-xs" data-group={group.key} data-testid="pr-check-group">
			<button
				aria-controls={open ? listId : undefined}
				aria-expanded={open}
				className="flex h-8 w-full items-center gap-1 px-3.5 text-left text-xs font-medium text-muted-foreground outline-none transition-colors duration-[120ms] hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-accent/50"
				onClick={() => setOpen((current) => !current)}
				type="button"
			>
				<span className="tabular-nums">{label}</span>
				<ChevronDown
					aria-hidden="true"
					className={cn("size-3 transition-transform motion-reduce:transition-none", !open && "-rotate-90")}
				/>
			</button>
			{open ? (
				<ul className="divide-y divide-border border-t border-border" id={listId}>
					{group.checks.map((check, index) => (
						<CheckRow check={check} key={`${check.workflow ?? ""}/${check.name}/${index}`} now={now} onFix={onFix} pr={pr} />
					))}
				</ul>
			) : null}
		</section>
	);
}

function CheckRow({
	check,
	now,
	onFix,
	pr,
}: {
	check: PRCheck;
	now: number;
	onFix?: (prompt: string) => void;
	pr: MergeBoxPR;
}) {
	const { t } = useTranslation();
	const label = checkLabel(check);
	const failing = checkGroupOf(check.status) === "failing";
	return (
		<li className="flex min-h-8 min-w-0 items-center gap-2.5 px-3.5 py-1 text-xs" data-status={check.status} data-testid="pr-check">
			<CheckGlyph status={check.status} />
			<span className="flex min-w-0 flex-1 items-baseline gap-2">
				<span className="min-w-0 truncate text-foreground" title={label}>{label}</span>
				<span className="shrink-0 tabular-nums text-muted-foreground" data-testid="pr-check-timing">
					{timingText(t, checkTiming(check, now))}
				</span>
			</span>
			{check.required ? <span className="shrink-0 text-2xs text-passive">{t("prMergeBox.required")}</span> : null}
			{failing && onFix ? (
				<Button
					aria-label={t("prMergeBox.fixLabel", { name: label })}
					className="h-6 shrink-0 px-2 text-xs"
					onClick={() => onFix(fixCheckPrompt(check, pr))}
					size="sm"
					type="button"
					variant="ghost"
				>
					{t("prMergeBox.fix")}
				</Button>
			) : null}
			{check.url ? (
				<ProductExternalLink
					ariaLabel={t("prMergeBox.openCheck", { name: label })}
					className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-muted-foreground hover:bg-interactive-hover hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/60"
					href={check.url}
				>
					<ArrowUpRight aria-hidden="true" className="size-3.5" />
				</ProductExternalLink>
			) : (
				<span aria-hidden="true" className="size-6 shrink-0" />
			)}
		</li>
	);
}

const TIMING_KEYS = {
	failedAfter: "prMergeBox.timing.failedAfter",
	cancelledAfter: "prMergeBox.timing.cancelledAfter",
	passedIn: "prMergeBox.timing.passedIn",
	completedIn: "prMergeBox.timing.completedIn",
	runningFor: "prMergeBox.timing.runningFor",
	failedAt: "prMergeBox.timing.failedAt",
	cancelledAt: "prMergeBox.timing.cancelledAt",
	passedAt: "prMergeBox.timing.passedAt",
	completedAt: "prMergeBox.timing.completedAt",
	skippedAt: "prMergeBox.timing.skippedAt",
	failed: "prMergeBox.timing.failed",
	cancelled: "prMergeBox.timing.cancelled",
	passed: "prMergeBox.timing.passed",
	completed: "prMergeBox.timing.completed",
	skipped: "prMergeBox.timing.skipped",
	running: "prMergeBox.timing.running",
	queued: "prMergeBox.timing.queued",
} as const satisfies Record<CheckTiming["kind"], MessageKey>;

function timingText(t: TFunction, timing: CheckTiming): string {
	if ("durationMs" in timing) return t(TIMING_KEYS[timing.kind], { duration: formatCheckDuration(timing.durationMs) });
	if ("at" in timing) return t(TIMING_KEYS[timing.kind], { time: formatTimeCompact(timing.at) });
	return t(TIMING_KEYS[timing.kind]);
}

const STATUS_LABEL_KEYS = {
	failed: "prMergeBox.timing.failed",
	cancelled: "prMergeBox.timing.cancelled",
	in_progress: "prMergeBox.timing.running",
	queued: "prMergeBox.timing.queued",
	skipped: "prMergeBox.timing.skipped",
	neutral: "prMergeBox.timing.completed",
	passed: "prMergeBox.timing.passed",
} as const satisfies Record<PRCheckStatus, MessageKey>;

function CheckGlyph({ status }: { status: PRCheckStatus }) {
	const { t } = useTranslation();
	return (
		<span className="flex size-4 shrink-0 items-center justify-center" data-testid="pr-check-glyph">
			{status === "failed" ? (
				<CircleX aria-hidden="true" className="size-3.5 text-status-exited" />
			) : status === "cancelled" ? (
				<CircleStop aria-hidden="true" className="size-3.5 text-status-exited" />
			) : status === "in_progress" ? (
				<span aria-hidden="true" className="size-2 animate-status-pulse rounded-full bg-status-validating motion-reduce:animate-none" />
			) : status === "queued" ? (
				<CircleDashed aria-hidden="true" className="size-3.5 text-status-validating" />
			) : status === "skipped" ? (
				<CircleSlash aria-hidden="true" className="size-3.5 text-passive" />
			) : status === "neutral" ? (
				<CircleMinus aria-hidden="true" className="size-3.5 text-passive" />
			) : (
				<Check aria-hidden="true" className="size-3.5 text-status-ready" />
			)}
			<span className="sr-only">{t(STATUS_LABEL_KEYS[status])}</span>
		</span>
	);
}

const BLOCKER_KEYS = {
	required: "prMergeBox.blocker.required",
	changes_requested: "prMergeBox.blocker.changesRequested",
	comments: "prMergeBox.blocker.comments",
} as const satisfies Record<"required" | "changes_requested" | "comments", MessageKey>;

const CONFLICT_FILES_SHOWN = 5;

function MergeStateRow({
	error,
	onFix,
	pr,
	verdict,
}: {
	error: string | null;
	onFix?: (prompt: string) => void;
	pr: MergeBoxPR;
	verdict: MergeVerdict;
}) {
	const { t } = useTranslation();
	const target = pr.targetBranch || t("prMergeBox.baseBranch");
	let title: string;
	let detail: ReactNode = null;
	switch (verdict.kind) {
		case "ready":
			title = t("pr.card.readyToMerge");
			detail = t("prMergeBox.merge.readyDetail");
			break;
		case "behind":
			title = t("prMergeBox.merge.behind", { target });
			detail = t("prMergeBox.merge.behindDetail");
			break;
		case "conflicts":
			title = t("prMergeBox.merge.conflicts");
			detail = t("prMergeBox.merge.conflictsDetail", { target });
			break;
		case "blocked": {
			title = t("prMergeBox.merge.blocked");
			const blockers = [
				verdict.checks ? t("prMergeBox.blocker.checks") : undefined,
				verdict.review ? t(BLOCKER_KEYS[verdict.review]) : undefined,
			].filter(Boolean);
			detail = blockers.length > 0
				? blockers.join(" · ")
				: t("pr.merge.providerBlocked", { provider: pr.provider === "gitlab" ? "GitLab" : "GitHub" });
			break;
		}
		case "checking":
			title = t("pr.merge.checkingReadiness");
			detail = t("pr.merge.checkingDetail");
			break;
		case "draft":
			title = t("prMergeBox.merge.draft");
			detail = t("prMergeBox.merge.draftDetail");
			break;
		case "unavailable":
			title = t("pr.merge.accessLost");
			detail = t(verdict.reason === "access" ? "pr.merge.accessLostDetail" : "pr.merge.checksUnavailableDetail");
			break;
		case "merged":
			title = verdict.at ? t("prMergeBox.merge.mergedAt", { time: formatTimeCompact(verdict.at) }) : t("prMergeBox.merge.merged");
			break;
		case "closed":
			title = t("prMergeBox.merge.closed");
			break;
	}
	const fixable = verdict.kind === "conflicts" || verdict.kind === "behind";
	const files = verdict.kind === "conflicts" ? verdict.files : [];
	const hiddenFiles = files.length - CONFLICT_FILES_SHOWN;
	return (
		<div className="flex min-w-0 items-start gap-2.5 px-3.5 py-2.5 text-xs" data-testid="pr-merge-state" data-verdict={verdict.kind}>
			<VerdictGlyph verdict={verdict} />
			<div className="min-w-0 flex-1">
				<p className="text-xs font-medium text-foreground">{title}</p>
				{detail ? <p className="mt-0.5 text-xs text-muted-foreground">{detail}</p> : null}
				{files.length > 0 ? (
					<ul className="mt-1.5 flex min-w-0 flex-col gap-0.5" data-testid="pr-conflict-files">
						{files.slice(0, CONFLICT_FILES_SHOWN).map((file) => (
							<li className="min-w-0 truncate font-mono text-2xs text-muted-foreground" key={file.path} title={file.path}>
								{file.url ? (
									<ProductExternalLink className="underline-offset-2 hover:text-foreground hover:underline" href={file.url}>
										{file.path}
									</ProductExternalLink>
								) : file.path}
							</li>
						))}
						{hiddenFiles > 0 ? (
							<li className="text-2xs text-passive">
								{t("pr.overflow", { n: hiddenFiles, noun: t(prNounKeys.file, { count: hiddenFiles }) })}
							</li>
						) : null}
					</ul>
				) : null}
				{error ? <p className="mt-1 text-xs text-destructive" role="alert">{error}</p> : null}
			</div>
			{fixable && onFix ? (
				<Button
					className="h-6 shrink-0 px-2 text-xs"
					onClick={() => onFix(fixMergePrompt(verdict.kind === "conflicts" ? "conflicts" : "behind", pr))}
					size="sm"
					type="button"
					variant="ghost"
				>
					{t("prMergeBox.fix")}
				</Button>
			) : null}
		</div>
	);
}

function VerdictGlyph({ verdict }: { verdict: MergeVerdict }) {
	return (
		<span aria-hidden="true" className="mt-px flex size-4 shrink-0 items-center justify-center">
			{verdict.kind === "ready" ? (
				<CircleCheck className="size-4 text-status-ready" />
			) : verdict.kind === "conflicts" ? (
				<TriangleAlert className="size-4 text-status-needs-you" />
			) : verdict.kind === "behind" || verdict.kind === "blocked" ? (
				<CircleAlert className="size-4 text-status-needs-you" />
			) : verdict.kind === "checking" ? (
				<CircleDashed className="size-4 text-status-validating" />
			) : verdict.kind === "draft" ? (
				<GitPullRequestDraft className="size-4 text-passive" />
			) : verdict.kind === "merged" ? (
				<GitMerge className="size-4 text-status-merged" />
			) : verdict.kind === "closed" ? (
				<GitPullRequestClosed className="size-4 text-muted-foreground" />
			) : (
				<CircleHelp className="size-4 text-muted-foreground" />
			)}
		</span>
	);
}
