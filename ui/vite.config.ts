// The SPA build. Production output is ui/dist: hashed assets, no
// inline script or style, nothing fetched at runtime but /api on the same
// origin. `--mode harness` builds the same app with the MSW service worker for
// the Playwright fixture harness into dist-harness/, which is never embedded.
import { paraglideVitePlugin } from "@inlang/paraglide-js";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";
import { cspHeader } from "./scripts/csp.ts";

export default defineConfig(({ mode }) => {
  // Under Vitest the generators stay off: `pnpm i18n` and the committed route
  // tree are the inputs, and a generator writing into src/ mid-run would make
  // Vite reload the browser under the running tests.
  const generators = mode !== "test";
  return {
    plugins: [
      generators &&
        tanstackRouter({
          target: "react",
          autoCodeSplitting: true,
          routesDirectory: "./src/routes",
          generatedRouteTree: "./src/routeTree.gen.ts",
          quoteStyle: "double",
          semicolons: true,
        }),
      generators &&
        paraglideVitePlugin({
          project: "./project.inlang",
          outdir: "./src/paraglide",
          strategy: ["localStorage", "preferredLanguage", "baseLocale"],
          emitGitIgnore: false,
          emitPrettierIgnore: false,
        }),
      react(),
      tailwindcss(),
    ],
    // The MSW worker script exists everywhere but in the production build.
    publicDir: mode === "production" ? false : "mocks/public",
    build: {
      outDir: mode === "harness" ? "dist-harness" : "dist",
      emptyOutDir: true,
      assetsDir: "assets",
      sourcemap: false,
      // No inline module-preload polyfill: every supported browser has it.
      modulePreload: { polyfill: false },
      assetsInlineLimit: 0,
    },
    // The preview server answers with the production CSP, so the Playwright
    // harness proves the app runs under it (plus worker-src for MSW).
    preview: {
      headers: { "Content-Security-Policy": cspHeader({ worker: mode === "harness" }) },
    },
  };
});
