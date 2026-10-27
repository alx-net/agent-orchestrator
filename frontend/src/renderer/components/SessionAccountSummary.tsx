import { LoaderCircle } from "lucide-react";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { AgentAccountError, agentAccountOptionName, agentAccountProviderFor, useAgentAccountActions, useAgentAccountLabel, useAgentAccounts, useSessionAgentAccounts } from "../hooks/useAgentAccounts";
import { interfaceTransitionIsActive, useSessionInterfaceTransitionStatus } from "../hooks/useSessionInterfaceTransition";
import { cn } from "../lib/utils";
import type { WorkspaceSession } from "../types/workspace";
import { ConfirmDialog } from "./ConfirmDialog";
import { AccountLimits } from "./settings/AgentAccountsSection";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "./ui/select";

type Note = { text: string; error: boolean };

/** The account the session's agent runs under, its limits, and the move to
 * another account. A move is the session's durable controller handoff: the
 * agent restarts in the same conversation on the new account, or stays on the
 * old one if that fails. Moving a working agent stops its turn after asking. */
export function SessionAccountSummary({ session }: { session: WorkspaceSession }) {
	const { t } = useTranslation();
	const provider = agentAccountProviderFor(session.provider);
	const accounts = useAgentAccounts();
	const current = useSessionAgentAccounts(provider ? session.id : undefined);
	const actions = useAgentAccountActions();
	const label = useAgentAccountLabel();
	const { transition } = useSessionInterfaceTransitionStatus(provider ? session.id : undefined);
	// The answer to the user's own pick; it wins over an older move's status.
	const [note, setNote] = useState<Note | null>(null);
	const [confirming, setConfirming] = useState<string | null>(null);
	const accountMove = transition?.targetAccountId ? transition : undefined;
	const moving = interfaceTransitionIsActive(accountMove);
	const settledPhase = accountMove && !moving ? accountMove.phase : undefined;

	// The binding commits at the end of the move; reread it once the move settles.
	useEffect(() => {
		if (settledPhase) void actions.refreshSession(session.id);
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [settledPhase, accountMove?.id, session.id]);
	// A new move, the user's or AO's after a usage limit, makes an earlier note stale.
	useEffect(() => {
		if (accountMove?.id) setNote(null);
	}, [accountMove?.id]);

	if (!provider || session.cloud || session.isTerminated) return null;
	const options = (accounts.data ?? []).filter((account) => account.provider === provider);
	const currentId = current.data?.[provider];
	const account = options.find((option) => option.id === currentId);
	const working = session.activity?.state === "active" || session.activity?.state === "blocked";
	// "Moved from … to …" answers the move; the next message makes it history.
	const messagedSinceMove = Boolean(accountMove && session.lastUserMessageAt &&
		Date.parse(session.lastUserMessageAt) > Date.parse(accountMove.updatedAt));

	const move = async (accountId: string, stopTurn = false) => {
		if (accountId === currentId) {
			setNote({ text: t("settings.agentAccounts.alreadyOn", { email: label(accountId) }), error: false });
			return;
		}
		// A working agent may only be waiting on the old account's limit; moving
		// stops that turn, so ask first.
		if (working && !stopTurn) {
			setConfirming(accountId);
			return;
		}
		setConfirming(null);
		setNote(null);
		try {
			await actions.switchSession(session.id, accountId, stopTurn);
		} catch (err) {
			if (err instanceof AgentAccountError && err.code === "AGENT_ACCOUNT_UNCHANGED") {
				// The account shown was out of date: the session is already there.
				await actions.refreshSession(session.id);
				setNote({ text: t("settings.agentAccounts.alreadyOn", { email: label(accountId) }), error: false });
				return;
			}
			setNote({ text: err instanceof Error ? err.message : String(err), error: true });
		}
	};

	const restart = accountMove?.sourceAccountId === accountMove?.targetAccountId;
	const status: Note | null = note
		?? (moving
			? { text: t("settings.agentAccounts.switching", { email: label(accountMove?.targetAccountId) }), error: false }
			: settledPhase === "completed" && accountMove?.targetAccountId === currentId && !messagedSinceMove
				? {
					text: restart
						? t("settings.agentAccounts.restarted", { email: label(currentId) })
						: t("settings.agentAccounts.moved", { from: label(accountMove?.sourceAccountId), to: label(currentId) }),
					error: false,
				}
				: settledPhase === "failed" || settledPhase === "recovery_required"
					? { text: accountMove?.errorDetail || t("settings.agentAccounts.switchFailed"), error: true }
					: null);
	const missingServers = account?.mcpNeedsAuth ?? [];

	return <section className="mt-3 border-t border-(--color-border-settings-input) pt-3" data-testid="session-account-summary">
		<div className="flex items-center justify-between gap-3">
			<p className="text-xs text-muted-foreground">{t("settings.agentAccounts.sessionAccount")}</p>
			<div className="flex min-w-0 items-center gap-2">
				{moving ? <LoaderCircle className="size-3.5 animate-spin text-muted-foreground" aria-hidden="true" /> : null}
				<Select value={currentId ?? ""} disabled={moving || options.length < 2} onValueChange={(value) => void move(value)}>
					<SelectTrigger size="sm" className="max-w-56" aria-label={t("settings.agentAccounts.sessionAccount")}><SelectValue /></SelectTrigger>
					<SelectContent align="end">
						{options.map((option) => <SelectItem key={option.id} value={option.id} disabled={!option.signedIn}>{agentAccountOptionName(option, options, t)}</SelectItem>)}
					</SelectContent>
				</Select>
			</div>
		</div>
		{status ? (
			<p className={cn("mt-2 text-xs", status.error ? "text-destructive" : "text-muted-foreground")} role={status.error ? "alert" : "status"}>
				{status.text}
			</p>
		) : null}
		{missingServers.length ? (
			<p className="mt-2 text-xs text-warning" data-testid="session-account-mcp-needs-auth">
				{t("settings.agentAccounts.mcpNeedsAuth", { servers: missingServers.join(", ") })}
			</p>
		) : null}
		{account ? <AccountLimits account={account} /> : null}
		{confirming ? <ConfirmDialog open
			title={t("settings.agentAccounts.stopAndMoveTitle")}
			description={session.activity?.state === "blocked"
				? t("settings.agentAccounts.stopAndMoveDeciding", { email: label(confirming) })
				: t("settings.agentAccounts.stopAndMoveWorking", { email: label(confirming) })}
			confirmLabel={t("settings.agentAccounts.stopAndMove")}
			onConfirm={() => void move(confirming, true)}
			onOpenChange={(open) => { if (!open) setConfirming(null); }}
		/> : null}
	</section>;
}
