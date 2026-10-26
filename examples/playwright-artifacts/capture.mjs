// Headless Chromium against the local fixture. The trace is stopped and the
// browser is closed before the caller uploads anything.

import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

import { chromium } from "playwright";

export async function captureFixture(fixturePath) {
  const browser = await chromium.launch({ headless: true });
  const directory = await mkdtemp(join(tmpdir(), "mountable-playwright-"));
  const tracePath = join(directory, "trace.zip");
  try {
    const context = await browser.newContext();
    await context.tracing.start({ screenshots: true, snapshots: true, sources: false });
    const page = await context.newPage();
    await page.goto(pathToFileURL(fixturePath).href);
    const screenshot = Buffer.from(await page.screenshot({ type: "png" }));
    await context.tracing.stop({ path: tracePath });
    const trace = await readFile(tracePath);
    await context.close();
    return { screenshot, trace };
  } finally {
    await browser.close();
    await rm(directory, { recursive: true, force: true });
  }
}
