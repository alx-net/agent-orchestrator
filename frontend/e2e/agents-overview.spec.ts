import { mkdirSync } from "node:fs";
import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// The global Agents page: every session grouped by project, the workers an
// orchestrator spawned nested under it, and each session's subagents under the
// session, each row with its now line and latest updates. A turn that ended
// says how it ended: a worker that finished on a BLOCKED line needs you.

const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();
const startedAt = at(240);

let sequence = 1;
function message(sessionId: string, turnId: string, text: string, secondsAgo: number) {
	return {
		kind: "message", id: `${sessionId}-voice-${sequence}`, turnId, sequence: sequence++, revision: 1, role: "assistant", origin: "provider",
		text, streaming: false, createdAt: at(secondsAgo),
	};
}
function activity(turnId: string, fields: Record<string, unknown>) {
	return { kind: "activity", turnId, sequence: sequence++, revision: 1, createdAt: at(200), ...fields };
}

type Turn = { id: string; state: string; requestedAt: string; startedAt?: string; completedAt?: string };

function snapshot(sessionId: string, turns: Turn[], messages: unknown[], activities: unknown[] = []) {
	const running = turns.some((turn) => turn.state === "running");
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
		messages: [
			{ kind: "message", id: `${sessionId}-user`, turnId: turns[0]!.id, sequence: 0, revision: 0, role: "user", origin: "human", text: "Go", streaming: false, createdAt: startedAt },
			...messages,
		],
		activities,
		capabilities: ["steer", "subagents"],
		settings: {},
	};
}

const live = (id: string): Turn => ({ id, state: "running", requestedAt: startedAt, startedAt });
const ended = (id: string): Turn => ({ id, state: "completed", requestedAt: startedAt, startedAt, completedAt: at(60) });

const conversations: Record<string, () => unknown> = {
	"orch-pausing": () => snapshot("orch-pausing", [live("orch-turn")], [
		message("orch-pausing", "orch-turn", [
			"Status: Reading the pausing issue",
			"Decision: Split the work into an API and a UI worker, since they touch different packages",
			"Status: Spawned both workers",
			"Status: Waiting on the API and UI workers",
		].join("\n"), 40),
	]),
	"worker-api": () => snapshot("worker-api", [live("api-turn")], [
		message("worker-api", "api-turn", "Status: Writing the pause handler\nDecision: Reuse the session lock instead of adding a second one", 20),
	]),
	// Finished on the person's own BLOCKED convention, after its last status line.
	"worker-ui": () => snapshot("worker-ui", [ended("ui-turn")], [
		message("worker-ui", "ui-turn", "Status: Looking up the provider settings\n", 120),
		message("worker-ui", "ui-turn", [
			"I checked every provider.",
			"BLOCKED on you turning on 2FA in Verda, Resend and Hetzner; recommendation: use the shared authenticator; default if no answer: skip Verda",
		].join("\n"), 70),
	]),
	// A standalone worker running two subagents: one still working, one done with an Outcome: line.
	"worker-secrets": () => snapshot("worker-secrets", [live("secrets-turn")], [
		message("worker-secrets", "secrets-turn", "Status: Scanning the deploy configs for secrets", 30),
	], [
		activity("secrets-turn", {
			id: "agent-scan", providerItemId: "agent-scan", activityKind: "mcp_tool", status: "running", summary: "Scan for hardcoded keys",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "Explore",
				input: { description: "Scan for hardcoded keys", prompt: "Find hardcoded API keys in deploy/.", subagent_type: "Explore" },
			},
		}),
		activity("secrets-turn", {
			id: "scan-voice", providerItemId: "scan-voice", activityKind: "mcp_tool", status: "running", summary: "Subagent response",
			detail: {
				nestedAgent: true, parentProviderItemId: "agent-scan",
				progress: "Status: Reading deploy/fly.toml\nDecision: Skip the vendored charts; they hold no keys\nStatus: Reading deploy/hetzner.tf",
			},
		}),
		activity("secrets-turn", {
			id: "agent-inventory", providerItemId: "agent-inventory", activityKind: "mcp_tool", status: "completed", summary: "Inventory the services",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "general-purpose",
				input: { description: "Inventory the services", prompt: "List every service that reads a secret.", subagent_type: "general-purpose" },
				content: [{ type: "content", content: { type: "text", text: "Listed the services.\nOutcome: done — Found 6 secrets across 3 services" } }],
			},
		}),
	]),
};

/** What the person did from the Agents page, as the fake daemon reflects it back. */
interface PageActions {
	requests: { path: string; body: unknown }[];
	steers: Map<string, { text: string; clientMessageId: string }[]>;
	replies: Map<string, { text: string; clientMessageId: string }>;
	interrupted: Set<string>;
	inbox: { id: string; text: string; state: "queued" | "delivered" | "dropped"; at: number }[];
	scanStopped: boolean;
}
const untouched = (): PageActions => ({ requests: [], steers: new Map(), replies: new Map(), interrupted: new Set(), inbox: [], scanStopped: false });

type Snapshot = ReturnType<typeof snapshot>;
type Row = Record<string, unknown> & { turnId: string; detail?: Record<string, unknown> };

/** A conversation with the person's steers, reply, stops and subagent messages in it. */
function reflect(sessionId: string, base: Snapshot, actions: PageActions): Snapshot {
	const turns = base.turns as Turn[];
	const messages = base.messages as Row[];
	const activities = base.activities as Row[];
	const running = turns.find((turn) => turn.state === "running");
	(actions.steers.get(sessionId) ?? []).forEach((steer, index) => {
		if (!running) return;
		// The steer lands in the running turn, and the agent says what it does with it.
		activities.push(activity(running.id, {
			id: `${sessionId}-steer-${index}`, activityKind: "system", status: "completed", summary: steer.text, createdAt: at(2),
			detail: { event: "steer", origin: "human", text: steer.text, clientMessageId: steer.clientMessageId },
		}) as Row);
		messages.push(message(sessionId, running.id, "Status: Keeping the v1 pause route working", 1) as Row);
	});
	const reply = actions.replies.get(sessionId);
	if (reply) {
		const turnId = `${sessionId}-reply`;
		turns.push({ id: turnId, state: "running", requestedAt: at(3), startedAt: at(3) });
		messages.push({
			kind: "message", id: `${turnId}-user`, turnId, sequence: sequence++, revision: 0, role: "user", origin: "human",
			text: reply.text, clientMessageId: reply.clientMessageId, streaming: false, createdAt: at(3),
		});
		messages.push(message(sessionId, turnId, "Status: Turning on 2FA with the shared authenticator", 2) as Row);
	}
	if (actions.interrupted.has(sessionId)) {
		for (const turn of turns) if (turn.state === "running") Object.assign(turn, { state: "interrupted", completedAt: at(0) });
	}
	for (const row of activities) {
		if (row.id !== "agent-scan" || !row.detail) continue;
		row.detail = { ...row.detail, subagentInbox: actions.inbox };
		if (actions.scanStopped) row.status = "failed";
	}
	const live = turns.some((turn) => turn.state === "running");
	return { ...base, turns, messages, activities, controller: live ? "busy" : "ready", latestSequence: sequence };
}

async function installOverview(page: Page, actions: PageActions = untouched()) {
	await installFakeAgent(page, {
		workers: [
			{ id: "orch-pausing", title: "Ship session pausing", kind: "orchestrator", mode: "chat", provider: "claude-code", createdAt: at(600) },
			{ id: "worker-api", title: "Pause API", parentSessionId: "orch-pausing", mode: "chat", provider: "claude-code", createdAt: at(500) },
			{ id: "worker-ui", title: "Pause UI", parentSessionId: "orch-pausing", mode: "chat", provider: "claude-code", activity: "idle", status: "idle", createdAt: at(400) },
			{ id: "worker-old", title: "Old pause spike", parentSessionId: "orch-pausing", mode: "chat", provider: "claude-code", status: "terminated", activity: "exited", createdAt: at(900) },
			{ id: "worker-secrets", title: "Rotate deploy secrets", mode: "chat", provider: "claude-code", createdAt: at(300) },
		],
	});
	await page.route("**/api/v1/sessions/**", async (route) => {
		const path = new URL(route.request().url()).pathname;
		const sessionId = path.split("/")[4] ?? "";
		const method = route.request().method();
		if (method === "POST" && /\/conversation\/(steer|messages|interrupt|subagents\/.+)$/.test(path)) {
			const body = route.request().postDataJSON() as { text?: string; clientMessageId?: string } | null;
			actions.requests.push({ path, body });
			if (path.endsWith("/conversation/steer")) {
				actions.steers.set(sessionId, [...(actions.steers.get(sessionId) ?? []), { text: String(body?.text), clientMessageId: String(body?.clientMessageId) }]);
				return route.fulfill({ status: 202, json: { providerTurnId: `provider-${sessionId}` } });
			}
			if (path.endsWith("/conversation/messages")) {
				actions.replies.set(sessionId, { text: String(body?.text), clientMessageId: String(body?.clientMessageId) });
				return route.fulfill({ status: 202, json: { turnId: `${sessionId}-reply`, state: "running" } });
			}
			if (path.endsWith("/conversation/interrupt")) {
				actions.interrupted.add(sessionId);
				return route.fulfill({ status: 202, json: { ok: true } });
			}
			if (path.endsWith("/subagents/agent-scan/messages")) {
				const id = `inbox-${actions.inbox.length + 1}`;
				actions.inbox.push({ id, text: String(body?.text), state: "queued", at: Date.now() });
				return route.fulfill({ status: 202, json: { id, state: "queued" } });
			}
			if (path.endsWith("/subagents/agent-scan/stop")) {
				// It finished before reading what was still waiting for it.
				actions.scanStopped = true;
				for (const entry of actions.inbox) if (entry.state === "queued") entry.state = "dropped";
				return route.fulfill({ status: 202, json: { ok: true } });
			}
			return route.fulfill({ status: 409, json: { error: "conflict", code: "CHAT_SUBAGENT_NOT_RUNNING", message: "subagent is not running" } });
		}
		if (path.endsWith("/conversation") && method === "GET" && conversations[sessionId]) {
			return route.fulfill({ json: reflect(sessionId, conversations[sessionId]() as Snapshot, actions) });
		}
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
}

async function capture(page: Page, name: string) {
	const directory = process.env.AO_OVERVIEW_EVIDENCE_DIR;
	if (!directory) return;
	mkdirSync(directory, { recursive: true });
	await page.screenshot({ path: `${directory}/${name}.png` });
}

test("the Agents page shows every session's agents as one hierarchy, with how each turn ended @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await installOverview(page);
	await page.goto("/#/");

	// The sidebar entry counts the sessions whose agent is working.
	const entry = page.getByRole("button", { name: "Agents", exact: true });
	await expect(entry).toBeVisible();
	await expect(entry).toContainText("4");
	await entry.click();
	await expect(page).toHaveURL(/#\/agents$/);

	const overview = page.getByTestId("agents-overview");
	const project = overview.getByRole("region", { name: "fake-proj" });
	await expect(project).toBeVisible();
	const row = (title: string) => overview.getByTestId("agents-overview-session").filter({ has: page.getByRole("button", { name: `Open ${title}`, exact: true }) });
	const branch = (id: string) => overview.locator(`[data-testid='agents-overview-branch'][data-session-id='${id}']`);

	// The orchestrator holds both live workers; the blocked one sorts first and needs you.
	await expect(row("Ship session pausing")).toHaveAttribute("data-state", "working");
	await expect(row("Ship session pausing").getByTestId("agents-overview-now")).toHaveText("Waiting on the API and UI workers");
	await expect(row("Ship session pausing")).toContainText("Split the work into an API and a UI worker");
	const workers = branch("orch-pausing").locator("[data-testid='agents-overview-branch']");
	await expect(workers).toHaveCount(2);
	await expect(workers.nth(0)).toHaveAttribute("data-session-id", "worker-ui");
	await expect(workers.nth(1)).toHaveAttribute("data-session-id", "worker-api");
	await expect(row("Pause UI")).toHaveAttribute("data-state", "needs_you");
	await expect(row("Pause UI").getByTestId("agents-overview-now")).toHaveText("Blocked on you turning on 2FA in Verda, Resend and Hetzner");
	await expect(row("Pause API").getByTestId("agents-overview-now")).toHaveText("Writing the pause handler");

	// The standalone worker's two subagents sit under it.
	const subagents = branch("worker-secrets").getByTestId("agents-overview-subagent");
	await expect(subagents).toHaveCount(2);
	await expect(subagents.nth(0)).toHaveAttribute("data-outcome", "working");
	await expect(subagents.nth(0)).toContainText("Scan for hardcoded keys");
	await expect(subagents.nth(0).getByTestId("agents-overview-now")).toHaveText("Reading deploy/hetzner.tf");
	await expect(subagents.nth(1)).toHaveAttribute("data-outcome", "completed");
	await expect(subagents.nth(1).getByTestId("agents-overview-now")).toHaveText("Done Found 6 secrets across 3 services");

	// The project's terminal orchestrator has no conversation to read.
	await expect(row("Project orchestrator")).toHaveAttribute("data-detail", "tui");
	await expect(row("Project orchestrator")).toContainText("Subagent detail needs chat mode");
	await expect(overview.getByTestId("agents-overview-summary")).toContainText("1 needs you");
	await capture(page, "agents-overview-1-hierarchy");

	// Opening a row's updates shows its full timeline; a subagent's opens in place.
	await row("Ship session pausing").getByRole("button", { name: "Updates for Ship session pausing" }).click();
	await expect(branch("orch-pausing").getByRole("region", { name: "Updates" }).first()).toContainText("Reading the pausing issue");
	await subagents.nth(0).click();
	await expect(branch("worker-secrets").getByRole("region", { name: "Updates" })).toContainText("Skip the vendored charts; they hold no keys");
	// The open rows' chevrons point down once their rotation settles.
	for (const disclosure of [
		row("Ship session pausing").getByRole("button", { name: "Updates for Ship session pausing" }),
		subagents.nth(0).getByRole("button", { name: "Updates for Scan for hardcoded keys" }),
	]) {
		await expect(disclosure).toHaveAttribute("aria-expanded", "true");
		await expect.poll(() => disclosure.locator("svg").evaluate((element) => getComputedStyle(element).rotate)).toBe("90deg");
	}
	await capture(page, "agents-overview-2-expanded");

	// Finished sessions wait behind one collapsed group.
	const finished = project.getByRole("button", { name: /Finished/ });
	await expect(finished).toHaveAttribute("aria-expanded", "false");
	await expect(row("Old pause spike")).toHaveCount(0);
	await finished.click();
	await expect(row("Old pause spike")).toHaveAttribute("data-state", "stopped");
	await expect.poll(() => finished.locator("svg").evaluate((element) => getComputedStyle(element).rotate)).toBe("90deg");
	await capture(page, "agents-overview-3-finished");

	// A session row opens that session, whose own Agents tab names the same outcome.
	await row("Pause UI").getByTestId("agents-overview-now").click();
	await expect(page).toHaveURL(/#\/projects\/fake-proj\/sessions\/worker-ui$/);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
	await page.getByRole("tab", { name: /Agents/ }).click();
	const main = page.getByTestId("subagent-board").getByRole("article", { name: "Main agent" });
	await expect(main).toHaveAttribute("data-state", "needs_you");
	await expect(main.getByTestId("main-agent-now")).toHaveText("Blocked on you turning on 2FA in Verda, Resend and Hetzner");
	await capture(page, "agents-overview-4-session-board");
});

async function openOverview(page: Page, actions: PageActions) {
	await page.setViewportSize({ width: 1440, height: 1000 });
	await installOverview(page, actions);
	await page.goto("/#/agents");
	const overview = page.getByTestId("agents-overview");
	await expect(overview.getByRole("region", { name: "fake-proj" })).toBeVisible();
	const row = (title: string) => overview.getByTestId("agents-overview-session").filter({ has: page.getByRole("button", { name: `Open ${title}`, exact: true }) });
	const branch = (id: string) => overview.locator(`[data-testid='agents-overview-branch'][data-session-id='${id}']`);
	return { overview, row, branch };
}

test("a running session is steered, one that needs you is answered, and a subagent is messaged, from the Agents page @T0", async ({ page }) => {
	const actions = untouched();
	const { row, branch } = await openOverview(page, actions);
	const paths = () => actions.requests.map((request) => request.path);
	const route = (sessionId: string, suffix: string) => `/api/v1/sessions/${sessionId}/conversation/${suffix}`;

	// Steering a running session goes into its turn, never into a queue behind it.
	const api = row("Pause API");
	await expect(api).toHaveAttribute("data-state", "working");
	const apiHeight = (await api.boundingBox())!.height;
	await api.hover();
	await api.getByRole("button", { name: "Steer main agent" }).click();
	const steer = branch("worker-api").getByRole("textbox", { name: "Steer main agent" });
	await expect(steer).toBeFocused();
	await steer.fill("Keep the v1 pause route working");
	await steer.press("Enter");
	await expect.poll(paths).toEqual([route("worker-api", "steer")]);
	expect(actions.requests[0]!.body).toMatchObject({ text: "Keep the v1 pause route working", clientMessageId: expect.any(String) });
	await expect(page).toHaveURL(/#\/agents$/);
	// The echo carries the same id, so the message shows once, newest first, with the agent's answer to it.
	const steered = api.getByTestId("sent-message");
	await expect(steered).toHaveCount(1);
	await expect(steered).toContainText("You:Keep the v1 pause route working");
	await expect(api.getByTestId("agents-overview-now")).toHaveText("Keeping the v1 pause route working");
	// What it says changed; its height did not.
	expect((await api.boundingBox())!.height).toBe(apiHeight);
	await steer.press("Escape");
	await expect(steer).toHaveCount(0);

	// A turn that ended waiting on the person is answered with a new message.
	const ui = row("Pause UI");
	await expect(ui).toHaveAttribute("data-state", "needs_you");
	await ui.hover();
	await expect(ui.getByRole("button", { name: "Stop turn" })).toHaveCount(0);
	await ui.getByRole("button", { name: "Message main agent" }).click();
	const reply = branch("worker-ui").getByRole("textbox", { name: "Message main agent" });
	await reply.fill("Use the shared authenticator");
	await reply.press("Enter");
	await expect.poll(paths).toEqual([route("worker-api", "steer"), route("worker-ui", "messages")]);
	expect(actions.requests[1]!.body).toMatchObject({ text: "Use the shared authenticator", clientMessageId: expect.any(String) });
	// Its answer starts the next turn, which the row now steers.
	await expect(ui).toHaveAttribute("data-state", "working");
	await expect(ui.getByTestId("agents-overview-now")).toHaveText("Turning on 2FA with the shared authenticator");
	await expect(branch("worker-ui").getByRole("textbox", { name: "Steer main agent" })).toBeVisible();

	// A running subagent gets a message of its own; it waits for its next step.
	const scan = branch("worker-secrets").getByTestId("agents-overview-subagent").filter({ hasText: "Scan for hardcoded keys" });
	await scan.hover();
	await scan.getByRole("button", { name: "Message subagent" }).click();
	const toScan = branch("worker-secrets").getByRole("textbox", { name: "Message subagent" });
	await toScan.fill("Check the Terraform state too");
	await toScan.press("Enter");
	await expect.poll(paths).toContain(route("worker-secrets", "subagents/agent-scan/messages"));
	expect(actions.requests.at(-1)!.body).toEqual({ text: "Check the Terraform state too" });
	await expect(scan.getByTestId("sent-message")).toContainText("waiting for its next step");
	await capture(page, "agents-overview-5-steer");

	// Stopping that subagent stops it alone; stopping a turn interrupts that session only.
	await scan.getByRole("button", { name: "Stop subagent" }).click();
	await expect.poll(paths).toContain(route("worker-secrets", "subagents/agent-scan/stop"));
	await expect(scan).toHaveAttribute("data-outcome", "stopped");
	await expect(scan.getByTestId("sent-message")).toContainText("not delivered — it finished first");
	await expect(row("Rotate deploy secrets")).toHaveAttribute("data-state", "working");
	const orchestrator = row("Ship session pausing");
	await orchestrator.hover();
	await orchestrator.getByRole("button", { name: "Stop turn" }).click();
	await expect.poll(paths).toContain(route("orch-pausing", "interrupt"));
	await expect(orchestrator).toHaveAttribute("data-state", "stopped");
	expect(paths().filter((path) => path.endsWith("/interrupt"))).toEqual([route("orch-pausing", "interrupt")]);
});

test("folded rows keep one height while their agents stream, and an open one scrolls newest first in place @T0", async ({ page }) => {
	const actions = untouched();
	const { overview, row, branch } = await openOverview(page, actions);
	await expect(row("Pause API").getByTestId("agents-overview-now")).toHaveText("Writing the pause handler");

	// Every folded session row is one height, however much or little it says.
	const heights = async (locator: ReturnType<Page["getByTestId"]>) => {
		const boxes = await locator.evaluateAll((rows) => rows.map((element) => element.getBoundingClientRect().height));
		return [...new Set(boxes.map((height) => Math.round(height)))];
	};
	const sessions = overview.getByTestId("agents-overview-session");
	expect(await heights(sessions)).toHaveLength(1);
	expect(await heights(overview.getByTestId("agents-overview-subagent"))).toHaveLength(1);
	const sessionHeight = (await heights(sessions))[0]!;

	// A new line streams into a row: it appears on top, and nothing moves.
	await row("Pause API").hover();
	await row("Pause API").getByRole("button", { name: "Steer main agent" }).click();
	await branch("worker-api").getByRole("textbox", { name: "Steer main agent" }).fill("Keep the v1 pause route working");
	await branch("worker-api").getByRole("textbox", { name: "Steer main agent" }).press("Enter");
	await expect(row("Pause API").getByTestId("agents-overview-now")).toHaveText("Keeping the v1 pause route working");
	await branch("worker-api").getByRole("textbox", { name: "Steer main agent" }).press("Escape");
	expect(await heights(sessions)).toEqual([sessionHeight]);

	// Opened, its updates scroll inside one height, the newest on top.
	await row("Ship session pausing").getByRole("button", { name: "Updates for Ship session pausing" }).click();
	const log = branch("orch-pausing").getByRole("region", { name: "Updates" }).first().getByRole("list");
	await expect(log.getByRole("listitem").first()).toContainText("Waiting on the API and UI workers");
	await expect(log.getByRole("listitem").last()).toContainText("Go");
	// 20rem, however few lines it holds.
	const [height, twentyRem] = await log.evaluate((element) => [
		element.getBoundingClientRect().height,
		Number.parseFloat(getComputedStyle(document.documentElement).fontSize) * 20,
	]);
	expect(Math.round(height!)).toBe(Math.round(twentyRem!));
	await capture(page, "agents-overview-6-steady");
});
