import type { StorybookConfig } from "@storybook/react-vite";

const config: StorybookConfig = {
  framework: "@storybook/react-vite",
  stories: ["../src/**/*.mdx", "../src/**/*.stories.tsx"],
  addons: ["@storybook/addon-docs", "@storybook/addon-a11y", "@storybook/addon-vitest", "msw-storybook-addon"],
  // The MSW worker script, served at /mockServiceWorker.js.
  staticDirs: ["../mocks/public"],
  core: { disableTelemetry: true, disableWhatsNewNotifications: true },
};

export default config;
