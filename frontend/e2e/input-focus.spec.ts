import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// The person types while a Claude Code chat streams: a CDC event every 150 ms,
// each one a new conversation snapshot (new progress lines, new tool calls),
// the window losing and regaining focus, and an agent's `ao preview` arriving.
// Whatever field they are in keeps the caret and every keystroke.

const sessionId = "chat-input-focus";
const startedAt = new Date(Date.now() - 4 * 60_000).toISOString();

function snapshot(tick: number) {
	let sequence = 2;
	const activity = (fields: Record<string, unknown>) => ({
		kind: "activity", turnId: "turn-main", sequence: sequence++, revision: tick, createdAt: startedAt, ...fields,
	});
	const reads = Array.from({ length: Math.min(tick, 40) }, (_, index) => activity({
		id: `audit-read-${index}`, providerItemId: `audit-read-${index}`, activityKind: "mcp_tool", status: "completed",
		summary: `Read backend/internal/auth/file-${index}.go`,
		detail: { parentProviderItemId: "agent-audit", providerToolName: "Read", input: { file_path: `backend/internal/auth/file-${index}.go` } },
	}));
	const activities = [
		activity({
			id: "agent-audit", providerItemId: "agent-audit", activityKind: "mcp_tool", status: "running", summary: "Audit the auth flow",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "Explore",
				input: { description: "Audit the auth flow", prompt: "Read backend/internal/auth.", subagent_type: "Explore" },
				progressSummary: `Reading file ${tick}`, progressAt: Date.now(),
				subagentInbox: [],
			},
		}),
		activity({
			id: "audit-voice", providerItemId: "audit-voice", activityKind: "mcp_tool", status: "running", summary: "Subagent response",
			detail: { nestedAgent: true, parentProviderItemId: "agent-audit", progress: `Status: Reading file ${tick}` },
		}),
		...reads,
	];
	const mainVoice = {
		kind: "message", id: "main-voice", turnId: "turn-main", sequence: sequence++, revision: tick, role: "assistant", origin: "provider",
		text: `Working through the audit.\nStatus: Step ${tick} of the audit`,
		streaming: true, createdAt: startedAt,
	};
	return {
		conversationId: "conversation-input-focus",
		sessionId,
		harness: "claude-code",
		mode: "chat",
		controller: "busy",
		latestSequence: sequence,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns: [{ id: "turn-main", state: "running", providerTurnId: "provider-main", requestedAt: startedAt, startedAt }],
		messages: [
			{ kind: "message", id: "user-main", turnId: "turn-main", sequence: 1, revision: 0, role: "user", origin: "human", text: "Audit the auth flow", streaming: false, createdAt: startedAt },
			mainVoice,
		],
		activities,
		capabilities: ["steer", "subagents"],
		settings: {},
	};
}

async function installStreamingChat(page: Page) {
	const served = { count: 0 };
	await installFakeAgent(page, { workers: [{ id: sessionId, title: "Audit the auth flow", mode: "chat", provider: "claude-code", activity: "active" }] });
	// The daemon's chat triggers put the conversation id in the CDC payload; the
	// fake's own pressure frames carry none. Keep the fake's streams reachable so
	// the spec can send those frames itself.
	await page.addInitScript(() => {
		const Base = window.EventSource as unknown as new (url: string) => EventSource;
		const sources: EventSource[] = [];
		(window as unknown as { __aoEventSources: EventSource[] }).__aoEventSources = sources;
		(window as unknown as { EventSource: unknown }).EventSource = class extends Base {
			constructor(url: string) {
				super(url);
				sources.push(this);
			}
		};
	});
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		const method = route.request().method();
		if (path.endsWith("/conversation") && method === "GET") {
			served.count += 1;
			return route.fulfill({ json: snapshot(served.count) });
		}
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
	return served;
}

/** Types one character at a time while the app keeps updating around the field. */
async function typeWhileStreaming(page: Page, text: string, served: { count: number }, minimumRefreshes = 1) {
	const before = served.count;
	for (const [index, character] of [...text].entries()) {
		await page.keyboard.type(character);
		if (index % 6 === 5) {
			// The window loses focus to another surface (the browser panel, another
			// app) and gets it back, as it does many times while agents work.
			await page.evaluate(() => {
				window.dispatchEvent(new Event("blur"));
				window.dispatchEvent(new Event("focus"));
				document.dispatchEvent(new Event("visibilitychange"));
			});
		}
		await page.waitForTimeout(40);
	}
	// The conversation really was refreshed under the person's fingers.
	expect(served.count - before).toBeGreaterThanOrEqual(minimumRefreshes);
}

const focusedLabel = (page: Page) => page.evaluate(() => document.activeElement?.getAttribute("aria-label") ?? document.activeElement?.id ?? document.activeElement?.tagName);

test("the composer and the Agents tab fields keep the caret while the chat streams @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	const served = await installStreamingChat(page);
	// Every 150 ms: a session update for the sidebar and board, and a conversation
	// change for the open chat, as a working Claude Code session produces.
	await page.evaluate(() => window.__aoFakeAgent!.startContinuousWorkspaceEvents(150));
	await page.evaluate(({ id, conversationId }) => {
		let seq = 1_000_000;
		(window as unknown as { __aoConversationTimer: number }).__aoConversationTimer = window.setInterval(() => {
			const sources = (window as unknown as { __aoEventSources: Array<EventSource & { _dispatch: (type: string, data: string) => void }> }).__aoEventSources;
			const frame = JSON.stringify({ seq: (seq += 1), projectId: "fake-proj", sessionId: id, type: "session_updated", payload: { id, conversationId }, createdAt: new Date().toISOString() });
			for (const source of sources) if (source.url.includes("/api/v1/events") && source.readyState === 1) source._dispatch("session_updated", frame);
		}, 150);
	}, { id: sessionId, conversationId: "conversation-input-focus" });
	await expect.poll(() => served.count, { timeout: 10_000 }).toBeGreaterThan(5);

	const composer = page.getByRole("combobox", { name: "Message the agent" });
	await composer.click();
	const prompt = "Check the refresh token rotation as well";
	await typeWhileStreaming(page, prompt, served);
	expect(await focusedLabel(page)).toBe("Message the agent");
	await expect(composer).toHaveText(prompt);

	// An agent's `ao preview` lands while the person keeps typing.
	await page.evaluate((id) => window.__aoFakeAgent!.setPreview(id, "http://localhost:4173/", 1), sessionId);
	await typeWhileStreaming(page, " now", served, 0);
	expect(await focusedLabel(page)).toBe("Message the agent");
	await expect(composer).toHaveText(`${prompt} now`);

	// The Agents tab covers the timeline; the composer stays mounted under it.
	await page.getByRole("tab", { name: /Agents/ }).click();
	const board = page.getByTestId("subagent-board");
	const main = board.getByRole("article", { name: "Main agent" });
	await main.getByRole("button", { name: "Steer main agent" }).click();
	const steer = main.getByRole("textbox", { name: "Steer main agent" });
	await expect(steer).toBeFocused();
	const steerText = "Keep the v1 routes working";
	await typeWhileStreaming(page, steerText, served);
	await expect(steer).toBeFocused();
	await expect(steer).toHaveValue(steerText);

	const audit = board.getByRole("article", { name: "Audit the auth flow" });
	await audit.getByRole("button", { name: "Message subagent" }).click();
	const message = audit.getByRole("textbox", { name: "Message subagent" });
	await expect(message).toBeFocused();
	const messageText = "Also read the middleware";
	await typeWhileStreaming(page, messageText, served);
	await expect(message).toBeFocused();
	await expect(message).toHaveValue(messageText);

	await page.evaluate(() => {
		window.__aoFakeAgent!.stopContinuousWorkspaceEvents();
		window.clearInterval((window as unknown as { __aoConversationTimer: number }).__aoConversationTimer);
	});
	// Nothing typed into one field leaked into another.
	await expect(composer).toHaveText(`${prompt} now`);
	await expect(steer).toHaveValue(steerText);
});
