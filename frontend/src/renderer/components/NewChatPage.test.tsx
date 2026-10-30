import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { prepareChatComposerDelivery, writeChatComposerText } from "../lib/chat-drafts";
import type { ChatWorkspaceProps } from "./chat/ChatWorkspace";
import { NewChatPage } from "./NewChatPage";

const h = vi.hoisted(() => ({
	navigate: vi.fn(),
	launch: vi.fn(),
	launchFallback: vi.fn(),
	fallbackAction: undefined as string | undefined,
	workspace: undefined as ChatWorkspaceProps | undefined,
	preparedSessionId: "",
	get: vi.fn(),
	post: vi.fn(),
}));

vi.mock("../lib/api-client", async (importOriginal) => ({
	...(await importOriginal<typeof import("../lib/api-client")>()),
	apiClient: { GET: h.get, POST: h.post },
}));

vi.mock("@tanstack/react-router", () => ({
	useNavigate: () => h.navigate,
	useRouter: () => ({ preloadRoute: async () => undefined, history: { canGoBack: () => false, back: vi.fn() } }),
}));
vi.mock("./ShellTopbar", () => ({ topbarDragStyle: undefined }));
vi.mock("./settings/MobileDevicesSection", () => ({ mobileDevicesQueryKey: ["mobile-devices"], fetchDevices: async () => [] }));
vi.mock("./chat/ChatWorkspace", () => ({
	ChatWorkspace: (props: ChatWorkspaceProps) => {
		h.workspace = props;
		return <>{props.draftNotice}</>;
	},
}));
vi.mock("./TaskComposer", () => ({
	useTaskLaunch: () => ({
		launch: h.launch,
		launchFallback: h.launchFallback,
		fallbackAction: h.fallbackAction,
		fallbackLabel: "Create as Terminal UI",
		preparedSessionId: h.preparedSessionId,
		submitMode: undefined,
		isCloudProject: false,
		canSubmit: true,
		launchSnapshot: () => ({ agent: "codex", model: "gpt-6", effort: "high", approvalMode: undefined, mode: undefined }),
		effectiveInterface: "chat",
		refreshModels: async () => undefined,
		chatModels: [{ id: "gpt-6", displayName: "GPT-6", default: true }],
		turnSettings: { model: "gpt-6", reasoningEffort: "high", approvalMode: "auto" },
		chooseTurnSettings: vi.fn(),
		agent: { value: "codex" },
		model: {},
		effort: {},
		showEffort: false,
		renderAgentControl: () => null,
		renderModelControl: () => null,
		renderEffortControl: () => null,
	}),
}));
vi.mock("../hooks/useWorkspaceQuery", () => ({
	useWorkspaceQuery: () => ({ data: [{ id: "proj-1", name: "Project", kind: "project", sessions: [] }] }),
	useRemoteWorkspaces: () => ({ data: [] }),
	workspaceQueryKeyForHost: () => ["workspaces"],
}));

const draftScope = { sessionId: "new-chat:local", incarnation: "new-chat:local" };

function renderPage(initialProject = "proj-1") {
	const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	const page = (projectId: string) => <QueryClientProvider client={queryClient}><NewChatPage projectId={projectId} /></QueryClientProvider>;
	const { rerender } = render(page(initialProject));
	return Object.assign(queryClient, { showProject: (projectId: string) => rerender(page(projectId)) });
}

beforeEach(() => {
	window.localStorage.removeItem("ao.newTask.pendingLaunch.v1");
	h.navigate.mockReset();
	h.launch.mockReset();
	h.launchFallback.mockReset();
	h.fallbackAction = undefined;
	h.workspace = undefined;
	h.preparedSessionId = "";
	h.get.mockReset();
	h.post.mockReset();
	window.localStorage.clear();
});

describe("NewChatPage", () => {
	it("takes files on Scratchpad's first message once its folder is prepared", async () => {
		h.preparedSessionId = "standalone-7";
		h.get.mockResolvedValue({ data: { files: [] } });
		h.post.mockResolvedValue({ data: { paths: [".ao/attachments/attachment-1.png"] } });
		renderPage("__standalone__");

		await waitFor(() => expect(h.workspace?.onStageAttachments).toBeTypeOf("function"));
		expect(h.workspace?.attachLater).toBe(false);
		// The opening message's images are served from the prepared session.
		expect(h.workspace?.snapshot.sessionId).toBe("standalone-7");
		await expect(h.workspace?.onStageAttachments?.([{ mimeType: "image/png", data: "AQID" }])).resolves.toEqual([".ao/attachments/attachment-1.png"]);
		expect(h.post).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/attachments", {
			params: { path: { sessionId: "standalone-7" } },
			body: { attachments: [{ mimeType: "image/png", data: "AQID" }] },
		});
	});

	it("leaves attachments for after the send when Scratchpad could not be prepared", () => {
		renderPage("__standalone__");
		expect(h.workspace?.attachLater).toBe(true);
		expect(h.workspace?.onStageAttachments).toBeUndefined();
		expect(h.get).not.toHaveBeenCalled();
	});

	it("does not offer files until the prepared folder answers", async () => {
		h.preparedSessionId = "standalone-7";
		h.get.mockResolvedValue({ error: { code: "SESSION_WORKSPACE_NOT_FOUND" } });
		renderPage("__standalone__");
		await waitFor(() => expect(h.get).toHaveBeenCalled());
		expect(h.workspace?.attachLater).toBe(true);
		expect(h.workspace?.onStageAttachments).toBeUndefined();
	});

	it("creates the session on first send and hands its brief to the session chat", async () => {
		let resolve!: (sessionId: string) => void;
		h.launch.mockReturnValue(new Promise<string>((done) => { resolve = done; }));
		const queryClient = renderPage();
		expect(h.workspace?.draftSettings).toBeTruthy();

		let sent!: Promise<unknown>;
		act(() => { sent = Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")); });
		// While the session is created the brief is already the user's turn; only the
		// project picker leaves, and the agent, model, and effort hold still.
		expect(h.workspace?.localEchos).toEqual([expect.objectContaining({ clientMessageId: "cid-1", text: "Fix the build" })]);
		expect(h.workspace?.draftSettings).toEqual(expect.objectContaining({ props: expect.objectContaining({
			harness: "codex",
			settings: { model: "gpt-6", reasoningEffort: "high", approvalMode: "auto" },
		}) }));
		expect(h.workspace?.openingStartedAt).toEqual(expect.any(String));
		await waitFor(() => expect(h.launch).toHaveBeenCalledWith("Fix the build", {
			clientRequestId: "cid-1",
			replay: { agent: "codex", model: "gpt-6", effort: "high", approvalMode: undefined, mode: undefined },
		}));

		await act(async () => {
			resolve("sess-1");
			await sent;
		});
		expect(queryClient.getQueryData(["session-launch-settings", "sess-1"])).toEqual(expect.objectContaining({
			harness: "codex",
			settings: { model: "gpt-6", reasoningEffort: "high", approvalMode: "auto" },
		}));
		expect(queryClient.getQueryData(["session-opening-started", "sess-1"])).toEqual(expect.any(String));
		expect(queryClient.getQueryData(["conversation-local-echos"])).toEqual({
			"sess-1": [expect.objectContaining({ clientMessageId: "cid-1", text: "Fix the build", opening: true })],
		});
		await waitFor(() => expect(h.navigate).toHaveBeenCalledWith({
			to: "/projects/$projectId/sessions/$sessionId",
			params: { projectId: "proj-1", sessionId: "sess-1" },
			replace: true,
		}));
	});

	it("returns a refused brief to the composer and restores the selectors", async () => {
		h.launch.mockRejectedValue(Object.assign(new Error("Agent is not installed"), { name: "TaskCreateError", code: "AGENT_NOT_INSTALLED" }));
		renderPage();
		let outcome: unknown;
		await act(async () => {
			outcome = await Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")).catch((error: unknown) => error);
		});
		expect(outcome).toMatchObject({ message: "Agent is not installed", code: "TASK_CREATE_FAILED" });
		expect(h.workspace?.localEchos).toBeUndefined();
		expect(h.workspace?.draftSettings).toBeTruthy();
		expect(h.navigate).not.toHaveBeenCalled();
	});

	it("leaves a lost response uncertain so the composer retries with the same request id", async () => {
		h.launch.mockRejectedValue(new TypeError("Failed to fetch"));
		renderPage();
		let outcome: unknown;
		await act(async () => {
			outcome = await Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")).catch((error: unknown) => error);
		});
		expect(outcome).toBeInstanceOf(TypeError);
		expect(outcome).not.toHaveProperty("code");
		expect(h.navigate).not.toHaveBeenCalled();
	});

	it("becomes a fresh new chat when it moved to another project during the hand-off", async () => {
		let resolve!: (sessionId: string) => void;
		h.launch.mockReturnValue(new Promise<string>((done) => { resolve = done; }));
		const page = renderPage();
		let sent!: Promise<unknown>;
		act(() => { sent = Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")); });
		await waitFor(() => expect(h.launch).toHaveBeenCalled());
		// The session exists; before the page hands over, a new task opens for another project.
		await act(async () => {
			resolve("sess-1");
			page.showProject("proj-2");
			await sent;
		});
		await waitFor(() => expect(h.workspace?.localEchos).toBeUndefined());
		expect(h.navigate).not.toHaveBeenCalledWith(expect.objectContaining({ params: expect.objectContaining({ sessionId: "sess-1" }) }));
		expect(h.workspace?.busy).toBe(false);
	});

	it("starts the fallback once when its button is clicked twice", async () => {
		h.fallbackAction = "tui";
		h.launchFallback.mockReturnValue(new Promise<string>(() => undefined));
		writeChatComposerText(draftScope, "Fix the build");
		renderPage();
		const button = screen.getByRole("button", { name: "Create as Terminal UI" });
		act(() => {
			fireEvent.click(button);
			fireEvent.click(button);
		});
		expect(h.launchFallback).toHaveBeenCalledTimes(1);
	});

	it("keeps a refused retry on its project and shows why", async () => {
		prepareChatComposerDelivery(draftScope, { kind: "send", clientMessageId: "cid-1", requestText: "Fix the build", composerText: "Fix the build", attachments: [] });
		h.launch.mockRejectedValueOnce(new TypeError("Failed to fetch"));
		renderPage();
		await act(async () => {
			await Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")).catch(() => undefined);
		});
		h.launch.mockRejectedValueOnce(Object.assign(new Error("Claude Code needs you to sign in again."), { name: "TaskCreateError", code: "AGENT_AUTH_REQUIRED" }));
		let outcome: unknown;
		await act(async () => {
			outcome = await Promise.resolve(h.workspace?.onSend?.("Fix the build", undefined, "cid-1")).catch((error: unknown) => error);
		});
		// The composer still holds the first attempt, whose outcome is unknown.
		expect(outcome).toMatchObject({ code: "AGENT_AUTH_REQUIRED" });
		expect(JSON.parse(window.localStorage.getItem("ao.newTask.pendingLaunch.v1") ?? "{}").local).toMatchObject({ clientMessageId: "cid-1", projectId: "proj-1" });
		expect(screen.getByRole("alert")).toHaveTextContent("Claude Code needs you to sign in again.");
	});
});
