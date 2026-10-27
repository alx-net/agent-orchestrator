import { mkdirSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// A session's decisions survive the person's reply, and the sessions board shows
// them. Turn 1 wrote six Status lines and a Decision, then asked a question; the
// person answered, and turn 2 is under way. The Agents tab keeps turn 1's lines
// under a divider naming the message that started each turn, and the board card
// shows the main agent's now line and its latest decision, from turn 1.

const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();

let sequence = 1;
function message(sessionId: string, turnId: string, role: "user" | "assistant", text: string, secondsAgo: number) {
	return {
		kind: "message", id: `${sessionId}-${sequence}`, turnId, sequence: sequence++, revision: 1, role, origin: role === "user" ? "human" : "provider",
		text, streaming: false, createdAt: at(secondsAgo),
	};
}

function snapshot(sessionId: string, turns: unknown[], messages: unknown[], activities: unknown[] = []) {
	const running = turns.some((turn) => (turn as { state: string }).state === "running");
	return {
		conversationId: `conversation-${sessionId}`,
		sessionId,
		harness: "claude-code",
		mode: "chat",
		controller: running ? "busy" : "ready",
		latestSequence: sequence,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns,
		messages,
		activities,
		capabilities: ["steer", "subagents"],
		settings: {},
	};
}

const conversations: Record<string, () => unknown> = {
	"chat-pause": () => snapshot("chat-pause", [
		{ id: "pause-1", state: "completed", requestedAt: at(600), startedAt: at(600), completedAt: at(300) },
		{ id: "pause-2", state: "running", requestedAt: at(240), startedAt: at(240) },
	], [
		message("chat-pause", "pause-1", "user", "Pause sessions from the board and keep their worktrees around while paused", 600),
		message("chat-pause", "pause-1", "assistant", [
			"Status: Reading the session lifecycle",
			"Status: Found the pause seam in the reaper",
			"Decision: Pause through the daemon's lifecycle service, since the CLI must not touch storage",
			"Status: Wrote the pause route",
			"Status: Wrote the client call",
			"Status: Wired the board button",
			"Status: Ran the backend tests",
			"Outcome: needs input — Should a paused session keep its port slot?",
		].join("\n"), 320),
		message("chat-pause", "pause-2", "user", "Yes, keep the slot", 240),
		message("chat-pause", "pause-2", "assistant", "Status: Keeping the port slot on pause\n", 200),
	], [
		{
			kind: "activity", turnId: "pause-2", sequence: sequence++, revision: 1, createdAt: at(150),
			id: "agent-slots", providerItemId: "agent-slots", activityKind: "mcp_tool", status: "running", summary: "Audit the slot allocator",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "Explore",
				input: { description: "Audit the slot allocator", prompt: "Find every place a port slot is released.", subagent_type: "Explore" },
			},
		},
	]),
	// Finished on the person's own BLOCKED convention: its card says so in the needs-you color.
	"chat-secrets": () => snapshot("chat-secrets", [
		{ id: "secrets-1", state: "completed", requestedAt: at(500), startedAt: at(500), completedAt: at(90) },
	], [
		message("chat-secrets", "secrets-1", "user", "Rotate the deploy secrets", 500),
		message("chat-secrets", "secrets-1", "assistant", "Status: Listing the providers\nBLOCKED on you turning on 2FA in Hetzner; default if no answer: skip it", 100),
	]),
};

async function installBoard(page: Page) {
	await installFakeAgent(page, {
		workers: [
			{ id: "chat-pause", title: "Pause sessions", mode: "chat", provider: "claude-code", createdAt: at(700) },
			{ id: "chat-secrets", title: "Rotate deploy secrets", mode: "chat", provider: "claude-code", activity: "idle", status: "idle", createdAt: at(650) },
			{ id: "tui-docs", title: "Update the docs", mode: "tui", provider: "codex", createdAt: at(640) },
		],
	});
	await page.route("**/api/v1/sessions/**", async (route) => {
		const path = new URL(route.request().url()).pathname;
		const sessionId = path.split("/")[4] ?? "";
		const method = route.request().method();
		if (path.endsWith("/conversation") && method === "GET" && conversations[sessionId]) return route.fulfill({ json: conversations[sessionId]() });
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
}

async function capture(page: Page, name: string) {
	const directory = process.env.AO_BOARD_EVIDENCE_DIR;
	if (!directory) return;
	mkdirSync(directory, { recursive: true });
	await page.screenshot({ path: `${directory}/${name}.png` });
}

test("a reply keeps the earlier turn's decisions, and the board card shows the now line and latest decision @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	await installBoard(page);
	await page.goto("/#/projects/fake-proj");

	const board = page.getByTestId("board");
	const card = (id: string) => board.locator(`[data-testid='board-session-card'][data-session-id='${id}']`);

	// The working session: the decision from turn 1, then what it does now, right above its status line.
	const pause = card("chat-pause");
	const context = pause.getByTestId("board-agent-context");
	const now = context.getByTestId("board-agent-now");
	const decision = context.getByTestId("board-agent-decision");
	await expect(now).toHaveText("Keeping the port slot on pause");
	await expect(decision).toHaveText("Decision Pause through the daemon's lifecycle service, since the CLI must not touch storage");
	const top = (locator: ReturnType<Page["locator"]>) => locator.evaluate((element) => element.getBoundingClientRect().top);
	expect(await top(decision)).toBeLessThan(await top(now));
	// Decisions read stronger than the now line; two lines at most, each.
	const weight = (locator: ReturnType<Page["locator"]>) => locator.evaluate((element) => Number(getComputedStyle(element).fontWeight));
	expect(await weight(decision)).toBeGreaterThan(await weight(now));
	for (const line of [now, decision]) {
		await expect.poll(() => line.evaluate((element) => getComputedStyle(element).webkitLineClamp)).toBe("2");
	}
	// The footer keeps its one status line; the running subagent is compact metadata beside the time.
	await expect(pause.getByTestId("session-status")).toHaveCount(1);
	await expect(pause.getByTestId("session-status")).toHaveText("Working");
	await expect(pause.getByTestId("session-card-meta")).toHaveText("1 subagent");
	await expect(pause.getByTestId("session-card-meta").locator("+ span")).toBeVisible();

	// The settled session says how it ended, in the needs-you color, with no subagent count or its separator.
	const secrets = card("chat-secrets");
	await expect(secrets.getByTestId("board-agent-now")).toHaveText("Blocked on you turning on 2FA in Hetzner");
	await expect(secrets.getByTestId("board-agent-now").getByTestId("agent-outcome")).toHaveClass(/text-status-needs-you/);
	await expect(secrets.getByTestId("session-card-meta")).toBeHidden();
	await expect(secrets.getByTestId("session-card-meta").locator("+ span")).toBeHidden();
	// A terminal session's card stays as it was.
	await expect(card("tui-docs")).toBeVisible();
	await expect(card("tui-docs").getByTestId("board-agent-context")).toHaveCount(0);
	await capture(page, "board-decisions-1-board");

	// The session's own Agents tab keeps turn 1's lines after the reply.
	await pause.click();
	await expect(page).toHaveURL(/#\/projects\/fake-proj\/sessions\/chat-pause$/);
	await page.getByRole("tab", { name: /Agents/ }).click();
	const main = page.getByTestId("subagent-board").getByRole("article", { name: "Main agent" });
	await expect(main.getByTestId("main-agent-now")).toHaveText("Keeping the port slot on pause");
	await expect(main.getByTestId("main-agent-decision")).toContainText("Pause through the daemon's lifecycle service");
	await main.getByRole("button", { name: /Main agent/ }).click();
	const updates = main.getByRole("region", { name: "Updates" });
	await expect(updates.getByTestId("turn-divider")).toHaveText([
		/You:\s*Pause sessions from the board and keep their worktrees around while paused/,
		/You:\s*Yes, keep the slot/,
	]);
	await expect(updates.locator("[data-note-kind='status']")).toHaveCount(7);
	await expect(updates.locator("[data-note-kind='decision']")).toContainText("Pause through the daemon's lifecycle service");
	await expect(updates.locator("[data-note-kind='outcome']")).toContainText("Should a paused session keep its port slot?");
	// The open log rests on its newest line.
	await expect(updates.locator("li").last()).toContainText("Keeping the port slot on pause");
	await expect(updates.locator("li").last()).toBeInViewport();
	await capture(page, "board-decisions-2-agents-tab");

	// The Agents page reads the same shared summaries.
	await page.getByRole("button", { name: "Agents", exact: true }).click();
	await expect(page).toHaveURL(/#\/agents$/);
	const overview = page.getByTestId("agents-overview");
	const row = overview.getByTestId("agents-overview-session").filter({ has: page.getByRole("button", { name: "Open Pause sessions", exact: true }) });
	await expect(row.getByTestId("agents-overview-now")).toHaveText("Keeping the port slot on pause");
	await row.getByRole("button", { name: "Updates for Pause sessions" }).click();
	await expect(overview.locator("[data-session-id='chat-pause']").getByRole("region", { name: "Updates" })).toContainText("Pause through the daemon's lifecycle service");
	await capture(page, "board-decisions-3-agents-page");
});
