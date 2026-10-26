import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// The Agents tab of a Claude Code chat: the main agent pinned on top with its
// own Status:/Decision: lines, then one card per Agent/Task call, placed in
// Working, Needs you or Done, with the subagent's lines as its "now" line and
// timeline.

const sessionId = "chat-subagents";
const startedAt = new Date(Date.now() - 4 * 60_000).toISOString();
const at = (secondsAgo: number) => new Date(Date.now() - secondsAgo * 1000).toISOString();
const migrateInput = { command: "npm run migration:create -- add_paused_at", description: "Create the migration" };

let sequence = 1;
function activity(fields: Record<string, unknown>) {
	return { kind: "activity", turnId: "turn-main", sequence: sequence++, revision: 1, createdAt: at(200), ...fields };
}

function snapshot() {
	sequence = 2;
	const activities = [
		// Working: narrating with Status:/Decision: lines while a Read runs.
		activity({
			id: "agent-audit", providerItemId: "agent-audit", activityKind: "mcp_tool", status: "running", summary: "Audit the auth flow",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "Explore",
				input: { description: "Audit the auth flow", prompt: "Read backend/internal/auth and list every place a token is validated.", subagent_type: "Explore" },
				progressSummary: "Tracing token validation through the middleware", progressAt: Date.now() - 20_000,
			},
		}),
		activity({
			id: "audit-voice", providerItemId: "audit-voice", activityKind: "mcp_tool", status: "running", summary: "Subagent response",
			detail: {
				nestedAgent: true, parentProviderItemId: "agent-audit",
				progress: [
					"Status: Listing the auth package",
					"Decision: Start from the HTTP middleware, since every route passes through it",
					"Status: Reading the session token validator",
					"Decision: Skip the legacy OAuth adapter; nothing imports it",
				].join("\n"),
			},
		}),
		activity({
			id: "audit-read", providerItemId: "audit-read", activityKind: "mcp_tool", status: "running", summary: "Read backend/internal/auth/token.go",
			detail: { parentProviderItemId: "agent-audit", providerToolName: "Read", input: { file_path: "backend/internal/auth/token.go" } },
		}),
		// Needs you: its Bash call waits on a permission request.
		activity({
			id: "agent-migrate", providerItemId: "agent-migrate", activityKind: "mcp_tool", status: "running", summary: "Write the paused_at migration",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "general-purpose",
				input: { description: "Write the paused_at migration", prompt: "Add a paused_at column to sessions with a new migration.", subagent_type: "general-purpose" },
			},
		}),
		activity({
			id: "migrate-voice", providerItemId: "migrate-voice", activityKind: "mcp_tool", status: "running", summary: "Subagent response",
			detail: { nestedAgent: true, parentProviderItemId: "agent-migrate", progress: "Status: Creating the migration file\nDecision: Use the migration generator instead of writing the ID by hand" },
		}),
		activity({
			id: "migrate-bash", providerItemId: "migrate-bash", activityKind: "mcp_tool", status: "pending", summary: "npm run migration:create -- add_paused_at",
			detail: { parentProviderItemId: "agent-migrate", providerToolName: "Bash", input: migrateInput },
		}),
		activity({
			id: "migrate-approval", activityKind: "approval", status: "pending", summary: "Run npm run migration:create -- add_paused_at",
			detail: { providerToolName: "Bash", input: migrateInput },
		}),
		// Done: finished with a report.
		activity({
			id: "agent-routes", providerItemId: "agent-routes", activityKind: "mcp_tool", status: "completed", summary: "Map the session routes",
			detail: {
				nestedAgent: true, providerToolName: "Agent", subagentType: "Explore",
				input: { description: "Map the session routes", prompt: "List the HTTP routes that read or write sessions.", subagent_type: "Explore" },
				content: [{ type: "content", content: { type: "text", text: "Found 14 session routes in httpd/controllers/sessions.go; 3 write paused state." } }],
			},
		}),
		activity({
			id: "routes-voice", providerItemId: "routes-voice", activityKind: "mcp_tool", status: "completed", summary: "Subagent response",
			detail: { nestedAgent: true, parentProviderItemId: "agent-routes", progress: "Status: Grepping the controllers\nDecision: Count only routes under /api/v1/sessions" },
		}),
	];
	// The main agent narrates with the same rule its subagents follow.
	const mainVoice = {
		kind: "message", id: "main-voice", turnId: "turn-main", sequence: sequence++, revision: 1, role: "assistant", origin: "provider",
		text: [
			"I'll split this across subagents.",
			"Status: Splitting the pausing work across three subagents",
			"Decision: Run the audit and the migration in parallel, since they touch different packages",
			"Status: Waiting on the migration subagent",
		].join("\n"),
		streaming: true, createdAt: at(30),
	};
	return {
		conversationId: "conversation-chat-subagents",
		sessionId,
		harness: "claude-code",
		mode: "chat",
		controller: "busy",
		latestSequence: sequence,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns: [{ id: "turn-main", state: "running", providerTurnId: "provider-main", requestedAt: startedAt, startedAt }],
		messages: [
			{ kind: "message", id: "user-main", turnId: "turn-main", sequence: 1, revision: 0, role: "user", origin: "human", text: "Add pausing to sessions", streaming: false, createdAt: startedAt },
			mainVoice,
		],
		activities,
		settings: {},
	};
}

async function installSubagentConversation(page: Page) {
	await installFakeAgent(page, { workers: [{ id: sessionId, title: "Add pausing to sessions", mode: "chat", provider: "claude-code" }] });
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		if (path.endsWith("/conversation") && route.request().method() === "GET") return route.fulfill({ json: snapshot() });
		if (path.endsWith("/conversation/models")) return route.fulfill({ json: { models: [], selected: {} } });
		if (path.endsWith("/conversation/skills")) return route.fulfill({ json: { skills: [] } });
		if (path.endsWith("/workspace/files")) return route.fulfill({ json: { files: [], truncated: false } });
		if (path.endsWith("/interface-transition")) return route.fulfill({ json: { supported: true, targetMode: "tui" } });
		return route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
}

async function capture(page: Page, name: string) {
	const directory = process.env.AO_SUBAGENT_EVIDENCE_DIR;
	if (directory) await page.screenshot({ path: `${directory}/${name}.png` });
}

test("the Agents tab shows the main agent and each subagent's lane, now line and decisions @T0", async ({ page }) => {
	await page.setViewportSize({ width: 1440, height: 900 });
	await installSubagentConversation(page);

	const tab = page.getByRole("tab", { name: /Agents/ });
	await expect(tab).toBeVisible();
	await tab.click();
	await expect(tab).toHaveAttribute("aria-selected", "true");

	const board = page.getByTestId("subagent-board");
	const card = (title: string) => board.getByRole("article", { name: title });
	// The main agent's newest Status: line and latest Decision: line, above the lanes.
	const main = card("Main agent");
	await expect(main).toHaveAttribute("data-state", "working");
	await expect(main.getByTestId("main-agent-now")).toHaveText("Waiting on the migration subagent");
	await expect(main.getByTestId("main-agent-decision")).toContainText("Run the audit and the migration in parallel, since they touch different packages");
	await expect(card("Audit the auth flow")).toHaveAttribute("data-lane", "working");
	await expect(card("Write the paused_at migration")).toHaveAttribute("data-lane", "needs_you");
	await expect(card("Map the session routes")).toHaveAttribute("data-lane", "done");
	// The newest Status:/Decision:/progress line is the working card's one status line.
	await expect(card("Audit the auth flow")).toContainText(/Read backend\/internal\/auth\/token\.go|Tracing token validation|Skip the legacy OAuth adapter/);
	await capture(page, "subagent-board-1-lanes");

	await card("Audit the auth flow").getByText("Audit the auth flow").click();
	await expect(card("Audit the auth flow").getByText("Start from the HTTP middleware, since every route passes through it")).toBeVisible();
	await capture(page, "subagent-board-2-expanded");

	await main.getByRole("button", { name: /Main agent/ }).click();
	await expect(main.getByRole("region", { name: "Updates" }).getByText("Splitting the pausing work across three subagents")).toBeVisible();
	await capture(page, "subagent-board-3-main-agent");
});
