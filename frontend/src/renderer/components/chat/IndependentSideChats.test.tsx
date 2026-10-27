import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SideComposer } from "./IndependentSideChats";
import { emptySideChatDraft } from "./sideChatDraft";
import { typeInLexicalEditor } from "../../test/lexical";

const side = {
	id: "side-1",
	sessionId: "session-1",
	mainConversationId: "main-1",
	contextMode: "native",
	createdAt: "2026-01-01T00:00:00Z",
	updatedAt: "2026-01-01T00:00:00Z",
	label: "Side",
	state: "ready",
	hasWork: false,
};

function renderSideComposer(
	draft = emptySideChatDraft(),
	flags: { sending?: boolean; ready?: boolean; deliveryPending?: boolean } = {},
	canCreate = true,
) {
	const onSend = vi.fn();
	const onFocusSide = vi.fn();
	const onCompact = vi.fn();
	const onNavigate = vi.fn();
	const onRemoveAllReferences = vi.fn();
	render(
		<SideComposer
			draft={draft}
			onChange={vi.fn()}
			onSend={onSend}
			onInterrupt={vi.fn()}
			onAttach={vi.fn()}
			files={[]}
			onRemoveFile={vi.fn()}
			ready
			running={false}
			sending={false}
			{...flags}
			models={[]}
			skills={[{ name: "review", displayName: "review", source: "repo" }]}
			side={side}
			onSettings={vi.fn()}
			onRemoveReference={vi.fn()}
			onRemoveAllReferences={onRemoveAllReferences}
			onFocusSide={canCreate ? onFocusSide : undefined}
			onCompact={onCompact}
			onNavigate={onNavigate}
			focusKey={0}
		/>,
	);
	return {
		onSend,
		onFocusSide,
		onCompact,
		onNavigate,
		onRemoveAllReferences,
		field: screen.getByLabelText("Side chat question") as HTMLElement,
	};
}

describe("side composer slash menu", () => {
	it("preserves provider commands when a new side cannot open", async () => {
		const { onSend, onFocusSide, field } = renderSideComposer(emptySideChatDraft(), {}, false);
		await typeInLexicalEditor(field, "/btw");
		expect(screen.queryByRole("option", { name: /\/btw/ })).not.toBeInTheDocument();
		fireEvent.keyDown(field, { key: "Enter" });
		await waitFor(() => expect(onSend).toHaveBeenCalledWith("/btw"));
		expect(onFocusSide).not.toHaveBeenCalled();
	});
	it("shows provider skills and opens /btw without sending", async () => {
		const { onSend, onFocusSide, field } = renderSideComposer();
		await typeInLexicalEditor(field, "/");
		expect(screen.getByRole("option", { name: /\/review/ })).toBeInTheDocument();
		expect(screen.getByRole("option", { name: /\/compact/ })).toBeInTheDocument();
		await userEvent.click(screen.getByRole("option", { name: /\/btw/ }));
		await waitFor(() => expect(onFocusSide).toHaveBeenCalled());
		expect(onSend).not.toHaveBeenCalled();
	});

	it("shows ordered references in a removable compact summary", async () => {
		const draft = {
			...emptySideChatDraft(),
			attachments: [{ id: "image", path: ".ao/attachments/shot.png", name: "shot.png", mimeType: "image/png", bytes: 4 }],
			references: [
				{
					id: "one",
					conversationId: "main",
					messageId: "m1",
					revision: 1,
					text: "sun",
					role: "assistant" as const,
				},
				{
					id: "two",
					conversationId: "side",
					messageId: "m2",
					revision: 1,
					text: "moon",
					role: "user" as const,
				},
			],
		};
		const { onRemoveAllReferences, onNavigate } = renderSideComposer(draft);
		const row = screen.getByTestId("side-chat-attachments-and-annotations");
		expect(row).toHaveClass("flex", "flex-wrap", "items-center");
		expect(row).toContainElement(screen.getByRole("button", { name: "2 annotations" }));
		expect(row).toContainElement(screen.getByRole("button", { name: "Remove shot.png" }));
		await userEvent.click(screen.getByRole("button", { name: "2 annotations" }));
		expect(screen.getByRole("button", { name: "sun" })).toBeInTheDocument();
		expect(screen.getAllByRole("button", { name: /Remove annotation/ })).toHaveLength(2);
		await userEvent.click(screen.getByRole("button", { name: "Remove all annotations" }));
		expect(onRemoveAllReferences).toHaveBeenCalledOnce();
		expect(onNavigate).not.toHaveBeenCalled();
	});

	it("keeps a typed bare /btw in this side without sending", async () => {
		const { onSend, onFocusSide, field } = renderSideComposer();
		await typeInLexicalEditor(field, "/btw");
		fireEvent.keyDown(field, { key: "Enter" });
		await waitFor(() => expect(onFocusSide).toHaveBeenCalled());
		expect(onSend).not.toHaveBeenCalled();
	});

	it("routes /compact to this side instead of sending it as a question", async () => {
		const { onSend, onCompact, field } = renderSideComposer();
		await typeInLexicalEditor(field, "/compact");
		fireEvent.keyDown(field, { key: "Enter" });
		await waitFor(() => expect(onCompact).toHaveBeenCalledTimes(1));
		expect(onSend).not.toHaveBeenCalled();
	});
	it("keeps editing enabled during delivery and blocks uncertain sends", async () => {
		const { field, onSend } = renderSideComposer(emptySideChatDraft(), {
			sending: true,
			deliveryPending: true,
		});
		expect(field).toHaveAttribute("contenteditable", "true");
		await typeInLexicalEditor(field, "next question");
		fireEvent.keyDown(field, { key: "Enter" });
		expect(onSend).not.toHaveBeenCalled();
	});
	it("navigates to the original main selection from its compact popover", async () => {
		const reference = {
			id: "ref",
			conversationId: "main",
			messageId: "source",
			revision: 3,
			text: "selected text",
			role: "assistant" as const,
		};
		const { onNavigate } = renderSideComposer({
			...emptySideChatDraft(),
			references: [reference],
		});
		await userEvent.click(screen.getByRole("button", { name: "1 annotation" }));
		await userEvent.click(screen.getByRole("button", { name: "selected text" }));
		expect(onNavigate).toHaveBeenCalledWith(reference);
	});
	it("allows an excerpt-only send with no typed wording", async () => {
		const reference = {
			id: "ref",
			conversationId: "main",
			messageId: "source",
			revision: 3,
			text: "selected text",
			role: "user" as const,
		};
		const { onSend } = renderSideComposer({
			...emptySideChatDraft(),
			references: [reference],
		});
		await userEvent.click(screen.getByRole("button", { name: "Send message" }));
		expect(onSend).toHaveBeenCalledWith("");
	});
});

describe("side composer focus", () => {
	it("takes focus when asked, not each time a side reply finishes", async () => {
		const user = userEvent.setup();
		const ui = (ready: boolean, focusKey: number) => (
			<>
				<input aria-label="Main composer" />
				<SideComposer
					draft={emptySideChatDraft()}
					onChange={vi.fn()}
					onSend={vi.fn()}
					onInterrupt={vi.fn()}
					onAttach={vi.fn()}
					files={[]}
					onRemoveFile={vi.fn()}
					ready={ready}
					running={!ready}
					sending={false}
					models={[]}
					skills={[]}
					side={side}
					onSettings={vi.fn()}
					onRemoveReference={vi.fn()}
					onRemoveAllReferences={vi.fn()}
					onCompact={vi.fn()}
					focusKey={focusKey}
				/>
			</>
		);
		const { rerender } = render(ui(false, 1));
		const field = screen.getByLabelText("Side chat question");
		// The person asked for the side chat before it was ready: it focuses once it is.
		rerender(ui(true, 1));
		await waitFor(() => expect(document.activeElement).toBe(field));

		const main = screen.getByLabelText("Main composer");
		await user.click(main);
		await user.keyboard("still ");
		// The side chat answers, goes busy again and settles, several times over.
		for (const ready of [false, true, false, true]) {
			rerender(ui(ready, 1));
			expect(document.activeElement).toBe(main);
		}
		await user.keyboard("mine");
		expect(main).toHaveValue("still mine");

		// A new request is still honoured.
		rerender(ui(true, 2));
		await waitFor(() => expect(document.activeElement).toBe(field));
	});
});
