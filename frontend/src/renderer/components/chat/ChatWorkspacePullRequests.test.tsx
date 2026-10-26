/**
 * The Pull requests tab inside the chat: a tab after Agents, offered once the
 * session has a PR, that covers the timeline with one merge box per PR while the
 * composer stays where it is.
 */

import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }));

vi.mock("../../lib/api-client", async (importOriginal) => ({
	...await importOriginal<typeof import("../../lib/api-client")>(),
	apiClient: { GET: getMock, POST: vi.fn() },
}));

import { ChatWorkspace } from "./ChatWorkspace";
import { chatFixture } from "../../lib/chat-fixture";
import { lexicalEditorText } from "../../test/lexical";
import { renderChat as render } from "../../test/render-chat";
import type { MergeBoxPR } from "../../lib/pr-merge-box";
import type { ConversationSnapshot } from "../../types/conversation";
import type { WorkspaceSession } from "../../types/workspace";

const failingRun = "https://github.com/acme/app/actions/runs/7/job/8";

function summary(number: number, overrides: Partial<MergeBoxPR>): MergeBoxPR {
	const htmlUrl = `https://github.com/acme/app/pull/${number}`;
	return {
		url: htmlUrl,
		htmlUrl,
		number,
		title: `PR ${number}`,
		state: "open",
		provider: "github",
		repo: "acme/app",
		author: "ada",
		sourceBranch: `feat/${number}`,
		targetBranch: "main",
		headSha: `sha-${number}`,
		additions: 1,
		deletions: 1,
		changedFiles: 1,
		ci: { autoInjectCI: true, state: "passing", failingChecks: [] },
		review: { decision: "none", hasUnresolvedHumanComments: false, unresolvedBy: [] },
		mergeability: { state: "mergeable", reasons: [], prUrl: htmlUrl, conflictFiles: [] },
		updatedAt: new Date().toISOString(),
		...overrides,
	};
}

const failing = summary(41, {
	ci: { autoInjectCI: true, state: "failing", failingChecks: [] },
	mergeability: { state: "blocked", reasons: ["ci_failing"], prUrl: "https://github.com/acme/app/pull/41", conflictFiles: [] },
	checks: [{ name: "test", workflow: "CI", status: "failed", required: true, url: failingRun }],
});
const clean = summary(42, { checks: [{ name: "lint", status: "passed", required: true }] });

const session = {
	id: chatFixture.sessionId,
	workspaceId: "project-1",
	workspaceName: "agent-orchestrator",
	title: "Harden auth",
	provider: "claude-code",
	kind: "worker",
	mode: "chat",
	status: "ci_failed",
	updatedAt: "2026-08-15T00:00:00Z",
	activity: { state: "active", lastActivityAt: "2026-08-15T00:00:00Z" },
	prs: [failing, clean].map((pr) => ({
		url: pr.url,
		number: pr.number,
		state: pr.state,
		ci: pr.ci.state,
		review: pr.review.decision,
		mergeability: pr.mergeability.state,
		reviewComments: false,
		updatedAt: pr.updatedAt,
	})),
} satisfies WorkspaceSession;

// The fixture's pending approval would take the composer's place.
const snapshot: ConversationSnapshot = {
	...chatFixture,
	items: chatFixture.items.filter((item) => item.kind !== "activity" || item.status !== "pending"),
};

function serveSummary() {
	getMock.mockImplementation(async (path: string) => path === "/api/v1/sessions/{sessionId}/pr"
		? { data: { prs: [failing, clean], linkedPrs: [] } }
		: { data: undefined });
}

describe("ChatWorkspace pull requests tab", () => {
	it("is offered only once the session has a PR, with the count tinted by failing CI", async () => {
		serveSummary();
		const view = render(<ChatWorkspace snapshot={snapshot} session={session} />);
		const tab = screen.getByRole("tab", { name: /Pull requests/ });
		expect(tab).toHaveAttribute("aria-selected", "false");
		await waitFor(() => expect(within(tab).getByText("2")).toHaveClass("text-status-exited"));

		view.rerender(<ChatWorkspace snapshot={snapshot} session={{ ...session, prs: [] }} />);
		expect(screen.queryByRole("tab", { name: /Pull requests/ })).not.toBeInTheDocument();
	});

	it("covers the timeline with the merge boxes, keeps the composer, and drafts a fix back in the chat", async () => {
		serveSummary();
		const user = userEvent.setup();
		const onSelectChat = vi.fn();
		render(<ChatWorkspace snapshot={snapshot} session={session} onSelectChat={onSelectChat} />);
		await user.click(screen.getByRole("tab", { name: /Pull requests/ }));

		expect(screen.getByRole("tab", { name: /Pull requests/ })).toHaveAttribute("aria-selected", "true");
		expect(onSelectChat).toHaveBeenCalled();
		const panel = screen.getByTestId("chat-pull-requests-panel");
		await waitFor(() => expect(within(panel).getAllByTestId("pr-merge-box")).toHaveLength(2));
		expect(screen.getByRole("log", { hidden: true }).closest("[aria-hidden='true']")).not.toBeNull();
		expect(within(panel).getByRole("button", { name: "Merge PR #42" })).toBeInTheDocument();
		expect(within(panel).queryByRole("button", { name: "Merge PR #41" })).not.toBeInTheDocument();
		const field = screen.getByLabelText("Message the agent");

		await user.click(await within(panel).findByRole("button", { name: "Fix this: CI / test" }));
		await waitFor(() => expect(lexicalEditorText(field)).toBe(`Fix the failing check "CI / test" on PR #41: ${failingRun}`));
		expect(screen.queryByTestId("chat-pull-requests-panel")).not.toBeInTheDocument();
		expect(screen.getByRole("tab", { name: /Pull requests/ })).toHaveAttribute("aria-selected", "false");
	});
});
