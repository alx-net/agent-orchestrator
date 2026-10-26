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

async function installOverview(page: Page) {
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
		if (path.endsWith("/conversation") && method === "GET" && conversations[sessionId]) return route.fulfill({ json: conversations[sessionId]() });
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
