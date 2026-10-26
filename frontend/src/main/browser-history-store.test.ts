import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, expect, it, vi } from "vitest";
import { BrowserHistoryStore, type BrowserHistoryEntry } from "./browser-history-store";

const profileId = "11111111-1111-4111-8111-111111111111";
const temporaryDirectories: string[] = [];

afterEach(async () => {
	await Promise.all(temporaryDirectories.splice(0).map((directory) => rm(directory, { recursive: true, force: true })));
});

async function temporaryState(): Promise<string> {
	const directory = await mkdtemp(path.join(os.tmpdir(), "ao-browser-history-"));
	temporaryDirectories.push(directory);
	return directory;
}

describe("BrowserHistoryStore", () => {
	it("merges, persists, reloads, suggests, and clears profile-scoped history", async () => {
		const stateDir = await temporaryState();
		const store = new BrowserHistoryStore({ stateDir });
		await expect(store.mergeImportedEntries(profileId, [
			{ url: "https://github.com/openai", title: "OpenAI", lastVisited: "2026-01-01T00:00:00.000Z", visitCount: 2 },
			{ url: "file:///secret", title: "Secret", lastVisited: "2026-01-02T00:00:00.000Z", visitCount: 1 },
		])).resolves.toEqual({ imported: 1, truncated: 0 });

		expect(await store.suggest(profileId, "openai")).toEqual([
			{ url: "https://github.com/openai", title: "OpenAI" },
		]);
		const reloaded = new BrowserHistoryStore({ stateDir });
		expect(await reloaded.suggest(profileId, "github")).toHaveLength(1);
		await reloaded.clear(profileId);
		expect(await new BrowserHistoryStore({ stateDir }).suggest(profileId, "github")).toEqual([]);
	});

	it("caps entry count and serialized output without retaining an oversized cache", async () => {
		const stateDir = await temporaryState();
		const store = new BrowserHistoryStore({ stateDir });
		const entries: BrowserHistoryEntry[] = Array.from({ length: 5_100 }, (_, index) => ({
			url: `https://example.com/${index}/${"x".repeat(900)}`,
			title: `Entry ${index}`,
			lastVisited: new Date(1_800_000_000_000 - index).toISOString(),
			visitCount: 1,
		}));
		const outcome = await store.mergeImportedEntries(profileId, entries);
		const file = path.join(stateDir, "browser-history", `${profileId}.json`);
		const metadata = await stat(file);
		const parsed = JSON.parse(await readFile(file, "utf8")) as { entries: BrowserHistoryEntry[] };
		expect(metadata.size).toBeLessThanOrEqual(4 * 1024 * 1024);
		expect(parsed.entries.length).toBe(outcome.imported);
		expect(parsed.entries.length).toBeLessThanOrEqual(5_000);
		expect(outcome.truncated).toBe(entries.length - outcome.imported);
		expect(await store.suggest(profileId, "entry 5099")).toEqual([]);
	});

	it("drains queued fire-and-forget records before shutdown completes", async () => {
		const stateDir = await temporaryState();
		const store = new BrowserHistoryStore({ stateDir });

		void store.record(profileId, "https://first.example/", "First", true);
		void store.record(profileId, "https://second.example/", "Second", true);
		await store.drain();

		const reloaded = new BrowserHistoryStore({ stateDir });
		expect(await reloaded.suggest(profileId, "example")).toHaveLength(2);
	});

	it("coalesces navigation records into one compact write that drain flushes", async () => {
		const stateDir = await temporaryState();
		let clock = Date.parse("2026-03-01T00:00:00.000Z");
		const store = new BrowserHistoryStore({ stateDir, now: () => new Date((clock += 1_000)), writeDelayMs: 60_000 });
		const file = path.join(stateDir, "browser-history", `${profileId}.json`);

		await store.record(profileId, "https://first.example/", "First", true);
		await store.record(profileId, "https://second.example/", "Second", true);
		await store.record(profileId, "https://first.example/", "First again", true);
		await store.record(profileId, "https://second.example/", "", false);
		await expect(stat(file)).rejects.toMatchObject({ code: "ENOENT" });
		expect(await store.suggest(profileId, "https://")).toEqual([
			{ url: "https://second.example/", title: "Second" },
			{ url: "https://first.example/", title: "First again" },
		]);

		await store.drain();
		const raw = await readFile(file, "utf8");
		expect(raw).not.toContain("\n ");
		expect(JSON.parse(raw)).toEqual({
			version: 1,
			entries: [
				{ url: "https://second.example/", title: "Second", lastVisited: "2026-03-01T00:00:04.000Z", visitCount: 1 },
				{ url: "https://first.example/", title: "First again", lastVisited: "2026-03-01T00:00:03.000Z", visitCount: 2 },
			],
		});
	});

	it("writes coalesced records after the delay without a drain", async () => {
		const stateDir = await temporaryState();
		const store = new BrowserHistoryStore({ stateDir, writeDelayMs: 10 });
		const file = path.join(stateDir, "browser-history", `${profileId}.json`);

		await store.record(profileId, "https://later.example/", "Later", true);
		await vi.waitFor(async () => {
			const parsed = JSON.parse(await readFile(file, "utf8")) as { entries: BrowserHistoryEntry[] };
			expect(parsed.entries.map((entry) => entry.url)).toEqual(["https://later.example/"]);
		});
	});

	it("reads legacy pretty-printed history files and keeps them sorted on record", async () => {
		const stateDir = await temporaryState();
		const directory = path.join(stateDir, "browser-history");
		await mkdir(directory, { recursive: true });
		await writeFile(path.join(directory, `${profileId}.json`), `${JSON.stringify({
			version: 1,
			entries: [
				{ url: "https://older.example/", title: "Older", lastVisited: "2026-01-01T00:00:00.000Z", visitCount: 1 },
				{ url: "https://newer.example/", title: "Newer", lastVisited: "2026-02-01T00:00:00.000Z", visitCount: 3 },
			],
		}, null, 2)}\n`);
		const store = new BrowserHistoryStore({
			stateDir,
			now: () => new Date("2026-01-15T00:00:00.000Z"),
			writeDelayMs: 60_000,
		});

		await store.record(profileId, "https://middle.example/", "Middle", true);
		await store.drain();
		const parsed = JSON.parse(await readFile(path.join(directory, `${profileId}.json`), "utf8")) as {
			entries: BrowserHistoryEntry[];
		};
		expect(parsed.entries.map((entry) => entry.url)).toEqual([
			"https://newer.example/",
			"https://middle.example/",
			"https://older.example/",
		]);
	});
});
