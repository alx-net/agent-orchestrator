import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { resetMcpConnectsForTest } from "../../hooks/useMcpServers";
import { TooltipProvider } from "../ui/tooltip";
import { McpServersSection } from "./McpServersSection";

const { getMock, postMock, patchMock, deleteMock, openExternal } = vi.hoisted(() => ({
	getMock: vi.fn(), postMock: vi.fn(), patchMock: vi.fn(), deleteMock: vi.fn(), openExternal: vi.fn(),
}));

vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock, PATCH: patchMock, DELETE: deleteMock },
	apiErrorMessage: (error: unknown) => (error as { message?: string })?.message ?? "request failed",
	apiErrorCode: () => undefined,
}));

vi.mock("../../lib/bridge", () => ({ aoBridge: { app: { openExternal } } }));

const accounts = [{ id: "device-claude", device: true }, { id: "acct-1", email: "a@example.test" }];
const checkedAt = new Date(Date.now() - 120_000).toISOString();

const oauth = {
	name: "linear", transport: "http", target: "https://mcp.linear.app/mcp", scope: "global", auth: "oauth",
	accounts: [
		{ accountId: "device-claude", status: "needs_sign_in", action: "connect", rawStatus: "needs-auth", checkedAt },
		{ accountId: "acct-1", status: "connected", toolCount: 30, checkedAt },
	],
};
const keyed = {
	name: "acme", transport: "http", target: "https://acme.test/mcp", scope: "global", auth: "api_key", editable: true,
	headerNames: ["X-Api-Key"],
	accounts: [{ accountId: "device-claude", status: "connected", toolCount: 2, action: "edit_key", checkedAt }],
};
const helper = {
	name: "sentry", transport: "http", target: "https://sentry.test/mcp", scope: "global", auth: "helper",
	accounts: [{ accountId: "device-claude", status: "key_rejected", checkedAt }],
};
const local = {
	name: "spec", transport: "stdio", target: "npx spec", scope: "global", auth: "none",
	accounts: [{ accountId: "device-claude", status: "failed", error: "spawn npx ENOENT", checkedAt }],
};
const repo = {
	name: "posthog", transport: "http", target: "https://p.test", scope: "repo", projectId: "app", projectName: "App", auth: "oauth",
	accounts: [{ accountId: "device-claude", status: "needs_approval", action: "approve", checkedAt }],
};
const connector = {
	name: "claude.ai Gmail", target: "https://gmail.test", scope: "connector", auth: "connector", manageUrl: "https://claude.ai/settings/connectors",
	accounts: [{ accountId: "device-claude", status: "needs_sign_in", action: "connect", checkedAt }],
};
const future = {
	name: "odd", transport: "carrier-pigeon", scope: "other", auth: "unknown",
	accounts: [{ accountId: "device-claude", status: "unknown", rawStatus: "hibernating", checkedAt }],
};

let list: unknown;

function respond(path: string) {
	if (path === "/api/v1/mcp/servers") return { data: list };
	if (path === "/api/v1/agents/accounts") return { data: { accounts: [] } };
	if (path.startsWith("/api/v1/agents/accounts")) return { data: { rows: [] } };
	return { data: {} };
}

function renderSection() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(<QueryClientProvider client={queryClient}><TooltipProvider><McpServersSection /></TooltipProvider></QueryClientProvider>);
}

const row = (name: string) => waitFor(() => {
	const element = document.querySelector<HTMLElement>(`[data-mcp-server="${name}"]`);
	if (!element) throw new Error(`no row for ${name}`);
	return element;
});

const accountRow = (server: HTMLElement, id: string) => {
	const element = server.querySelector<HTMLElement>(`[data-mcp-account="${id}"]`);
	if (!element) throw new Error(`no account ${id}`);
	return element;
};

beforeEach(() => {
	vi.clearAllMocks();
	resetMcpConnectsForTest();
	list = { servers: [oauth, keyed, helper, local, repo, connector, future], accounts, checking: false };
	getMock.mockImplementation(async (path: string) => respond(path));
});

it("shows each server's plain status, tools, how it signs in, and when it was checked", async () => {
	renderSection();
	const linear = await row("linear");
	expect(within(accountRow(linear, "device-claude")).getByText("Needs sign-in")).toBeTruthy();
	const connected = within(accountRow(linear, "acct-1"));
	expect(connected.getByText("Connected")).toBeTruthy();
	expect(connected.getByText(/30 tools/)).toBeTruthy();
	expect(connected.getByText(/Checked 2 minutes ago/)).toBeTruthy();
	expect(within(linear).getByText(/Browser sign-in/)).toBeTruthy();
	expect(within(linear).getByText(/Global/)).toBeTruthy();
});

it("offers what each kind of server needs and nothing else", async () => {
	renderSection();
	expect(within(accountRow(await row("linear"), "device-claude")).getByRole("button", { name: "Connect" })).toBeTruthy();
	expect(within(accountRow(await row("linear"), "acct-1")).queryByRole("button", { name: "Connect" })).toBeNull();

	const key = within(await row("acme"));
	expect(key.getByRole("button", { name: "Change key" })).toBeTruthy();
	expect(key.queryByRole("button", { name: "Connect" })).toBeNull();

	// A helper server: no sign-in and no key to edit, and it says what to do.
	const helperRow = within(await row("sentry"));
	expect(helperRow.getByText("Key rejected")).toBeTruthy();
	expect(helperRow.getByText(/Fix the helper/)).toBeTruthy();
	expect(helperRow.queryByRole("button", { name: "Connect" })).toBeNull();
	expect(helperRow.queryByRole("button", { name: "Change key" })).toBeNull();

	// A local process: whether it starts, and the real error.
	const localRow = within(await row("spec"));
	expect(localRow.getByText("Failed")).toBeTruthy();
	expect(localRow.getByText("spawn npx ENOENT")).toBeTruthy();
	expect(localRow.queryByRole("button", { name: "Connect" })).toBeNull();

	// A repo server waiting on approval offers that first.
	const repoRow = within(await row("posthog"));
	expect(repoRow.getByRole("button", { name: "Approve" })).toBeTruthy();
	expect(repoRow.queryByRole("button", { name: "Test" })).toBeNull();
	expect(repoRow.getByText(/Repo App/)).toBeTruthy();
	expect(repoRow.queryByRole("button", { name: /Remove/ })).toBeNull();

	// A kind of server nobody has heard of is still listed, with its raw status.
	expect(within(await row("odd")).getByText("hibernating")).toBeTruthy();
});

it("connects one account, follows the sign-in, and refreshes the list when it lands", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "device-claude", state: "waiting", applied: {}, authUrl: "https://auth.test/x" } } });
	getMock.mockImplementation(async (path: string) => {
		if (path === "/api/v1/mcp/connect/{operationId}") return { data: { operation: { id: "op1", name: "linear", accountId: "device-claude", state: "connected", applied: { inPlace: ["s1"] } } } };
		return respond(path);
	});
	renderSection();
	const device = accountRow(await row("linear"), "device-claude");
	await user.click(within(device).getByRole("button", { name: "Connect" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/connect", { body: { name: "linear", accountId: "device-claude" } });

	// The browser opened by itself; the fallback link is there if it did not.
	expect(await within(device).findByText(/Approve it in the browser that just opened/)).toBeTruthy();
	await user.click(within(device).getByRole("button", { name: "Open it again" }));
	expect(openExternal).toHaveBeenCalledWith("https://auth.test/x");

	await within(device).findByText("Connected. Running agents have it now.", {}, { timeout: 4000 });
	await waitFor(() => expect(getMock.mock.calls.filter(([path]) => path === "/api/v1/mcp/servers").length).toBeGreaterThan(1));
});

it("shows Claude Code's own words when a sign-in fails", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "device-claude", state: "failed", error: "the server said no", applied: {} } } });
	renderSection();
	const device = accountRow(await row("linear"), "device-claude");
	await user.click(within(device).getByRole("button", { name: "Connect" }));
	expect((await within(device).findByRole("alert")).textContent).toBe("the server said no");
});

it("lets a waiting sign-in be cancelled", async () => {
	const user = userEvent.setup();
	postMock
		.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "device-claude", state: "waiting", applied: {} } } })
		.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "device-claude", state: "cancelled", applied: {} } } });
	renderSection();
	const device = accountRow(await row("linear"), "device-claude");
	await user.click(within(device).getByRole("button", { name: "Connect" }));
	await user.click(await within(device).findByRole("button", { name: "Cancel sign-in" }));
	expect(postMock).toHaveBeenLastCalledWith("/api/v1/mcp/connect/{operationId}/cancel", { params: { path: { operationId: "op1" } } });
	await within(device).findByRole("button", { name: "Connect" });
});

it("approves a repo server for that subscription and project", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { applied: {} } });
	renderSection();
	await user.click(within(await row("posthog")).getByRole("button", { name: "Approve" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/approve", { body: { name: "posthog", accountId: "device-claude", projectId: "app" } });
});

it("runs a real test and shows a clear pass or fail with the reason", async () => {
	const user = userEvent.setup();
	postMock
		.mockResolvedValueOnce({ data: { ok: true, name: "spec", accountId: "device-claude", status: "connected", reason: "Connected. It exposes 2 tools.", checkedAt } })
		.mockResolvedValueOnce({ data: { ok: false, name: "spec", accountId: "device-claude", status: "failed", reason: "It did not connect: spawn npx ENOENT", checkedAt } });
	renderSection();
	const spec = within(await row("spec"));
	await user.click(spec.getByRole("button", { name: "Test" }));
	expect((await spec.findByTestId("mcp-test-result")).textContent).toBe("Passed: Connected. It exposes 2 tools.");
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/test", { body: { name: "spec", accountId: "device-claude" } });
	await user.click(spec.getByRole("button", { name: "Test" }));
	await waitFor(() => expect(spec.getByTestId("mcp-test-result").textContent).toBe("Failed: It did not connect: spawn npx ENOENT"));
});

it("links a connector to claude.ai rather than a dead button", async () => {
	const user = userEvent.setup();
	renderSection();
	const gmail = within(await row("claude.ai Gmail"));
	await user.click(gmail.getByRole("button", { name: /Manage on claude.ai/ }));
	expect(openExternal).toHaveBeenCalledWith("https://claude.ai/settings/connectors");
	expect(gmail.getByRole("button", { name: "Connect" })).toBeTruthy();
});

it("says how to start when there are no servers", async () => {
	list = { servers: [], accounts, checking: false };
	renderSection();
	const empty = await screen.findByTestId("mcp-empty");
	expect(empty.textContent).toContain("https://mcp.linear.app/mcp");
	expect(empty.textContent).toContain("ao mcp login");
});

it("adds a URL server, masks its key, and explains itself with an example", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { applied: {} } });
	renderSection();
	await user.click(await screen.findByRole("button", { name: "Add server" }));
	expect(screen.getByText(/for example linear and https:\/\/mcp\.linear\.app\/mcp/)).toBeTruthy();
	await user.type(screen.getByLabelText("Name"), "acme2");
	await user.type(screen.getByLabelText("URL or command"), "https://acme.test/mcp");
	await user.click(screen.getByRole("button", { name: "Add header" }));
	await user.type(screen.getByRole("textbox", { name: "Header" }), "X-Api-Key");
	const value = screen.getByLabelText("Value");
	expect(value.getAttribute("type")).toBe("password");
	await user.type(value, "s3cret");
	const dialog = screen.getByRole("dialog");
	await user.click(within(dialog).getByRole("button", { name: "Add server" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/servers", { body: { name: "acme2", url: "https://acme.test/mcp", headers: { "X-Api-Key": "s3cret" } } });
	await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
});

it("adds a command server and shows the daemon's refusal", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ error: { message: "an MCP server with that name already exists: spec" } });
	renderSection();
	await user.click(await screen.findByRole("button", { name: "Add server" }));
	await user.type(screen.getByLabelText("Name"), "spec");
	await user.type(screen.getByLabelText("URL or command"), "npx -y @acme/spec");
	const dialog = screen.getByRole("dialog");
	await user.click(within(dialog).getByRole("button", { name: "Add server" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/servers", { body: { name: "spec", command: "npx", args: ["-y", "@acme/spec"] } });
	expect((await within(dialog).findByRole("alert")).textContent).toContain("already exists");
});

it("changes a key without ever seeing the old one", async () => {
	const user = userEvent.setup();
	patchMock.mockResolvedValueOnce({ data: { applied: {} } });
	renderSection();
	await user.click(within(await row("acme")).getByRole("button", { name: "Change key" }));
	const value = screen.getByLabelText("New value") as HTMLInputElement;
	expect(value.value).toBe("");
	expect(value.type).toBe("password");
	await user.type(value, "new-key");
	await user.click(screen.getByRole("button", { name: "Save" }));
	expect(patchMock).toHaveBeenCalledWith("/api/v1/mcp/servers/{serverName}", {
		params: { path: { serverName: "acme" } }, body: { headers: { "X-Api-Key": "new-key" } },
	});
});

it("removes a global server after asking", async () => {
	const user = userEvent.setup();
	deleteMock.mockResolvedValueOnce({ data: { applied: {} } });
	renderSection();
	await user.click(within(await row("linear")).getByRole("button", { name: "Remove linear" }));
	await user.click(await screen.findByRole("button", { name: "Remove" }));
	expect(deleteMock).toHaveBeenCalledWith("/api/v1/mcp/servers/{serverName}", { params: { path: { serverName: "linear" } } });
});

it("checks all on demand, and polls while a check runs", async () => {
	const user = userEvent.setup();
	list = { servers: [oauth], accounts, checking: true };
	getMock.mockImplementation(async (path: string) => {
		const answer = respond(path);
		if (path === "/api/v1/mcp/servers") list = { servers: [oauth], accounts, checking: false };
		return answer;
	});
	renderSection();
	expect(await screen.findByRole("button", { name: "Checking" })).toBeTruthy();
	await user.click(await screen.findByRole("button", { name: "Check all" }, { timeout: 4000 }));
	expect(getMock).toHaveBeenCalledWith("/api/v1/mcp/servers", { params: { query: { refresh: true } } });
});
