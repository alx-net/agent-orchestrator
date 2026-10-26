import { randomUUID } from "node:crypto";
import { mkdir, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import {
	BROWSER_IMPORT_MAX_HISTORY_ENTRIES,
	type BrowserHistorySuggestion,
} from "../shared/browser-profile-import";
import { isBrowserProfileId } from "../shared/browser-profiles";

const HISTORY_VERSION = 1 as const;
const HISTORY_DIRECTORY = "browser-history";
const HISTORY_MAX_FILE_BYTES = 4 * 1024 * 1024;
const HISTORY_MAX_URL_LENGTH = 4_096;
const HISTORY_MAX_TITLE_LENGTH = 512;
const HISTORY_MAX_SUGGESTIONS = 8;
const HISTORY_WRITE_DELAY_MS = 1_000;

export type BrowserHistoryEntry = {
	url: string;
	title?: string;
	lastVisited: string;
	visitCount: number;
};

type BrowserHistoryFile = {
	version: typeof HISTORY_VERSION;
	entries: BrowserHistoryEntry[];
};

export type BrowserHistoryStoreOptions = {
	stateDir: string;
	now?: () => Date;
	// Navigation records within this window share one disk write; drain()
	// flushes pending writes immediately.
	writeDelayMs?: number;
};

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function normalizeEntry(value: unknown): BrowserHistoryEntry | null {
	if (!isRecord(value) || typeof value.url !== "string" || value.url.length > HISTORY_MAX_URL_LENGTH) return null;
	let parsed: URL;
	try {
		parsed = new URL(value.url);
	} catch {
		return null;
	}
	if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return null;
	if (
		typeof value.lastVisited !== "string" ||
		!Number.isFinite(Date.parse(value.lastVisited)) ||
		typeof value.visitCount !== "number" ||
		!Number.isSafeInteger(value.visitCount) ||
		value.visitCount < 1
	) {
		return null;
	}
	const title =
		typeof value.title === "string" && value.title.trim().length > 0
			? value.title.trim().slice(0, HISTORY_MAX_TITLE_LENGTH)
			: undefined;
	return {
		url: parsed.href,
		...(title ? { title } : {}),
		lastVisited: value.lastVisited,
		visitCount: value.visitCount,
	};
}

function parseHistory(raw: unknown): BrowserHistoryFile {
	if (!isRecord(raw) || raw.version !== HISTORY_VERSION || !Array.isArray(raw.entries)) {
		throw new Error("The browser history file has an unsupported format.");
	}
	if (raw.entries.length > BROWSER_IMPORT_MAX_HISTORY_ENTRIES) {
		throw new Error("The browser history file exceeds the entry limit.");
	}
	const entries = raw.entries.map(normalizeEntry);
	if (entries.some((entry) => entry === null)) throw new Error("The browser history file is invalid.");
	return { version: HISTORY_VERSION, entries: entries as BrowserHistoryEntry[] };
}

function compact(entries: BrowserHistoryEntry[]): BrowserHistoryEntry[] {
	const byURL = new Map<string, BrowserHistoryEntry>();
	for (const entry of entries) {
		const normalized = normalizeEntry(entry);
		if (!normalized) continue;
		const existing = byURL.get(normalized.url);
		if (!existing) {
			byURL.set(normalized.url, normalized);
			continue;
		}
		const newer = Date.parse(normalized.lastVisited) >= Date.parse(existing.lastVisited) ? normalized : existing;
		byURL.set(normalized.url, {
			...newer,
			visitCount: Math.min(Number.MAX_SAFE_INTEGER, existing.visitCount + normalized.visitCount),
		});
	}
	// Parse each timestamp once rather than twice per comparison.
	return [...byURL.values()]
		.map((entry) => ({ entry, time: Date.parse(entry.lastVisited) }))
		.sort((a, b) => b.time - a.time || a.entry.url.localeCompare(b.entry.url))
		.map(({ entry }) => entry);
}

function newerFirst(a: BrowserHistoryEntry, b: BrowserHistoryEntry): number {
	return Date.parse(b.lastVisited) - Date.parse(a.lastVisited) || a.url.localeCompare(b.url);
}

// Insert into a list already in compact() order.
function insertSorted(entries: BrowserHistoryEntry[], entry: BrowserHistoryEntry): void {
	let low = 0;
	let high = entries.length;
	while (low < high) {
		const middle = (low + high) >>> 1;
		if (newerFirst(entries[middle]!, entry) <= 0) low = middle + 1;
		else high = middle;
	}
	entries.splice(low, 0, entry);
}

function fittedHistory(entries: BrowserHistoryEntry[]): { entries: BrowserHistoryEntry[]; serialized: string } {
	// Compact JSON; older pretty-printed files still parse. Size each entry
	// once and keep the newest prefix that fits the file limit.
	const prefix = `{"version":${HISTORY_VERSION},"entries":[`;
	const suffix = "]}\n";
	const parts: string[] = [];
	let bytes = Buffer.byteLength(prefix) + Buffer.byteLength(suffix);
	for (const entry of entries) {
		const part = JSON.stringify(entry);
		const next = bytes + Buffer.byteLength(part) + (parts.length > 0 ? 1 : 0);
		if (next > HISTORY_MAX_FILE_BYTES) break;
		parts.push(part);
		bytes = next;
	}
	return { entries: entries.slice(0, parts.length), serialized: `${prefix}${parts.join(",")}${suffix}` };
}

export class BrowserHistoryStore {
	private readonly directory: string;
	private readonly now: () => Date;
	private readonly writeDelayMs: number;
	// Per profile: entries in compact() order. Arrays are replaced, never
	// mutated, once stored here.
	private readonly cache = new Map<string, BrowserHistoryEntry[]>();
	private readonly queues = new Map<string, Promise<void>>();
	private readonly pendingWrites = new Map<string, ReturnType<typeof setTimeout>>();

	constructor(options: BrowserHistoryStoreOptions) {
		this.directory = path.join(options.stateDir, HISTORY_DIRECTORY);
		this.now = options.now ?? (() => new Date());
		this.writeDelayMs = options.writeDelayMs ?? HISTORY_WRITE_DELAY_MS;
	}

	private file(profileId: string): string {
		if (!isBrowserProfileId(profileId)) throw new Error("Browser profile ID is invalid.");
		return path.join(this.directory, `${profileId}.json`);
	}

	private async load(profileId: string): Promise<BrowserHistoryEntry[]> {
		const cached = this.cache.get(profileId);
		if (cached) return cached;
		const file = this.file(profileId);
		try {
			const metadata = await stat(file);
			if (!metadata.isFile() || metadata.size > HISTORY_MAX_FILE_BYTES) {
				throw new Error("The browser history file exceeds the size limit.");
			}
			const parsed = parseHistory(JSON.parse(await readFile(file, "utf8")));
			// A concurrent caller may have loaded and updated the cache meanwhile.
			const loaded = this.cache.get(profileId);
			if (loaded) return loaded;
			const entries = compact(parsed.entries);
			this.cache.set(profileId, entries);
			return entries;
		} catch (error) {
			if ((error as NodeJS.ErrnoException).code === "ENOENT") {
				const loaded = this.cache.get(profileId);
				if (loaded) return loaded;
				this.cache.set(profileId, []);
				return [];
			}
			throw error;
		}
	}

	private async write(profileId: string, entries: BrowserHistoryEntry[]): Promise<BrowserHistoryEntry[]> {
		await mkdir(this.directory, { recursive: true, mode: 0o750 });
		const file = this.file(profileId);
		const temporary = path.join(this.directory, `.${profileId}.${process.pid}.${randomUUID()}.tmp`);
		const fitted = fittedHistory(entries);
		try {
			await writeFile(temporary, fitted.serialized, { mode: 0o600 });
			await rename(temporary, file);
			return fitted.entries;
		} catch (error) {
			await rm(temporary, { force: true }).catch(() => undefined);
			throw error;
		}
	}

	private enqueue(profileId: string, operation: () => Promise<void>): Promise<void> {
		const previous = this.queues.get(profileId) ?? Promise.resolve();
		const queued = previous.then(operation, operation);
		const tail = queued.then(
			() => undefined,
			() => undefined,
		);
		this.queues.set(profileId, tail);
		void tail.then(() => {
			if (this.queues.get(profileId) === tail) this.queues.delete(profileId);
		});
		return queued;
	}

	private scheduleWrite(profileId: string): void {
		if (this.pendingWrites.has(profileId)) return;
		const timer = setTimeout(() => void this.flush(profileId).catch(() => undefined), this.writeDelayMs);
		timer.unref?.();
		this.pendingWrites.set(profileId, timer);
	}

	private cancelWrite(profileId: string): void {
		const timer = this.pendingWrites.get(profileId);
		if (timer === undefined) return;
		clearTimeout(timer);
		this.pendingWrites.delete(profileId);
	}

	private flush(profileId: string): Promise<void> {
		this.cancelWrite(profileId);
		return this.enqueue(profileId, async () => {
			const entries = this.cache.get(profileId);
			if (entries) this.cache.set(profileId, await this.write(profileId, entries));
		});
	}

	// Flushes coalesced record() writes and waits for queued work. The browser
	// view host awaits this during app quit.
	async drain(): Promise<void> {
		while (this.queues.size > 0 || this.pendingWrites.size > 0) {
			for (const profileId of [...this.pendingWrites.keys()]) void this.flush(profileId).catch(() => undefined);
			await Promise.all([...this.queues.values()]);
		}
	}

	async mergeImportedEntries(
		profileId: string,
		imported: BrowserHistoryEntry[],
	): Promise<{ imported: number; truncated: number }> {
		let importedCount = 0;
		let truncated = 0;
		await this.enqueue(profileId, async () => {
			const normalized = compact(imported);
			const combined = compact([...(await this.load(profileId)), ...normalized])
				.slice(0, BROWSER_IMPORT_MAX_HISTORY_ENTRIES);
			// This write includes any coalesced record() changes.
			this.cancelWrite(profileId);
			const entries = await this.write(profileId, combined);
			const retainedURLs = new Set(entries.map((entry) => entry.url));
			importedCount = normalized.filter((entry) => retainedURLs.has(entry.url)).length;
			truncated = normalized.length - importedCount;
			this.cache.set(profileId, entries);
		});
		return { imported: importedCount, truncated };
	}

	async record(profileId: string, rawURL: string, rawTitle: string, incrementVisit: boolean): Promise<void> {
		const normalized = normalizeEntry({
			url: rawURL,
			title: rawTitle,
			lastVisited: this.now().toISOString(),
			visitCount: 1,
		});
		if (!normalized) return;
		// Runs on every navigation: update only the changed entry in the sorted
		// cache and coalesce the disk write.
		await this.enqueue(profileId, async () => {
			const current = await this.load(profileId);
			const index = current.findIndex((entry) => entry.url === normalized.url);
			const existing = index >= 0 ? current[index] : undefined;
			const nextEntry = existing
				? {
						...existing,
						...(normalized.title ? { title: normalized.title } : {}),
						lastVisited: normalized.lastVisited,
						visitCount: incrementVisit ? Math.min(Number.MAX_SAFE_INTEGER, existing.visitCount + 1) : existing.visitCount,
					}
				: normalized;
			const entries = current.slice();
			if (index >= 0) entries.splice(index, 1);
			insertSorted(entries, nextEntry);
			entries.length = Math.min(entries.length, BROWSER_IMPORT_MAX_HISTORY_ENTRIES);
			this.cache.set(profileId, entries);
			this.scheduleWrite(profileId);
		});
	}

	async suggest(profileId: string, rawQuery: string): Promise<BrowserHistorySuggestion[]> {
		const query = rawQuery.trim().toLowerCase().slice(0, 512);
		if (!query) return [];
		const entries = await this.load(profileId);
		return entries
			.map((entry) => {
				const title = entry.title?.toLowerCase() ?? "";
				const url = entry.url.toLowerCase();
				const score = url.startsWith(query) ? 0 : title.startsWith(query) ? 1 : url.includes(query) ? 2 : title.includes(query) ? 3 : -1;
				return { entry, score };
			})
			.filter((candidate) => candidate.score >= 0)
			.sort((a, b) => a.score - b.score || Date.parse(b.entry.lastVisited) - Date.parse(a.entry.lastVisited))
			.slice(0, HISTORY_MAX_SUGGESTIONS)
			.map(({ entry }) => ({ url: entry.url, ...(entry.title ? { title: entry.title } : {}) }));
	}

	async clear(profileId: string): Promise<void> {
		await this.enqueue(profileId, async () => {
			this.cancelWrite(profileId);
			await rm(this.file(profileId), { force: true });
			this.cache.delete(profileId);
		});
	}
}
