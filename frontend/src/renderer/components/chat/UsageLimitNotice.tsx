import { useEffect, useState } from "react";
import type { TFunction } from "i18next";
import { Trans, useTranslation } from "react-i18next";
import { Gauge, Loader2, Play } from "lucide-react";
import { useAgentAccountLabel } from "../../hooks/useAgentAccounts";
import { cn } from "../../lib/utils";
import type { ConversationUsageLimit, UsageLimitSwitch } from "../../types/conversation";
import { Button } from "../ui/button";

export interface UsageLimitNoticeProps {
	usageLimit: ConversationUsageLimit;
	onResume?: () => void;
	onAutoResumeChange?: (enabled: boolean) => void;
	pending?: boolean;
	/** Prevents starting work; cancellation only waits for an in-flight request. */
	disabled?: boolean;
	error?: string;
	/** A historical marker; current resume controls belong by the composer. */
	compact?: boolean;
	/** AO moving the turn to another account with usage left. */
	accountSwitch?: UsageLimitSwitch;
}

const resetTimeFormatter = new Intl.DateTimeFormat(undefined, {
	dateStyle: "medium",
	timeStyle: "short",
});

function remainingTime(milliseconds: number, t: TFunction): string {
	const seconds = Math.max(0, Math.ceil(milliseconds / 1000));
	const hours = Math.floor(seconds / 3600);
	const minutes = Math.floor((seconds % 3600) / 60);
	if (hours > 0) return t("chat.usageLimit.remainingHours", { hours, minutes });
	if (minutes > 0) return t("chat.usageLimit.remainingMinutes", { minutes, seconds: seconds % 60 });
	return t("chat.usageLimit.remainingSeconds", { seconds });
}

/** What a move after a usage limit says: where it goes, then where the turn
 * continued. Shared by the composer notice and the timeline row. */
export function usageLimitSwitchCopy(
	sw: UsageLimitSwitch,
	label: (accountId: string | undefined) => string,
	t: TFunction,
): { message: string; detail?: string } {
	const email = label(sw.accountId);
	const from = label(sw.fromAccountId);
	// The same account: the agent restarted to pick up the account's current sign-in.
	const restart = Boolean(sw.accountId) && sw.accountId === sw.fromAccountId;
	const missing = sw.mcpNeedsAuth?.length
		? t("chat.limitSwitch.mcpMissing", { servers: sw.mcpNeedsAuth.join(", ") })
		: undefined;
	const join = (...parts: (string | undefined)[]) => parts.filter(Boolean).join(" ");
	switch (sw.phase) {
		case "switching":
			return {
				message: t(restart ? "chat.limitSwitch.restarting" : "chat.limitSwitch.switching", { email }),
				detail: restart ? t("chat.limitSwitch.restartingDetail") : t("chat.limitSwitch.switchingDetail", { from }),
			};
		case "continued":
			return {
				message: t(restart ? "chat.limitSwitch.restarted" : "chat.limitSwitch.continued", { email }),
				detail: join(restart ? t("chat.limitSwitch.restartedDetail") : t("chat.limitSwitch.continuedDetail", { from }), missing),
			};
		case "switched":
			return {
				message: t("chat.limitSwitch.switched", { email }),
				detail: join(t("chat.limitSwitch.switchedDetail", { from }), missing),
			};
		case "failed":
			return {
				message: t("chat.limitSwitch.failed", { email }),
				detail: join(sw.error, t("chat.limitSwitch.failedDetail")),
			};
	}
}

function AccountSwitchStatus({ accountSwitch }: { accountSwitch: UsageLimitSwitch }) {
	const { t } = useTranslation();
	const label = useAgentAccountLabel();
	const copy = usageLimitSwitchCopy(accountSwitch, label, t);
	const failed = accountSwitch.phase === "failed";
	return (
		<p
			role={failed ? "alert" : "status"}
			className={cn("mt-1 text-pretty text-xs leading-5", failed ? "text-destructive" : "text-muted-foreground")}
			data-testid="usage-limit-account-switch"
		>
			{accountSwitch.phase === "switching" ? (
				<Loader2 aria-hidden="true" className="mr-1.5 inline size-3 animate-spin align-[-2px] motion-reduce:animate-none" />
			) : null}
			<span className={cn(!failed && "text-foreground")}>{copy.message}</span>
			{copy.detail ? <> <span>{copy.detail}</span></> : null}
		</p>
	);
}

/** The timer updates presentation only; the daemon owns automatic resumption. */
export function UsageLimitNotice({
	usageLimit,
	onResume,
	onAutoResumeChange,
	pending = false,
	disabled = false,
	error,
	compact = false,
	accountSwitch,
}: UsageLimitNoticeProps) {
	const { t } = useTranslation();
	const [now, setNow] = useState(Date.now);
	const parsedReset = usageLimit.resetsAt ? Date.parse(usageLimit.resetsAt) : NaN;
	const resetAt = Number.isFinite(parsedReset) ? parsedReset : undefined;
	// A move to another account replaces the wait for the old account's reset.
	const moving = accountSwitch?.phase === "switching";
	const moved = accountSwitch?.phase === "switched" || accountSwitch?.phase === "continued";
	const waitsForReset = !moving && !moved;
	const ready = moved || resetAt === undefined || now >= resetAt;
	const actionError = error ?? usageLimit.resumeError;
	const scheduled = usageLimit.autoResume;
	const showResume = onResume && ready && !scheduled && !moving;
	const showAutoResume = onAutoResumeChange && waitsForReset && (scheduled || (resetAt !== undefined && !ready));

	useEffect(() => {
		if (compact || resetAt === undefined || !waitsForReset) return;
		const update = () => setNow(Date.now());
		update();
		const timer = window.setInterval(() => {
			update();
			if (Date.now() >= resetAt) window.clearInterval(timer);
		}, 1000);
		window.addEventListener("focus", update);
		document.addEventListener("visibilitychange", update);
		return () => {
			window.clearInterval(timer);
			window.removeEventListener("focus", update);
			document.removeEventListener("visibilitychange", update);
		};
	}, [compact, resetAt, waitsForReset]);

	return (
		<div
			data-testid="usage-limit-notice"
			aria-busy={pending || moving || undefined}
			className={cn(
				"flex min-w-0 flex-wrap items-center gap-x-4 gap-y-2",
				compact ? "py-1" : "px-2 py-2.5",
			)}
		>
			<div className={cn("flex min-w-0 flex-1 items-start gap-2.5", !compact && "basis-64")}>
				<Gauge aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-warning" />
				<div className="min-w-0 flex-1">
					<div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
						<strong className="text-balance text-xs font-medium text-foreground">{t("chat.usageLimit.title")}</strong>
						{!compact && waitsForReset && resetAt !== undefined && (!ready || !scheduled) ? (
							<span className="text-pretty text-xs text-muted-foreground">
								{ready ? <span role="status">{t("chat.usageLimit.ready")}</span> : (
									<Trans
										t={t} i18nKey="chat.usageLimit.availableIn"
										values={{ duration: remainingTime(resetAt - now, t) }}
										components={{ duration: <span className="font-medium tabular-nums text-foreground" /> }}
									/>
								)}
							</span>
						) : null}
					</div>
					{!compact && accountSwitch ? <AccountSwitchStatus accountSwitch={accountSwitch} /> : null}
					{!compact && waitsForReset ? (
						<p className="mt-1 text-pretty text-xs leading-5 text-muted-foreground">
							{scheduled ? <span role="status" className="mr-2 text-foreground">{t(ready ? "chat.usageLimit.waiting" : "chat.usageLimit.scheduled")}</span> : null}
							{resetAt === undefined ? <span>{t("chat.usageLimit.unknownReset")}</span> : (
								<Trans
									t={t} i18nKey="chat.usageLimit.resetsAt"
									values={{ time: resetTimeFormatter.format(resetAt) }}
									components={{ time: <time className="tabular-nums" dateTime={usageLimit.resetsAt} /> }}
								/>
							)}
							{resetAt === undefined && !scheduled ? <> <span>{t("chat.usageLimit.manualResumeHint")}</span></> : null}
						</p>
					) : null}
					{actionError ? <p role="alert" className="mt-1 whitespace-pre-wrap wrap-anywhere text-pretty text-xs text-destructive">{actionError}</p> : null}
				</div>
			</div>
			{!compact && (showResume || showAutoResume) ? (
				<div className="ml-auto flex flex-wrap items-center gap-1">
					{showResume ? (
						<Button
							type="button" size="sm" variant="primary" onClick={onResume}
							disabled={pending || disabled}
							className="focus-visible:ring-2 focus-visible:ring-ring"
						>
							{pending ? <Loader2 aria-hidden="true" className="size-3.5 animate-spin motion-reduce:animate-none" /> : <Play aria-hidden="true" className="size-3.5" />}
							{t("chat.usageLimit.resume")}
						</Button>
					) : null}
					{showAutoResume ? (
						<Button
							type="button" size="sm" variant={scheduled ? "ghost" : "secondary"}
							onClick={() => onAutoResumeChange?.(!usageLimit.autoResume)}
								disabled={pending || (!scheduled && disabled)}
							className="focus-visible:ring-2 focus-visible:ring-ring"
						>
							{t(usageLimit.autoResume ? "chat.usageLimit.cancelAutoResume" : "chat.usageLimit.autoResume")}
						</Button>
					) : null}
				</div>
			) : null}
		</div>
	);
}
