// The UI keeps two lists Go owns (GET /api/v1/vocabulary): the health order
// tables sort by, and the cluster sections a link may name. They are pinned
// to the API's own fixture so a change in Go fails here, not in a reader's
// table.
import { expect, test } from "vitest";
import vocabulary from "../../mocks/fixtures/json/getVocabulary.json";
import { healthRank } from "../ds/format";
import { resolveClusterLink } from "./clusterLink";

test("health sort order is Go's", () => {
  const order = vocabulary.healthOrder;
  expect([...order].sort((a, b) => healthRank(a) - healthRank(b))).toEqual(order);
  expect(order.map(healthRank)).toEqual(order.map((_, i) => i));
});

test("every section a Go link may name resolves to a route", () => {
  const p = { namespace: "team-a", cluster: "orders" };
  for (const s of vocabulary.sections) {
    // logs needs a pod, as instances/<pod> does; the bare section has no screen of its own.
    const link = s === "logs" ? "logs/orders-1" : s;
    expect(resolveClusterLink(link, p), s).not.toBeNull();
  }
});
