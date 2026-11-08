import { CSPProvider } from "@base-ui/react/csp-provider";
import { QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { startLive } from "./api/live";
import { applyStoredPrefs } from "./app/prefs";
import { createQueryClient } from "./app/queryClient";
import { getLocale } from "./paraglide/runtime.js";
import { routeTree } from "./routeTree.gen";
import "./ds/styles.css";

applyStoredPrefs();
document.documentElement.lang = getLocale();

const queryClient = createQueryClient();
const router = createRouter({
  routeTree,
  context: { queryClient },
  defaultPreload: "intent",
  scrollRestoration: true,
});

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

async function boot() {
  // Mocks exist only in `vite` (dev) and `--mode harness`; the production
  // build drops this branch and the mocks chunk entirely.
  if (import.meta.env.DEV || import.meta.env.MODE === "harness") {
    const { startMocks } = await import("../mocks/browser");
    await startMocks();
  }
  startLive(queryClient);
  const root = document.getElementById("root");
  if (!root) throw new Error("#root is missing from index.html");
  createRoot(root).render(
    <StrictMode>
      {/* Base UI must not inject style elements: style-src is self only. */}
      <CSPProvider disableStyleElements>
        <QueryClientProvider client={queryClient}>
          <RouterProvider router={router} />
        </QueryClientProvider>
      </CSPProvider>
    </StrictMode>,
  );
}

void boot();
