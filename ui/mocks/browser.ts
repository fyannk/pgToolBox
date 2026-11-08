// Starts MSW in the browser for `pnpm dev` and the Playwright harness build.
// The scenario comes from localStorage (pgtoolbox-console.mock), set by the harness.
import { setupWorker } from "msw/browser";
import { handlersFor, SCENARIOS, type Scenario } from "./handlers";

function scenario(): Scenario {
  try {
    const v = localStorage.getItem("pgtoolbox-console.mock");
    return (SCENARIOS as readonly string[]).includes(v ?? "") ? (v as Scenario) : "default";
  } catch {
    return "default";
  }
}

export async function startMocks() {
  const worker = setupWorker(...handlersFor(scenario()));
  await worker.start({ onUnhandledFrame: "bypass", quiet: true });
}
