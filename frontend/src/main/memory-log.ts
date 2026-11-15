/**
 * Logs the desktop app's memory, per Chromium process type, every few minutes to
 * <dataDir>/diagnostics/app-memory.log. The window's memory grew to many GB with
 * nothing on disk to say which process or when; this keeps a small record.
 */

import { appendFile, mkdir, rename, stat } from "node:fs/promises";
import path from "node:path";

/** The part of Electron's ProcessMetric this reads. */
export type ProcessMetricLike = { type: string; pid: number; memory: { workingSetSize: number; privateBytes?: number } };

const MAX_LOG_BYTES = 1 << 20;

/** One line: MB per process type (working set; Electron reports KB). */
export function memoryLine(metrics: ProcessMetricLike[], at: Date): string {
	const byType = new Map<string, { mb: number; count: number }>();
	let total = 0;
	for (const metric of metrics) {
		const mb = metric.memory.workingSetSize / 1024;
		total += mb;
		const entry = byType.get(metric.type) ?? { mb: 0, count: 0 };
		entry.mb += mb;
		entry.count += 1;
		byType.set(metric.type, entry);
	}
	const parts = [...byType.entries()]
		.sort((a, b) => b[1].mb - a[1].mb)
		.map(([type, entry]) => `${type}=${Math.round(entry.mb)}MB${entry.count > 1 ? `x${entry.count}` : ""}`);
	return `${at.toISOString()} total=${Math.round(total)}MB ${parts.join(" ")}\n`;
}

/** Starts the periodic log; returns a stop function. */
export function startMemoryLog(
	dataDir: string,
	readMetrics: () => ProcessMetricLike[],
	everyMs = 10 * 60_000,
): () => void {
	const file = path.join(dataDir, "diagnostics", "app-memory.log");
	const write = async () => {
		try {
			await mkdir(path.dirname(file), { recursive: true, mode: 0o700 });
			const size = await stat(file).then((s) => s.size, () => 0);
			// Keep one previous file; the log stays under 2 MB.
			if (size > MAX_LOG_BYTES) await rename(file, `${file}.1`);
			await appendFile(file, memoryLine(readMetrics(), new Date()), { mode: 0o600 });
		} catch {
			// A diagnostics log must never disturb the app.
		}
	};
	void write();
	const timer = setInterval(() => void write(), everyMs);
	timer.unref?.();
	return () => clearInterval(timer);
}
