/**
 * Stepping in from the agents board, end to end inside the renderer: the board's
 * fields and stop buttons, through the controls hook, to the daemon routes.
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactElement } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/api-client")>(),
	apiClient: { POST: postMock },
}));

import { SubagentBoard } from "../components/chat/SubagentBoard";
import { TooltipProvider } from "../components/ui/tooltip";
import type { MainAgentSummary, SubagentCard } from "../lib/subagent-board";
import type { ChatSteerOutcome } from "../types/conversation";
import { conversationQueryKey } from "./useConversation";
import { useAgentBoardControls } from "./useAgentBoardControls";

const MESSAGES_ROUTE = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/messages";
const STOP_ROUTE = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop";

const audit: SubagentCard = {
	id: "agent-row",
	providerItemId: "toolu_audit",
	turnId: "turn-1",
	title: "Audit the auth flow",
	lane: "working",
	outcome: "working",
	startedAt: new Date(Date.now() - 60_000).toISOString(),
	updatedAt: new Date().toISOString(),
	notes: [{ kind: "status", text: "Reading the token validator", at: new Date(Date.now() - 30_000).toISOString() }],
	decisions: [],
	toolCount: 2,
	inbox: [],
};

const main: MainAgentSummary = {
	state: "working",
	turnId: "turn-1",
	startedAt: new Date(Date.now() - 120_000).toISOString(),
	nowLine: "Waiting on the audit",
	notes: [],
	decisions: [],
	sent: [],
};

type Steer = (text: string, attachments: undefined, clientMessageId: string) => Promise<ChatSteerOutcome | void>;

function Harness({ cards, summary, steer }: { cards: SubagentCard[]; summary: MainAgentSummary; steer?: Steer }) {
	const controls = useAgentBoardControls({ sessionId: "ao-1", cards, main: summary, steer });
	return <SubagentBoard activeTurnId="turn-1" cards={cards} controls={controls} main={summary} mode="chat" />;
}

function renderHarness(props: { cards?: SubagentCard[]; summary?: MainAgentSummary; steer?: Steer } = {}) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const invalidate = vi.spyOn(client, "invalidateQueries").mockResolvedValue();
	const wrap = (ui: ReactElement) => (
		<QueryClientProvider client={client}>
			<TooltipProvider>{ui}</TooltipProvider>
		</QueryClientProvider>
	);
	let current = { cards: [audit], summary: main, ...props };
	const view = render(wrap(<Harness {...current} />));
	return {
		invalidate,
		rerender: (next: Partial<typeof current>) => {
			current = { ...current, ...next };
			view.rerender(wrap(<Harness {...current} />));
		},
	};
}

function deferred<T>() {
	let resolve!: (value: T) => void;
	const promise = new Promise<T>((done) => {
		resolve = done;
	});
	return { promise, resolve };
}

const accepted = (data: unknown) => ({ data, error: undefined, response: { status: 202 } });
const refused = (status: number, code: string, message: string) => ({
	data: undefined,
	error: { error: "refused", code, message },
	response: { status },
});

const subagentCard = () => screen.getByRole("article", { name: "Audit the auth flow" });

async function messageSubagent(user: ReturnType<typeof userEvent.setup>, text: string) {
	await user.click(within(subagentCard()).getByRole("button", { name: "Message subagent" }));
	const field = within(subagentCard()).getByRole("textbox", { name: "Message subagent" });
	await user.type(field, `${text}{Enter}`);
	return field;
}

beforeEach(() => {
	postMock.mockReset();
});

describe("useAgentBoardControls with the agents board", () => {
	it("queues a subagent message on its tool use id, waiting for its next step until the inbox echoes it", async () => {
		const user = userEvent.setup();
		const response = deferred<ReturnType<typeof accepted>>();
		postMock.mockReturnValueOnce(response.promise);
		const view = renderHarness();

		const field = await messageSubagent(user, "Use the v2 client");
		expect(postMock).toHaveBeenCalledWith(MESSAGES_ROUTE, {
			params: { path: { sessionId: "ao-1", toolUseId: "toolu_audit" } },
			body: { text: "Use the v2 client" },
		});
		expect(field).toHaveValue("");
		// Shown at once, before the daemon answers.
		let sent = within(subagentCard()).getByTestId("sent-message");
		expect(sent).toHaveAttribute("data-sent-state", "queued");
		expect(sent).toHaveTextContent("You:Use the v2 client· waiting for its next step");

		response.resolve(accepted({ id: "inbox-1", state: "queued" }));
		await vi.waitFor(() => expect(view.invalidate).toHaveBeenCalledWith({ queryKey: conversationQueryKey("ao-1") }));

		// The inbox echo takes over, and the send shows once.
		view.rerender({
			cards: [{ ...audit, inbox: [{ id: "inbox-1", text: "Use the v2 client", state: "delivered", at: new Date().toISOString() }] }],
		});
		sent = within(subagentCard()).getByTestId("sent-message");
		expect(sent).toHaveAttribute("data-sent-state", "delivered");
		expect(sent).toHaveTextContent("· delivered");
	});

	it("says inline that a subagent already finished, and gives the words back", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValueOnce(refused(409, "CHAT_SUBAGENT_NOT_RUNNING", "subagent is not running"));
		renderHarness();

		const field = await messageSubagent(user, "Check logout too");
		await vi.waitFor(() => expect(within(subagentCard()).getByRole("alert")).toHaveTextContent("This subagent already finished"));
		expect(field).toHaveValue("Check logout too");
		expect(within(subagentCard()).queryByTestId("sent-message")).not.toBeInTheDocument();
	});

	it("shows the daemon's reason when the session cannot message subagents", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValueOnce(refused(422, "CHAT_SUBAGENT_UNSUPPORTED", "This agent cannot message its subagents"));
		renderHarness();
		await messageSubagent(user, "Hello");
		await vi.waitFor(() => expect(within(subagentCard()).getByRole("alert")).toHaveTextContent("This agent cannot message its subagents"));
	});

	it("stops only that subagent on its stop route, shows it stopping, then settles it as stopped", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValueOnce(accepted({ ok: true }));
		const view = renderHarness();

		await user.click(within(subagentCard()).getByRole("button", { name: "Stop subagent" }));
		expect(postMock).toHaveBeenCalledWith(STOP_ROUTE, { params: { path: { sessionId: "ao-1", toolUseId: "toolu_audit" } } });
		await vi.waitFor(() => expect(within(subagentCard()).getByRole("status")).toHaveTextContent("Stopping…"));
		expect(within(subagentCard()).getByRole("button", { name: "Stop subagent" })).toBeDisabled();
		// The main agent keeps its own stop and keeps running.
		expect(screen.getByRole("article", { name: "Main agent" })).toHaveAttribute("data-state", "working");

		view.rerender({ cards: [{ ...audit, lane: "done", outcome: "failed" }] });
		expect(subagentCard()).toHaveAttribute("data-lane", "done");
		expect(subagentCard()).toHaveAttribute("data-outcome", "stopped");
		expect(within(subagentCard()).queryByRole("status")).not.toBeInTheDocument();
	});

	it("explains a stop the daemon refused because the subagent had finished", async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValueOnce(refused(409, "CHAT_SUBAGENT_NOT_RUNNING", "subagent is not running"));
		renderHarness();
		await user.click(within(subagentCard()).getByRole("button", { name: "Stop subagent" }));
		await vi.waitFor(() => expect(within(subagentCard()).getByRole("alert")).toHaveTextContent("This subagent already finished"));
		expect(within(subagentCard()).getByRole("button", { name: "Stop subagent" })).toBeEnabled();
	});

	it("steers the main turn directly through the conversation's steer, never a subagent route", async () => {
		const user = userEvent.setup();
		const steer = vi.fn<Steer>(async () => ({ status: "accepted" }));
		const view = renderHarness({ steer });
		const mainCard = screen.getByRole("article", { name: "Main agent" });

		await user.click(within(mainCard).getByRole("button", { name: "Steer main agent" }));
		await user.type(within(mainCard).getByRole("textbox", { name: "Steer main agent" }), "Keep the old route working{Enter}");
		expect(steer).toHaveBeenCalledWith("Keep the old route working", undefined, expect.any(String));
		expect(postMock).not.toHaveBeenCalled();
		const clientMessageId = steer.mock.calls[0]![2];
		await vi.waitFor(() => expect(within(mainCard).getByTestId("sent-message")).toHaveAttribute("data-sent-state", "delivered"));

		// The steer's own row carries the same id, and replaces the local entry.
		view.rerender({
			summary: { ...main, sent: [{ id: clientMessageId, text: "Keep the old route working", state: "delivered", at: new Date().toISOString() }] },
		});
		expect(within(mainCard).getAllByTestId("sent-message")).toHaveLength(1);
	});

	it("shows why a steer was refused and keeps the words", async () => {
		const user = userEvent.setup();
		const reason = "The turn finished before this landed. Send it as a message instead.";
		const steer = vi.fn<Steer>(async () => ({ status: "not-accepted", reason }));
		renderHarness({ steer });
		const mainCard = screen.getByRole("article", { name: "Main agent" });
		await user.click(within(mainCard).getByRole("button", { name: "Steer main agent" }));
		const field = within(mainCard).getByRole("textbox", { name: "Steer main agent" });
		await user.type(field, "Use the cookie{Enter}");
		await vi.waitFor(() => expect(within(mainCard).getByRole("alert")).toHaveTextContent(reason));
		expect(field).toHaveValue("Use the cookie");
		expect(within(mainCard).queryByTestId("sent-message")).not.toBeInTheDocument();
	});
});
