// @vitest-environment node
import { mkdir, mkdtemp, rm } from "node:fs/promises";
import { builtinModules, createRequire } from "node:module";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import Database from "better-sqlite3";
import { build } from "vite";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { BrowserHistoryStore } from "./browser-history-store";
import { BrowserProfileImportService } from "./browser-profile-import";
import { BrowserProfileStore } from "./browser-profile-store";

let root: string;
let workerFile: string;

beforeAll(async () => {
	root = await mkdtemp(path.join(os.tmpdir(), "ao-browser-import-worker-"));
	// Build the worker entry the way Forge builds a main target: a CommonJS lib
	// with Node builtins and better-sqlite3 left external.
	const outDir = path.join(root, "build");
	await build({
		configFile: false,
		logLevel: "silent",
		build: {
			outDir,
			minify: false,
			lib: {
				entry: path.join(path.dirname(fileURLToPath(import.meta.url)), "browser-profile-import-worker.ts"),
				formats: ["cjs"],
				fileName: () => "browser-profile-import-worker.js",
			},
			rollupOptions: {
				external: ["better-sqlite3", "electron", ...builtinModules, ...builtinModules.map((name) => `node:${name}`)],
				output: { paths: { "better-sqlite3": createRequire(import.meta.url).resolve("better-sqlite3") } },
			},
		},
	});
	workerFile = path.join(outDir, "browser-profile-import-worker.js");
}, 60_000);

afterAll(async () => {
	await rm(root, { recursive: true, force: true });
});

async function firefoxFixture(name: string, withHistoryTable: boolean): Promise<string> {
	const appData = path.join(root, name, "roaming");
	const profileRoot = path.join(appData, "Mozilla", "Firefox", "Profiles", `${name}.default-release`);
	await mkdir(profileRoot, { recursive: true });
	const cookies = new Database(path.join(profileRoot, "cookies.sqlite"));
	cookies.exec(`
		CREATE TABLE moz_cookies (
			host TEXT, name TEXT, value TEXT, path TEXT, expiry INTEGER,
			isSecure INTEGER, isHttpOnly INTEGER, sameSite INTEGER,
			originAttributes TEXT, lastAccessed INTEGER, creationTime INTEGER
		);
		INSERT INTO moz_cookies VALUES ('.example.com', 'session', 'from-worker', '/', 1893456000, 1, 1, 1, '', 2, 1);
		INSERT INTO moz_cookies VALUES ('.container.example', 'container', 'private', '/', 1893456000, 1, 1, 1, '^userContextId=2', 3, 2);
	`);
	cookies.close();
	const history = new Database(path.join(profileRoot, "places.sqlite"));
	history.exec(withHistoryTable
		? `CREATE TABLE moz_places (url TEXT, title TEXT, visit_count INTEGER, last_visit_date INTEGER);
			INSERT INTO moz_places VALUES ('https://example.com/worker', 'Worker read', 2, 1767225600000000);`
		: "CREATE TABLE unrelated (id INTEGER)");
	history.close();
	return appData;
}

function createService(appData: string, setCookie: (details: unknown) => Promise<void>, readWorkerFile = workerFile) {
	const stateDir = path.join(appData, "..", "ao-state");
	const profileStore = new BrowserProfileStore({ stateDir });
	const service = new BrowserProfileImportService({
		stateDir,
		profileStore,
		historyStore: new BrowserHistoryStore({ stateDir }),
		platform: "win32",
		homeDir: root,
		env: { APPDATA: appData },
		now: () => new Date("2026-01-01T00:00:00.000Z"),
		readWorkerFile,
		fromPartition: () => ({
			cookies: { set: setCookie },
			clearStorageData: async () => undefined,
			clearCache: async () => undefined,
		}),
	});
	return { service, profileStore };
}

describe("browser profile import worker", () => {
	it("reads snapshot cookies and history on a worker thread", async () => {
		const appData = await firefoxFixture("worker-ok", true);
		const setCookie = vi.fn(async () => undefined);
		const { service, profileStore } = createService(appData, setCookie);
		await profileStore.load();
		const source = (await service.discover()).sources[0]!;

		const result = await service.import({
			requestId: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
			sourceId: source.id,
			profileIds: [source.profiles[0]!.id],
			includeCookies: true,
			includeHistory: true,
			destination: { mode: "merge", name: "Worker Firefox" },
		}, vi.fn());

		expect(result.entries[0]).toMatchObject({ importedCookies: 1, importedHistoryEntries: 1, skippedCookies: 1 });
		expect(result.entries[0]!.warnings).toContainEqual({ code: "isolated-cookies-skipped", count: 1 });
		expect(setCookie).toHaveBeenCalledWith(expect.objectContaining({ name: "session", value: "from-worker" }));
	});

	it("surfaces a worker read failure without creating a destination", async () => {
		const appData = await firefoxFixture("worker-bad", false);
		const { service, profileStore } = createService(appData, vi.fn(async () => undefined));
		await profileStore.load();
		const source = (await service.discover()).sources[0]!;

		await expect(service.import({
			requestId: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
			sourceId: source.id,
			profileIds: [source.profiles[0]!.id],
			includeCookies: true,
			includeHistory: true,
			destination: { mode: "merge", name: "Broken Firefox" },
		}, vi.fn())).rejects.toThrow("The selected Firefox profile does not contain supported history data.");
		expect(profileStore.profiles).toEqual([]);
	});

	it("reads on the calling thread when the worker cannot start", async () => {
		const appData = await firefoxFixture("worker-missing", true);
		const setCookie = vi.fn(async () => undefined);
		const warn = vi.spyOn(console, "warn").mockImplementation(() => undefined);
		try {
			const { service, profileStore } = createService(appData, setCookie, path.join(root, "missing-worker.js"));
			await profileStore.load();
			const source = (await service.discover()).sources[0]!;

			const result = await service.import({
				requestId: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
				sourceId: source.id,
				profileIds: [source.profiles[0]!.id],
				includeCookies: true,
				includeHistory: true,
				destination: { mode: "merge", name: "Fallback Firefox" },
			}, vi.fn());

			expect(result.entries[0]).toMatchObject({ importedCookies: 1, importedHistoryEntries: 1 });
			expect(warn).toHaveBeenCalledWith(expect.stringContaining("browser import worker unavailable"), expect.anything());
		} finally {
			warn.mockRestore();
		}
	});
});
