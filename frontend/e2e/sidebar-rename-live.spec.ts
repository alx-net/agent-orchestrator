import { expect, test } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// Renaming a session in the sidebar while agents stream updates: every typed
// character must land in the field, and the field must keep focus.

test("the sidebar rename field keeps focus and text while sessions update live @T0", async ({ page }) => {
	await installFakeAgent(page, {
		expandProject: true,
		workers: [
			{ id: "w1", title: "Fix the flaky test", mode: "chat", provider: "claude-code", activity: "active" },
			{ id: "w2", title: "Write the docs", mode: "chat", provider: "claude-code", activity: "active" },
		],
	});
	await page.goto("/#/");
	const row = page.getByLabel("Open Fix the flaky test");
	await expect(row).toBeVisible();

	await page.evaluate(() => window.__aoFakeAgent?.startContinuousWorkspaceEvents(150, 2));
	await row.getByText("Fix the flaky test").dblclick();
	await expect(page.locator("input:focus")).toHaveCount(1);
	await page.keyboard.press("Meta+a");
	await page.keyboard.type("Clarity: fix the flaky test", { delay: 40 });
	await expect(page.locator("input:focus")).toHaveValue("Clarity: fix the flaky test");
	await page.evaluate(() => window.__aoFakeAgent?.stopContinuousWorkspaceEvents());
});
