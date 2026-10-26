/**
 * The agents board inside the chat: a tab beside the chat tab that covers the
 * timeline with the main agent and its subagents while the composer stays where
 * it is.
 */

import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("../../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../../lib/api-client")>(),
	apiClient: { POST: postMock },
}));

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
	return {
		...chatFixture,
		harness: "claude-code",
		// The patched Claude bridge advertises per-subagent messaging and stop.
		capabilities: [...(chatFixture.capabilities ?? []), "subagents"],
		items: [...items, agent, voice, reply],
		latestSequence: chatFixture.latestSequence + 3,
	};
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

	it("starts a draft about the subagent, stops only it from its card, and stops the turn from the main card", async () => {
		const user = userEvent.setup();
		const onInterrupt = vi.fn();
		postMock.mockResolvedValueOnce({ data: { ok: true }, error: undefined, response: { status: 202 } });
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onInterrupt={onInterrupt} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const board = screen.getByTestId("chat-subagents-panel");
		const subagent = within(board).getByRole("article", { name: "Audit the auth flow" });

		await user.click(within(subagent).getByRole("button", { name: "Ask main agent" }));
		const field = screen.getByLabelText("Message the agent");
		await waitFor(() => expect(lexicalEditorText(field)).toBe('About the subagent "Audit the auth flow": '));

		// A subagent's own stop leaves the main turn running.
		expect(within(subagent).queryByRole("button", { name: "Stop turn" })).not.toBeInTheDocument();
		await user.click(within(subagent).getByRole("button", { name: "Stop subagent" }));
		expect(postMock).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop", {
			params: { path: { sessionId: session.id, toolUseId: "agent-1" } },
		});
		expect(onInterrupt).not.toHaveBeenCalled();
		await user.click(within(within(board).getByRole("article", { name: "Main agent" })).getByRole("button", { name: "Stop turn" }));
		expect(onInterrupt).toHaveBeenCalledTimes(1);
	});

	it("keeps the turn-wide stop on subagent cards when the bridge cannot reach one subagent", async () => {
		const user = userEvent.setup();
		const snapshot = withSubagent();
		render(<ChatWorkspace snapshot={{ ...snapshot, capabilities: snapshot.capabilities?.filter((c) => c !== "subagents") }} session={session} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const subagent = within(screen.getByTestId("chat-subagents-panel")).getByRole("article", { name: "Audit the auth flow" });
		expect(within(subagent).queryByRole("button", { name: "Stop subagent" })).not.toBeInTheDocument();
		expect(within(subagent).queryByRole("button", { name: "Message subagent" })).not.toBeInTheDocument();
	});

	it("steers the running main turn from the main card instead of queueing behind it", async () => {
		const user = userEvent.setup();
		const onSteer = vi.fn(async () => ({ status: "accepted" as const }));
		const onSend = vi.fn();
		render(<ChatWorkspace snapshot={withSubagent()} session={session} onSend={onSend} onSteer={onSteer} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const main = within(screen.getByTestId("chat-subagents-panel")).getByRole("article", { name: "Main agent" });

		await user.click(within(main).getByRole("button", { name: "Steer main agent" }));
		await user.type(within(main).getByRole("textbox", { name: "Steer main agent" }), "Keep the v1 route{Enter}");
		expect(onSteer).toHaveBeenCalledWith("Keep the v1 route", undefined, expect.any(String), undefined);
		expect(onSend).not.toHaveBeenCalled();
		// Beside the fixture's own earlier steers into this turn.
		await waitFor(() => expect(
			within(main).getAllByTestId("sent-message").find((row) => row.textContent?.includes("Keep the v1 route")),
		).toHaveTextContent("Keep the v1 route· delivered"));
	});

	it("offers no subagent stop or message for a cloud session, which keeps stopping the turn", async () => {
		const user = userEvent.setup();
		const cloud: WorkspaceSession = { ...session, cloud: { orgId: "org-1" } };
		render(<ChatWorkspace snapshot={withSubagent()} session={cloud} onInterrupt={vi.fn()} />);
		await user.click(screen.getByRole("tab", { name: /Agents/ }));
		const subagent = within(screen.getByTestId("chat-subagents-panel")).getByRole("article", { name: "Audit the auth flow" });
		expect(within(subagent).queryByRole("button", { name: "Stop subagent" })).not.toBeInTheDocument();
		expect(within(subagent).queryByRole("button", { name: "Message subagent" })).not.toBeInTheDocument();
		expect(within(subagent).getByRole("button", { name: "Stop turn" })).toBeInTheDocument();
	});
});
