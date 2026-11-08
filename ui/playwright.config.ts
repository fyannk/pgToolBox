// Screen tests against the harness build (the production bundle plus the MSW
// worker serving the Go goldens) under the production CSP: screenshots at
// 375/768/1440 in both themes, axe serious/critical = 0, the user journeys.
//
// Screenshots are only comparable from one browser build and one font set, so
// the committed baselines are rendered in the CI image (node:22-bookworm with
// Playwright's Chromium and its Debian font dependencies); `pnpm e2e:docker`
// renders them locally the same way. UI_E2E_IGNORE_SNAPSHOTS=1 skips the
// pixel comparison where the fonts differ; every other assertion still runs.
import { defineConfig, devices } from "@playwright/test";

const port = 4317;
const ignoreSnapshots = process.env.UI_E2E_IGNORE_SNAPSHOTS === "1";

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  snapshotPathTemplate: "{testDir}/__screenshots__/{testFilePath}/{arg}{ext}",
  fullyParallel: true,
  workers: process.env.CI ? 2 : 2,
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: [["list"]],
  ignoreSnapshots,
  expect: {
    toHaveScreenshot: { animations: "disabled", caret: "hide", scale: "css", maxDiffPixelRatio: 0.001 },
  },
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: "retain-on-failure",
    timezoneId: "UTC",
    locale: "en-US",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } } }],
  webServer: {
    command: `pnpm build:harness && pnpm exec vite preview --mode harness --port ${port} --strictPort --host 127.0.0.1`,
    url: `http://127.0.0.1:${port}/`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
  },
});
