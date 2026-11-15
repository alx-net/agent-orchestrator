import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, expect, it, vi } from "vitest";
import { resetMcpConnectsForTest } from "../../hooks/useMcpServers";
import { signInMcpServers, type ConversationSnapshot } from "../../types/conversation";
import { TooltipProvider } from "../ui/tooltip";
import { McpConnectBanner } from "./ChatStatusBanners";

const { getMock, postMock } = vi.hoisted(() => ({ getMock: vi.fn(), postMock: vi.fn() }));

vi.mock("../../lib/api-client", () => ({
	apiClient: { GET: getMock, POST: postMock },
	apiErrorMessage: (error: unknown) => (error as { message?: string })?.message ?? "request failed",
	apiErrorCode: () => undefined,
}));

const connectable = (name: string) => ({ name, scope: "global", auth: "oauth", accounts: [{ accountId: "a", status: "needs_sign_in", action: "connect", live: true }] });
const keyRefused = (name: string) => ({ name, scope: "global", auth: "helper", accounts: [{ accountId: "a", status: "key_rejected", live: true }] });

let servers: unknown[];

function renderBanner(names: string[], sessionId = "s1") {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const view = (list: string[]) => <QueryClientProvider client={queryClient}><TooltipProvider>
		<McpConnectBanner sessionId={sessionId} servers={list.map((name) => ({ name, status: "needs_auth" }))} />
	</TooltipProvider></QueryClientProvider>;
	const result = render(view(names));
	return { ...result, update: (list: string[]) => result.rerender(view(list)) };
}

beforeEach(() => {
	vi.clearAllMocks();
	window.localStorage.clear();
	resetMcpConnectsForTest();
	servers = [connectable("linear")];
	getMock.mockImplementation(async (path: string) => {
		if (path === "/api/v1/sessions/{sessionId}/mcp-servers") return { data: { live: true, accountId: "a", servers } };
		return { data: {} };
	});
});

it("names the server and connects it for this session with one click", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "a", state: "waiting", applied: {} } } });
	renderBanner(["linear"]);
	expect(await screen.findByText("Linear isn't connected")).toBeTruthy();
	await user.click(screen.getByRole("button", { name: "Connect linear" }));
	expect(postMock).toHaveBeenCalledWith("/api/v1/mcp/connect", { body: { name: "linear", sessionId: "s1" } });
	expect(await screen.findByRole("button", { name: "Approve in browser" })).toBeTruthy();
});

it("clears by itself once the agent has the server", async () => {
	const { update } = renderBanner(["linear"]);
	await screen.findByText("Linear isn't connected");
	update([]);
	await waitFor(() => expect(screen.queryByTestId("mcp-connect-banner")).toBeNull());
});

it("offers one button per server when several need it", async () => {
	servers = [connectable("linear"), connectable("posthog")];
	renderBanner(["linear", "posthog"]);
	expect(await screen.findByText("2 MCP servers aren't connected")).toBeTruthy();
	expect(screen.getByRole("button", { name: "Connect linear" })).toBeTruthy();
	expect(screen.getByRole("button", { name: "Connect posthog" })).toBeTruthy();
});

it("never offers a sign-in for a server that uses a key", async () => {
	servers = [keyRefused("sentry")];
	renderBanner(["sentry"]);
	await waitFor(() => expect(getMock).toHaveBeenCalled());
	await waitFor(() => expect(screen.queryByRole("button", { name: /Connect/ })).toBeNull());
	expect(screen.queryByTestId("mcp-connect-banner")).toBeNull();
});

it("shows Claude Code's reason when the sign-in fails", async () => {
	const user = userEvent.setup();
	postMock.mockResolvedValueOnce({ data: { operation: { id: "op1", name: "linear", accountId: "a", state: "failed", error: "the server said no", applied: {} } } });
	renderBanner(["linear"]);
	await user.click(await screen.findByRole("button", { name: "Connect linear" }));
	expect((await screen.findByRole("alert")).textContent).toBe("the server said no");
});

it("stays dismissed for this set of servers, and speaks up for a new one", async () => {
	const user = userEvent.setup();
	const { unmount } = renderBanner(["linear"]);
	await user.click(await screen.findByRole("button", { name: "Hide this notice" }));
	expect(screen.queryByTestId("mcp-connect-banner")).toBeNull();
	unmount();

	renderBanner(["linear"]);
	await waitFor(() => expect(getMock).toHaveBeenCalled());
	expect(screen.queryByTestId("mcp-connect-banner")).toBeNull();

	servers = [connectable("linear"), connectable("posthog")];
	renderBanner(["linear", "posthog"], "s1");
	expect(await screen.findByText("2 MCP servers aren't connected")).toBeTruthy();
});

it("reads needs-auth from the snapshot, leaving claude.ai connectors out", () => {
	const snapshot = { mcpServers: [
		{ name: "linear", status: "needs_auth", scope: "user" },
		{ name: "claude.ai Gmail", status: "needs_auth", scope: "claudeai" },
		{ name: "sentry", status: "ready" },
		{ name: "spec", status: "failed" },
	] } as unknown as ConversationSnapshot;
	expect(signInMcpServers(snapshot).map((server) => server.name)).toEqual(["linear"]);
});
