import { test, expect } from "@playwright/test";

const fixture = new URL("../fixture.html", import.meta.url).href;

test("deliberate local assertion failure", async ({ page }) => {
  // Block network requests even if the synthetic fixture is changed later.
  await page.route(/^https?:/, (route) => route.abort());
  await page.goto(fixture);
  await expect(page.locator("h1")).toHaveText("INTENTIONAL-FAILURE", { timeout: 100 });
});
