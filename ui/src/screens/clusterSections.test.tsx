import { expect, test, vi } from "vitest";
import {
  admin,
  backups,
  healthyOverview,
  metrics,
  objects,
  operationsOperator,
  operationsViewer,
  replication,
  storage,
  storageForecasts,
} from "../../mocks/fixtures";
import { recoveryNone, recoveryReportedOnly } from "../../mocks/fixtures/extra";
import { renderInFrame } from "../testing/render";
import { AdminView } from "./Admin";
import { BackupsView } from "./Backups";
import { MetricsView } from "./Metrics";
import { ObjectsView } from "./Objects";
import { OperationsView } from "./Operations";
import { BackupsPanel } from "./Overview";
import { ReplicationView } from "./Replication";
import { StorageView } from "./Storage";

const params = { namespace: "team-a", cluster: "orders" };

test("replication: a replica whose status was not read is an error row, its fields not reported", async () => {
  const screen = await renderInFrame(<ReplicationView data={replication} />);
  const errorRows = screen.container.querySelectorAll("tr[data-error-row]");
  expect(errorRows.length).toBeGreaterThan(0);
  expect(errorRows[0]?.textContent).toContain("not reported");
});

test("backups: a running plugin backup says its fields come at completion, not that they are missing", async () => {
  const screen = await renderInFrame(<BackupsView data={backups} />);
  await expect.element(screen.getByText(/reported at completion/)).toBeVisible();
});

test("storage: usage is shown only where an instance reports it", async () => {
  const withoutUsage = { ...storage, volumes: (storage.volumes ?? []).map((v) => ({ ...v, usage: undefined })) };
  const screen = await renderInFrame(<StorageView data={withoutUsage} />);
  const cells = [
    ...screen.container.querySelectorAll('section[aria-label="Persistent volume claims"] tbody tr td:last-child'),
  ].map((c) => c.textContent);
  expect(cells.length).toBeGreaterThan(0);
  expect(cells.every((c) => c === "not reported")).toBe(true);
});

test("objects: a declarative object's PostgreSQL error is shown on its row", async () => {
  const failed = (objects.databases ?? []).find((d) => d.message);
  const screen = await renderInFrame(<ObjectsView data={objects} />);
  if (failed?.message) await expect.element(screen.getByText(failed.message)).toBeVisible();
});

test("metrics: a monitoring URL that is not http(s) is never a link", async () => {
  const data = { ...metrics, monitoring: { state: "configured" as const, url: "javascript:alert(1)" } };
  const screen = await renderInFrame(<MetricsView data={data} params={params} />);
  expect(screen.container.querySelector('a[href^="javascript"]')).toBeNull();
  await expect.element(screen.getByText(/No monitoring link is configured/)).toBeVisible();
});

test("operations: nothing allowed for a viewer, and the refused verbs are named", async () => {
  const screen = await renderInFrame(<OperationsView data={operationsViewer} actor="v" onRequest={vi.fn()} />);
  for (const b of screen.getByRole("button", { name: /Request…/ }).all()) await expect.element(b).toBeDisabled();
  expect(screen.container.textContent).toMatch(/Not allowed for you|could not be asked/);
});

test("operations: promote cannot be confirmed before an instance is chosen", async () => {
  const onRequest = vi.fn(async () => ({}));
  const screen = await renderInFrame(<OperationsView data={operationsOperator} actor="v" onRequest={onRequest} />);
  await screen.getByRole("button", { name: /Promote a replica/ }).click();
  const field = screen.getByLabelText(/To confirm, type/);
  await field.fill("the target instance name");
  await expect.element(screen.getByRole("button", { name: "Request", exact: true })).toBeDisabled();
  expect(onRequest).not.toHaveBeenCalled();
});

test("admin: a pgAdmin address that is not https is not a link", async () => {
  const ns = (admin.namespaces ?? [])[0];
  if (!ns) throw new Error("fixture has no namespace");
  const data = {
    ...admin,
    namespaces: [{ ...ns, pgAdmins: (ns.pgAdmins ?? []).map((p) => ({ ...p, url: "http://pgadmin.example" })) }],
  };
  const screen = await renderInFrame(<AdminView data={data} />);
  expect(screen.container.querySelector('a[href^="http://"]')).toBeNull();
});

test("storage: on stock CloudNativePG the forecast panel quotes Go's reason and draws nothing", async () => {
  const screen = await renderInFrame(<StorageView data={storage} />);
  await expect.element(screen.getByText("No volume forecast for this Cluster")).toBeVisible();
  if (storage.forecasts.reason) await expect.element(screen.getByText(storage.forecasts.reason)).toBeVisible();
  expect(screen.container.querySelector("article[data-forecast]")).toBeNull();
});

test("storage: one card per instance and volume, each forecast a sentence with its window and confidence", async () => {
  const screen = await renderInFrame(<StorageView data={storageForecasts} />);
  const cards = screen.container.querySelectorAll("article[data-forecast]");
  expect(cards.length).toBe(storageForecasts.forecasts.volumes?.length);
  const wal = screen.getByRole("article", { name: "billing-1 · wal" });
  await expect.element(wal.getByText(/over the last 96 h, medium confidence\.$/)).toBeVisible();
  await expect.element(wal.getByText("WAL archiving failing")).toBeVisible();
  await expect.element(wal.getByText("primary")).toBeVisible();
  // The data volume's trend has a gap: it stays a gap.
  const data = screen.getByRole("article", { name: "billing-1 · pgdata" });
  expect(data.element().querySelectorAll("svg[role=img] polyline").length).toBeGreaterThan(1);
});

test("backups: the recovery bar has a text twin listing every span", async () => {
  const screen = await renderInFrame(<BackupsView data={backups} />);
  const items = screen.getByRole("list", { name: "Spans of the recovery window" }).getByRole("listitem").all();
  expect(items).toHaveLength(backups.recoveryTimeline.segments?.length ?? 0);
  expect(items[1]?.element().textContent).toMatch(/WAL missing — WAL is missing from the repository/);
});

test("backups: the window's end is the WAL received, and the last safe target is named apart", async () => {
  const t = backups.recoveryTimeline;
  if (!t.lastSafeTarget || !t.latest) throw new Error("fixture has no last safe target");
  const screen = await renderInFrame(<BackupsView data={backups} />);
  const received = screen.container.querySelector("[data-wal-received]")?.textContent ?? "";
  const safe = screen.container.querySelector("[data-last-safe-target]")?.textContent ?? "";
  expect(received).toMatch(/^WAL received until /);
  expect(safe).toMatch(/^last safe target /);
  expect(safe).not.toBe(received.replace("WAL received until", "last safe target"));
  expect(screen.container.textContent).not.toMatch(/latest 2026/);
  const none = await renderInFrame(
    <BackupsView data={{ ...backups, recoveryTimeline: { ...t, lastSafeTarget: undefined } }} />,
  );
  expect(none.container.querySelector("[data-last-safe-target]")).toBeNull();
});

test("backups: an operator-only window is hatched, labelled unverified, never called covered", async () => {
  const screen = await renderInFrame(<BackupsView data={{ ...backups, recoveryTimeline: recoveryReportedOnly }} />);
  expect(screen.container.querySelector('rect[data-kind="reported"]')?.getAttribute("fill")).toMatch(/^url\(#hatch/);
  const spans = screen.getByRole("list", { name: "Spans of the recovery window" });
  await expect.element(spans).toBeVisible();
  const text = spans.element().textContent ?? "";
  expect(text).toContain("reported by the operator, not verified");
  expect(text).not.toMatch(/structurally covered/);
});

test("backups: without any window there is no bar, only Go's reason", async () => {
  const screen = await renderInFrame(<BackupsView data={{ ...backups, recoveryTimeline: recoveryNone }} />);
  await expect.element(screen.getByText("No recovery window")).toBeVisible();
  expect(screen.container.querySelector('svg[aria-label^="Recovery window from"]')).toBeNull();
});

test("backups: structurally usable evidence says 'not restore-tested' until a drill passes in the window", async () => {
  const untested = {
    ...backups,
    drills: { ...backups.drills, restoreTested: false, lastPassedAt: undefined },
    recoveryTimeline: { ...backups.recoveryTimeline, restoreTested: false },
  };
  const screen = await renderInFrame(<BackupsView data={untested} />);
  expect(screen.container.querySelector('[data-restore-tested="false"]')?.textContent).toMatch(/^Not restore-tested/);
  expect(screen.container.textContent).toMatch(/structurally usable\s*Not restore-tested/);

  const tested = await renderInFrame(<BackupsView data={backups} />);
  expect(tested.container.querySelector('[data-restore-tested="false"]')).toBeNull();
  expect(tested.container.textContent).not.toMatch(/Not restore-tested/);
});

test("backups: an uncertain recovery edge offers the archiver checks as text only", async () => {
  const segments = backups.recoveryTimeline.segments ?? [];
  const last = segments.at(-1);
  if (!last) throw new Error("fixture has no span");
  const uncertain = {
    ...backups,
    recoveryTimeline: {
      ...backups.recoveryTimeline,
      segments: [...segments.slice(0, -1), { ...last, kind: "unknown" as const }],
    },
  };
  const screen = await renderInFrame(<BackupsView data={uncertain} />);
  const hint = screen.container.querySelector("[data-pitr-edge-hint]");
  expect(hint?.textContent).toContain("SELECT * FROM pg_stat_archiver;");
  expect(hint?.textContent).toContain("SELECT pg_switch_wal();");
  expect(hint?.querySelectorAll("form, a[href]")).toHaveLength(0);

  const certain = await renderInFrame(<BackupsView data={backups} />);
  expect(certain.container.querySelector("[data-pitr-edge-hint]")).toBeNull();
});

test("backups: a Backup of an earlier Cluster of the same name is listed and labelled", async () => {
  const first = backups.items?.[0];
  if (!first) throw new Error("fixture has no Backup");
  const screen = await renderInFrame(
    <BackupsView
      data={{ ...backups, items: [{ ...first, predatesCluster: true }, ...(backups.items ?? []).slice(1)] }}
    />,
  );
  await expect.element(screen.getByText("predates this Cluster")).toBeVisible();
  const plain = await renderInFrame(<BackupsView data={backups} />);
  expect(plain.container.textContent).not.toContain("predates this Cluster");
});

test("backups: an operator-only window ends at the last base backup, never called the end of the archived WAL", async () => {
  const screen = await renderInFrame(<BackupsView data={{ ...backups, recoveryTimeline: recoveryReportedOnly }} />);
  await expect.element(screen.getByText(/^last base backup .*, not the end of the archived WAL$/)).toBeVisible();
  expect(screen.container.textContent).not.toMatch(/latest 2026/);
});

test("overview: the operator's window end is labelled the last base backup", async () => {
  const reported = {
    ...healthyOverview.backups,
    recoveryWindow: {
      first: "2026-10-22T07:08:00Z",
      last: "2026-10-29T06:00:00Z",
      provenance: { origin: "operator" as const, source: "ObjectStore.status.serverRecoveryWindow[orders]" },
    },
  };
  const screen = await renderInFrame(<BackupsPanel b={reported} />);
  await expect.element(screen.getByText("last base backup")).toBeVisible();
  const fromAgent = {
    ...reported,
    recoveryWindow: { ...reported.recoveryWindow, provenance: { origin: "evidence" as const } },
  };
  const agent = await renderInFrame(<BackupsPanel b={fromAgent} />);
  expect(agent.container.textContent).not.toContain("last base backup");
});
