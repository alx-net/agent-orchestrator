/**
 * Restarts a daemon that died on its own, with backoff, and never one that was
 * stopped on purpose. Without it a crashed daemon (a panic, SIGQUIT for a
 * goroutine dump, the OOM killer) left the app with no backend until someone
 * pressed Restart.
 */

export type DaemonExit = { code: number | null; signal: NodeJS.Signals | string | null };

/**
 * A daemon exits 0 when it was asked to stop: `ao stop` and /shutdown, SIGTERM,
 * or its supervisor link dropping as the app quits. Anything else is a crash.
 */
export function isUnexpectedDaemonExit(exit: DaemonExit): boolean {
	return exit.signal != null || exit.code !== 0;
}

export type RunFileSnapshot = { pid: number; owner?: string } | null;

/**
 * An attached daemon (one this app did not spawn) has no exit event, only a
 * port that stops answering. A graceful stop removes running.json, so a run
 * file that still names a dead process is what a crash leaves behind. A live
 * process that stopped answering is hung, not gone, and is left alone.
 */
export function attachedDaemonCrashed(runFile: RunFileSnapshot, isAlive: (pid: number) => boolean): boolean {
	return runFile !== null && runFile.pid > 0 && !isAlive(runFile.pid);
}

export const DAEMON_RESTART_DELAYS_MS = [1_000, 2_000, 5_000, 10_000, 30_000];
/** Consecutive crashes before giving up and leaving the failure banner up. */
export const DAEMON_RESTART_MAX_ATTEMPTS = 5;
/** A daemon that stayed up this long before crashing starts the backoff over. */
export const DAEMON_RESTART_STABLE_MS = 60_000;

type Timer = ReturnType<typeof setTimeout>;

export function createDaemonAutoRestart(options: {
	restart(): void;
	/** False while the app quits or installs an update. */
	canRestart(): boolean;
	now?: () => number;
	setTimer?: (fn: () => void, ms: number) => Timer;
	clearTimer?: (timer: Timer) => void;
}) {
	const now = options.now ?? Date.now;
	const setTimer = options.setTimer ?? ((fn: () => void, ms: number): Timer => setTimeout(fn, ms));
	const clearTimer = options.clearTimer ?? ((t: Timer) => clearTimeout(t));
	let attempts = 0;
	let readySince: number | null = null;
	let timer: Timer | null = null;

	return {
		/** The daemon is up. Repeated calls keep the first time. */
		ready(): void {
			if (readySince === null) readySince = now();
		},
		/**
		 * The daemon died on its own. Schedules a restart and returns its delay,
		 * or null when one is already pending, restarting is blocked, or it has
		 * crashed too often in a row.
		 */
		unexpectedExit(): number | null {
			if (readySince !== null && now() - readySince >= DAEMON_RESTART_STABLE_MS) attempts = 0;
			readySince = null;
			if (timer !== null || !options.canRestart()) return null;
			if (attempts >= DAEMON_RESTART_MAX_ATTEMPTS) return null;
			const delay = DAEMON_RESTART_DELAYS_MS[Math.min(attempts, DAEMON_RESTART_DELAYS_MS.length - 1)];
			attempts += 1;
			timer = setTimer(() => {
				timer = null;
				// Checked again: a quit or update may have started while waiting.
				if (options.canRestart()) options.restart();
			}, delay);
			return delay;
		},
		/** An intentional stop or a manual restart: drop any pending restart and start over. */
		cancel(): void {
			if (timer !== null) clearTimer(timer);
			timer = null;
			attempts = 0;
			readySince = null;
		},
	};
}
