// What every story renders inside: the app's providers with deterministic
// inputs — a fresh QueryClient that neither retries nor polls, the clock of the
// Go fixtures, and a memory router so links render real hrefs.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";
import { NOW_MS } from "../mocks/fixtures/extra";
import { NowProvider } from "../src/ds/time";

export function StoryFrame({ children }: { children: () => ReactNode }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: { queries: { retry: false, refetchInterval: false, staleTime: Number.POSITIVE_INFINITY } },
      }),
  );
  const [router] = useState(() =>
    createRouter({
      routeTree: createRootRoute({ component: () => <>{children()}</> }),
      history: createMemoryHistory({ initialEntries: ["/"] }),
    }),
  );
  return (
    <QueryClientProvider client={client}>
      <NowProvider now={NOW_MS}>
        <RouterProvider router={router} />
      </NowProvider>
    </QueryClientProvider>
  );
}
