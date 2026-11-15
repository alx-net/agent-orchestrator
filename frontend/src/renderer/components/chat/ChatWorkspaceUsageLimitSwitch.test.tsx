import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ChatWorkspace } from "./ChatWorkspace";
import { agentAccountsQueryKey } from "../../hooks/useAgentAccounts";
import { chatFixture } from "../../lib/chat-fixture";
import type { ConversationSnapshot, UsageLimitSwitchPhase } from "../../types/conversation";
import { TooltipProvider } from "../ui/tooltip";

/** turn-2 hit the computer login's usage limit; AO is moving it to acct-b. */
function limitedSnapshot(phase: UsageLimitSwitchPhase, extra: Record<string, unknown> = {}): ConversationSnapshot {
	const resetsAt = new Date(Date.now() + 41 * 60_000).toISOString();
	const turns = chatFixture.turns.map((turn) =>
		turn.id === "turn-2"
			? {
				...turn, state: "failed" as const, completedAt: turn.requestedAt, hasHumanPrompt: true,
				errorMessage: "You've hit your session limit · resets 9:40pm (Europe/Berlin)",
				usageLimit: { resetsAt, autoResume: false },
			}
			: turn.state === "running"
				? { ...turn, state: "completed" as const, completedAt: turn.requestedAt }
				: turn,
	);
	return {
		...chatFixture,
		controller: { state: "ready" },
		turns,
		items: [
			...chatFixture.items,
			{
				kind: "activity",
				id: "limit-switch-1",
				turnId: "turn-2",
				sequence: chatFixture.latestSequence + 1,
				revision: 0,
				activityKind: "system",
				status: phase === "switching" ? "running" : phase === "failed" ? "failed" : "completed",
				summary: "Switching account",
				detail: { event: "account.limit_switch", phase, accountId: "acct-b", fromAccountId: "device-claude", ...extra },
				createdAt: chatFixture.items[0]!.createdAt,
			},
		],
		latestSequence: chatFixture.latestSequence + 1,
	};
}

function renderLimited(snapshot: ConversationSnapshot) {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
	queryClient.setQueryData(agentAccountsQueryKey, [
		{ id: "device-claude", provider: "claude", device: true, default: true, signedIn: true, email: "alex.netz1@web.de" },
		{ id: "acct-b", provider: "claude", device: false, default: false, signedIn: true, email: "alex.seeverlp@gmail.com" },
	]);
	return render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<ChatWorkspace snapshot={snapshot} onSend={vi.fn()} retryControl={{ retry: vi.fn(), setAutoResume: vi.fn() }} />
			</TooltipProvider>
		</QueryClientProvider>,
	);
}

describe("ChatWorkspace usage-limit account switch", () => {
	it("says it is switching instead of offering to wait for the reset", () => {
		renderLimited(limitedSnapshot("switching"));

		const notice = screen.getByTestId("usage-limit-notice");
		expect(within(notice).getByTestId("usage-limit-account-switch")).toHaveTextContent(
			"Switching to alex.seeverlp@gmail.com… alex.netz1@web.de reached its usage limit.",
		);
		expect(within(notice).queryByRole("button", { name: "Auto-resume at reset" })).not.toBeInTheDocument();
		expect(within(notice).queryByRole("button", { name: "Resume" })).not.toBeInTheDocument();
		expect(within(notice).queryByText(/Available in/)).not.toBeInTheDocument();
		// The timeline shows the move in place of the usage-limit outcome.
		expect(screen.getAllByTestId("usage-limit-notice")).toHaveLength(1);
	});

	it("marks where the turn continued and the MCP servers missing there", () => {
		renderLimited(limitedSnapshot("continued", { mcpNeedsAuth: ["linear", "posthog"] }));

		// The timeline row; the composer notice says the same until the retry shows up.
		expect(screen.getAllByText("Continued on alex.seeverlp@gmail.com").length).toBeGreaterThan(0);
		expect(screen.getAllByText(/reached its usage limit, so AO moved this chat and ran the turn again\. Not signed in on this account: linear, posthog\./).length).toBeGreaterThan(0);
		expect(screen.queryByRole("button", { name: "Auto-resume at reset" })).not.toBeInTheDocument();
	});

	it("keeps today's notice, with the reason, when the move failed", () => {
		renderLimited(limitedSnapshot("failed", { error: "The account is signed out" }));

		const notice = screen.getAllByTestId("usage-limit-notice").at(-1)!;
		expect(within(notice).getByRole("alert")).toHaveTextContent("Couldn't switch to alex.seeverlp@gmail.com. The account is signed out The chat waits for the usage limit to reset.");
		expect(within(notice).getByRole("button", { name: "Auto-resume at reset" })).toBeInTheDocument();
	});
});
