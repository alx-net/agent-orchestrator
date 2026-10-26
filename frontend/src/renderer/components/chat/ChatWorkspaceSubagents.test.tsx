/**
 * The subagent board inside the chat: a tab beside the chat tab that covers the
 * timeline with the board while the composer stays where it is.
 */

import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ChatWorkspace } from "./ChatWorkspace";
import { chatFixture } from "../../lib/chat-fixture";
import { lexicalEditorText } from "../../test/lexical";
import { renderChat as render } from "../../test/render-chat";
import type { ConversationActivity, ConversationSnapshot } from "../../types/conversation";
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

function withSubagent(): ConversationSnapshot {
	const running = chatFixture.turns.find((turn) => turn.state === "running")!;
	const at = new Date().toISOString();
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
	const items = chatFixture.items.filter((item) => item.kind !== "activity" || item.status !== "pending");
	return { ...chatFixture, harness: "claude-code", items: [...items, agent, voice], latestSequence: chatFixture.latestSequence + 2 };
}

describe("ChatWorkspace subagent board", () => {
	it("offers a Subagents tab with the running count only where subagents can run", () => {
		const view = render(<ChatWorkspace snapshot={withSubagent()} session={session} />);
		const tab = screen.getByRole("tab", { name: /Subagents/ });
		expect(tab).toHaveAttribute("aria-selected", "false");
		expect(within(tab).getByText("1 running")).toBeInTheDocument();

		view.rerender(<ChatWorkspace snapshot={chatFixture} session={{ ...session, provider: "codex" }} />);
		expect(screen.queryByRole("tab", { name: /Subagents/ })).not.toBeInTheDocument();
	});

	it("covers the timeline with the board, keeps the composer, and goes back from the chat tab", async () => {
		const user = userEvent.setup();
		const onSelectChat = vi.fn();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onSelectChat={onSelectChat} />);
		await user.click(screen.getByRole("tab", { name: /Subagents/ }));

		expect(screen.getByRole("tab", { name: /Subagents/ })).toHaveAttribute("aria-selected", "true");
		const board = screen.getByTestId("chat-subagents-panel");
		expect(within(board).getByText("Audit the auth flow")).toBeInTheDocument();
		expect(within(board).getByTestId("subagent-now")).toHaveTextContent("Reading the login handler");
		expect(screen.getByRole("log", { hidden: true }).closest("[aria-hidden='true']")).not.toBeNull();
		expect(screen.getByLabelText("Message the agent")).toBeInTheDocument();
		expect(onSelectChat).toHaveBeenCalled();

		await user.click(screen.getByRole("tab", { name: /Harden auth/ }));
		expect(screen.queryByTestId("chat-subagents-panel")).not.toBeInTheDocument();
		expect(screen.getByRole("tab", { name: /Subagents/ })).toHaveAttribute("aria-selected", "false");
	});

	it("starts a draft about the subagent and stops the main turn from a card", async () => {
		const user = userEvent.setup();
		const onInterrupt = vi.fn();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onInterrupt={onInterrupt} />);
		await user.click(screen.getByRole("tab", { name: /Subagents/ }));
		const board = screen.getByTestId("chat-subagents-panel");

		await user.click(within(board).getByRole("button", { name: "Ask main agent" }));
		const field = screen.getByLabelText("Message the agent");
		await waitFor(() => expect(lexicalEditorText(field)).toBe('About the subagent "Audit the auth flow": '));

		await user.click(within(board).getByRole("button", { name: "Stop turn" }));
		expect(onInterrupt).toHaveBeenCalledTimes(1);
	});
});
