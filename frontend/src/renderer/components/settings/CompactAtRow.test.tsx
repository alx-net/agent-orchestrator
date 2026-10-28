import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { appI18n } from "../../i18n";
import { CompactAtRow } from "./CompactAtRow";

const mocks = vi.hoisted(() => ({
	settings: { compactAt: 350_000 } as { compactAt?: number } | undefined,
	update: vi.fn(),
	error: undefined as string | undefined,
}));

vi.mock("../../hooks/useSettings", () => ({
	useSettings: () => ({ settings: mocks.settings, isLoading: false }),
	useUpdateCompactAt: () => ({ update: mocks.update, saving: false, error: mocks.error }),
}));

describe("CompactAtRow", () => {
	beforeEach(async () => {
		await appI18n.changeLanguage("en");
		mocks.settings = { compactAt: 350_000 };
		mocks.error = undefined;
		mocks.update.mockReset();
	});

	it("shows the daemon's size and saves a new one", async () => {
		render(<CompactAtRow />);
		const trigger = screen.getByRole("button", { name: "Compact agent conversations at" });
		expect(trigger).toHaveTextContent("350K (default)");
		await userEvent.click(trigger);
		expect(screen.getAllByRole("menuitem").map((item) => item.textContent)).toEqual(["200K", "350K (default)", "500K", "Repository default"]);
		await userEvent.click(screen.getByRole("menuitem", { name: "500K" }));
		expect(mocks.update).toHaveBeenCalledWith(500_000);
	});

	it("hands compaction back to the repository", async () => {
		render(<CompactAtRow />);
		await userEvent.click(screen.getByRole("button", { name: "Compact agent conversations at" }));
		await userEvent.click(screen.getByRole("menuitem", { name: "Repository default" }));
		expect(mocks.update).toHaveBeenCalledWith(0);
	});

	it("shows a size set through the API and the repository default", () => {
		mocks.settings = { compactAt: 420_000 };
		const { unmount } = render(<CompactAtRow />);
		expect(screen.getByRole("button", { name: "Compact agent conversations at" })).toHaveTextContent("420K");
		unmount();
		mocks.settings = { compactAt: 0 };
		render(<CompactAtRow />);
		expect(screen.getByRole("button", { name: "Compact agent conversations at" })).toHaveTextContent("Repository default");
	});

	it("cannot be changed on a daemon that predates the setting", () => {
		mocks.settings = {};
		render(<CompactAtRow />);
		expect(screen.getByRole("button", { name: "Compact agent conversations at" })).toBeDisabled();
	});

	it("reports a failed save", () => {
		mocks.error = "tokens must be 0 or from 100000 to 900000";
		render(<CompactAtRow />);
		expect(screen.getByRole("alert")).toHaveTextContent("tokens must be 0");
	});
});
