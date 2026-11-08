import { expect, test } from "vitest";
import { revision } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { ChangePreviewPanel, type Preview } from "./ChangePreview";

function base(): Preview {
  const p = revision.preview;
  if (!p) throw new Error("no preview in getRevision.orders.json");
  return p;
}

test("preview: an unknown change with nothing known leads with 'Unknown', never 'No restart'", async () => {
  const screen = await renderInFrame(
    <ChangePreviewPanel
      preview={{
        ...base(),
        effect: "unknown",
        atLeast: "none",
        primary: "unknown",
        verified: false,
        unknown: 1,
        parameters: [{ name: "timescaledb.max_background_workers", after: "8", effect: "unknown" }],
        fields: [],
        steps: [],
      }}
    />,
  );
  await expect.element(screen.getByText(/a restart cannot be ruled out/)).toBeVisible();
  await expect.element(screen.getByText(/1 change\(s\) could not be classified/)).toBeVisible();
  expect(screen.container.textContent).not.toMatch(/No restart/);
  // The coverage banner is not the reason here: the unknown lead is.
  expect(screen.container.textContent).not.toMatch(/is not covered by the tables/);
  await expect.element(screen.getByText(/cannot be said: a change could not be classified/)).toBeVisible();
});

test("preview: an unknown change beside a restart says 'unknown, at least a restart' and labels the order a floor", async () => {
  const screen = await renderInFrame(
    <ChangePreviewPanel
      preview={{
        ...base(),
        effect: "unknown",
        atLeast: "restart",
        primary: "unknown",
        verified: false,
        unknown: 1,
        steps: [
          { role: "replicas", instances: ["orders-2", "orders-3"], action: "recreate" },
          { role: "primary", instances: ["orders-1"], action: "restartInPlace" },
        ],
      }}
    />,
  );
  await expect.element(screen.getByText("Unknown, at least a restart")).toBeVisible();
  await expect.element(screen.getByRole("heading", { name: "Order of the update (at least)" })).toBeVisible();
  // The lead is the first health line; a field classified "none" may still say so in its own row.
  const lead = screen.container.querySelector("p.health");
  expect(lead?.getAttribute("data-health")).toBe("unknown");
  expect(lead?.textContent).not.toMatch(/No restart/);
});

test("preview: fencing, a lifted fence, an absent parameter and new certificates are worded", async () => {
  const screen = await renderInFrame(
    <ChangePreviewPanel
      preview={{
        ...base(),
        effect: "shutdown",
        primary: "start",
        verified: true,
        unknown: 0,
        parameters: [{ name: "summarize_wal", after: "on", effect: "unknown", context: "absent" }],
        fields: [
          { path: "metadata.annotations.cnpg.io/fencedInstances", effect: "shutdown", reason: "fencing" },
          { path: "spec.certificates", effect: "reload", reason: "certificates" },
          { path: "spec.postgresql.parameters.pg_stat_statements.*", effect: "restart", reason: "managedExtension" },
        ],
        steps: [
          { role: "replicas", instances: ["orders-2"], action: "shutdown" },
          { role: "primary", instances: ["orders-1"], action: "start" },
        ],
      }}
    />,
  );
  await expect.element(screen.getByText(/stops on the instances below/)).toBeVisible();
  await expect.element(screen.getByText(/PostgreSQL rejects the configuration/)).toBeVisible();
  await expect.element(screen.getByText("TLS certificates")).toBeVisible();
  await expect.element(screen.getByText(/added to or removed from shared_preload_libraries/)).toBeVisible();
  await expect.element(screen.getByText(/PostgreSQL shut down \(fencing or hibernation\)/)).toBeVisible();
  expect(screen.container.textContent).toMatch(/PostgreSQL started/);
});

test("preview: a restart recreates the replica Pods and restarts the primary in place", async () => {
  const screen = await renderInFrame(
    <ChangePreviewPanel
      preview={{
        ...base(),
        effect: "restart",
        primary: "restartInPlace",
        method: "restart",
        verified: true,
        unknown: 0,
        steps: [
          { role: "replicas", instances: ["orders-2", "orders-3"], action: "recreate" },
          { role: "primary", instances: ["orders-1"], action: "restartInPlace" },
        ],
      }}
    />,
  );
  await expect.element(screen.getByText(/each replica's Pod is recreated on its volumes/)).toBeVisible();
  const order = [...screen.container.querySelectorAll("ol li")].map((li) =>
    li.textContent?.replace(/\s+/g, " ").trim(),
  );
  expect(order).toEqual(["Replicas (orders-2, orders-3): Pod recreated", "Primary (orders-1): restarted in place"]);
});
