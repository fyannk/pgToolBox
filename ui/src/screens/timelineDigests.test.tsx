import { expect, test, vi } from "vitest";
import { admin, digest, digests, revision, timeline } from "../../mocks/fixtures";
import { renderInFrame } from "../testing/render";
import { DigestCard } from "./Admin";
import { ChangePreviewPanel } from "./ChangePreview";
import { DigestsView, DigestView } from "./Digests";
import { TimelineView } from "./History";

const params = { namespace: "team-a", cluster: "billing" };

test("timeline: the kind chips count the whole window and toggle the filter in the URL state", async () => {
  const onSearch = vi.fn();
  const screen = await renderInFrame(<TimelineView data={timeline} params={params} search={{}} onSearch={onSearch} />);
  const findings = screen.getByRole("button", { name: /^Findings/ });
  expect(findings.element().textContent?.replace(/\s+/g, " ").trim()).toBe("Findings 2");
  await findings.click();
  expect(onSearch).toHaveBeenCalledWith({ kind: "finding" });
});

test("timeline: the honesty notes (journal start, Events window) are always said", async () => {
  const screen = await renderInFrame(<TimelineView data={timeline} params={params} search={{}} onSearch={vi.fn()} />);
  await expect.element(screen.getByText(/journal holds what it observed since/)).toBeVisible();
  await expect.element(screen.getByText(/Events are kept for 1h0m0s only/)).toBeVisible();
});

test("timeline: switched off is said, with no list and no filter", async () => {
  const off = { ...timeline, source: { source: "timeline", state: "disabled" as const, health: "inactive" as const } };
  const screen = await renderInFrame(<TimelineView data={off} params={params} search={{}} onSearch={vi.fn()} />);
  await expect.element(screen.getByText("The timeline is switched off")).toBeVisible();
  expect(screen.container.querySelector("button")).toBeNull();
});

test("preview: absent fields are 'unset', a decrease is explained, an empty instance list gives the order by role", async () => {
  const p = revision.preview;
  if (!p) throw new Error("no preview");
  const screen = await renderInFrame(
    <ChangePreviewPanel
      preview={{
        ...p,
        parameters: [{ name: "max_wal_senders", after: "5", effect: "restart", context: "postmaster", decrease: true }],
        steps: [{ role: "primary", instances: [], action: "restartFirst" }],
      }}
    />,
  );
  await expect.element(screen.getByText("unset")).toBeVisible();
  await expect.element(screen.getByText(/the primary restarts first/)).toBeVisible();
  await expect.element(screen.getByText(/order is given by role/)).toBeVisible();
});

test("digests: disabled is said instead of an empty list", async () => {
  const off = { ...digests, source: { source: "digest", state: "disabled" as const, health: "inactive" as const } };
  const screen = await renderInFrame(<DigestsView data={off} />);
  await expect.element(screen.getByText("Digests are switched off")).toBeVisible();
  expect(screen.container.querySelector("table")).toBeNull();
});

test("digest: Clusters the reader may not get are counted, never named", async () => {
  const screen = await renderInFrame(<DigestView data={{ ...digest, hidden: 2 }} />);
  await expect.element(screen.getByText(/2 Cluster\(s\) you may not see are left out/)).toBeVisible();
  const regions = [...screen.container.querySelectorAll("section[aria-labelledby] h2")].map((h) => h.textContent);
  expect(regions).toEqual(expect.arrayContaining(["team-a/billing", "team-a/orders"]));
});

test("admin digest: an unconfigured channel shows no delivery state, a failing one its error and retry", async () => {
  const d = admin.console?.digest;
  if (!d) throw new Error("no digest in getAdmin.json");
  const screen = await renderInFrame(<DigestCard digest={d} />);
  const rows = [...screen.container.querySelectorAll("tbody tr")].map((r) => r.textContent ?? "");
  const webhook = rows.find((r) => r.startsWith("Webhook"));
  expect(webhook).toBe("Webhookoff");
  const mail = rows.find((r) => r.startsWith("Mail"));
  expect(mail).toMatch(/retry pending/);
  expect(mail).toMatch(/STARTTLS/);
});

test("digest: a catch-up dates its health, and a partial one says why", async () => {
  const late = { ...digest, generatedAt: "2026-10-29T11:30:00Z" };
  const screen = await renderInFrame(<DigestView data={late} />);
  await expect.element(screen.getByText(/Made late \(a catch-up\): health and attention are as of/)).toBeVisible();
  await expect.element(screen.getByText("The console was down during the window.")).toBeVisible();
  const onTime = await renderInFrame(<DigestView data={digest} />);
  expect(onTime.container.textContent).not.toMatch(/Made late/);
});
