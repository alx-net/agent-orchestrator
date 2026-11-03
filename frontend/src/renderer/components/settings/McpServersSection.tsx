import { ExternalLink, LoaderCircle, Plus, RefreshCw, Trash2 } from "lucide-react";
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { agentAccountName, useAgentAccounts } from "../../hooks/useAgentAccounts";
import { useMcpActions, useMcpConnect, useMcpServers, useMcpTest, type AddMcpServerInput, type McpAccountStatus, type McpServerRow } from "../../hooks/useMcpServers";
import { aoBridge } from "../../lib/bridge";
import { ConfirmDialog } from "../ConfirmDialog";
import { Button } from "../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { HarnessStateText, type HarnessTone } from "./harness/harness-ui";
import { SettingsSection } from "./SettingsSection";

/** The status in plain words. A state AO has no word for shows Claude Code's own. */
export function mcpStatusWords(status: Pick<McpAccountStatus, "status" | "rawStatus">, t: TFunction): string {
	switch (status.status) {
		case "connected": return t("settings.mcp.status.connected");
		case "needs_sign_in": return t("settings.mcp.status.needsSignIn");
		case "key_rejected": return t("settings.mcp.status.keyRejected");
		case "needs_approval": return t("settings.mcp.status.needsApproval");
		case "rejected": return t("settings.mcp.status.rejected");
		case "failed": return t("settings.mcp.status.failed");
		case "starting": return t("settings.mcp.status.starting");
		case "disabled": return t("settings.mcp.status.disabled");
		case "not_checked": return t("settings.mcp.status.notChecked");
		default: return status.rawStatus || t("settings.mcp.status.unknown");
	}
}

function statusTone(status: McpAccountStatus["status"]): HarnessTone {
	if (status === "connected") return "ok";
	if (status === "failed" || status === "key_rejected" || status === "needs_sign_in" || status === "needs_approval" || status === "rejected") return "error";
	return "off";
}

function authWords(server: McpServerRow, t: TFunction): string {
	switch (server.auth) {
		case "oauth": return t("settings.mcp.auth.oauth");
		case "api_key": return t("settings.mcp.auth.apiKey");
		case "helper": return t("settings.mcp.auth.helper");
		case "none": return t("settings.mcp.auth.none");
		case "connector": return t("settings.mcp.auth.connector");
		default: return t("settings.mcp.auth.unknown");
	}
}

function scopeWords(server: McpServerRow, t: TFunction): string {
	switch (server.scope) {
		case "global": return t("settings.mcp.scope.global");
		case "repo": return server.projectName ? t("settings.mcp.scope.repoNamed", { project: server.projectName }) : t("settings.mcp.scope.repo");
		case "local": return t("settings.mcp.scope.local");
		case "connector": return t("settings.mcp.scope.connector");
		case "plugin": return t("settings.mcp.scope.plugin");
		default: return t("settings.mcp.scope.other");
	}
}

function checkedWords(iso: string | null | undefined, t: TFunction): string | null {
	if (!iso) return null;
	const seconds = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
	const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
	const time = seconds < 60 ? rtf.format(-seconds, "second")
		: seconds < 3600 ? rtf.format(-Math.round(seconds / 60), "minute")
			: rtf.format(-Math.round(seconds / 3600), "hour");
	return t("settings.mcp.checked", { time });
}

/** The state of one sign-in, in the words a person needs next. */
function ConnectNote({ connect, t }: { connect: ReturnType<typeof useMcpConnect>; t: TFunction }) {
	const { operation, error } = connect;
	if (error) return <p className="text-xs text-destructive" role="alert">{error}</p>;
	if (!operation) return null;
	switch (operation.state) {
		case "waiting":
			return <p className="text-xs text-muted-foreground" role="status">
				{t("settings.mcp.connect.waiting")}
				{operation.authUrl ? <> <button type="button" className="underline underline-offset-2" onClick={() => void aoBridge.app.openExternal(operation.authUrl ?? "")}>{t("settings.mcp.connect.openAgain")}</button></> : null}
			</p>;
		case "waiting_external":
			return <p className="text-xs text-muted-foreground" role="status">{t("settings.mcp.connect.external")}</p>;
		case "connected":
			return <p className="text-xs text-muted-foreground" role="status">
				{operation.applied.restarting?.length ? t("settings.mcp.connect.connectedRestarting") : t("settings.mcp.connect.connected")}
			</p>;
		case "cancelled":
			return null;
		default:
			return <p className="text-xs text-destructive" role="alert">{operation.error || t("settings.mcp.connect.failed")}</p>;
	}
}

function AccountRow({ server, status, label, onEditKey }: {
	server: McpServerRow;
	status: McpAccountStatus;
	label: string;
	onEditKey: () => void;
}) {
	const { t } = useTranslation();
	const actions = useMcpActions();
	const target = { name: server.name, accountId: status.accountId, ...(server.projectId ? { projectId: server.projectId } : {}) };
	const connect = useMcpConnect(target);
	const test = useMcpTest();
	const [approveError, setApproveError] = useState<string>();
	const [approving, setApproving] = useState(false);
	const approve = async () => {
		setApproving(true);
		setApproveError(undefined);
		try {
			await actions.approve(target);
		} catch (reason) {
			setApproveError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setApproving(false);
		}
	};
	const checked = checkedWords(status.checkedAt, t);
	const details = [
		status.toolCount !== undefined ? t("settings.mcp.tools", { count: status.toolCount }) : null,
		checked,
	].filter(Boolean).join(" · ");
	return <div className="flex flex-col gap-1 py-1" data-mcp-account={status.accountId}>
		<div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1">
			<div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5">
				<span className="min-w-0 truncate text-xs text-settings-muted">{label}</span>
				<HarnessStateText tone={statusTone(status.status)}>{mcpStatusWords(status, t)}</HarnessStateText>
				{details ? <span className="text-xs text-settings-muted tabular-nums">{details}</span> : null}
			</div>
			<div className="flex shrink-0 items-center gap-1.5">
				{status.action === "connect" ? (
					connect.busy
						? <Button type="button" size="sm" variant="ghost" onClick={() => void connect.cancel()}>{t("settings.mcp.connect.cancel")}</Button>
						: <Button type="button" size="sm" variant="outline" onClick={() => void connect.start()}>{t("settings.mcp.connect.action")}</Button>
				) : null}
				{status.action === "approve" ? <Button type="button" size="sm" variant="outline" disabled={approving} onClick={() => void approve()}>{t("settings.mcp.approve")}</Button> : null}
				{status.action === "edit_key" ? <Button type="button" size="sm" variant="ghost" onClick={onEditKey}>{t("settings.mcp.editKey")}</Button> : null}
				{server.manageUrl ? <Button type="button" size="sm" variant="ghost" onClick={() => void aoBridge.app.openExternal(server.manageUrl ?? "")}>
					{t("settings.mcp.manage")}<ExternalLink aria-hidden="true" className="ml-1 size-3" />
				</Button> : null}
				{status.status !== "needs_approval" ? <Button type="button" size="sm" variant="ghost" disabled={test.busy} onClick={() => void test.run(target)}>
					{test.busy ? <LoaderCircle aria-hidden="true" className="mr-1 size-3 animate-spin" /> : null}
					{t("settings.mcp.test")}
				</Button> : null}
			</div>
		</div>
		{status.error ? <p className="text-xs break-words text-muted-foreground">{status.error}</p> : null}
		{status.status === "key_rejected" && server.auth === "helper" ? <p className="text-xs text-muted-foreground">{t("settings.mcp.helperRejected")}</p> : null}
		{status.status === "needs_approval" ? <p className="text-xs text-muted-foreground">{t("settings.mcp.approvalHint")}</p> : null}
		{approveError ? <p className="text-xs text-destructive" role="alert">{approveError}</p> : null}
		<ConnectNote connect={connect} t={t} />
		{test.error ? <p className="text-xs text-destructive" role="alert">{test.error}</p> : null}
		{test.result ? <p className={test.result.ok ? "text-xs text-muted-foreground" : "text-xs text-destructive"} role="status" data-testid="mcp-test-result">
			{test.result.ok ? t("settings.mcp.testPassed") : t("settings.mcp.testFailed")}: {test.result.reason}
		</p> : null}
	</div>;
}

function ServerRow({ server, labels, onEditKey, onRemove }: {
	server: McpServerRow;
	labels: Map<string, string>;
	onEditKey: () => void;
	onRemove: () => void;
}) {
	const { t } = useTranslation();
	return <div className="settings-content-row flex flex-col gap-1" data-mcp-server={server.name}>
		<div className="flex items-start justify-between gap-3">
			<div className="min-w-0">
				<div className="truncate text-sm leading-5 text-settings-label">{server.name}</div>
				<div className="text-xs leading-4 text-settings-muted [overflow-wrap:anywhere]">
					{[scopeWords(server, t), authWords(server, t), server.target].filter(Boolean).join(" · ")}
				</div>
			</div>
			{server.scope === "global" ? <Button type="button" size="icon" variant="ghost" className="size-6 shrink-0" aria-label={t("settings.mcp.removeNamed", { name: server.name })} onClick={onRemove}>
				<Trash2 aria-hidden="true" className="size-3.5" />
			</Button> : null}
		</div>
		{server.accounts.map((status) => <AccountRow key={status.accountId} server={server} status={status} label={labels.get(status.accountId) ?? status.accountId} onEditKey={onEditKey} />)}
	</div>;
}

type Pair = { name: string; value: string };

function PairFields({ pairs, onChange, nameLabel, valueLabel, addLabel }: {
	pairs: Pair[];
	onChange: (pairs: Pair[]) => void;
	nameLabel: string;
	valueLabel: string;
	addLabel: string;
}) {
	return <div className="flex flex-col gap-2">
		{pairs.map((pair, index) => <div key={index} className="flex items-center gap-2">
			<Input aria-label={nameLabel} placeholder={nameLabel} value={pair.name} autoComplete="off"
				onChange={(event) => onChange(pairs.map((item, i) => i === index ? { ...item, name: event.target.value } : item))} />
			{/* Masked: a key on screen is a key someone can read over a shoulder. */}
			<Input aria-label={valueLabel} placeholder={valueLabel} type="password" value={pair.value} autoComplete="off"
				onChange={(event) => onChange(pairs.map((item, i) => i === index ? { ...item, value: event.target.value } : item))} />
		</div>)}
		<div><Button type="button" size="sm" variant="ghost" onClick={() => onChange([...pairs, { name: "", value: "" }])}>
			<Plus aria-hidden="true" className="mr-1 size-3.5" />{addLabel}
		</Button></div>
	</div>;
}

function pairsToObject(pairs: Pair[]): Record<string, string> | undefined {
	const out: Record<string, string> = {};
	for (const pair of pairs) {
		if (pair.name.trim()) out[pair.name.trim()] = pair.value;
	}
	return Object.keys(out).length ? out : undefined;
}

function isUrl(value: string): boolean {
	return /^https?:\/\//i.test(value.trim());
}

function AddServerDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
	const { t } = useTranslation();
	const actions = useMcpActions();
	const [name, setName] = useState("");
	const [target, setTarget] = useState("");
	const [pairs, setPairs] = useState<Pair[]>([]);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const url = isUrl(target);
	const close = (next: boolean) => {
		if (!next) {
			setName("");
			setTarget("");
			setPairs([]);
			setError("");
		}
		onOpenChange(next);
	};
	const submit = async (event: FormEvent) => {
		event.preventDefault();
		setBusy(true);
		setError("");
		const values = pairsToObject(pairs);
		const body: AddMcpServerInput = url
			? { name: name.trim(), url: target.trim(), ...(values ? { headers: values } : {}) }
			: (() => {
				const [command = "", ...args] = target.trim().split(/\s+/);
				return { name: name.trim(), command, args, ...(values ? { env: values } : {}) };
			})();
		try {
			await actions.add(body);
			close(false);
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setBusy(false);
		}
	};
	return <Dialog open={open} onOpenChange={close}>
		<DialogContent>
			<DialogHeader>
				<DialogTitle>{t("settings.mcp.add.title")}</DialogTitle>
				<DialogDescription>{t("settings.mcp.add.description")}</DialogDescription>
			</DialogHeader>
			<form className="flex flex-col gap-3" onSubmit={(event) => void submit(event)}>
				<div className="flex flex-col gap-1.5">
					<Label htmlFor="mcp-add-name">{t("settings.mcp.add.name")}</Label>
					<Input id="mcp-add-name" value={name} autoComplete="off" placeholder={t("settings.mcp.add.namePlaceholder")} onChange={(event) => setName(event.target.value)} />
				</div>
				<div className="flex flex-col gap-1.5">
					<Label htmlFor="mcp-add-target">{t("settings.mcp.add.target")}</Label>
					<Input id="mcp-add-target" value={target} autoComplete="off" placeholder={t("settings.mcp.add.targetPlaceholder")} onChange={(event) => setTarget(event.target.value)} />
				</div>
				<div className="flex flex-col gap-1.5">
					<Label>{url ? t("settings.mcp.add.headers") : t("settings.mcp.add.env")}</Label>
					<PairFields pairs={pairs} onChange={setPairs}
						nameLabel={url ? t("settings.mcp.add.headerName") : t("settings.mcp.add.envName")}
						valueLabel={t("settings.mcp.add.value")}
						addLabel={url ? t("settings.mcp.add.addHeader") : t("settings.mcp.add.addEnv")} />
				</div>
				{error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
				<DialogFooter>
					<Button type="button" variant="ghost" onClick={() => close(false)}>{t("settings.mcp.cancel")}</Button>
					<Button type="submit" disabled={busy || !name.trim() || !target.trim()}>{t("settings.mcp.add.submit")}</Button>
				</DialogFooter>
			</form>
		</DialogContent>
	</Dialog>;
}

function EditKeyDialog({ server, onClose }: { server: McpServerRow; onClose: () => void }) {
	const { t } = useTranslation();
	const actions = useMcpActions();
	// Existing values never reach the app: blank keeps that header as it is.
	const [pairs, setPairs] = useState<Pair[]>((server.headerNames ?? []).map((name) => ({ name, value: "" })));
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const submit = async (event: FormEvent) => {
		event.preventDefault();
		setBusy(true);
		setError("");
		try {
			await actions.setKey(server.name, pairsToObject(pairs) ?? {});
			onClose();
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setBusy(false);
		}
	};
	return <Dialog open onOpenChange={(open) => { if (!open) onClose(); }}>
		<DialogContent>
			<DialogHeader>
				<DialogTitle>{t("settings.mcp.key.title", { name: server.name })}</DialogTitle>
				<DialogDescription>{t("settings.mcp.key.description")}</DialogDescription>
			</DialogHeader>
			<form className="flex flex-col gap-3" onSubmit={(event) => void submit(event)}>
				<PairFields pairs={pairs} onChange={setPairs} nameLabel={t("settings.mcp.add.headerName")} valueLabel={t("settings.mcp.key.value")} addLabel={t("settings.mcp.add.addHeader")} />
				{error ? <p className="text-xs text-destructive" role="alert">{error}</p> : null}
				<DialogFooter>
					<Button type="button" variant="ghost" onClick={onClose}>{t("settings.mcp.cancel")}</Button>
					<Button type="submit" disabled={busy}>{t("settings.mcp.key.submit")}</Button>
				</DialogFooter>
			</form>
		</DialogContent>
	</Dialog>;
}

/** Every MCP server and how it stands on each subscription, with what fixes it. */
export function McpServersSection({ titleHidden }: { titleHidden?: boolean }) {
	const { t } = useTranslation();
	const servers = useMcpServers();
	const accounts = useAgentAccounts();
	const actions = useMcpActions();
	const [adding, setAdding] = useState(false);
	const [editing, setEditing] = useState<McpServerRow | null>(null);
	const [removing, setRemoving] = useState<McpServerRow | null>(null);
	const [refreshing, setRefreshing] = useState(false);
	const [error, setError] = useState("");
	const labels = new Map<string, string>();
	for (const account of servers.data?.accounts ?? []) {
		const known = accounts.data?.find((row) => row.id === account.id);
		labels.set(account.id, account.device ? t("settings.harness.thisComputer") : known ? agentAccountName(known, t) : account.email ?? account.id);
	}
	const refresh = async () => {
		setRefreshing(true);
		setError("");
		try {
			await actions.refresh();
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setRefreshing(false);
		}
	};
	const remove = async () => {
		if (!removing) return;
		setError("");
		try {
			await actions.remove(removing.name);
		} catch (reason) {
			setError(reason instanceof Error ? reason.message : String(reason));
		} finally {
			setRemoving(null);
		}
	};
	const rows = servers.data?.servers ?? [];
	const checking = refreshing || Boolean(servers.data?.checking);
	return <SettingsSection
		title={t("settings.mcp.title")}
		titleHidden={titleHidden}
		sectionId="mcpServers"
		grouped
		action={<div className="flex items-center gap-1.5">
			<Button type="button" size="sm" variant="ghost" disabled={checking} onClick={() => void refresh()}>
				{checking ? <LoaderCircle aria-hidden="true" className="mr-1 size-3.5 animate-spin" /> : <RefreshCw aria-hidden="true" className="mr-1 size-3.5" />}
				{checking ? t("settings.mcp.checking") : t("settings.mcp.refresh")}
			</Button>
			<Button type="button" size="sm" variant="outline" onClick={() => setAdding(true)}>
				<Plus aria-hidden="true" className="mr-1 size-3.5" />{t("settings.mcp.add.action")}
			</Button>
		</div>}
	>
		{error ? <div className="settings-content-row"><p className="text-xs text-destructive" role="alert">{error}</p></div> : null}
		{servers.error ? <div className="settings-content-row"><p className="text-xs text-destructive" role="alert">{servers.error.message}</p></div> : null}
		{!servers.isLoading && !servers.error && rows.length === 0 ? <div className="settings-content-row" data-testid="mcp-empty">
			<p className="text-sm text-settings-label">{t("settings.mcp.empty.title")}</p>
			<p className="mt-1 text-pretty text-xs leading-4 text-settings-muted">{t("settings.mcp.empty.body")}</p>
		</div> : null}
		{rows.map((server) => <ServerRow
			key={`${server.scope}:${server.projectId ?? ""}:${server.name}`}
			server={server}
			labels={labels}
			onEditKey={() => setEditing(server)}
			onRemove={() => setRemoving(server)}
		/>)}
		<AddServerDialog open={adding} onOpenChange={setAdding} />
		{editing ? <EditKeyDialog server={editing} onClose={() => setEditing(null)} /> : null}
		{removing ? <ConfirmDialog open
			title={t("settings.mcp.remove.title", { name: removing.name })}
			description={t("settings.mcp.remove.description")}
			confirmLabel={t("settings.mcp.remove.action")}
			onConfirm={() => void remove()}
			onOpenChange={(open) => { if (!open) setRemoving(null); }}
		/> : null}
	</SettingsSection>;
}
