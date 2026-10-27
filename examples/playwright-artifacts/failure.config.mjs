import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./runner",
  testMatch: "failure.spec.mjs",
  outputDir: `${process.env.PW_RECIPE_OUT}/results`,
  reporter: [["json", { outputFile: `${process.env.PW_RECIPE_OUT}/report.json` }]],
  workers: 1,
  retries: 0,
  timeout: 10_000,
  use: {
    browserName: "chromium",
    headless: true,
    screenshot: "off",
    video: "off",
    trace: { mode: "retain-on-failure", screenshots: true, snapshots: true, sources: false },
  },
});
