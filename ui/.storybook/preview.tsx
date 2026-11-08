import type { Decorator, Preview } from "@storybook/react-vite";
import { mswLoader } from "msw-storybook-addon/csf3";
import { applyDensity, applyTheme } from "../src/app/prefs";
import { overwriteGetLocale } from "../src/paraglide/runtime.js";
import { StoryFrame } from "./StoryFrame";
import "../src/ds/styles.css";

/** Theme and density are globals so every story can be pinned to a combination. */
const withPrefs: Decorator = (Story, ctx) => {
  applyTheme(ctx.globals.theme === "dark" ? "dark" : "light");
  applyDensity(ctx.globals.density === "compact" ? "compact" : "comfortable");
  overwriteGetLocale(() => (ctx.globals.locale === "fr" ? "fr" : "en"));
  return <Story />;
};

const withProviders: Decorator = (Story) => <StoryFrame>{() => <Story />}</StoryFrame>;

const preview: Preview = {
  loaders: [mswLoader()],
  decorators: [withProviders, withPrefs],
  globalTypes: {
    theme: {
      description: "Theme",
      toolbar: { title: "Theme", items: ["light", "dark"], dynamicTitle: true },
    },
    density: {
      description: "Density",
      toolbar: { title: "Density", items: ["comfortable", "compact"], dynamicTitle: true },
    },
    locale: {
      description: "Language",
      toolbar: { title: "Language", items: ["en", "fr"], dynamicTitle: true },
    },
  },
  initialGlobals: { theme: "light", density: "comfortable", locale: "en" },
  parameters: {
    layout: "padded",
    // axe runs on every story and a violation fails the story test.
    a11y: { test: "error" },
    backgrounds: { disable: true },
  },
};

export default preview;
