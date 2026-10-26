/**
 * The agents board inside the chat: a tab beside the chat tab that covers the
 * timeline with the main agent and its subagents while the composer stays where
 * it is.
 */

import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ChatWorkspace } from "./ChatWorkspace";
import { chatFixture } from "../../lib/chat-fixture";
import { lexicalEditorText } from "../../test/lexical";
import { renderChat as render } from "../../test/render-chat";
import type { ConversationActivity, ConversationMessage, ConversationSnapshot } from "../../types/conversation";
import type { WorkspaceSession } from "../../types/workspace";

const session = {
	id: chatFixture.sessionId,
	workspaceId: "project-1",
	workspaceName: "agent-orchestrator",
	title: "Harden auth",
	provider: "claude-code",
	kind: "worker",
	mode: "chat",
	status: "working",
	updatedAt: "2026-08-15T00:00:00Z",
	activity: { state: "active", lastActivityAt: "2026-08-15T00:00:00Z" },
	prs: [],
} satisfies WorkspaceSession;

function withSubagent({ keepPending = false }: { keepPending?: boolean } = {}): ConversationSnapshot {
	const running = chatFixture.turns.find((turn) => turn.state === "running")!;
	const at = new Date().toISOString();
	const reply: ConversationMessage = {
		kind: "message",
		id: "main-status",
		turnId: running.id,
		sequence: chatFixture.latestSequence + 3,
		revision: 1,
		role: "assistant",
		origin: "provider",
		text: "Status: Handing the auth audit to a subagent\nDecision: Audit with a subagent, since the main turn keeps the migration going",
		streaming: true,
		createdAt: at,
	};
	const agent: ConversationActivity = {
		kind: "activity",
		id: "agent-row",
		turnId: running.id,
		sequence: chatFixture.latestSequence + 1,
		revision: 1,
		activityKind: "mcp_tool",
		status: "running",
		summary: "Audit the auth flow",
		providerItemId: "agent-1",
		createdAt: at,
		detail: {
			nestedAgent: true,
			providerToolName: "Agent",
			input: { description: "Audit the auth flow", prompt: "Read the auth module.", subagent_type: "Explore" },
		},
	};
	const voice: ConversationActivity = {
		kind: "activity",
		id: "agent-voice",
		turnId: running.id,
		sequence: chatFixture.latestSequence + 2,
		revision: 1,
		activityKind: "mcp_tool",
		status: "running",
		summary: "Subagent response",
		providerItemId: "voice-1",
		createdAt: at,
		detail: { parentProviderItemId: "agent-1", nestedAgent: true, progress: "Status: Reading the login handler" },
	};
	// The fixture's pending approval would take the composer's place.
	const items = keepPending ? chatFixture.items : chatFixture.items.filter((item) => item.kind !== "activity" || item.status !== "pending");
	return { ...chatFixture, harness: "claude-code", items: [...items, agent, voice, reply], latestSequence: chatFixture.latestSequence + 3 };
}

describe("ChatWorkspace agents board", () => {
	it("offers an Agents tab with the running count only where subagents can run", () => {
		const view = render(<ChatWorkspace snapshot={withSubagent()} session={session} />);
		const tab = screen.getByRole("tab", { name: /Agents/ });
		expect(tab).toHaveAttribute("aria-selected", "false");
		expect(within(tab).getByText("1 running")).toBeInTheDocument();
		expect(within(tab).getByText("1")).toHaveClass("text-passive");

		view.rerender(<ChatWorkspace snapshot={chatFixture} session={{ ...session, provider: "codex" }} />);
		expect(screen.queryByRole("tab", { name: /Agents/ })).not.toBeInTheDocument();
	});

	it("tints the tab's count when the main agent needs you", () => {
		render(<ChatWorkspace snapshot={withSubagent({ keepPending: true })} session={session} />);
		expect(within(screen.getByRole("tab", { name: /Agents/ })).getByText("1")).toHaveClass("text-status-needs-you");
	});

	it("covers the timeline with the board, keeps the composer, and goes back from the chat tab", async () => {
		const user = userEvent.setup();
		const onSelectChat = vi.fn();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onSelectChat={onSelectChat} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));

		expect(screen.getByRole("tab", { name: /Agents/ })).toHaveAttribute("aria-selected", "true");
		const board = screen.getByTestId("chat-subagents-panel");
		expect(within(board).getByText("Audit the auth flow")).toBeInTheDocument();
		expect(within(board).getByTestId("subagent-now")).toHaveTextContent("Reading the login handler");
		expect(screen.getByRole("log", { hidden: true }).closest("[aria-hidden='true']")).not.toBeNull();
		expect(screen.getByLabelText("Message the agent")).toBeInTheDocument();
		expect(onSelectChat).toHaveBeenCalled();

		await user.click(screen.getByRole("tab", { name: /Harden auth/ }));
		expect(screen.queryByTestId("chat-subagents-panel")).not.toBeInTheDocument();
		expect(screen.getByRole("tab", { name: /Agents/ })).toHaveAttribute("aria-selected", "false");
	});

	it("shows the main agent's status and decision lines from the running turn", async () => {
		const user = userEvent.setup();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const main = within(screen.getByTestId("chat-subagents-panel")).getByRole("article", { name: "Main agent" });
		expect(main).toHaveAttribute("data-state", "working");
		expect(within(main).getByTestId("main-agent-now")).toHaveTextContent("Handing the auth audit to a subagent");
		expect(within(main).getByTestId("main-agent-decision")).toHaveTextContent(
			"Audit with a subagent, since the main turn keeps the migration going",
		);
	});

	it("starts a draft about the subagent and stops the main turn from a card", async () => {
		const user = userEvent.setup();
		const onInterrupt = vi.fn();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onInterrupt={onInterrupt} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const board = screen.getByTestId("chat-subagents-panel");
		const subagent = within(board).getByRole("article", { name: "Audit the auth flow" });

		await user.click(within(subagent).getByRole("button", { name: "Ask main agent" }));
		const field = screen.getByLabelText("Message the agent");
		await waitFor(() => expect(lexicalEditorText(field)).toBe('About the subagent "Audit the auth flow": '));

		await user.click(within(subagent).getByRole("button", { name: "Stop turn" }));
		expect(onInterrupt).toHaveBeenCalledTimes(1);
		await user.click(within(within(board).getByRole("article", { name: "Main agent" })).getByRole("button", { name: "Stop turn" }));
		expect(onInterrupt).toHaveBeenCalledTimes(2);
	});
});
