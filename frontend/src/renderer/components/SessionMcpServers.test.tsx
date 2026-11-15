import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { resetMcpConnectsForTest } from "../hooks/useMcpServers";
import type { WorkspaceSession } from "../types/workspace";
import { SessionMcpServers } from "./SessionMcpServers";
import { TooltipProvider } from "./ui/tooltip";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: (error: unknown) => (error as { message?: string })?.message ?? "request failed",
	apiErrorCode: () => undefined,
}));

vi.mock("../lib/bridge", () => ({ aoBridge: { app: { openExternal: vi.fn() } } }));

const liveServers = [
	{ name: "linear", scope: "global", auth: "oauth", accounts: [{ accountId: "a", status: "needs_sign_in", action: "connect", live: true }] },
	{ name: "sentry", scope: "global", auth: "helper", accounts: [{ accountId: "a", status: "connected", toolCount: 12, live: true }] },
	{ name: "spec", scope: "global", auth: "none", accounts: [{ accountId: "a", status: "failed", error: "spawn npx ENOENT", live: true }] },
];

const session = (over: Partial<WorkspaceSession> = {}) => ({ id: "s1", provider: "claude-code", ...over }) as WorkspaceSession;

function renderPanel(value = session()) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	return render(<QueryClientProvider client={queryClient}><TooltipProvider><SessionMcpServers session={value} /></TooltipProvider></QueryClientProvider>);
}

beforeEach(() => {
	vi.clearAllMocks();
	resetMcpConnectsForTest();
	getMock.mockResolvedValue({ data: { live: true, accountId: "a", servers: liveServers } });
});

it("shows what the running agent has now, with tools and the real error", async () => {
	renderPanel();
	expect(await screen.findByText("MCP servers this agent has right now")).toBeTruthy();
	expect(getMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/mcp-servers", { params: { path: { sessionId: "s1" }, query: {} } });
	expect(within(screen.getByTestId("session-mcp-sentry")).getByText("12 tools")).toBeTruthy();
	expect(within(screen.getByTestId("session-mcp-spec")).getByText("spawn npx ENOENT")).toBeTruthy();
});

it("connects for this session, and reconnects a server that is down", async () => {
	const user = userEvent.setup();
	postMock
		.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", state: "waiting", applied: {} } } })
		.mockResolvedValueOnce({ data: { live: true, accountId: "a", servers: liveServers } });
	renderPanel();
	await user.click(within(await screen.findByTestId("session-mcp-linear")).getByRole("button", { name: "Connect" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/connect", { body: { name: "linear", sessionId: "s1" } });
	expect(await screen.findByText(/Approve it in the browser/)).toBeTruthy();

	await user.click(within(screen.getByTestId("session-mcp-spec")).getByRole("button", { name: "Reconnect" }));
	expect(postMock).toHaveBeenLastCalledWith("/api/v1/sessions/{sessionId}/mcp-servers/{serverName}/reconnect", {
		params: { path: { sessionId: "s1", serverName: "spec" } },
	});
});

it("says so when no agent is running", async () => {
	getMock.mockResolvedValue({ data: { live: false, accountId: "a", servers: liveServers } });
	renderPanel();
	expect(await screen.findByText("MCP servers this session will have when its agent starts")).toBeTruthy();
});

it("stays out of the way for anything but a running Claude Code session", async () => {
	const { container } = renderPanel(session({ provider: "codex" }));
	await waitFor(() => expect(container.textContent).toBe(""));
	expect(getMock).not.toHaveBeenCalled();
});
