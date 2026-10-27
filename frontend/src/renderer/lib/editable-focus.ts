const NON_TEXT_INPUT_TYPES = new Set([
	"button",
	"checkbox",
	"color",
	"file",
	"hidden",
	"image",
	"radio",
	"range",
	"reset",
	"submit",
]);

/**
 * Whether the keyboard is in a field the person types in: a text input, a
 * textarea, a select, or editable content such as a composer. Focus that
 * moves on its own (a composer becoming usable again, the window coming
 * back) checks this first, so it never takes the caret out of a field the
 * person is typing in. Focus the person asks for does not need it.
 */
export function isTypingInEditableElement(): boolean {
	if (typeof document === "undefined") return false;
	const active = document.activeElement;
	if (!(active instanceof HTMLElement) || active === document.body) return false;
	if (active instanceof HTMLInputElement) return !NON_TEXT_INPUT_TYPES.has(active.type) && !active.readOnly;
	if (active instanceof HTMLTextAreaElement) return !active.readOnly;
	if (active instanceof HTMLSelectElement) return true;
	return active.isContentEditable || active.closest("[contenteditable]:not([contenteditable='false'])") !== null;
}
