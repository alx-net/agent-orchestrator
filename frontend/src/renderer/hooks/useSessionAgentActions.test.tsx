/**
 * Stepping in on one session's agents without its chat open: what the shared
 * summaries say the conversation allows decides the actions, and each action is
 * the conversation's own request.
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const { postMock } = vi.hoisted(() => ({ postMock: vi.fn() }));

vi.mock("../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../lib/api-client")>(),
	apiClient: { POST: postMock },
}));
// No workspaces, so the provider mounts no loader and the test's summaries stand.
vi.mock("./useWorkspaceQuery", () => ({ useWorkspaceQuery: () => ({ data: [] }) }));

import { AgentSummariesProvider, createAgentSummaryStore, type AgentSummaryStore } from "../components/AgentSummariesProvider";
import type { SessionAgentsDetail, SessionControlFacts } from "../lib/agents-overview";
import type { MainAgentSummary, SubagentCard } from "../lib/subagent-board";
import { MAIN_AGENT_CONTROL_KEY } from "./useAgentBoardControls";
import { useSessionAgentActions } from "./useSessionAgentActions";

const STEER = "/api/v1/sessions/{sessionId}/conversation/steer";
const MESSAGES = "/api/v1/sessions/{sessionId}/conversation/messages";
const INTERRUPT = "/api/v1/sessions/{sessionId}/conversation/interrupt";
const SUBAGENT_MESSAGES = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/messages";
const SUBAGENT_STOP = "/api/v1/sessions/{sessionId}/conversation/subagents/{toolUseId}/stop";

const audit: SubagentCard = {
	id: "agent-row",
	providerItemId: "toolu_audit",
	turnId: "turn-1",
	title: "Audit the auth flow",
	lane: "working",
	outcome: "working",
	startedAt: "2026-10-26T10:00:00.000Z",
	updatedAt: "2026-10-26T10:00:05.000Z",
	notes: [],
	decisions: [],
	toolCount: 1,
	inbox: [],
};

const working: MainAgentSummary = { state: "working", turnId: "turn-1", startedAt: "2026-10-26T10:00:00.000Z", notes: [], decisions: [], sent: [] };
const needsInput: MainAgentSummary = {
	state: "needs_you",
	turnId: "turn-1",
	nowLine: "Should paused sessions keep their slot?",
	nowSource: "outcome",
	result: { kind: "needs_input", text: "Should paused sessions keep their slot?", at: "2026-10-26T10:05:00.000Z", source: "outcome_line" },
	notes: [{ kind: "turn", text: "Pause sessions", at: "2026-10-26T10:00:00.000Z", fromYou: true }],
	decisions: [],
	sent: [],
};

const RUNNING: SessionControlFacts = { runningTurnId: "turn-1", steer: true, subagents: true };
const ENDED: SessionControlFacts = { steer: true, subagents: true };

function ready(main: MainAgentSummary, control: SessionControlFacts | undefined, cards: SubagentCard[] = [audit]): SessionAgentsDetail {
	return { status: "ready", model: { main, cards }, control };
}

/** `asked` is the session the caller passes; null passes none. */
function setup(detail: SessionAgentsDetail | undefined, asked: string | null = "ao-1") {
	const sessionId = asked ?? undefined;
	const store: AgentSummaryStore = createAgentSummaryStore();
	if (detail) store.set("ao-1", detail);
	const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
	vi.spyOn(client, "invalidateQueries").mockResolvedValue();
	const wrapper = ({ children }: { children: ReactNode }) => (
		<QueryClientProvider client={client}>
			<AgentSummariesProvider store={store}>{children}</AgentSummariesProvider>
		</QueryClientProvider>
	);
	const view = renderHook(() => useSessionAgentActions(sessionId), { wrapper });
	return { ...view, store };
}

const accepted = (data: unknown) => ({ data, error: undefined, response: { status: 202 } });
const refused = (status: number, code: string, message: string) => ({ data: undefined, error: { error: "refused", code, message }, response: { status } });

beforeEach(() => {
	postMock.mockReset();
});

describe("useSessionAgentActions", () => {
	it("steers the running turn through the conversation's steer, never a queued send", async () => {
		postMock.mockResolvedValueOnce(accepted({ providerTurnId: "provider-1" }));
		const { result } = setup(ready(working, RUNNING));
		expect(result.current.mainMessage).toBe("steer");
		expect(result.current.sendMain).toBeUndefined();

		let sent = false;
		await act(async () => {
			sent = await result.current.steerMain!("Keep the v1 route working");
		});
		expect(sent).toBe(true);
		expect(postMock).toHaveBeenCalledTimes(1);
		expect(postMock).toHaveBeenCalledWith(STEER, expect.objectContaining({
			params: { path: { sessionId: "ao-1" } },
			body: expect.objectContaining({ text: "Keep the v1 route working", clientMessageId: expect.any(String) }),
		}));
		const pending = result.current.states.get(MAIN_AGENT_CONTROL_KEY)?.pending;
		expect(pending).toEqual([expect.objectContaining({ text: "Keep the v1 route working", state: "delivered" })]);
	});

	it("sends a new message once the turn ended, held until the conversation echoes it", async () => {
		postMock.mockResolvedValueOnce(accepted({ turnId: "turn-2", state: "running" }));
		const { result, store } = setup(ready(needsInput, ENDED));
		expect(result.current.mainMessage).toBe("send");
		expect(result.current.steerMain).toBeUndefined();
		expect(result.current.stopMain).toBeUndefined();

		await act(async () => {
			await result.current.sendMain!("Yes, keep the slot");
		});
		const [route, request] = postMock.mock.calls[0]!;
		expect(route).toBe(MESSAGES);
		expect(request).toMatchObject({ params: { path: { sessionId: "ao-1" } }, body: { text: "Yes, keep the slot" } });
		const clientMessageId = (request as { body: { clientMessageId: string } }).body.clientMessageId;
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)?.pending).toEqual([
			expect.objectContaining({ id: clientMessageId, text: "Yes, keep the slot", state: "delivered" }),
		]);

		// The message that opened the new turn carries the same id, and replaces the local entry.
		act(() => store.set("ao-1", ready({
			...working,
			turnId: "turn-2",
			notes: [...needsInput.notes, { kind: "turn", text: "Yes, keep the slot", at: "2026-10-26T10:06:00.000Z", fromYou: true, clientMessageId }],
		}, { ...RUNNING, runningTurnId: "turn-2" })));
		await waitFor(() => expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)?.pending).toEqual([]));
		expect(result.current.mainMessage).toBe("steer");
	});

	it("stops the running turn on the interrupt route, and clears the stop once that turn ends", async () => {
		postMock.mockResolvedValueOnce(accepted(undefined));
		const { result, store } = setup(ready(working, RUNNING));
		await act(async () => {
			await result.current.stopMain!();
		});
		expect(postMock).toHaveBeenCalledWith(INTERRUPT, { params: { path: { sessionId: "ao-1" } } });
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)).toMatchObject({ stopping: true, stopped: true });

		act(() => store.set("ao-1", ready({ ...working, state: "stopped", startedAt: undefined }, ENDED)));
		await waitFor(() => expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)).toMatchObject({ stopping: false, stopped: false }));
		expect(result.current.stopMain).toBeUndefined();
		expect(result.current.mainMessage).toBe("send");
	});

	it("messages and stops one subagent on its own routes", async () => {
		postMock.mockResolvedValueOnce(accepted({ id: "inbox-1", state: "queued" })).mockResolvedValueOnce(accepted({ ok: true }));
		const { result } = setup(ready(working, RUNNING));
		await act(async () => {
			await result.current.messageSubagent!("toolu_audit", "Use the v2 client");
		});
		expect(postMock).toHaveBeenLastCalledWith(SUBAGENT_MESSAGES, {
			params: { path: { sessionId: "ao-1", toolUseId: "toolu_audit" } },
			body: { text: "Use the v2 client" },
		});
		expect(result.current.states.get("toolu_audit")?.pending).toEqual([expect.objectContaining({ id: "inbox-1", state: "queued" })]);

		await act(async () => {
			await result.current.stopSubagent!("toolu_audit");
		});
		expect(postMock).toHaveBeenLastCalledWith(SUBAGENT_STOP, { params: { path: { sessionId: "ao-1", toolUseId: "toolu_audit" } } });
		expect(result.current.states.get("toolu_audit")).toMatchObject({ stopped: true });
		// Stopping one subagent never interrupts the main turn.
		expect(postMock).not.toHaveBeenCalledWith(INTERRUPT, expect.anything());
	});

	it("offers only what the conversation allows", () => {
		// A running turn that cannot be steered can only be stopped; subagents are out of reach.
		const locked = setup(ready(working, { runningTurnId: "turn-1", steer: false, subagents: false }));
		expect(locked.result.current).toMatchObject({ mainMessage: undefined, steerMain: undefined, sendMain: undefined, messageSubagent: undefined, stopSubagent: undefined });
		expect(locked.result.current.stopMain).toEqual(expect.any(Function));
		locked.unmount();

		// Nothing is known about a session the summaries do not hold, or hold without its conversation's facts.
		for (const detail of [undefined, { status: "loading" } as const, ready(working, undefined)]) {
			const { result, unmount } = setup(detail);
			expect(result.current).toMatchObject({ mainMessage: undefined, steerMain: undefined, sendMain: undefined, stopMain: undefined, messageSubagent: undefined, stopSubagent: undefined });
			unmount();
		}
		// A row that is not a chat session asks for none.
		const none = setup(ready(working, RUNNING), null);
		expect(none.result.current.stopMain).toBeUndefined();
	});

	it("names why an action did not go through", async () => {
		postMock
			.mockResolvedValueOnce(refused(409, "CHAT_CONTROLLER_NOT_READY", "the agent controller for this session is not running"))
			.mockResolvedValueOnce(refused(409, "CHAT_SUBAGENT_NOT_RUNNING", "that subagent is not running"))
			.mockResolvedValueOnce(refused(409, "CHAT_CONTROLLER_NOT_READY", "the agent controller for this session is not running"))
			.mockResolvedValueOnce(refused(503, "CHAT_INTERRUPT_FAILED", "could not interrupt"));
		const { result } = setup(ready(working, RUNNING));

		let sent = true;
		await act(async () => {
			sent = await result.current.steerMain!("Use the cookie");
		});
		expect(sent).toBe(false);
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)).toMatchObject({ sendError: { kind: "not_running" }, pending: [] });

		await act(async () => {
			await result.current.messageSubagent!("toolu_audit", "Check logout too");
		});
		expect(result.current.states.get("toolu_audit")?.sendError).toEqual({ kind: "finished" });
		// A 409 that says the agent is not running is not a finished subagent.
		await act(async () => {
			await result.current.stopSubagent!("toolu_audit");
		});
		expect(result.current.states.get("toolu_audit")?.stopError).toEqual({ kind: "not_running" });

		await act(async () => {
			await result.current.stopMain!();
		});
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)).toMatchObject({ stopping: false, stopError: { kind: "message", message: "could not interrupt" } });
	});

	it("names a refused send in the daemon's words, and an agent that is not running", async () => {
		postMock
			.mockResolvedValueOnce(refused(409, "CHAT_CONTROLLER_NOT_READY", "the agent controller for this session is not running"))
			.mockResolvedValueOnce(refused(400, "CHAT_TEXT_REQUIRED", "message text is required"));
		const { result } = setup(ready(needsInput, ENDED));
		await act(async () => {
			await result.current.sendMain!("Yes");
		});
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)).toMatchObject({ sendError: { kind: "not_running" }, pending: [] });
		await act(async () => {
			await result.current.sendMain!(" ");
		});
		expect(result.current.states.get(MAIN_AGENT_CONTROL_KEY)?.sendError).toEqual({ kind: "message", message: "message text is required" });
	});
});
