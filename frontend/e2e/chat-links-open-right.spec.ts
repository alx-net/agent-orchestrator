import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// A chat on a private GitHub repo: AO's Browser panel is not signed in to
// GitHub, so PR links open in the system browser. Paths the agent names open
// in Files when the worktree has them; a path it wrote as code that the
// worktree lacks opens on the open PR's branch on GitHub; a bare path in prose
// that the worktree lacks stays plain.

const sessionId = "chat-links-open-right";
const pr = "https://github.com/Kugelaudio/KugelAudio/pull/5449";
const branch = "ao/kugelaudio-3/cloud-workspace-spec";
const existing = "docs/rollout-plan.md";
const plan = "products/platform/docs/22-workspaces.md";
const now = "2026-10-27T10:00:00Z";
const reply = [
	`Opened [#5449](${pr}).`,
	`The spec is \`${plan}\`; the rollout lives in ${existing}.`,
	"Nothing is in notes/missing/file.txt yet.",
].join(" ");

function quietEventStream(route: Parameters<Parameters<Page["route"]>[1]>[0]) {
	return route.fulfill({ status: 200, headers: { "content-type": "text/event-stream" }, body: "retry: 600000\n: connected\n\n" });
}

async function installSession(page: Page) {
	await installFakeAgent(page, {
		workers: [{
			id: sessionId, title: "Cloud workspace spec", mode: "chat", provider: "claude-code",
			prs: [{ url: pr, number: 5449, state: "open", ci: "passing", review: "none", mergeability: "mergeable", reviewComments: false, updatedAt: now }],
		}],
	});
	// Record what reaches the system browser and every Browser panel call.
	await page.addInitScript(() => {
		const calls = { external: [] as string[], panel: [] as string[] };
		(window as unknown as { __linkCalls: typeof calls }).__linkCalls = calls;
		const ao = (window as unknown as { ao: { app: Record<string, unknown>; browser?: Record<string, unknown> } }).ao;
		ao.app.openExternal = async (url: string) => { calls.external.push(url); };
		for (const [name, value] of Object.entries(ao.browser ?? {})) {
			if (typeof value !== "function" || name.startsWith("on")) continue;
			ao.browser![name] = (...args: unknown[]) => {
				calls.panel.push(`${name} ${JSON.stringify(args)}`);
				return (value as (...input: unknown[]) => unknown)(...args);
			};
		}
	});
	const fileRequests: string[] = [];
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		if (route.request().headers().accept?.includes("text/event-stream")) return quietEventStream(route);
		const url = new URL(route.request().url());
		const path = url.pathname.replace(`/api/v1/sessions/${sessionId}`, "");
		if (path === "/pr") {
			return route.fulfill({ json: { prs: [{
				url: pr, htmlUrl: pr, number: 5449, title: "Cloud workspace spec", state: "open", provider: "github", repo: "Kugelaudio/KugelAudio",
				author: "ada", sourceBranch: branch, targetBranch: "master", headSha: "abc", additions: 1, deletions: 0, changedFiles: 1,
				ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
				review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
				mergeability: { state: "mergeable", reasons: [], prUrl: pr, conflictFiles: [] },
				updatedAt: now, observedAt: now, ciObservedAt: now,
			}], linkedPrs: [] } });
		}
		if (path === "/conversation" && route.request().method() === "GET") {
			return route.fulfill({ json: {
				conversationId: "conversation-links", sessionId, harness: "claude-code", mode: "chat", controller: "ready",
				latestSequence: 2, oldestSequence: 1, hasMoreBefore: false,
				turns: [{ id: "turn-1", state: "completed", requestedAt: now, startedAt: now, completedAt: now }],
				messages: [
					{ id: "user-1", turnId: "turn-1", sequence: 1, revision: 0, role: "user", origin: "human", text: "Write the workspace spec", streaming: false, createdAt: now },
					{ id: "assistant-1", turnId: "turn-1", sequence: 2, revision: 0, role: "assistant", origin: "provider", text: reply, streaming: false, createdAt: now },
				],
				activities: [], settings: {},
			} });
		}
		if (path === "/conversation/models") return route.fulfill({ json: { models: [], selected: {} } });
		if (path === "/conversation/skills") return route.fulfill({ json: { skills: [] } });
		if (path === "/interface-transition") return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		const content = "# Rollout plan\n\n1. Ship the spec\n2. Migrate workspaces\n";
		const file = { path: existing, status: "unchanged", additions: 0, deletions: 0, size: content.length, binary: false, editable: true, fileFingerprint: "fp-1" };
		if (path === "/workspace/files") {
			return route.fulfill({ json: { files: [{ path: existing, status: "unchanged" }, { path: "README.md", status: "unchanged" }], truncated: false } });
		}
		if (path === "/workspace/manifest") {
			return route.fulfill({ json: { sessionId, workspaceVersion: "workspace-1", files: [file], sections: { committed: [], staged: [], unstaged: [], untracked: [] }, commits: [], summary: { additions: 0, deletions: 0, files: 0 }, truncated: false } });
		}
		if (path === "/workspace/history") return route.fulfill({ json: { sessionId, commits: [] } });
		if (path === "/workspace/file") {
			fileRequests.push(url.searchParams.get("path") ?? "");
			return route.fulfill({ json: {
				...file, sessionId, deleted: false, content, contentTruncated: false, diff: "", diffTruncated: false, workspaceVersion: "workspace-1",
			} });
		}
		return route.fallback();
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
	return { fileRequests };
}

const linkCalls = (page: Page) => page.evaluate(() => (window as unknown as { __linkCalls: { external: string[]; panel: string[] } }).__linkCalls);

test("GitHub links open in the system browser and named workspace files open in Files @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	const { fileRequests } = await installSession(page);
	const conversation = page.getByRole("log", { name: "Conversation" });

	// Nothing opened on the user's behalf when the reply arrived.
	expect((await linkCalls(page)).external).toEqual([]);

	await conversation.getByRole("link", { name: "#5449" }).click();
	const branchFile = conversation.getByRole("link", { name: plan });
	await expect(branchFile).toHaveAttribute("href", `https://github.com/Kugelaudio/KugelAudio/blob/${branch}/${plan}`);
	await branchFile.click();
	await expect.poll(async () => (await linkCalls(page)).external).toEqual([pr, `https://github.com/Kugelaudio/KugelAudio/blob/${branch}/${plan}`]);
	expect((await linkCalls(page)).panel.filter((call) => call.includes("github.com"))).toEqual([]);

	// A bare path the worktree lacks stays prose, open PR or not.
	await expect(conversation.getByText("notes/missing/file.txt", { exact: false })).toBeVisible();
	await expect(conversation.locator("button, a").filter({ hasText: "notes/missing/file.txt" })).toHaveCount(0);
	await page.screenshot({ path: test.info().outputPath("chat.png") });

	await conversation.getByRole("button", { name: `Open ${existing} in Files` }).click();
	await expect.poll(() => fileRequests).toContain(existing);
	// A Markdown file opens rendered.
	await expect(page.getByRole("tab", { name: "Rich preview" })).toHaveAttribute("aria-selected", "true");
	await expect(page.getByRole("heading", { name: "Rollout plan" })).toBeVisible();
	await page.screenshot({ path: test.info().outputPath("links.png") });
});
