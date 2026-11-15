import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMcpActions, useMcpConnect, useSessionMcpServers, type McpServerRow } from "../hooks/useMcpServers";
import type { WorkspaceSession } from "../types/workspace";
import { Button } from "./ui/button";
import { HarnessStateText } from "./settings/harness/harness-ui";
import { mcpStatusWords } from "./settings/McpServersSection";

function LiveServerRow({ sessionId, server }: { sessionId: string; server: McpServerRow }) {
	const { t } = useTranslation();
	const actions = useMcpActions();
	const status = server.accounts[0];
	const connect = useMcpConnect({ name: server.name, sessionId });
	const [reconnecting, setReconnecting] = useState(false);
	const [error, setError] = useState("");
	if (!status) return null;
	const reconnect = async () => {
		setReconnecting(true);
		setError("");
		try {
			await actions.reconnect(sessionId, server.name);
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setReconnecting(false);
		}
	};
	const connected = status.status === "connected";
	const tone = connected ? "ok" : status.status === "disabled" || status.status === "starting" ? "off" : "error";
	return <li className="flex flex-col gap-0.5 py-1" data-testid={`session-mcp-${server.name}`}>
		<div className="flex items-center justify-between gap-2">
			<span className="min-w-0 truncate text-xs text-foreground">{server.name}</span>
			<div className="flex shrink-0 items-center gap-2">
				<HarnessStateText tone={tone}>
					{connected && status.toolCount !== undefined ? t("settings.mcp.tools", { count: status.toolCount }) : mcpStatusWords(status, t)}
				</HarnessStateText>
				{status.action === "connect" ? (
					<Button type="button" size="sm" variant="outline" disabled={connect.busy} onClick={() => void connect.start()}>{t("settings.mcp.connect.action")}</Button>
				) : !connected && status.status !== "disabled" ? (
					<Button type="button" size="sm" variant="ghost" disabled={reconnecting} onClick={() => void reconnect()}>{t("settings.mcp.reconnect")}</Button>
				) : null}
			</div>
		</div>
		{status.error ? <p className="text-xs break-words text-muted-foreground">{status.error}</p> : null}
		{connect.busy ? <p className="text-xs text-muted-foreground" role="status">{t("settings.mcp.connect.waiting")}</p> : null}
		{connect.error || error ? <p className="text-xs text-destructive" role="alert">{connect.error || error}</p> : null}
	</li>;
}

/** What this running agent actually has right now, from the agent itself: config
 * and a running agent can differ, and only this answers "does it have it". */
export function SessionMcpServers({ session }: { session: WorkspaceSession }) {
	const { t } = useTranslation();
	const eligible = session.provider === "claude-code" && !session.cloud && !session.isTerminated;
	const servers = useSessionMcpServers(session.id, eligible);
	if (!eligible) return null;
	const rows = servers.data?.servers ?? [];
	if (rows.length === 0) return null;
	return <section className="mt-3 border-t border-(--color-border-settings-input) pt-3" data-testid="session-mcp-servers">
		<p className="text-xs text-muted-foreground">
			{servers.data?.live ? t("settings.mcp.session.live") : t("settings.mcp.session.notRunning")}
		</p>
		<ul className="mt-1">
			{rows.map((server) => <LiveServerRow key={server.name} sessionId={session.id} server={server} />)}
		</ul>
	</section>;
}
