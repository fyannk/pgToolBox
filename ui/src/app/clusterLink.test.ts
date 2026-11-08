import { expect, test } from "vitest";
import { resolveClusterLink } from "./clusterLink";

const p = { namespace: "team-a", cluster: "billing" };

test("cluster routes resolve", () => {
  expect(resolveClusterLink("instances", p)?.to).toBe("/c/$namespace/$cluster/instances");
  expect(resolveClusterLink("instances/billing-1", p)).toEqual({
    to: "/c/$namespace/$cluster/instances/$pod",
    params: { ...p, pod: "billing-1" },
  });
  expect(resolveClusterLink("diagnostics", p)?.to).toBe("/c/$namespace/$cluster/diagnostics");
  expect(resolveClusterLink("triage", p)?.to).toBe("/c/$namespace/$cluster/diagnostics");
  expect(resolveClusterLink("", p)?.to).toBe("/c/$namespace/$cluster");
  expect(resolveClusterLink(undefined, p)).toBeNull();
});

test("the section routes resolve", () => {
  for (const s of ["replication", "backups", "storage", "objects", "metrics", "operations", "events"])
    expect(resolveClusterLink(s, p)?.to, s).toBe(`/c/$namespace/$cluster/${s}`);
  expect(resolveClusterLink("/backups", p)?.to).toBe("/c/$namespace/$cluster/backups");
  expect(resolveClusterLink("history/12", p)).toEqual({
    to: "/c/$namespace/$cluster/history/$revision",
    params: { ...p, revision: 12 },
  });
  expect(resolveClusterLink("logs/billing-1", p)).toEqual({
    to: "/c/$namespace/$cluster/logs/$pod",
    params: { ...p, pod: "billing-1" },
  });
});

test("the insight section and its screens resolve", () => {
  expect(resolveClusterLink("insight", p)).toEqual({ to: "/c/$namespace/$cluster/insight", params: p });
  for (const s of ["vacuum", "blocking", "statements", "indexes", "connections"])
    expect(resolveClusterLink(`insight/${s}`, p), s).toEqual({ to: `/c/$namespace/$cluster/insight/${s}`, params: p });
  // Diagnose findings name the screen with a leading slash.
  expect(resolveClusterLink("/insight/vacuum", p)?.to).toBe("/c/$namespace/$cluster/insight/vacuum");
});

test("a path this build does not have is not linked, nor guessed", () => {
  for (const l of [
    "backups/evidence",
    "cluster/pods",
    "cluster/metrics",
    "pods",
    "databases",
    "poolers/metrics",
    "logs",
    "toString",
    "constructor",
    "events/x",
    "history/latest",
    "insight/locks",
    "insight/toString",
    "insight/vacuum/x",
  ]) {
    expect(resolveClusterLink(l, p), l).toBeNull();
  }
});
