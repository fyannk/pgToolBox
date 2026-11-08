// What each screen does with the Go fields it renders verbatim: evidence
// refusals, chart source, waiting reasons, the session menu.
import { expect, test, vi } from "vitest";
import {
  accessRequests,
  accessRequestsReadOnly,
  accessRequestsRequester,
  backups,
  clusterList,
  failingOverOverview,
  metrics,
  session,
} from "../../mocks/fixtures";
import {
  evidenceAudienceMismatch,
  evidenceStatusNotFromOperator,
  metricsPrometheus,
  metricsPrometheusDown,
  operationBlocked,
} from "../../mocks/fixtures/extra";
import { SessionMenuView } from "../app/Shell";
import { OperationTracker } from "../ds/patterns/OperationTracker/OperationTracker";
import { renderInFrame } from "../testing/render";
import { AccessRequestsView } from "./AccessRequests";
import { BackupsView } from "./Backups";
import { MetricsView } from "./Metrics";
import { OverviewView } from "./Overview";

const params = { namespace: "team-a", cluster: "orders" };

test("operation tracker: the chip's health is Go's, not a table in the UI", async () => {
  const screen = await renderInFrame(<OperationTracker operation={operationBlocked} live />);
  await expect.element(screen.getByRole("status")).toBeVisible();
  expect(screen.getByRole("status").element().querySelector("[data-health]")?.getAttribute("data-health")).toBe(
    "warning",
  );
});

test("metrics: Go's chartSource says where the charts come from; Prometheus down is drawn with Go's health and reason", async () => {
  const prom = await renderInFrame(<MetricsView data={metricsPrometheus} params={params} />);
  await expect.element(prom.getByText(/From Prometheus over the last 6 h/)).toBeVisible();
  expect(prom.container.querySelector("[data-chart-source]")?.getAttribute("data-chart-source")).toBe("prometheus");
  prom.unmount();
  const down = await renderInFrame(<MetricsView data={metricsPrometheusDown} params={params} />);
  await expect.element(down.getByText(/Prometheus did not answer/)).toBeVisible();
  expect(
    down.container.querySelector('[data-prometheus="unavailable"] [data-health]')?.getAttribute("data-health"),
  ).toBe("warning");
  await expect.element(down.getByRole("heading", { name: "Monitoring" })).toBeVisible();
  down.unmount();
  const ring = await renderInFrame(<MetricsView data={metrics} params={params} />);
  await expect.element(ring.getByText(/console's short ring/)).toBeVisible();
  expect(ring.container.querySelector("[data-prometheus]")).toBeNull();
});

test("evidence: an unverifiable answer shows Go's reason verbatim instead of the generic word", async () => {
  for (const e of [evidenceAudienceMismatch, evidenceStatusNotFromOperator]) {
    const screen = await renderInFrame(<BackupsView data={{ ...backups, evidence: e }} />);
    await expect.element(screen.getByText(e.reason)).toBeVisible();
    expect(screen.container.querySelector("[data-evidence-text]")?.getAttribute("data-evidence-text")).toBe("reason");
    screen.unmount();
  }
});

test("session menu: the signed-in user and a POST sign-out form", async () => {
  const sso = await renderInFrame(<SessionMenuView session={session} />);
  await expect.element(sso.getByRole("button", { name: "Sign out" })).toBeVisible();
  expect(sso.container.querySelector("form")?.getAttribute("method")).toBe("post");
  sso.unmount();
});

test("access requests: no review queue for a requester; read-only console offers no form and no decision", async () => {
  const props = {
    clusters: (clusterList.items ?? []).map((c) => c.ref),
    actor: "oidc:jane@corp.example",
    onCreate: vi.fn(async () => ({})),
    onDecide: vi.fn(async () => ({})),
  };
  const requester = await renderInFrame(<AccessRequestsView data={accessRequestsRequester} {...props} />);
  await expect.element(requester.getByRole("heading", { name: "Request access" })).toBeVisible();
  expect(requester.container.textContent).not.toContain("To review");
  expect(requester.container.textContent).toContain("only those naming you are shown");
  requester.unmount();
  const ro = await renderInFrame(<AccessRequestsView data={accessRequestsReadOnly} {...props} />);
  await expect.element(ro.getByText(/This console does not write/)).toBeVisible();
  expect(ro.container.querySelectorAll("button").length).toBe(0);
  ro.unmount();
  const reviewer = await renderInFrame(<AccessRequestsView data={accessRequests} {...props} />);
  await expect.element(reviewer.getByRole("heading", { name: "To review" })).toBeVisible();
});

test("overview: why the operator waits lists Go's reasons, a heuristic labelled as such, with their evidence", async () => {
  const screen = await renderInFrame(<OverviewView data={failingOverOverview} />);
  await expect.element(screen.getByText("Cluster.status.targetPrimary = billing-2")).toBeVisible();
  const reasons = [...screen.container.querySelectorAll("[data-reason]")];
  expect(reasons.length).toBe((failingOverOverview.waiting?.reasons ?? []).length);
  expect(screen.container.textContent).toContain("reported by the operator");
});
