/**
 * Run a shell command from an agent's reply, as the user.
 *
 * Agents often stop on a step only the user may take (a guarded apply, an
 * unlocked 1Password, an interactive prompt) and hand over a code block to
 * paste into a terminal, then ask for the output back. The Run button does
 * both halves: the daemon opens a shell tab in the session's worktree, types
 * the command, and sends the exit status and output to the agent when the
 * shell returns to its prompt.
 */

import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Loader2, Play } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { apiClient, apiErrorMessage } from "../../lib/api-client";
import { markTerminalHandleFresh } from "../../lib/fresh-terminal-handles";
import { shellTerminalsQueryKey, toShellTerminal, type ShellTerminal } from "../../hooks/useShellTerminals";
import { terminalShellRequestValue, useTerminalShellStore } from "../../stores/terminal-shell-store";
import { useUiStore } from "../../stores/ui-store";
import { ConfirmDialog } from "../ConfirmDialog";

const SHELL_LANGUAGES = new Set(["bash", "sh", "zsh", "shell", "shellscript"]);

/** Fences the button may run: shell scripts, not `console` transcripts with prompts and output. */
export function isRunnableShellLanguage(language: string | undefined): boolean {
	return SHELL_LANGUAGES.has((language ?? "").trim().toLowerCase());
}

type RunCommand = { request: (command: string) => void; pending: string | null };

const RunCommandContext = createContext<RunCommand | null>(null);
/** Set by agent replies only: reasoning, worker reports, and PR comments never get a Run button. */
const RunnableCodeScope = createContext(false);

export function RunnableCode({ children }: { children: ReactNode }) {
	return <RunnableCodeScope.Provider value>{children}</RunnableCodeScope.Provider>;
}

export async function runSessionCommand(sessionId: string, command: string, shell?: string) {
	const { data, error } = await apiClient.POST("/api/v1/sessions/{sessionId}/terminal-commands", {
		params: { path: { sessionId } },
		body: { command, shell },
	});
	if (error || !data) throw new Error(apiErrorMessage(error, "Could not run the command."));
	return toShellTerminal(data.shellTerminal);
}

/**
 * Owns the confirmation and the request for one local session's timeline. The
 * dialog shows the whole command, wrapped: the block itself scrolls sideways
 * and may hide the part that matters.
 */
export function RunCommandProvider({ sessionId, children }: { sessionId?: string; children: ReactNode }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const setActiveShellTerminal = useUiStore((state) => state.setActiveShellTerminal);
	const showGlobalToast = useUiStore((state) => state.showGlobalToast);
	const [confirming, setConfirming] = useState<string | null>(null);
	const [pending, setPending] = useState<string | null>(null);
	const pendingRef = useRef(false);

	const run = useCallback(async (command: string) => {
		if (pendingRef.current || !sessionId) return;
		pendingRef.current = true;
		setPending(command);
		setConfirming(null);
		try {
			await useTerminalShellStore.getState().load();
			const shell = terminalShellRequestValue(useTerminalShellStore.getState().preference);
			const terminal = await runSessionCommand(sessionId, command, shell);
			if (!queryClient.getQueryData<ShellTerminal[]>(shellTerminalsQueryKey)?.some((item) => item.handleId === terminal.handleId)) {
				markTerminalHandleFresh(terminal.handleId);
			}
			queryClient.setQueryData<ShellTerminal[]>(shellTerminalsQueryKey, (current = []) => [
				terminal,
				...current.filter((item) => item.handleId !== terminal.handleId),
			]);
			setActiveShellTerminal(terminal.handleId);
		} catch (error) {
			showGlobalToast(t("runCommand.failed"), apiErrorMessage(error, t("runCommand.failed")), "error");
		} finally {
			pendingRef.current = false;
			setPending(null);
		}
	}, [queryClient, sessionId, setActiveShellTerminal, showGlobalToast, t]);

	// Without a runnable session (remote, cloud, ended) the blocks show no button.
	// The provider itself stays mounted so the timeline beneath it never remounts.
	const value = useMemo<RunCommand | null>(() => (sessionId ? { request: setConfirming, pending } : null), [pending, sessionId]);
	return (
		<RunCommandContext.Provider value={value}>
			{children}
			<ConfirmDialog
				open={confirming !== null}
				title={t("runCommand.title")}
				description={t("runCommand.description")}
				confirmLabel={t("runCommand.confirm")}
				onConfirm={() => { if (confirming !== null) void run(confirming); }}
				onOpenChange={(open) => { if (!open) setConfirming(null); }}
			>
				<pre
					data-testid="run-command-preview"
					className="settings-thin-scrollbar max-h-60 overflow-y-auto whitespace-pre-wrap break-all rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs leading-[1.6] text-foreground"
				>
					{confirming}
				</pre>
			</ConfirmDialog>
		</RunCommandContext.Provider>
	);
}

/** The code block's Run action. Visible, unlike the hover-revealed copy and wrap: it is the block's point. */
export function RunCodeButton({ code, language, streaming }: { code: string; language?: string; streaming: boolean }) {
	const { t } = useTranslation();
	const runner = useContext(RunCommandContext);
	const runnable = useContext(RunnableCodeScope);
	if (!runner || !runnable || streaming || !isRunnableShellLanguage(language) || !code.trim()) return null;
	const busy = runner.pending === code;
	return (
		<button
			type="button"
			onClick={() => runner.request(code)}
			disabled={runner.pending !== null}
			aria-label={t("runCommand.runLabel")}
			title={t("runCommand.runLabel")}
			className="flex h-7 items-center gap-1 rounded-md px-2 text-xs text-muted-foreground transition-[background-color,color,transform] hover:bg-interactive-hover hover:text-foreground disabled:opacity-50"
		>
			{busy ? <Loader2 aria-hidden="true" className="size-3 animate-spin motion-reduce:animate-none" /> : <Play aria-hidden="true" className="size-3" />}
			{t("runCommand.run")}
		</button>
	);
}
