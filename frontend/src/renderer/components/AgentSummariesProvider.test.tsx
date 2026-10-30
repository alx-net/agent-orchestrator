import { act, render, screen } from "@testing-library/react";
import { useEffect, useRef, useSyncExternalStore } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ConversationItem, ConversationSnapshot, ConversationTurn } from "../types/conversation";
import type { WorkspaceSession, WorkspaceSummary } from "../types/workspace";

const { conversations, mounted, workspaceQueryMock } = vi.hoisted(() => {
	const listeners = new Set<() => void>();
	const data = new Map<string, unknown>();
	return {
		conversations: {
			get: (id: string) => data.get(id),
			set(id: string, snapshot: unknown) {
				data.set(id, snapshot);
				for (const listener of [...listeners]) listener();
			},
			clear: () => data.clear(),
			subscribe(listener: () => void) {
				listeners.add(listener);
				return () => {
					listeners.delete(listener);
				};
			},
		},
		/** Live loaders per session: a conversation subscription each. */
		mounted: new Map<string, number>(),
		workspaceQueryMock: vi.fn(),
	};
});

vi.mock("../hooks/useConversation", () => ({
	useConversation: (sessionId: string) => {
		useEffect(() => {
			mounted.set(sessionId, (mounted.get(sessionId) ?? 0) + 1);
			return () => {
				mounted.set(sessionId, (mounted.get(sessionId) ?? 1) - 1);
			};
		}, [sessionId]);
		const snapshot = useSyncExternalStore(conversations.subscribe, () => conversations.get(sessionId));
		return { snapshot, isLoading: !snapshot, hasOlder: false, isLoadingOlder: false, loadOlder: () => {} };
	},
}));

vi.mock("../hooks/useWorkspaceQuery", () => ({
	useWorkspaceQuery: () => workspaceQueryMock(),
}));

import {
	AgentSummariesProvider,
	createAgentSummaryStore,
	useAgentSummaries,
	useAgentSummary,
	useAgentSummaryDemand,
} from "./AgentSummariesProvider";

function session(id: string, overrides: Partial<WorkspaceSession> = {}): WorkspaceSession {
	return {
		id,
		workspaceId: "app",
		workspaceName: "app",
		title: id,
		provider: "claude-code",
		kind: "worker",
		mode: "chat",
		status: "working",
		activity: { state: "active", lastActivityAt: "2026-10-26T10:00:00.000Z" },
		createdAt: "2026-10-26T09:00:00.000Z",
		updatedAt: "2026-10-26T10:00:00.000Z",
		prs: [],
		...overrides,
	};
}

const workspaces: WorkspaceSummary[] = [{
	id: "app",
	name: "app",
	path: "/repos/app",
	sessions: [
		session("api", { updatedAt: "2026-10-26T10:05:00.000Z" }),
		session("ui"),
		session("tui", { mode: "tui" }),
		session("remote", { hostId: "host-1" }),
	],
}];

let sequence = 0;
function snapshotSaying(sessionId: string, text: string): ConversationSnapshot {
	sequence += 1;
	const turns: ConversationTurn[] = [{ id: `${sessionId}-turn`, state: "running", requestedAt: "2026-10-26T10:00:00.000Z" }];
	const items: ConversationItem[] = [{
		kind: "message",
		id: `${sessionId}-said`,
		turnId: `${sessionId}-turn`,
		sequence,
		revision: sequence,
		role: "assistant",
		origin: "provider",
		text,
		streaming: true,
		createdAt: "2026-10-26T10:00:01.000Z",
	}];
	return { items, turns } as unknown as ConversationSnapshot;
}

const renders = new Map<string, number>();

function Reader({ sessionId }: { sessionId: string }) {
	const detail = useAgentSummary(sessionId);
	const count = useRef(0);
	count.current += 1;
	renders.set(sessionId, count.current);
	const text = detail?.status === "ready" ? detail.model.main.nowLine ?? "" : detail?.status ?? "none";
	return <p data-testid={`reader-${sessionId}`}>{text}</p>;
}

function Overview({ ids }: { ids: readonly string[] }) {
	useAgentSummaryDemand(ids);
	const details = useAgentSummaries();
	return <p data-testid="overview">{[...details.keys()].sort().join(",")}</p>;
}

beforeEach(() => {
	conversations.clear();
	mounted.clear();
	renders.clear();
	workspaceQueryMock.mockReset().mockReturnValue({ data: workspaces });
});

const live = () => [...mounted].filter(([, count]) => count > 0).map(([id, count]) => `${id}:${count}`).sort();

describe("AgentSummariesProvider", () => {
	it("loads nothing until a surface asks, then each asked-for chat session once, however many read it", () => {
		const { rerender } = render(<AgentSummariesProvider><p>idle</p></AgentSummariesProvider>);
		expect(live()).toEqual([]);

		conversations.set("api", snapshotSaying("api", "Status: Writing the pause handler\n"));
		const ids = ["api", "tui", "remote", "missing"];
		rerender(
			<AgentSummariesProvider>
				<Reader sessionId="api" />
				<Reader sessionId="api" />
				<Overview ids={ids} />
			</AgentSummariesProvider>,
		);
		// One loader for the session three readers asked for; terminal, remote and unknown sessions load nothing.
		expect(live()).toEqual(["api:1"]);
		expect(screen.getAllByTestId("reader-api").map((reader) => reader.textContent)).toEqual(["Writing the pause handler", "Writing the pause handler"]);
		expect(screen.getByTestId("overview")).toHaveTextContent("api");
	});

	it("re-renders a reader only when its own session's summary changes", () => {
		conversations.set("api", snapshotSaying("api", "Status: Writing the handler\n"));
		conversations.set("ui", snapshotSaying("ui", "Status: Drawing the button\n"));
		render(
			<AgentSummariesProvider>
				<Reader sessionId="api" />
				<Reader sessionId="ui" />
			</AgentSummariesProvider>,
		);
		expect(screen.getByTestId("reader-ui")).toHaveTextContent("Drawing the button");
		const before = { api: renders.get("api")!, ui: renders.get("ui")! };

		act(() => conversations.set("api", snapshotSaying("api", "Status: Writing the handler\nStatus: Testing the handler\n")));
		expect(screen.getByTestId("reader-api")).toHaveTextContent("Testing the handler");
		expect(renders.get("api")).toBeGreaterThan(before.api);
		expect(renders.get("ui")).toBe(before.ui);
	});

	it("says a session is loading until its conversation arrives", () => {
		render(<AgentSummariesProvider><Reader sessionId="ui" /></AgentSummariesProvider>);
		expect(screen.getByTestId("reader-ui")).toHaveTextContent("loading");
		act(() => conversations.set("ui", snapshotSaying("ui", "Status: Drawing the button\n")));
		expect(screen.getByTestId("reader-ui")).toHaveTextContent("Drawing the button");
	});

	it("stops loading, and forgets, a session once no surface asks for it", () => {
		conversations.set("api", snapshotSaying("api", "Status: Writing\n"));
		const store = createAgentSummaryStore();
		const { rerender } = render(<AgentSummariesProvider store={store}><Reader sessionId="api" /></AgentSummariesProvider>);
		expect(live()).toEqual(["api:1"]);
		expect(store.get("api")?.status).toBe("ready");

		rerender(<AgentSummariesProvider store={store}><p>elsewhere</p></AgentSummariesProvider>);
		expect(live()).toEqual([]);
		expect(store.get("api")).toBeUndefined();
		expect(store.wanted()).toEqual([]);
	});

	it("keeps one demand per surface and the union of what is asked for", () => {
		const store = createAgentSummaryStore();
		const releaseBoard = store.demand(["api", "ui"]);
		const wanted = store.wanted();
		const releaseCard = store.demand(["api"]);
		// Nothing new asked for: the same array.
		expect(store.wanted()).toBe(wanted);
		releaseBoard();
		expect(store.wanted()).toEqual(["api"]);
		releaseBoard();
		expect(store.wanted()).toEqual(["api"]);
		releaseCard();
		expect(store.wanted()).toEqual([]);
	});

	it("reads no summary without a provider", () => {
		render(<Reader sessionId="api" />);
		expect(screen.getByTestId("reader-api")).toHaveTextContent("none");
		render(<Overview ids={["api"]} />);
		expect(screen.getByTestId("overview")).toBeEmptyDOMElement();
	});
});
