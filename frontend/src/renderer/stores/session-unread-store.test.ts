import { beforeEach, describe, expect, it } from "vitest";
import type { WorkspaceSummary } from "../types/workspace";
import { SESSION_UNREAD_STORAGE_KEY, useSessionUnreadStore } from "./session-unread-store";

function snapshot(states: Record<string, "active" | "idle" | "waiting_input">): WorkspaceSummary[] {
	return [{
		id: "kugelaudio",
		sessions: Object.entries(states).map(([id, state]) => ({ id, activity: { state, lastActivityAt: "2026-10-29T10:00:00Z" } })),
	}] as unknown as WorkspaceSummary[];
}

const unread = () => Object.keys(useSessionUnreadStore.getState().unread).sort();
const observe = (states: Record<string, "active" | "idle" | "waiting_input">, open?: string) =>
	useSessionUnreadStore.getState().observe(snapshot(states), open);

beforeEach(() => {
	window.localStorage.clear();
	useSessionUnreadStore.setState({ unread: {}, working: {} });
});

describe("session unread tracking", () => {
	it("marks a session whose agent finishes while the user is elsewhere", () => {
		observe({ a: "active", b: "active" }, "b");
		observe({ a: "idle", b: "active" }, "b");
		expect(unread()).toEqual(["a"]);
		expect(JSON.parse(window.localStorage.getItem(SESSION_UNREAD_STORAGE_KEY) ?? "[]")).toEqual(["a"]);
	});

	it("counts stopping to ask the user as finished too", () => {
		observe({ a: "active" });
		observe({ a: "waiting_input" });
		expect(unread()).toEqual(["a"]);
	});

	it("never marks the session the user has open", () => {
		observe({ a: "active" }, "a");
		observe({ a: "idle" }, "a");
		expect(unread()).toEqual([]);
	});

	it("marks nothing on the first snapshot after a launch", () => {
		observe({ a: "idle", b: "waiting_input" });
		expect(unread()).toEqual([]);
	});

	it("clears a session once the user opens it", () => {
		observe({ a: "active" });
		observe({ a: "idle" });
		observe({ a: "idle" }, "a");
		expect(unread()).toEqual([]);
		expect(window.localStorage.getItem(SESSION_UNREAD_STORAGE_KEY)).toBe("[]");
	});

	it("forgets sessions that no longer exist", () => {
		observe({ a: "active", b: "active" });
		observe({ a: "idle", b: "idle" });
		observe({ b: "idle" });
		expect(unread()).toEqual(["b"]);
	});
});
