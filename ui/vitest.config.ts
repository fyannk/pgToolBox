// Two Vitest projects, both in a real browser (Chromium through Playwright):
//  - unit: src/**/*.test.tsx with vitest-browser-react;
//  - storybook: every story is a test (render + play + axe), via the
//    Storybook Vitest addon.
import { fileURLToPath } from "node:url";
import { storybookTest } from "@storybook/addon-vitest/vitest-plugin";
import { playwright } from "@vitest/browser-playwright";
import { defineConfig, mergeConfig } from "vitest/config";
import viteConfig from "./vite.config.ts";

// A fresh object per project: Vitest mutates it.
const browser = () => ({
  enabled: true,
  headless: true,
  provider: playwright(),
  instances: [{ browser: "chromium" as const }],
});

export default defineConfig((env) =>
  mergeConfig(
    viteConfig({ ...env, mode: "test" }),
    defineConfig({
      // Scan everything up front so a dependency discovered mid-run does not
      // reload the browser and fail the tests that were running.
      optimizeDeps: { entries: ["src/**/*.tsx", ".storybook/*.tsx", "mocks/**/*.ts"] },
      test: {
        projects: [
          {
            extends: true,
            test: {
              name: "unit",
              include: ["src/**/*.test.{ts,tsx}"],
              setupFiles: ["./src/testing/setup.ts"],
              browser: browser(),
            },
          },
          {
            extends: true,
            plugins: [storybookTest({ configDir: fileURLToPath(new URL("./.storybook", import.meta.url)) })],
            test: {
              name: "storybook",
              browser: browser(),
            },
          },
        ],
      },
    }),
  ),
);
