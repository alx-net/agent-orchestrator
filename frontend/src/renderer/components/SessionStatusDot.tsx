import { LoaderCircle } from "lucide-react";
import { getSessionStatusDotView } from "../lib/session-presentation";
import { cn } from "../lib/utils";
import type { WorkspaceSession } from "../types/workspace";

// Colour tracks the session's board section, preserving SCM state while the
// agent runs; motion stays on raw agent activity. A no-PR idle session turns
// blue when it starts working. See getSessionStatusDotView for the lane mapping.
// Shared by the sidebar's session rows and the PRs page's session rows.
export function SessionStatusDot({ session }: { session: WorkspaceSession }) {
	const dot = getSessionStatusDotView(session);
	return (
		<span
			aria-hidden="true"
			className="relative z-[1] inline-flex shrink-0 items-center justify-center px-1.5"
		>
			{dot.breathe ? (
				// A spinner reads as "running" far more clearly than a pulsing dot.
				<LoaderCircle
					className={cn(
						"size-3 animate-spin motion-reduce:animate-none",
						dot.className.replace(/^bg-/, "text-"),
					)}
					data-session-status={session.status}
					data-working="true"
				/>
			) : (
				<span
					className={cn("size-2 rounded-full", dot.className)}
					data-session-status={session.status}
				/>
			)}
		</span>
	);
}
