// @vitest-environment node
import {
	existsSync,
	mkdirSync,
	mkdtempSync,
	readFileSync,
	readdirSync,
	rmSync,
	symlinkSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { afterEach, describe, expect, it } from "vitest";
import {
	archiveExtraction,
	createWorkDirectory,
	npmInvocation,
	patchClaudeContextUsage,
	patchClaudeFoldedPromptSettlement,
	patchClaudeHibernationCheck,
	patchClaudeRetryDetails,
	patchClaudeStaleIdleDebt,
	patchClaudeSteerSettlement,
	patchClaudeSubagentProgress,
	pruneNodeDistribution,
	runtimeSourceFiles,
} from "./build-acp-runtime-helpers.mjs";

const temporaryDirectories = [];

afterEach(() => {
	for (const directory of temporaryDirectories.splice(0)) {
		rmSync(directory, { recursive: true, force: true });
	}
});

describe("createWorkDirectory", () => {
	it("places extraction on the output filesystem", () => {
		const outputRoot = temporaryDirectory();
		const workDirectory = createWorkDirectory(outputRoot);

		expect(dirname(workDirectory)).toBe(outputRoot);
		expect(existsSync(workDirectory)).toBe(true);
	});
});

describe("runtimeSourceFiles", () => {
	it("packages and fingerprints the ACP runtime manifest", () => {
		expect(runtimeSourceFiles()).toEqual([
			"package.json",
			"package-lock.json",
		]);
	});
});

describe("npmInvocation", () => {
	it("runs the parent npm CLI through Node on Windows", () => {
		expect(
			npmInvocation(["ci", "--omit=dev"], {
				platform: "win32",
				execPath: "C:\\node\\node.exe",
				npmExecPath: "C:\\node\\node_modules\\npm\\bin\\npm-cli.js",
				commandInterpreter: "C:\\Windows\\System32\\cmd.exe",
			}),
		).toEqual({
			command: "C:\\node\\node.exe",
			args: ["C:\\node\\node_modules\\npm\\bin\\npm-cli.js", "ci", "--omit=dev"],
		});
	});

	it("falls back to cmd.exe for a directly invoked Windows build script", () => {
		expect(
			npmInvocation(["ci"], {
				platform: "win32",
				npmExecPath: null,
				commandInterpreter: "C:\\Windows\\System32\\cmd.exe",
			}),
		).toEqual({
			command: "C:\\Windows\\System32\\cmd.exe",
			args: ["/d", "/s", "/c", "npm.cmd", "ci"],
		});
	});

	it("invokes npm directly on Unix when no parent npm CLI is available", () => {
		expect(npmInvocation(["ci"], { platform: "linux", npmExecPath: null })).toEqual({
			command: "npm",
			args: ["ci"],
		});
	});
});

describe("patchClaudeRetryDetails", () => {
	it("keeps Claude's retry delay in the published session failure", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, `
                            case "api_retry": {
                                const title = "retrying";
                                await publishSessionFailure(message.error_status === null
                                    ? "transport_lost"
                                    : providerFailureCategory(message.error), {
                                    title,
                                    severity: "warning",
                                });
                                break;
                            }
                            case "model_refusal_fallback": {
`);

		expect(patchClaudeRetryDetails(adapterPath)).toBe(true);
		expect(patchClaudeRetryDetails(adapterPath)).toBe(false);
		const patched = readFileSync(adapterPath, "utf8");
		expect(patched).toContain("message.retry_delay_ms / 1000");
		expect(patched).toContain("Trying again in ${retryDelay}.");
		expect(patched).toContain("details: retryDetails");
	});
});

describe("patchClaudeHibernationCheck", () => {
	const anchors = [
		"// session.liveBackgroundTasks.set(message.task_id",
		"// session.lastSessionState = message.state;",
		"agent.endTask = (session, message) => { session.liveBackgroundTasks.delete(message.task_id); };",
		"// session.liveBackgroundTasks.delete(message.task_id);",
		"connection.onRequest(GOAL_CONTROL_METHOD, { parse: parseGoalRequest }, (ctx) => agent.goal(ctx.params));",
	];

	it("keeps live native tasks awake and allows sleep after they settle", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, anchors.join("\n"));
		expect(patchClaudeHibernationCheck(adapterPath)).toBe(true);
		expect(patchClaudeHibernationCheck(adapterPath)).toBe(false);
		const handlers = new Map();
		const connection = { onRequest: (method, parser, handler) => {
			expect(typeof parser.parse).toBe("function");
			expect(typeof handler).toBe("function");
			handlers.set(method, (ctx) => handler({ params: parser.parse(ctx.params) }));
			return connection;
		} };
		const tasks = new Map();
		const agent = { sessions: { native: { liveBackgroundTasks: tasks } } };
		new Function("connection", "agent", "GOAL_CONTROL_METHOD", "parseGoalRequest", "RequestError", readFileSync(adapterPath, "utf8"))(
			connection, agent, "goal", params => params, { invalidParams: () => new Error("invalid session") },
		);
		const check = handlers.get("_ao/session/can_hibernate");
		const ctx = { params: { sessionId: "native" } };
		expect(check(ctx)).toEqual({ canHibernate: true });
		tasks.set("server", { isSubagent: false });
		expect(check(ctx)).toEqual({ canHibernate: false });
		tasks.delete("server");
		expect(check(ctx)).toEqual({ canHibernate: true });
		tasks.set("ended", { isSubagent: true, endedPerLevel: "ended" });
		expect(check(ctx)).toEqual({ canHibernate: true });
		// A finished task's notification starts a cycle AO has no turn for.
		agent.sessions.native.lastSessionState = "running";
		expect(check(ctx)).toEqual({ canHibernate: false });
		agent.sessions.native.lastSessionState = "requires_action";
		expect(check(ctx)).toEqual({ canHibernate: false });
		agent.sessions.native.lastSessionState = "idle";
		expect(check(ctx)).toEqual({ canHibernate: true });
		// The notification cycle is reported as running a moment after the task leaves.
		tasks.set("shell", { isSubagent: false });
		agent.endTask(agent.sessions.native, { task_id: "shell" });
		expect(check(ctx)).toEqual({ canHibernate: false });
		agent.sessions.native.aoTaskEndedAt -= 10000;
		expect(check(ctx)).toEqual({ canHibernate: true });
		for (const sessionId of [undefined, "missing", "__proto__"]) {
			expect(() => check({ params: { sessionId } })).toThrow("invalid session");
		}
		agent.sessions.native.queryClosed = true;
		expect(() => check(ctx)).toThrow("invalid session");
	});

	it("fails packaging if the pinned native task registry changes", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		for (const missing of anchors.keys()) {
			writeFileSync(adapterPath, anchors.filter((_, i) => i !== missing).join("\n"));
			expect(() => patchClaudeHibernationCheck(adapterPath)).toThrow("native-task hibernation check");
		}
	});
});

describe("patchClaudeContextUsage", () => {
	it("publishes the SDK context snapshot through ACP after a result", async () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, `
                            // Send usage_update notification
                            if (lastAssistantTotalUsage !== null) {
                                await sendUpdate({
                                    update: {
                                        used: lastAssistantTotalUsage,
                                        size: session.contextWindowSize,
                                    },
                                });
                            }
                            if (session.cancelled) {
`);

		expect(patchClaudeContextUsage(adapterPath)).toBe(true);
		expect(patchClaudeContextUsage(adapterPath)).toBe(false);
		const patched = readFileSync(adapterPath, "utf8");
		expect(patched).toContain("session.query.getContextUsage()");
		expect(patched).toContain("lastAssistantTotalUsage = contextUsage.totalTokens");
		expect(patched).toContain("session.contextWindowSize = contextUsage.rawMaxTokens");

		const start = patched.indexOf("// AO: use the SDK's context snapshot.");
		const end = patched.indexOf("if (session.cancelled) {", start);
		const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
		const run = new AsyncFunction("session", "sendUpdate", `
			let lastAssistantTotalUsage = 9;
			${patched.slice(start, end)}
		`);
		const updates = [];
		const session = {
			contextWindowSize: 100,
			contextWindowAuthoritative: false,
			query: { getContextUsage: async () => ({ totalTokens: 17, rawMaxTokens: 200 }) },
		};
		await run.call({ logger: { error: () => {} } }, session, (notification) => {
			updates.push(notification.update);
		});
		expect(updates.map(({ used, size }) => [used, size])).toEqual([[17, 200]]);
		expect(session.contextWindowAuthoritative).toBe(true);

		const fallback = {
			contextWindowSize: 100,
			contextWindowAuthoritative: false,
			query: { getContextUsage: async () => { throw new Error("unavailable"); } },
		};
		const fallbackUpdates = [];
		await run.call({ logger: { error: () => {} } }, fallback, (notification) => {
			fallbackUpdates.push(notification.update);
		});
		expect(fallbackUpdates.map(({ used, size }) => [used, size])).toEqual([[9, 100]]);
		expect(fallback.contextWindowAuthoritative).toBe(false);

		const zero = {
			contextWindowSize: 100,
			contextWindowAuthoritative: false,
			query: { getContextUsage: async () => ({ totalTokens: 0, rawMaxTokens: 200 }) },
		};
		const zeroUpdates = [];
		await run.call({ logger: { error: () => {} } }, zero, (notification) => {
			zeroUpdates.push(notification.update);
		});
		expect(zeroUpdates.map(({ used, size }) => [used, size])).toEqual([[9, 100]]);
		expect(zero.contextWindowAuthoritative).toBe(false);
	});
});

describe("patchClaudeFoldedPromptSettlement", () => {
	it("routes a task-notification result that names a pending prompt to the user lane", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, `
        const findUnsettledTurn = (uuid) => (session.turnQueue ?? []).find((t) => t.promptUuid === uuid && !t.settled);
                    case "result": {
                        const isAutonomousResult = message.origin != null && AUTONOMOUS_RESULT_ORIGINS.has(message.origin.kind);
                        try {
`);

		expect(patchClaudeFoldedPromptSettlement(adapterPath)).toBe(true);
		expect(patchClaudeFoldedPromptSettlement(adapterPath)).toBe(false);
		const patched = readFileSync(adapterPath, "utf8");

		const start = patched.indexOf("// AO: a result naming a pending prompt answers it.");
		const end = patched.indexOf("try {", start);
		const classify = new Function("message", "session", `
			const AUTONOMOUS_RESULT_ORIGINS = new Set(["task-notification", "peer"]);
			const isHeldOpen = (turn) => turn.deferredSettle !== undefined && !turn.settled;
			const findUnsettledTurn = (uuid) => (session.turnQueue ?? []).find((t) => t.promptUuid === uuid && !t.settled);
			${patched.slice(start, end)}
			return isAutonomousResult;
		`);
		const notification = { kind: "task-notification" };
		const session = {
			turnQueue: [
				{ promptUuid: "pending", settled: false },
				{ promptUuid: "settled", settled: true },
				{ promptUuid: "held", settled: false, deferredSettle: { stopReason: "end_turn" } },
			],
		};

		expect(classify({ origin: notification, user_message_uuids: ["pending"] }, session)).toBe(false);
		expect(classify({ origin: notification, user_message_uuid: "pending" }, session)).toBe(false);
		expect(classify({ origin: notification, user_message_uuids: ["settled", "held", "unknown"] }, session)).toBe(true);
		expect(classify({ origin: notification }, session)).toBe(true);
		expect(classify({ origin: { kind: "human" } }, session)).toBe(false);
		expect(classify({}, session)).toBe(false);
	});

	it("fails packaging when the adapter's classification changes", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, "const isAutonomousResult = isAutonomous(message);\n");

		expect(() => patchClaudeFoldedPromptSettlement(adapterPath)).toThrow(/folded-prompt patch/);
	});
});

describe("patchClaudeStaleIdleDebt", () => {
	it("drops unpaid idle debt when Claude starts running again", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, `
                            case "session_state_changed": {
                                session.lastSessionState = message.state;
                                if (message.state === "idle") {
                                    if (session.owedTrailingIdles > 0) {
                                        session.owedTrailingIdles--;
                                    }
                                }
                                break;
                            }
`);

		expect(patchClaudeStaleIdleDebt(adapterPath)).toBe(true);
		expect(patchClaudeStaleIdleDebt(adapterPath)).toBe(false);
		const patched = readFileSync(adapterPath, "utf8");

		const start = patched.indexOf("// AO: drop idle debt that can no longer be paid.");
		const end = patched.indexOf("break;", start);
		const onState = new Function("message", "session", `${patched.slice(start, end)}`);
		const session = { lastSessionState: "idle", owedTrailingIdles: 2 };

		onState({ state: "idle" }, session);
		expect(session.owedTrailingIdles).toBe(1);
		onState({ state: "running" }, session);
		expect(session.owedTrailingIdles).toBe(0);
		session.owedTrailingIdles = 1;
		onState({ state: "running" }, session);
		expect(session.owedTrailingIdles).toBe(1);
		onState({ state: "idle" }, session);
		expect(session.owedTrailingIdles).toBe(0);
		expect(session.lastSessionState).toBe("idle");
	});

	it("fails packaging when the adapter's state handling changes", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, 'case "session_state_changed": {\n');

		expect(() => patchClaudeStaleIdleDebt(adapterPath)).toThrow(/idle-debt patch/);
	});
});

describe("patchClaudeSteerSettlement", () => {
	// claude-agent-acp 0.70's steer and settle lanes, cut down to the lines the
	// settlement patches anchor on. Each test replays an SDK frame order a real
	// session produces.
	const consumerSource = `
function isSteering(turn) {
    return turn != null && turn.steeredEchoes !== undefined && !turn.settled;
}
const AUTONOMOUS_RESULT_ORIGINS = new Set(["task-notification", "peer"]);
const isHeldOpen = (turn) => turn != null && turn.deferredSettle !== undefined && !turn.settled;
const findUnsettledTurn = (uuid) => session.turnQueue.find((t) => t.promptUuid === uuid && !t.settled);
const ensureActiveTurn = () => { session.activeTurn ??= session.turnQueue.find((turn) => !turn.settled); };
const recordResultForOrphanCommands = () => {};
const settleActive = (outcome) => {
    session.activeTurn.settled = outcome;
    session.activeTurn = undefined;
};
const settleDeferredIfDrained = () => {
    if (isHeldOpen(session.activeTurn)) settleActive(session.activeTurn.deferredSettle);
};
function steer(steeredUuid) {
        const turnInFlight = session.turnQueue.find((turn) => !turn.settled);
        (turnInFlight.steeredEchoes ??= new Set()).add(steeredUuid);
        if (turnInFlight.deferredSettle !== undefined) {
            turnInFlight.steeredSettle = turnInFlight.deferredSettle;
            turnInFlight.deferredSettle = undefined;
        }
}
function onMessage(message) {
        const settleOrDefer = (outcome) => {
            if (isSteering(session.activeTurn)) {
                session.activeTurn.steeredSettle = outcome;
                return;
            }
            settleActive(outcome);
        };
        switch (message.type) {
                    case "result": {
                        const isAutonomousResult = message.origin != null && AUTONOMOUS_RESULT_ORIGINS.has(message.origin.kind);
                        try {
                            if (!isAutonomousResult) {
                                recordResultForOrphanCommands();
                                ensureActiveTurn();
                            }
                            const owesTrailingIdle = isAutonomousResult || !isSteering(session.activeTurn);
                            if (owesTrailingIdle &&
                                (isAutonomousResult || !session.cancelled || !session.activeTurn)) {
                                session.owedTrailingIdles++;
                            }
                            if (!isAutonomousResult && !session.cancelled && session.activeTurn) {
                                settleOrDefer(message.result);
                            }
                        } finally {}
                        break;
                    }
                    case "user": {
                            if ("isReplay" in message && message.isReplay) {
                                if (isSteering(session.activeTurn)) {
                                    session.activeTurn.steeredEchoes.delete(message.uuid);
                                }
                                break;
                            }
                        break;
                    }
                    case "system": {
                        switch (message.subtype) {
                            case "session_state_changed": {
                                session.lastSessionState = message.state;
                                if (message.state === "idle") {
                                    if (isHeldOpen(session.activeTurn)) {
                                        if (session.owedTrailingIdles > 0) {
                                            session.owedTrailingIdles--;
                                        }
                                        settleDeferredIfDrained();
                                    }
                                    else if (session.owedTrailingIdles > 0) {
                                        // Absorb a settled turn's trailing idle. Also covers a
                                        session.owedTrailingIdles--;
                                    }
                                    else if (isSteering(session.activeTurn)) {
                                        // Plain Turn so the fields can be cleared below; isSteering
                                        // narrows steeredEchoes to non-optional.
                                        const steered = session.activeTurn;
                                        if (steered.steeredEchoes?.size === 0 && steered.steeredSettle !== undefined) {
                                            // Via the subagent gate, not settleActive: a steered turn
                                            // can also have spawned background subagents, which own it
                                            // from here (settles now if none is live, holds otherwise).
                                            steered.deferredSettle = steered.steeredSettle;
                                            steered.steeredEchoes = undefined;
                                            steered.steeredSettle = undefined;
                                            settleDeferredIfDrained();
                                        }
                                    }
                                    else if (!session.cancelled && session.activeTurn) {
                                        settleActive("rejected: idle without a result (#825)");
                                    }
                                }
                                break;
                            }
                        }
                        break;
                    }
        }
}
`;

	function steerConsumer({ fold = true } = {}) {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, consumerSource);
		// Same order as scripts/build-acp-runtime.mjs.
		if (fold) patchClaudeFoldedPromptSettlement(adapterPath);
		patchClaudeStaleIdleDebt(adapterPath);
		expect(patchClaudeSteerSettlement(adapterPath)).toBe(true);
		expect(patchClaudeSteerSettlement(adapterPath)).toBe(false);

		const session = { turnQueue: [], owedTrailingIdles: 0, lastSessionState: "idle" };
		const consumer = new Function("session", `${readFileSync(adapterPath, "utf8")}\nreturn { steer, onMessage };`)(session);
		const turn = { promptUuid: "prompt" };
		session.turnQueue.push(turn);
		session.activeTurn = turn;
		return {
			session,
			turn,
			steer: consumer.steer,
			result: (fields) => consumer.onMessage({ type: "result", origin: { kind: "human" }, ...fields }),
			echo: (uuid) => consumer.onMessage({ type: "user", isReplay: true, uuid }),
			state: (state) => consumer.onMessage({ type: "system", subtype: "session_state_changed", state }),
		};
	}

	it("settles at the result that names the steer, which still owes its trailing idle", () => {
		const { session, turn, steer, result, echo, state } = steerConsumer();
		steer("steer");
		result({ result: "interrupted", user_message_uuid: "prompt" });
		echo("steer");
		result({ result: "steered", user_message_uuids: ["steer"] });
		expect(turn.settled).toBe("steered");
		expect(session.owedTrailingIdles).toBe(1);

		// The next prompt's echo can land before that trailing idle.
		const next = { promptUuid: "next" };
		session.turnQueue.push(next);
		session.activeTurn = next;
		state("idle");
		expect(next.settled).toBeUndefined();
		result({ result: "answer", user_message_uuid: "next" });
		expect(next.settled).toBe("answer");
	});

	it("waits for the steered result when an idle lands after the steered echo", () => {
		const { session, turn, steer, result, echo, state } = steerConsumer();
		result({ origin: { kind: "task-notification" }, result: "autonomous" });
		steer("steer");
		result({ result: "interrupted", user_message_uuid: "prompt" });
		echo("steer");
		state("idle"); // the autonomous cycle's lagging trailer
		expect(turn.settled).toBeUndefined();

		result({ result: "steered" });
		state("idle");
		expect(turn.settled).toBe("steered");
		expect(session.owedTrailingIdles).toBe(0);
	});

	it("settles a ready steered turn ahead of idle debt and leaves the debt to the running sweep", () => {
		const { session, turn, steer, result, echo, state } = steerConsumer();
		result({ origin: { kind: "task-notification" }, result: "autonomous" });
		steer("steer");
		result({ result: "interrupted", user_message_uuid: "prompt" });
		echo("steer");
		result({ result: "steered" });
		state("idle"); // Claude Code coalesced the autonomous trailer into this idle
		expect(turn.settled).toBe("steered");
		expect(session.owedTrailingIdles).toBe(1);
		state("running");
		expect(session.owedTrailingIdles).toBe(0);
	});

	it.each([true, false])("lets an autonomous result that names the steer answer it (fold patch: %s)", (fold) => {
		const { session, turn, steer, result, echo } = steerConsumer({ fold });
		steer("steer");
		echo("steer");
		result({ origin: { kind: "task-notification" }, result: "hand-back", user_message_uuids: ["steer"] });
		expect(turn.settled).toBe("hand-back");
		expect(session.owedTrailingIdles).toBe(1);
	});

	it.each([true, false])("settles a queued prompt whose steer an autonomous cycle folded in (fold patch: %s)", (fold) => {
		const { session, turn, steer, result } = steerConsumer({ fold });
		session.activeTurn = undefined; // Claude Code's own cycle is running; the prompt is still queued
		steer("steer");
		result({
			origin: { kind: "task-notification" },
			result: "folded",
			user_message_uuid: "steer",
			user_message_uuids: ["prompt", "steer"],
		});
		expect(turn.settled).toBe("folded");
		expect(session.owedTrailingIdles).toBe(1);
	});

	it("keeps the steer lane while another steer is still outstanding", () => {
		const { turn, steer, result, echo, state } = steerConsumer();
		steer("first");
		steer("second");
		result({ result: "first answer", user_message_uuids: ["first"] });
		expect(turn.settled).toBeUndefined();
		expect([...turn.steeredEchoes]).toEqual(["second"]);
		state("idle");
		expect(turn.settled).toBeUndefined();

		echo("second");
		result({ result: "second answer", user_message_uuids: ["second"] });
		expect(turn.settled).toBe("second answer");
	});

	it("leaves a cancelled turn's steers to cancel()", () => {
		const { session, turn, steer, result } = steerConsumer();
		steer("steer");
		session.cancelled = true;
		result({ origin: { kind: "task-notification" }, result: "cancelled", user_message_uuid: "steer" });
		expect(turn.steeredEchoes).toEqual(new Set(["steer"]));
	});

	it("fails packaging without modifying a changed adapter", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		const changed = consumerSource.replace("ensureActiveTurn();", "await ensureActiveTurn(message.user_message_uuid);");
		writeFileSync(adapterPath, changed);

		expect(() => patchClaudeSteerSettlement(adapterPath)).toThrow(/steer settlement patch/);
		expect(readFileSync(adapterPath, "utf8")).toBe(changed);
	});
});

describe("patchClaudeSubagentProgress", () => {
	// claude-agent-acp 0.70's query options and SDK message switch, cut down to
	// the lines the subagent progress patch anchors on.
	const adapterSource = `
        const options = {
            includePartialMessages: true,
            forwardSubagentText,
            mcpServers: {},
            hooks: {
                ...userProvidedOptions?.hooks,
                ...(fileChangeAuditSupport
                    ? {
                        PreToolUse: [
                            ...(userProvidedOptions?.hooks?.PreToolUse || []),
                            { hooks: [fileChangeAuditSupport.preToolUseHook] },
                        ],
                    }
                    : {}),
                PostToolUse: [],
            },
        };
        const onMessage = async (message) => {
            switch (message.subtype) {
                            case "files_persisted":
                            case "task_progress":
                                break;
                            case "task_started":
                                session.liveBackgroundTasks.set(message.task_id, {
                                    parentToolUseId: message.tool_use_id,
                                    isSubagent: !!message.subagent_type,
                                });
                                break;
            }
        };
`;

	function load({ userHooks, audit } = {}) {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		writeFileSync(adapterPath, adapterSource);
		expect(patchClaudeSubagentProgress(adapterPath)).toBe(true);
		expect(patchClaudeSubagentProgress(adapterPath)).toBe(false);
		const updates = [];
		const session = { liveBackgroundTasks: new Map() };
		const { options, onMessage } = new Function(
			"userProvidedOptions", "fileChangeAuditSupport", "forwardSubagentText", "session", "sendUpdate",
			`${readFileSync(adapterPath, "utf8")}\nreturn { options, onMessage };`,
		)(
			userHooks ? { hooks: { PreToolUse: userHooks } } : undefined,
			audit ? { preToolUseHook: audit } : undefined,
			true,
			session,
			async (notification) => { updates.push(notification); },
		);
		return { options, onMessage, updates };
	}

	it("asks the SDK for subagent summaries", () => {
		expect(load().options.agentProgressSummaries).toBe(true);
	});

	it("forwards a subagent's progress as meta on its Agent call and nothing else", async () => {
		const { onMessage, updates } = load();
		await onMessage({ subtype: "task_started", task_id: "agent-1", tool_use_id: "toolu_agent", subagent_type: "Explore" });
		await onMessage({
			subtype: "task_progress", task_id: "agent-1", session_id: "s1", description: "Scan",
			usage: { total_tokens: 900, tool_uses: 3, duration_ms: 4000 },
			last_tool_name: "Grep", summary: "Searching the parser for retry handling",
		});
		// Between summaries the SDK still reports the latest tool and count.
		await onMessage({
			subtype: "task_progress", task_id: "agent-1", tool_use_id: "toolu_agent", session_id: "s1",
			usage: { total_tokens: 1200, tool_uses: 4, duration_ms: 6000 }, last_tool_name: "Read",
		});

		expect(updates).toHaveLength(2);
		for (const { sessionId, update } of updates) {
			expect(sessionId).toBe("s1");
			expect(Object.keys(update).sort()).toEqual(["_meta", "sessionUpdate", "toolCallId"]);
			expect(update.sessionUpdate).toBe("tool_call_update");
			expect(update.toolCallId).toBe("toolu_agent");
			expect(Object.keys(update._meta)).toEqual(["claudeCode"]);
			expect(Object.keys(update._meta.claudeCode)).toEqual(["subagentProgress"]);
			expect(typeof update._meta.claudeCode.subagentProgress.at).toBe("number");
		}
		const [first, second] = updates.map(({ update }) => {
			const { at: _at, ...progress } = update._meta.claudeCode.subagentProgress;
			return progress;
		});
		expect(first).toEqual({ summary: "Searching the parser for retry handling", lastToolName: "Grep", toolUses: 3 });
		expect(second).toEqual({ lastToolName: "Read", toolUses: 4 });
	});

	it("ignores progress that does not belong to a subagent call", async () => {
		const { onMessage, updates } = load();
		await onMessage({ subtype: "task_started", task_id: "shell-1", tool_use_id: "toolu_bash" });
		await onMessage({ subtype: "task_progress", task_id: "shell-1", usage: { tool_uses: 0 }, summary: "50%" });
		await onMessage({ subtype: "task_progress", task_id: "unknown", usage: { tool_uses: 1 }, summary: "Working" });
		expect(updates).toEqual([]);
	});

	it("appends the reporting rule to Agent and Task prompts after the existing hooks", async () => {
		const userHook = { matcher: "Bash", hooks: [async () => ({})] };
		const audit = async () => ({});
		const { options } = load({ userHooks: [userHook], audit });
		const [user, auditMatcher, reporting] = options.hooks.PreToolUse;
		expect(options.hooks.PreToolUse).toHaveLength(3);
		expect(user).toBe(userHook);
		expect(auditMatcher.hooks).toEqual([audit]);
		expect(reporting.matcher).toBe("Agent|Task");
		expect(load().options.hooks.PreToolUse).toHaveLength(1);

		const [hook] = reporting.hooks;
		const input = { description: "Scan", prompt: "Find the retry loop.", subagent_type: "Explore" };
		const result = await hook({ hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: input });
		const { updatedInput } = result.hookSpecificOutput;
		expect(result.hookSpecificOutput.hookEventName).toBe("PreToolUse");
		expect(result.hookSpecificOutput).not.toHaveProperty("permissionDecision");
		expect(updatedInput).toEqual({ ...input, prompt: updatedInput.prompt });
		expect(updatedInput.prompt).toBe(
			"Find the retry loop.\n\nWhile you work, before each major step write one short line that starts with " +
			"\"Status:\" saying what you are doing now. Whenever you choose between approaches, write one line that " +
			"starts with \"Decision:\" stating the choice and the reason in under 20 words.",
		);
		expect(input.prompt).toBe("Find the retry loop.");
		expect(await hook({ hook_event_name: "PreToolUse", tool_name: "Task", tool_input: updatedInput })).toEqual({});
		expect(await hook({ hook_event_name: "PreToolUse", tool_name: "TaskCreate", tool_input: input })).toEqual({});
		expect(await hook({ hook_event_name: "PreToolUse", tool_name: "Agent", tool_input: { description: "x" } })).toEqual({});
	});

	it("fails packaging without modifying a changed adapter", () => {
		const adapterPath = join(temporaryDirectory(), "acp-agent.js");
		for (const changed of [
			adapterSource.replace('case "task_progress":\n', ""),
			adapterSource.replace("forwardSubagentText,", "forwardSubagentText: true,"),
			adapterSource.replace("{ hooks: [fileChangeAuditSupport.preToolUseHook] },", "{ hooks: [audit] },"),
		]) {
			writeFileSync(adapterPath, changed);
			expect(() => patchClaudeSubagentProgress(adapterPath)).toThrow(/subagent progress patch/);
			expect(readFileSync(adapterPath, "utf8")).toBe(changed);
		}
	});
});

describe("pruneNodeDistribution", () => {
	it("removes Unix package-manager links before deleting their targets", () => {
		const nodeRoot = temporaryDirectory();
		const bin = join(nodeRoot, "bin");
		const npmBin = join(nodeRoot, "lib", "node_modules", "npm", "bin");
		mkdirSync(bin, { recursive: true });
		mkdirSync(npmBin, { recursive: true });
		writeFileSync(join(bin, "node"), "node");
		writeFileSync(join(npmBin, "npm-cli.js"), "npm");
		writeFileSync(join(npmBin, "npx-cli.js"), "npx");
		symlinkSync("../lib/node_modules/npm/bin/npm-cli.js", join(bin, "npm"));
		symlinkSync("../lib/node_modules/npm/bin/npx-cli.js", join(bin, "npx"));
		symlinkSync("../lib/node_modules/corepack/dist/corepack.js", join(bin, "corepack"));

		pruneNodeDistribution(nodeRoot);

		expect(readdirSync(bin)).toEqual(["node"]);
		expect(existsSync(join(nodeRoot, "lib"))).toBe(false);
	});

	it("removes package-manager files and modules from a Windows distribution", () => {
		const nodeRoot = temporaryDirectory();
		writeFileSync(join(nodeRoot, "node.exe"), "node");
		writeFileSync(join(nodeRoot, "LICENSE"), "license");
		for (const name of ["corepack", "corepack.cmd", "npm", "npm.cmd", "npx", "npx.cmd"]) {
			writeFileSync(join(nodeRoot, name), name);
		}
		mkdirSync(join(nodeRoot, "node_modules", "npm"), { recursive: true });

		pruneNodeDistribution(nodeRoot);

		expect(readdirSync(nodeRoot).sort()).toEqual(["LICENSE", "node.exe"]);
	});
});

function temporaryDirectory() {
	const directory = mkdtempSync(join(tmpdir(), "ao-acp-runtime-test-"));
	temporaryDirectories.push(directory);
	return directory;
}

describe("archiveExtraction", () => {
	// Extraction must not go through PowerShell's Expand-Archive: it is bound by
	// MAX_PATH, and with LongPathsEnabled=0 a deep checkout pushes Node's bundled
	// npm tree past 260 characters, where it fails while still exiting zero.
	it("uses bsdtar for the Windows zip", () => {
		expect(archiveExtraction("C:\\w\\node.zip", "C:\\w", {
			platform: "win32",
			systemRoot: "C:\\Windows",
		})).toEqual({
			command: "C:\\Windows\\System32\\tar.exe",
			args: ["-xf", "C:\\w\\node.zip", "-C", "C:\\w"],
		});
	});

	it("keeps gzip handling on the other platforms", () => {
		for (const platform of ["darwin", "linux"]) {
			expect(archiveExtraction("/w/node.tar.gz", "/w", { platform })).toEqual({
				command: "tar",
				args: ["-xzf", "/w/node.tar.gz", "-C", "/w"],
			});
		}
	});

	it("never shells out to a command interpreter", () => {
		for (const platform of ["win32", "darwin", "linux"]) {
			const { command } = archiveExtraction("/w/a", "/w", { platform, systemRoot: "C:\\Windows" });
			expect(command).not.toMatch(/powershell|cmd\.exe|\bsh\b/i);
		}
	});
});
