import { describe, expect, it } from "vitest";
import { memoryLine } from "./memory-log";

describe("memoryLine", () => {
	it("sums MB per process type, largest first", () => {
		const line = memoryLine([
			{ type: "Renderer", pid: 1, memory: { workingSetSize: 700 * 1024 } },
			{ type: "Renderer", pid: 2, memory: { workingSetSize: 100 * 1024 } },
			{ type: "Browser", pid: 3, memory: { workingSetSize: 150 * 1024 } },
		], new Date("2026-11-15T10:00:00Z"));
		expect(line).toBe("2026-11-15T10:00:00.000Z total=950MB Renderer=800MBx2 Browser=150MB\n");
	});
});
