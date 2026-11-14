import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { AgentAccount } from "../hooks/useAgentAccounts";
import type { WorkspaceSession } from "../types/workspace";
import { SessionAccountSummary } from "./SessionAccountSummary";

const state = vi.hoisted(() => ({
	current: { claude: "seever" } as Record<string, string>,
	transition: undefined as unknown,
	switchSession: vi.fn(),
	refreshSession: vi.fn(),
}));

vi.mock("../hooks/useAgentAccounts", async (importOriginal) => {
	const actual = await importOriginal<typeof import("../hooks/useAgentAccounts")>();
	return {
		...actual,
		useSessionAgentAccounts: () => ({ data: state.current }),
		useAgentAccountActions: () => ({ switchSession: state.switchSession, refreshSession: state.refreshSession }),
	};
});

vi.mock("../hooks/useSessionInterfaceTransition", async (importOriginal) => {
	const actual = await importOriginal<typeof import("../hooks/useSessionInterfaceTransition")>();
	return { ...actual, useSessionInterfaceTransitionStatus: () => ({ transition: state.transition }) };
});

vi.mock("./settings/AgentAccountsSection", () => ({ AccountLimits: () => null }));

const accounts: AgentAccount[] = [
	{ id: "device-claude", provider: "claude", device: true, default: true, signedIn: true, email: "alex.seeverlp@gmail.com" },
	{ id: "seever", provider: "claude", device: false, default: false, signedIn: true, email: "alex.seeverlp@gmail.com", mcpNeedsAuth: ["linear", "posthog"] },
	{ id: "kugel", provider: "claude", device: false, default: false, signedIn: true, email: "alex@kugelaudio.com" },
];

const session = { id: "kugelaudio-2", provider: "claude-code", activity: { state: "idle" } } as unknown as WorkspaceSession;

function renderSummary() {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
	queryClient.setQueryData(["agent-accounts"], accounts);
	return render(<QueryClientProvider client={queryClient}><SessionAccountSummary session={session} /></QueryClientProvider>);
}

beforeEach(() => {
	state.current = { claude: "seever" };
	state.transition = undefined;
	state.switchSession.mockReset();
	state.refreshSession.mockReset();
});

describe("SessionAccountSummary", () => {
	it("tells the computer's login apart from a saved account with the same email", async () => {
		const user = userEvent.setup();
		renderSummary();

		await user.click(screen.getByRole("combobox", { name: "Account" }));
		expect(screen.getByRole("option", { name: "alex.seeverlp@gmail.com · This computer" })).toBeInTheDocument();
		expect(screen.getByRole("option", { name: "alex.seeverlp@gmail.com" })).toBeInTheDocument();
	});

	it("names the MCP servers the session's account is not signed in to", () => {
		renderSummary();

		expect(screen.getByTestId("session-account-mcp-needs-auth")).toHaveTextContent(
			"MCP servers not signed in on this account: linear, posthog",
		);
	});

	it("shows a refused move as an error instead of failing silently", async () => {
		const { AgentAccountError } = await import("../hooks/useAgentAccounts");
		state.switchSession.mockRejectedValue(new AgentAccountError("The account is signed out; sign it in again first", "AGENT_ACCOUNT_SIGNED_OUT"));
		const user = userEvent.setup();
		renderSummary();

		await user.click(screen.getByRole("combobox", { name: "Account" }));
		await user.click(screen.getByRole("option", { name: "alex@kugelaudio.com" }));

		expect(state.switchSession).toHaveBeenCalledWith("kugelaudio-2", "kugel", false);
		expect(await screen.findByRole("alert")).toHaveTextContent("The account is signed out; sign it in again first");
	});

	it("answers a pick of the account the session is already on without an error", async () => {
		const { AgentAccountError } = await import("../hooks/useAgentAccounts");
		state.switchSession.mockRejectedValue(new AgentAccountError("The session already uses this account", "AGENT_ACCOUNT_UNCHANGED"));
		const user = userEvent.setup();
		renderSummary();

		await user.click(screen.getByRole("combobox", { name: "Account" }));
		await user.click(screen.getByRole("option", { name: "alex@kugelaudio.com" }));

		await waitFor(() => expect(state.refreshSession).toHaveBeenCalledWith("kugelaudio-2"));
		expect(screen.getByRole("status")).toHaveTextContent("This session already runs on alex@kugelaudio.com.");
		expect(screen.queryByRole("alert")).not.toBeInTheDocument();
	});

	it("names a restart on the same account as a restart, not a move", () => {
		state.transition = {
			id: "t-1", sessionId: "kugelaudio-2", phase: "completed", sourceMode: "chat", targetMode: "chat",
			sourceAccountId: "seever", targetAccountId: "seever", policy: "drain",
			createdAt: "2026-10-27T12:00:00Z", updatedAt: "2026-10-27T12:00:05Z",
		};
		renderSummary();

		expect(screen.getByRole("status")).toHaveTextContent("Restarted on alex.seeverlp@gmail.com");
	});
});
