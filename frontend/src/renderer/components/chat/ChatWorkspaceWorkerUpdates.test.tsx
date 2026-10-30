/**
 * The orchestrator chat draws worker traffic as rows. These pin what a reader
 * keeps when it does: a quiet reply still copies and rolls back, a reply that asks
 * something stays a full reply, and a turn with no reply still shows its work.
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";
import { ChatWorkspace } from "./ChatWorkspace";
import { useChatLinkWorkspaces } from "./ChatMarkdown";
import { usePlainReferences } from "./WorkerUpdates";
import { workspaceQueryKey } from "../../hooks/useWorkspaceQuery";
import { chatFixtureEmpty } from "../../lib/chat-fixture";
import type { ConversationActivity, ConversationItem, ConversationMessage, ConversationSnapshot } from "../../types/conversation";
import type { WorkspaceSummary } from "../../types/workspace";
import { TooltipProvider } from "../ui/tooltip";

const at = (minute: number) => `2026-10-22T03:${String(minute).padStart(2, "0")}:00Z`;

function automation(turnId: string, minute: number, text: string): ConversationMessage {
	return {
		kind: "message", id: `${turnId}-report`, turnId, sequence: minute * 10, revision: 0, role: "user", origin: "automation",
		streaming: false, createdAt: at(minute), text,
	};
}

const report = (turnId: string, minute: number, entry: string) =>
	automation(turnId, minute, `Reports since your previous turn:\n\n${entry}`);

function command(turnId: string, minute: number, text: string): ConversationActivity {
	return {
		kind: "activity", id: `${turnId}-command`, turnId, sequence: minute * 10 + 1, revision: 0, activityKind: "command",
		status: "completed", summary: text, detail: { command: text, output: "ok" }, createdAt: at(minute),
	};
}

function reply(turnId: string, minute: number, text: string): ConversationMessage {
	return {
		kind: "message", id: `${turnId}-reply`, turnId, sequence: minute * 10 + 2, revision: 0, role: "assistant", origin: "provider",
		streaming: false, createdAt: at(minute), text,
	};
}

function snapshot(items: ConversationItem[]): ConversationSnapshot {
	const turnIds = [...new Set(items.map((item) => item.turnId as string))];
	return {
		...chatFixtureEmpty,
		latestSequence: Math.max(...items.map((item) => item.sequence)),
		turns: turnIds.map((id) => ({
			id, state: "completed" as const, providerTurnId: `provider-${id}`,
			requestedAt: items.find((item) => item.turnId === id)!.createdAt,
			completedAt: items.filter((item) => item.turnId === id).at(-1)!.createdAt,
		})),
		items,
	};
}

function renderOrchestrator(items: ConversationItem[], onRollback = vi.fn(), workspaces: WorkspaceSummary[] = [], sessionRole: "orchestrator" | "worker" = "orchestrator") {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	queryClient.setQueryData(workspaceQueryKey, workspaces);
	render(
		<QueryClientProvider client={queryClient}>
			<TooltipProvider>
				<ChatWorkspace snapshot={snapshot(items)} sessionRole={sessionRole} onRollback={onRollback} />
			</TooltipProvider>
		</QueryClientProvider>,
	);
	return onRollback;
}

describe("orchestrator worker updates", () => {
	it("draws a report as a row and an answer that asks nothing as one line that keeps copy and rollback", async () => {
		const onRollback = renderOrchestrator([
			report("t1", 1, "[done] [Check regressions](ao://sessions/proj/proj-288)\nOne strong candidate: #7043\nScreened 58 PRs."),
			command("t1", 1, "gh pr merge 7054"),
			reply("t1", 2, "Merged [#7054](https://github.com/o/r/pull/7054)."),
		]);

		const row = screen.getByTestId("worker-update");
		expect(row).toHaveTextContent("Check regressions");
		expect(row).toHaveTextContent("One strong candidate: #7043");
		expect(screen.queryByText(/Reports since your previous turn/)).not.toBeInTheDocument();

		const answer = screen.getByTestId("orchestrator-reply");
		expect(answer).toHaveTextContent("Merged #7054.");
		expect(screen.queryByRole("button", { name: /^Worked for/ })).not.toBeInTheDocument();
		expect(within(answer).getByRole("button", { name: "Copy message as markdown" })).toBeInTheDocument();

		const work = within(answer).getByRole("button", { name: "Show the work behind this reply" });
		await userEvent.click(work);
		expect(work).toHaveAttribute("aria-expanded", "true");

		await userEvent.click(within(answer).getByRole("button", { name: "Roll back to here" }));
		await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Roll back" }));
		expect(onRollback).toHaveBeenCalledWith("t1");
	});

	it("keeps an answer that asks the user something as the full reply", () => {
		renderOrchestrator([
			report("t1", 1, "[needs_input] [iOS pages](ao://sessions/proj/proj-286)\nTunnel or loopback?"),
			command("t1", 1, "ao session logs proj-286"),
			reply("t1", 2, "Checked its logs.\n\nShould it load over the tunnel?"),
		]);

		expect(screen.queryByTestId("orchestrator-reply")).not.toBeInTheDocument();
		expect(screen.getByText("Checked its logs.")).toBeInTheDocument();
		expect(screen.getByText("Should it load over the tunnel?")).toBeInTheDocument();
		expect(screen.getByRole("button", { name: /^Worked for/ })).toBeInTheDocument();
	});

	it("draws an older worker message that names its sender only in its prefix as a row titled by the session", () => {
		const workspaces = [{
			id: "proj", name: "proj", path: "/tmp/proj",
			sessions: [{ id: "proj-187", workspaceId: "proj", title: "PR 5051 rebase", provider: "codex", kind: "worker", status: "idle", prs: [] }],
		}] as unknown as WorkspaceSummary[];
		renderOrchestrator([
			automation("t1", 1, "[from proj-187] Rebased #5051; disk full during validation, retrying."),
			reply("t1", 2, "Noted."),
		], vi.fn(), workspaces);

		const row = screen.getByTestId("worker-update");
		expect(row).toHaveTextContent("PR 5051 rebase");
		expect(row).toHaveTextContent("Rebased #5051");
		expect(row).not.toHaveTextContent("[from");
		expect(within(row).getByRole("link", { name: "PR 5051 rebase" })).toHaveAttribute("href", "ao://sessions/proj/proj-187");
	});

	it("draws AO's task-naming prompt as the name it produced", () => {
		renderOrchestrator([
			automation("t1", 1, "AO TASK TITLE UPDATE\nChoose a concise task title from the brief and run:\nao session rename proj-150 \"<title>\""),
			command("t1", 1, 'ao session rename proj-150 "Chat Link Browser"'),
			reply("t1", 2, "Renamed the worker task to **Chat Link Browser**."),
		]);

		expect(screen.getByText("Chat Link Browser", { selector: "span" }).closest("p")).toHaveTextContent("New task named Chat Link Browser");
		expect(screen.queryByText(/AO TASK TITLE UPDATE/)).not.toBeInTheDocument();
	});

	it("still shows the work of a turn that answered with tools only", () => {
		renderOrchestrator([
			report("t1", 1, "[done] [Menus](ao://sessions/proj/proj-285)\nMenus render above the browser"),
			command("t1", 1, "gh pr merge 7054"),
		]);

		expect(screen.getByTestId("worker-update")).toHaveTextContent("Menus");
		expect(screen.queryByTestId("orchestrator-reply")).not.toBeInTheDocument();
		expect(screen.getByRole("button", { name: /^Worked for/ })).toBeInTheDocument();
	});
});

describe("agent messages in a worker chat", () => {
	const workspaces = [{
		id: "proj", name: "proj", path: "/tmp/proj",
		sessions: [
			{ id: "proj-1", workspaceId: "proj", title: "Orchestrator", provider: "codex", kind: "orchestrator", status: "working", prs: [] },
			{ id: "proj-7", workspaceId: "proj", title: "Sibling worker", provider: "codex", kind: "worker", status: "idle", prs: [] },
		],
	}] as unknown as WorkspaceSummary[];
	const sent = (text: string, sender: string): ConversationMessage => ({
		...automation("t1", 1, `[from ${sender}] ${text}`), senderSessionId: sender, senderProjectId: "proj",
	});

	const toggle = (message: HTMLElement) => within(message).getByRole("button", { expanded: false });

	it("folds the orchestrator's message into one row: its name linked to its session, the gist unsigned, and time", () => {
		const body = "Orchestrator: keep the draft local until Dhruv approves. Then open the PR as a draft and report back with its number.";
		renderOrchestrator([sent(body, "proj-1"), reply("t1", 2, "Kept it local.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		expect(within(message).getByRole("link", { name: "Orchestrator" })).toHaveAttribute("href", "ao://sessions/proj/proj-1");
		const row = toggle(message);
		expect(row).toHaveAttribute("aria-expanded", "false");
		expect(row).toHaveTextContent("keep the draft local until Dhruv approves");
		expect(message).not.toHaveTextContent("Orchestrator: keep");
		expect(message).not.toHaveTextContent("Then open the PR");
		expect(message).not.toHaveTextContent("[from");
		expect(screen.queryByRole("button", { name: "Show more" })).not.toBeInTheDocument();
	});

	it("opens to the full message on click, Enter or Space, and closes again", async () => {
		const user = userEvent.setup();
		const body = "Orchestrator: keep the draft local until Dhruv approves. Then open the PR as a draft and report back with its number.";
		renderOrchestrator([sent(body, "proj-1"), reply("t1", 2, "Kept it local.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		const row = toggle(message);
		await user.click(row);
		expect(row).toHaveAttribute("aria-expanded", "true");
		expect(message).toHaveTextContent(body);
		await user.click(row);
		expect(row).toHaveAttribute("aria-expanded", "false");
		expect(message).not.toHaveTextContent("Then open the PR");

		row.focus();
		await user.keyboard("{Enter}");
		expect(row).toHaveAttribute("aria-expanded", "true");
		await user.keyboard(" ");
		expect(row).toHaveAttribute("aria-expanded", "false");
	});

	it("keeps several agent messages one row each", () => {
		const second = { ...sent("proj-7: rebased on main. All checks green.", "proj-7"), id: "t2-report", turnId: "t2", sequence: 30, createdAt: at(3) };
		renderOrchestrator([
			sent("Orchestrator, two things: (1) rebase, (2) report.", "proj-1"),
			reply("t1", 2, "On it."),
			second,
			reply("t2", 4, "Noted."),
		], vi.fn(), workspaces, "worker");

		const messages = screen.getAllByTestId("agent-message");
		expect(messages).toHaveLength(2);
		expect(toggle(messages[0]!)).toHaveTextContent("two things: (1) rebase, (2) report");
		expect(toggle(messages[1]!)).toHaveTextContent("rebased on main");
		expect(messages[1]).not.toHaveTextContent("All checks green");
	});

	it("folds an agent's mid-turn steer into the same row", () => {
		const steer: ConversationActivity = {
			kind: "activity", id: "t1-steer", turnId: "t1", sequence: 15, revision: 0, activityKind: "system", status: "completed",
			summary: "Steer", createdAt: at(1),
			detail: { event: "steer", text: "[from proj-1] Orchestrator: stop pushing. #5480 is merged.", senderSessionId: "proj-1", senderProjectId: "proj" },
		};
		renderOrchestrator([sent("Rebase #5480.", "proj-1"), steer, reply("t1", 2, "Stopped.")], vi.fn(), workspaces, "worker");

		const messages = screen.getAllByTestId("agent-message");
		expect(messages).toHaveLength(2);
		expect(toggle(messages[1]!)).toHaveTextContent("stop pushing");
		expect(messages[1]).not.toHaveTextContent("#5480 is merged");
	});

	it("marks the paths in its plain prose as code and links the sessions it names", async () => {
		renderOrchestrator([sent("Compare /tmp/proj/.ao/shot.png with proj-7, then report.", "proj-1"), reply("t1", 2, "On it.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		await userEvent.click(toggle(message));
		expect(within(message).getByText("/tmp/proj/.ao/shot.png").tagName).toBe("CODE");
		expect(within(message).getByRole("link", { name: "proj-7" })).toHaveAttribute("href", "ao://sessions/proj/proj-7");
		expect(message).toHaveTextContent("Compare /tmp/proj/.ao/shot.png with proj-7, then report.");
	});

	it("keeps an authored reference link to its own target", async () => {
		renderOrchestrator([sent("See [proj-7][target].\n\n[target]: ao://sessions/proj/proj-1", "proj-1"), reply("t1", 2, "Ok.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		await userEvent.click(toggle(message));
		const links = within(message).getAllByRole("link", { name: "proj-7" });
		expect(links).toHaveLength(1);
		expect(links[0]).toHaveAttribute("href", "ao://sessions/proj/proj-1");
	});

	it("shares one plugin list until the set of sessions changes, so session events do not re-parse messages", async () => {
		const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		queryClient.setQueryData(workspaceQueryKey, workspaces);
		const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
		const { result } = renderHook(() => ({ plugins: usePlainReferences(), workspaces: useChatLinkWorkspaces() }), { wrapper });
		const first = result.current.plugins;
		expect(renderHook(usePlainReferences, { wrapper }).result.current).toBe(first);
		const [project] = workspaces;
		act(() => queryClient.setQueryData(workspaceQueryKey, [{ ...project, sessions: project!.sessions.map((entry) => ({ ...entry, status: "idle" })) }]));
		await waitFor(() => expect(result.current.workspaces).not.toBe(workspaces));
		expect(result.current.plugins).toBe(first);
		act(() => queryClient.setQueryData(workspaceQueryKey, [{ ...project, sessions: [...project!.sessions, { ...project!.sessions[1]!, id: "proj-8" }] }]));
		await waitFor(() => expect(result.current.plugins).not.toBe(first));
	});

	it("names a sibling worker by its session id, as agents name each other", () => {
		renderOrchestrator([sent("Rebased on main.", "proj-7"), reply("t1", 2, "Noted.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		expect(toggle(message)).toHaveTextContent("Rebased on main");
		expect(within(message).getByRole("link", { name: "proj-7" })).toHaveAttribute("href", "ao://sessions/proj/proj-7");
		expect(message).not.toHaveTextContent("Sibling worker");
	});

	it("folds a message whose sender this chat cannot find into a row named by its session id", () => {
		renderOrchestrator([sent("proj-404: rebased on main. Pushed.", "proj-404"), reply("t1", 2, "Noted.")], vi.fn(), workspaces, "worker");

		const message = screen.getByTestId("agent-message");
		expect(within(message).getByRole("link", { name: "proj-404" })).toHaveAttribute("href", "ao://sessions/proj/proj-404");
		expect(toggle(message)).toHaveTextContent("rebased on main");
		expect(message).not.toHaveTextContent("Pushed");
	});
});
