import { parentPort, workerData } from "node:worker_threads";
import {
	runBrowserImportReadTask,
	type BrowserImportReadReply,
	type BrowserImportReadTask,
} from "./browser-profile-import";

// Worker entry (built next to main.js by Forge). One staged-snapshot read per
// worker: better-sqlite3 and cookie decryption are synchronous, so running
// them here keeps a large import from freezing every window.
let reply: BrowserImportReadReply;
try {
	reply = { ok: true, result: runBrowserImportReadTask(workerData as BrowserImportReadTask) };
} catch (error) {
	reply = { ok: false, message: error instanceof Error ? error.message : "Browser data could not be read." };
}
parentPort?.postMessage(reply);
