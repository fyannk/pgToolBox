// Renders social-card.html to static/img/social-card.png. Run from ui/ (it
// borrows the console's Playwright): node ../docs/site/brand/render.mjs
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

// Resolved from the working directory (ui/), which has Playwright installed.
const playwright = await import(
  pathToFileURL(createRequire(`${process.cwd()}/`).resolve("@playwright/test")).href
);
const { chromium } = playwright.default ?? playwright;

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 640 } });
await page.goto(`file://${here("social-card.html")}`);
await page.evaluate(() => document.fonts.ready);
await page.screenshot({ path: here("../static/img/social-card.png") });
await browser.close();
