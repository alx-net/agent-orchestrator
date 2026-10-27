import { describe, expect, it } from "vitest";
import type { ConversationActivity, ConversationItem, ConversationMessage } from "../../types/conversation";
import {
	agentMessageGist,
	asksUser,
	isWorkerLead,
	piggybackedUpdates,
	quietWorkerReply,
	renamedTitle,
	spawnedSessionIds,
	splitHeadline,
	stripSenderPrefix,
	workerUpdatesOf,
} from "./WorkerUpdates";

const createdAt = "2026-10-22T03:59:00Z";

function message(overrides: Partial<ConversationMessage>): ConversationMessage {
	return { kind: "message", id: "m1", sequence: 1, revision: 0, role: "user", origin: "automation", text: "", streaming: false, createdAt, ...overrides };
}

function command(command: string, output = ""): ConversationActivity {
	return { kind: "activity", id: command, sequence: 2, revision: 0, activityKind: "command", status: "completed", summary: command, detail: { command, output }, createdAt };
}

const batch = [
	"Reports since your previous turn:",
	"",
	"[needs_input] [Load chat \\[iOS\\] pages](ao://sessions/proj/proj-286)",
	"Tunnel or loopback?",
	"",
	"[done] [Check regressions](ao://sessions/proj/proj-288) (repeated 2 times)",
	"One strong candidate: #7043",
	"artifact: /home/u/.ao/data/artifacts/proj-288/check.md",
].join("\n");

describe("worker updates", () => {
	it("splits a report batch into one update per worker", () => {
		expect(workerUpdatesOf(message({ text: batch }))).toEqual([
			{ key: "m1:0", state: "needs_input", name: "Load chat [iOS] pages", projectId: "proj", sessionId: "proj-286", body: "Tunnel or loopback?", createdAt },
			{
				key: "m1:1", state: "done", name: "Check regressions", projectId: "proj", sessionId: "proj-288",
				body: "One strong candidate: #7043\nartifact: /home/u/.ao/data/artifacts/proj-288/check.md", createdAt,
			},
		]);
	});

	it("reads the older report format that linked the session without a name", () => {
		const text = "Reports since your previous turn:\n\n[done] ao://sessions/proj/proj-208\nInvestigated the screenshot.\n\n[checkpoint] ao://sessions/proj/proj-191 (repeated 2 times)\nPR #5391 updated";
		expect(workerUpdatesOf(message({ text }))).toMatchObject([
			{ state: "done", name: "proj-208", projectId: "proj", sessionId: "proj-208", body: "Investigated the screenshot." },
			{ state: "checkpoint", name: "proj-191", projectId: "proj", sessionId: "proj-191", body: "PR #5391 updated" },
		]);
	});

	it("keeps a malformed session link from breaking the timeline", () => {
		const text = "Reports since your previous turn:\n\n[done] [x](ao://sessions/p/100%)\nok";
		expect(workerUpdatesOf(message({ text }))).toMatchObject([{ projectId: "p", sessionId: "100%" }]);
	});

	it("reads a direct worker message or mid-turn steer without its sender prefix", () => {
		const direct = message({ text: '[from proj-151 "sqlc generation"] Regenerated the queries', senderSessionId: "proj-151", senderProjectId: "proj", senderDisplayName: "sqlc generation" });
		expect(workerUpdatesOf(direct)).toMatchObject([{ name: "sqlc generation", sessionId: "proj-151", body: "Regenerated the queries" }]);
		const steer: ConversationActivity = {
			kind: "activity", id: "s1", sequence: 3, revision: 0, activityKind: "system", status: "completed", summary: "Steer", createdAt,
			detail: { event: "steer", text: "[from proj-286] Loopback works", senderSessionId: "proj-286", senderProjectId: "proj" },
		};
		expect(workerUpdatesOf(steer)).toMatchObject([{ name: "proj-286", sessionId: "proj-286", body: "Loopback works" }]);
	});

	it("reads the sender from the prefix when an older message has no sender metadata", () => {
		expect(workerUpdatesOf(message({ text: "[from proj-155] Session proj-155 finished the rebase" }))).toMatchObject([
			{ name: "proj-155", sessionId: "proj-155", projectId: undefined, body: "Session proj-155 finished the rebase" },
		]);
		expect(workerUpdatesOf(message({ text: '[from worker a1b2c3d4 "Builder"] Please help' }))).toMatchObject([
			{ name: "Builder", sessionId: "a1b2c3d4", body: "Please help" },
		]);
		expect(workerUpdatesOf(message({ origin: "human", text: '[from worker a1b2c3d4 "Builder"] Please help' }))).toEqual([]);
	});

	it("treats AO's task-naming prompt as a worker turn and reads the name it produced", () => {
		const prompt = message({ text: "AO TASK TITLE UPDATE\nChoose a concise task title and run:\nao session rename proj-150 \"<title>\"" });
		expect(isWorkerLead(prompt)).toBe(true);
		expect(isWorkerLead(message({ origin: "human", text: "AO TASK TITLE UPDATE" }))).toBe(false);
		expect(renamedTitle([prompt, command('ao session rename proj-150 "Chat Link Browser"')])).toBe("Chat Link Browser");
		expect(renamedTitle([prompt])).toBeUndefined();
	});

	it("leaves the user's own messages alone, but finds reports folded into them", () => {
		const human = message({ origin: "human", text: `How is it going?\n\n<ao-worker-reports>\n${batch}\n</ao-worker-reports>` });
		expect(workerUpdatesOf(human)).toEqual([]);
		expect(piggybackedUpdates(human).map((update) => update.sessionId)).toEqual(["proj-286", "proj-288"]);
		expect(piggybackedUpdates(message({ origin: "human", text: "How is it going?" }))).toEqual([]);
	});

	it("takes a short first line as the headline, else the first sentence", () => {
		expect(splitHeadline("Fixed the menu\nIt now renders in a portal.")).toEqual({ headline: "Fixed the menu", detail: "It now renders in a portal." });
		expect(splitHeadline("Rebased **#5051**; disk full during validation. Retrying.").headline).toBe("Rebased #5051");
	});

	it("takes the sender's own name off the front of what it sent", () => {
		const names = ["kugelaudio-8", "Orchestrator"];
		expect(stripSenderPrefix("kugelaudio-8: ready #5506 @2716046663, gate pass", names)).toBe("ready #5506 @2716046663, gate pass");
		expect(stripSenderPrefix("Orchestrator: both Publish defaults accepted.", names)).toBe("both Publish defaults accepted.");
		expect(stripSenderPrefix("Orchestrator, Alex's bar for staging: everything visible must work.", names)).toBe("Alex's bar for staging: everything visible must work.");
		expect(stripSenderPrefix("kugelaudio-8, re-diffed against fa84259fe9.", names)).toBe("re-diffed against fa84259fe9.");
		expect(stripSenderPrefix("orchestrator:\n\nMerge it now.", names)).toBe("Merge it now.");
		expect(stripSenderPrefix("**Orchestrator:** merge it.", names)).toBe("merge it.");
		expect(stripSenderPrefix("**kugelaudio-8**: merged.", names)).toBe("merged.");
	});

	it("keeps the topic after a sender's name, and leaves anything else that merely starts with a name", () => {
		const names = ["kugelaudio-8", "Orchestrator"];
		expect(stripSenderPrefix("kugelaudio-8 re Publish (553b6f6c1f): confirmed, with notes.", names)).toBe("re Publish (553b6f6c1f): confirmed, with notes.");
		expect(stripSenderPrefix("kugelaudio-8 correction to my last message: not in #5505.", names)).toBe("kugelaudio-8 correction to my last message: not in #5505.");
		expect(stripSenderPrefix("kugelaudio-8 (1) Curb in the workspace image", names)).toBe("kugelaudio-8 (1) Curb in the workspace image");
		expect(stripSenderPrefix("kugelaudio-88: a different session.", names)).toBe("kugelaudio-88: a different session.");
		expect(stripSenderPrefix("kugelaudio-3: another session.", names)).toBe("kugelaudio-3: another session.");
		expect(stripSenderPrefix("Orchestrators: plural is not a signature.", names)).toBe("Orchestrators: plural is not a signature.");
		expect(stripSenderPrefix("Orchestrator:", names)).toBe("Orchestrator:");
		expect(stripSenderPrefix("proj.1: dots are literal", ["proj-1"])).toBe("proj.1: dots are literal");
		expect(stripSenderPrefix("kugelaudio-8: ready", [undefined, ""])).toBe("kugelaudio-8: ready");
	});

	it("gists an agent's message as its first sentence, unsigned", () => {
		const names = ["kugelaudio-2", "Orchestrator"];
		expect(agentMessageGist("Orchestrator: #5502 MERGED (57151acf68). #5506 now targets master.", names)).toBe("#5502 MERGED (57151acf68)");
		expect(agentMessageGist("Orchestrator: go with the plan (W1, A1, K1); I merge each green PR.", names)).toBe("go with the plan (W1, A1, K1)");
		expect(agentMessageGist("  kugelaudio-2: see [the run](https://ci.example/run/4) for `deploy`. Then push.", names)).toBe("see the run for deploy");
		expect(agentMessageGist("## Status\nAll green.", names)).toBe("Status");
		expect(agentMessageGist("Merge -> staging, measured today:\n\nWEB 10m12s", names)).toBe("Merge -> staging, measured today:");
	});

	it("tells a question from a ? in code, links and chained calls", () => {
		expect(asksUser("Merged #7054. Should I start a fix?")).toBe(true);
		expect(asksUser("#7043 の修正を始めますか？")).toBe(true);
		expect(asksUser("Opened [the run](https://ci.example/run?id=4).")).toBe(false);
		expect(asksUser("Guarded `config?.retries` and https://x.dev/a?b=1 now.")).toBe(false);
		expect(asksUser("Used value?.trim() for the name.")).toBe(false);
	});

	it("reads spawned workers from what `ao spawn` prints", () => {
		const items: ConversationItem[] = [
			command("ao spawn --prompt 'check regressions'", 'spawned session proj-288 "Check regressions" (working)\n'),
			command("ao spawn --prompt 'fix menus'", 'spawned session proj-285 "Fix menus" (working) [prompt 812 B, system 4096 B]'),
			command("ao status", "spawned session proj-1 (listing, not a spawn)"),
		];
		expect(spawnedSessionIds(items)).toEqual(["proj-288", "proj-285"]);
	});

	it("finds a worker turn's quiet reply only when it asks nothing", () => {
		const reply = (text: string) => message({ id: "a1", role: "assistant", origin: "provider", text });
		expect(quietWorkerReply([message({ text: batch }), reply("Noted.")])?.text).toBe("Noted.");
		expect(quietWorkerReply([message({ text: batch }), reply("Tunnel or loopback?")])).toBeUndefined();
		expect(quietWorkerReply([message({ text: batch }), command("gh pr merge 7054")])).toBeUndefined();
		expect(quietWorkerReply([message({ origin: "human", text: "Hi" }), reply("Noted.")])).toBeUndefined();
	});
});
