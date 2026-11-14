import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
	DAEMON_RESTART_DELAYS_MS,
	DAEMON_RESTART_MAX_ATTEMPTS,
	DAEMON_RESTART_STABLE_MS,
	attachedDaemonCrashed,
	createDaemonAutoRestart,
	isUnexpectedDaemonExit,
} from "./daemon-auto-restart";

describe("isUnexpectedDaemonExit", () => {
	it("treats exit 0 as a requested stop and anything else as a crash", () => {
		expect(isUnexpectedDaemonExit({ code: 0, signal: null })).toBe(false);
		// Go's SIGQUIT handler dumps stacks and exits 2.
		expect(isUnexpectedDaemonExit({ code: 2, signal: null })).toBe(true);
		expect(isUnexpectedDaemonExit({ code: null, signal: "SIGKILL" })).toBe(true);
		expect(isUnexpectedDaemonExit({ code: null, signal: null })).toBe(true);
	});
});

describe("attachedDaemonCrashed", () => {
	it("is a crash only when the run file still names a dead process", () => {
		expect(attachedDaemonCrashed({ pid: 42 }, () => false)).toBe(true);
		// A graceful stop removed the run file.
		expect(attachedDaemonCrashed(null, () => false)).toBe(false);
		// Alive but not answering is hung, not gone.
		expect(attachedDaemonCrashed({ pid: 42 }, () => true)).toBe(false);
		expect(attachedDaemonCrashed({ pid: 0 }, () => false)).toBe(false);
	});
});

describe("createDaemonAutoRestart", () => {
	let now = 0;
	beforeEach(() => {
		vi.useFakeTimers();
		now = 0;
	});
	afterEach(() => vi.useRealTimers());

	function setup(canRestart = () => true) {
		const restart = vi.fn();
		const auto = createDaemonAutoRestart({ restart, canRestart, now: () => now });
		return { restart, auto };
	}

	it("restarts after a crash with growing delays, then gives up", () => {
		const { restart, auto } = setup();
		for (let i = 0; i < DAEMON_RESTART_MAX_ATTEMPTS; i++) {
			const delay = auto.unexpectedExit();
			expect(delay).toBe(DAEMON_RESTART_DELAYS_MS[Math.min(i, DAEMON_RESTART_DELAYS_MS.length - 1)]);
			vi.advanceTimersByTime(delay ?? 0);
			expect(restart).toHaveBeenCalledTimes(i + 1);
		}
		expect(auto.unexpectedExit()).toBeNull();
		vi.runAllTimers();
		expect(restart).toHaveBeenCalledTimes(DAEMON_RESTART_MAX_ATTEMPTS);
	});

	it("schedules one restart for repeated reports of the same crash", () => {
		const { restart, auto } = setup();
		expect(auto.unexpectedExit()).toBe(DAEMON_RESTART_DELAYS_MS[0]);
		expect(auto.unexpectedExit()).toBeNull();
		vi.runAllTimers();
		expect(restart).toHaveBeenCalledOnce();
	});

	it("starts the backoff over after the daemon stayed up", () => {
		const { auto } = setup();
		auto.unexpectedExit();
		vi.runAllTimers();
		auto.unexpectedExit();
		vi.runAllTimers();
		auto.ready();
		now += DAEMON_RESTART_STABLE_MS;
		expect(auto.unexpectedExit()).toBe(DAEMON_RESTART_DELAYS_MS[0]);
	});

	it("keeps backing off when the daemon crashes soon after starting", () => {
		const { auto } = setup();
		auto.unexpectedExit();
		vi.runAllTimers();
		auto.ready();
		now += 1_000;
		expect(auto.unexpectedExit()).toBe(DAEMON_RESTART_DELAYS_MS[1]);
	});

	it("never restarts while the app quits or updates", () => {
		let quitting = true;
		const { restart, auto } = setup(() => !quitting);
		expect(auto.unexpectedExit()).toBeNull();
		quitting = false;
		auto.unexpectedExit();
		quitting = true;
		vi.runAllTimers();
		expect(restart).not.toHaveBeenCalled();
	});

	it("drops a pending restart on an intentional stop and resets the backoff", () => {
		const { restart, auto } = setup();
		for (let i = 0; i < DAEMON_RESTART_MAX_ATTEMPTS; i++) {
			auto.unexpectedExit();
			vi.runAllTimers();
		}
		restart.mockClear();
		auto.cancel();
		auto.unexpectedExit();
		auto.cancel();
		vi.runAllTimers();
		expect(restart).not.toHaveBeenCalled();
		expect(auto.unexpectedExit()).toBe(DAEMON_RESTART_DELAYS_MS[0]);
	});
});
