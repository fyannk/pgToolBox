// Generates the typed client (TanStack Query hooks + types) and the MSW
// handlers from the console's OpenAPI document.
//
// INPUT: the document the Huma registrations of internal/console/api generate
// (internal/console/api/openapi.yaml, `make openapi`). `pnpm
// generate:check` (CI) regenerates and fails when src/api/generated drifts
// from it.
import { defineConfig } from "orval";

const input = "../internal/console/api/openapi.yaml";

export default defineConfig({
  console: {
    input: { target: input },
    output: {
      mode: "tags-split",
      target: "./src/api/generated/endpoints",
      schemas: "./src/api/generated/model",
      client: "react-query",
      httpClient: "fetch",
      clean: true,
      mock: { generators: [{ type: "msw", delay: false }] },
      override: {
        mutator: { path: "./src/api/fetcher.ts", name: "apiFetch" },
        fetch: { includeHttpResponseReturnType: false },
        // useQuery is left at its default (GET only): set explicitly it routes
        // POST and DELETE to query hooks too; they are mutations.
        query: { useSuspenseQuery: false, signal: true },
      },
    },
  },
});
