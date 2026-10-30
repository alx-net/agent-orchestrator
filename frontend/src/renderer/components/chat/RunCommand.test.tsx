import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useUiStore } from "../../stores/ui-store";
import { ChatMarkdown } from "./ChatMarkdown";
import { isRunnableShellLanguage, RunCommandProvider, RunnableCode } from "./RunCommand";

const mocks = vi.hoisted(() => ({ post: vi.fn() }));
vi.mock("../../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../../lib/api-client")>(),
	apiClient: { POST: mocks.post },
}));
vi.mock("../../stores/terminal-shell-store", () => ({
	useTerminalShellStore: { getState: () => ({ load: async () => {}, preference: "auto" }) },
	terminalShellRequestValue: () => undefined,
}));

const COMMAND = "cd ~/conductor/repos/kugelaudio && git fetch origin\nOP_ACCOUNT=kugelaudio.1password.eu op run --no-masking --env-file=infrastructure/hetzner-agents/.env.op -- infrastructure/hetzner-agents/scripts/plan";

function renderReply(text: string, { sessionId = "kugelaudio-2", agentReply = true } = {}) {
	const markdown = <ChatMarkdown text={text} />;
	return render(
		<QueryClientProvider client={new QueryClient()}>
			<RunCommandProvider sessionId={sessionId || undefined}>
				{agentReply ? <RunnableCode>{markdown}</RunnableCode> : markdown}
			</RunCommandProvider>
		</QueryClientProvider>,
	);
}

beforeEach(() => {
	mocks.post.mockReset();
	useUiStore.setState({ activeShellTerminalHandleId: null });
});

describe("Run on an agent's shell block", () => {
	it("confirms the whole command, then opens it in a terminal tab", async () => {
		mocks.post.mockResolvedValue({
			data: { shellTerminal: { handleId: "shellterm-run1", sessionId: "kugelaudio-2", workingDir: "/wt", title: "Run: cd ~/conductor/repos/kugelaudio", createdAt: "2026-10-27T20:30:00Z" } },
		});
		renderReply(`Run this:\n\n\`\`\`zsh\n${COMMAND}\n\`\`\``);

		await userEvent.click(screen.getByRole("button", { name: "Run this command in a terminal" }));
		// The block scrolls sideways; the dialog must show every character.
		expect(await screen.findByTestId("run-command-preview")).toHaveTextContent("infrastructure/hetzner-agents/scripts/plan");
		expect(mocks.post).not.toHaveBeenCalled();

		await userEvent.click(screen.getByRole("button", { name: "Run" }));
		await waitFor(() => expect(useUiStore.getState().activeShellTerminalHandleId).toBe("shellterm-run1"));
		expect(mocks.post).toHaveBeenCalledWith("/api/v1/sessions/{sessionId}/terminal-commands", {
			params: { path: { sessionId: "kugelaudio-2" } },
			body: { command: COMMAND, shell: undefined },
		});
	});

	it("does not run anything when the user cancels", async () => {
		renderReply("```bash\nterraform apply\n```");
		await userEvent.click(screen.getByRole("button", { name: "Run this command in a terminal" }));
		await userEvent.click(await screen.findByRole("button", { name: "Cancel" }));
		expect(mocks.post).not.toHaveBeenCalled();
	});

	it("offers Run only on shell blocks in a live local agent reply", () => {
		const { unmount } = renderReply("```text\nPlan: 1 to add\n```\n\n```console\n$ ls\n```");
		expect(screen.queryByRole("button", { name: "Run this command in a terminal" })).toBeNull();
		unmount();

		renderReply("```bash\nls\n```", { agentReply: false });
		expect(screen.queryByRole("button", { name: "Run this command in a terminal" })).toBeNull();
	});

	it("hides Run where no local session can run it", () => {
		renderReply("```bash\nls\n```", { sessionId: "" });
		expect(screen.queryByRole("button", { name: "Run this command in a terminal" })).toBeNull();
	});
});

describe("isRunnableShellLanguage", () => {
	it("accepts shell fences and rejects transcripts and other languages", () => {
		expect(["bash", "sh", "zsh", "shell", "Shell"].every(isRunnableShellLanguage)).toBe(true);
		expect(["console", "text", "python", "", undefined].some(isRunnableShellLanguage)).toBe(false);
	});
});
