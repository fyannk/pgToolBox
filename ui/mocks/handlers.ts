// MSW handlers for `pnpm dev`, the Playwright harness and Storybook. Every
// response body is one of the API's own fixtures (internal/console/api/
// testdata/fixtures, copied to mocks/fixtures), served through the
// orval-generated handlers so the routes match the OpenAPI document.
//
// A scenario switches the whole API into one state the UI must handle:
// loading, 503 (could not be asked), 403, 401 (re-authentication), a list
// with Clusters left out, an empty list, or the event stream down (polling
// backstop).

import { delay, HttpResponse, http } from "msw";
import { sse } from "msw/sse";
import {
  getApproveAccessRequestMockHandler,
  getCreateAccessRequestMockHandler,
  getDenyAccessRequestMockHandler,
  getListAccessRequestsMockHandler,
  getRevokeAccessRequestMockHandler,
} from "../src/api/generated/endpoints/access/access.msw";
import { getGetAdminMockHandler } from "../src/api/generated/endpoints/admin/admin.msw";
import {
  getGetAssistantAgentsMockHandler,
  getGetAssistantSetupMockHandler,
  getGetAssistantSkillMockHandler,
} from "../src/api/generated/endpoints/assistant/assistant.msw";
import {
  getCompareClustersMockHandler,
  getGetBackupsMockHandler,
  getGetClusterMockHandler,
  getGetDiagnosticsMockHandler,
  getGetFleetStatementsMockHandler,
  getGetInsightBlockingMockHandler,
  getGetInsightMockHandler,
  getGetMetricsMockHandler,
  getGetObjectsMockHandler,
  getGetReplicationMockHandler,
  getGetRestoreCheckMockHandler,
  getGetRevisionMockHandler,
  getGetStorageMockHandler,
  getGetTimelineMockHandler,
  getGetTopologyMockHandler,
  getGetTriageMockHandler,
  getListClustersMockHandler,
  getListEventsMockHandler,
  getListHistoryMockHandler,
  getListInstancesMockHandler,
} from "../src/api/generated/endpoints/clusters/clusters.msw";
import { getGetDigestMockHandler, getListDigestsMockHandler } from "../src/api/generated/endpoints/digests/digests.msw";
import { getGetLogTailMockHandler } from "../src/api/generated/endpoints/logs/logs.msw";
import {
  getGetOperationMockHandler,
  getListOperationsMockHandler,
  getRequestOperationMockHandler,
} from "../src/api/generated/endpoints/operations/operations.msw";
import { getCreateDatabaseRoleMockHandler } from "../src/api/generated/endpoints/pgadmin/pgadmin.msw";
import {
  getGetSessionMockHandler,
  getGetVocabularyMockHandler,
  getListGrantsMockHandler,
  getRevokeAllGrantsMockHandler,
  getRevokeGrantMockHandler,
} from "../src/api/generated/endpoints/session/session.msw";
import * as fx from "./fixtures";
import { insightBlockingNotEnabled } from "./fixtures/insight";
import streamEvents from "./fixtures/json/stream.events.json";

export const SCENARIOS = [
  "default",
  "loading",
  "unavailable",
  "forbidden",
  "reauth",
  "partial",
  "empty",
  "streamDown",
  "mcpOff",
] as const;
export type Scenario = (typeof SCENARIOS)[number];

const REQUEST_ID = "req-fixture-0001";

/** An API problem body (a fixture) with the status and request id the server would send. */
const problem = (body: { status?: number }) =>
  HttpResponse.json(body, {
    status: body.status ?? 500,
    headers: { "Content-Type": "application/problem+json", "X-Request-Id": REQUEST_ID },
  });

/** The fixture for the named cluster; the API's own 404 for the clusters the fixtures do not cover. */
function overviewFor(name: string) {
  if (name === fx.failingOverOverview.ref.name) return fx.failingOverOverview;
  if (name === fx.healthyOverview.ref.name) return fx.healthyOverview;
  return null;
}
const billing = fx.instances.ref.name;
const orders = fx.replication.ref.name;
/** The API's own 404 for a Cluster the fixtures do not cover (a thrown Response is the response). */
const noFixture = () => Promise.reject(problem(fx.clusterNotFound));
/** A handler that serves `body` for one Cluster and the API's 404 for the others. */
const forCluster =
  <T>(name: string, body: T) =>
  ({ params }: { params: Record<string, string | readonly string[] | undefined> }) =>
    params.name === name ? body : noFixture();

/**
 * The timeline fixture for a query: a window that ends before it starts is
 * the API's 422, the kind filter of the filtered golden its answer, anything
 * else the unfiltered golden (the harness has no other window to serve).
 */
function timelineFor(q: URLSearchParams) {
  const from = q.get("from");
  const to = q.get("to");
  if (from && to && Date.parse(from) >= Date.parse(to)) return Promise.reject(problem(fx.timelineBadWindow));
  const kinds = (q.get("kind") ?? "").split(",").filter(Boolean).sort().join(",");
  if (kinds && kinds === [...(fx.timelineFiltered.kinds ?? [])].sort().join(",")) return fx.timelineFiltered;
  return fx.timeline;
}

function streamHandler(up: boolean) {
  if (!up) return http.get("*/api/v1/stream", () => new HttpResponse(null, { status: 503 }));
  const hello = streamEvents.find((e) => e.event === "hello");
  return sse<{ message: string; hello: unknown }>("*/api/v1/stream", ({ client }) => {
    // The server's opening event; the fixtures do not change, so nothing is invalidated.
    if (hello && "data" in hello) client.send({ event: "hello", data: hello.data });
  });
}

/** The log stream: the opening tail as line events, then the stream stays open. */
const logStreamHandler = sse(
  "*/api/v1/namespaces/:namespace/clusters/:name/instances/:pod/logs/stream",
  ({ client }) => {
    for (const l of fx.logTail.lines ?? []) client.send({ event: "line", data: JSON.stringify(l) } as never);
  },
);

/**
 * The assistant endpoints: the fixture user's grants (a disconnected one
 * leaves the list, as on the server), the setup and the prompt pack; with
 * MCP off, the API's 404 on every one.
 */
export function assistantHandlers(mcp: "on" | "off" = "on") {
  if (mcp === "off")
    return [
      getListGrantsMockHandler(() => Promise.reject(problem(fx.grantsNotServed))),
      getRevokeAllGrantsMockHandler(() => Promise.reject(problem(fx.grantsRevokeAllNotServed))),
      getGetAssistantSetupMockHandler(() => Promise.reject(problem(fx.assistantSetupNotServed))),
      http.get("*/api/v1/assistant-setup/*", () => problem(fx.assistantSetupNotServed)),
    ];
  const revoked = new Set<string>();
  return [
    getListGrantsMockHandler(() => ({ items: (fx.grants.items ?? []).filter((g) => !revoked.has(g.id)) })),
    getRevokeGrantMockHandler((({ params }: { params: { id: string } }) => {
      if (revoked.has(params.id) || !(fx.grants.items ?? []).some((g) => g.id === params.id))
        return Promise.reject(problem(fx.grantNotFound));
      revoked.add(params.id);
    }) as never),
    // Go answers 204 whatever the count (none is not an error).
    getRevokeAllGrantsMockHandler(() => {
      for (const g of fx.grants.items ?? []) revoked.add(g.id);
    }),
    getGetAssistantSkillMockHandler(fx.assistantSkill),
    getGetAssistantAgentsMockHandler(fx.assistantAgents),
    getGetAssistantSetupMockHandler(fx.assistantSetup),
  ];
}

export function handlersFor(scenario: Scenario = "default") {
  switch (scenario) {
    case "loading":
      return [
        http.all("*/api/v1/*", async () => {
          await delay("infinite");
          return new HttpResponse(null, { status: 204 });
        }),
      ];
    case "unavailable":
      return [streamHandler(true), http.get("*/api/v1/*", () => problem(fx.clusterUnchecked))];
    case "forbidden":
      return [
        streamHandler(true),
        getListClustersMockHandler(fx.clusterListDenied),
        http.get("*/api/v1/namespaces/*", () => problem(fx.clusterForbidden)),
        http.get("*/api/v1/compare", () => problem(fx.comparisonForbidden)),
      ];
    case "reauth":
      return [
        http.get("*/api/v1/stream", () => new HttpResponse(null, { status: 401 })),
        http.all("*/api/v1/*", () =>
          problem({ title: "Unauthorized", status: 401, detail: "sign in to use the console" } as { status: number }),
        ),
      ];
    case "partial":
      return [streamHandler(true), getListClustersMockHandler(fx.clusterListPartial)];
    case "empty":
      return [
        streamHandler(true),
        getListClustersMockHandler({ ...fx.clusterList, items: [], namespaces: fx.clusterList.namespaces.slice(0, 1) }),
      ];
    default:
      return [
        streamHandler(scenario !== "streamDown"),
        getGetSessionMockHandler((scenario === "mcpOff" ? fx.sessionMcpOff : fx.session) as never),
        getListClustersMockHandler(fx.clusterList),
        getGetVocabularyMockHandler(fx.vocabulary),
        getListInstancesMockHandler(forCluster(billing, fx.instances) as never),
        getListEventsMockHandler(forCluster(billing, fx.events) as never),
        getGetDiagnosticsMockHandler(forCluster(billing, fx.diagnostics) as never),
        getGetTriageMockHandler(forCluster(billing, fx.triage) as never),
        getGetClusterMockHandler(
          (({ params }: { params: Record<string, unknown> }) =>
            overviewFor(String(params.name)) ?? noFixture()) as never,
        ),
        getGetReplicationMockHandler(forCluster(orders, fx.replication) as never),
        getCompareClustersMockHandler((({ request }: { request: Request }) =>
          new URL(request.url).searchParams.get("rightName") === "gone"
            ? Promise.reject(problem(fx.comparisonNotFound))
            : fx.comparison) as never),
        // orders backs up with barman-cloud; billing with pgBackRest (Dalibo).
        getGetBackupsMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders ? fx.backups : params.name === billing ? fx.backupsPgBackRest : noFixture()) as never),
        // orders runs stock CloudNativePG (no volume usage); billing a build that reports it.
        getGetStorageMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders ? fx.storage : params.name === billing ? fx.storageForecasts : noFixture()) as never),
        getGetRestoreCheckMockHandler(forCluster(orders, fx.restoreCheck) as never),
        getGetObjectsMockHandler(forCluster(orders, fx.objects) as never),
        getGetMetricsMockHandler(forCluster(orders, fx.metrics) as never),
        getGetTopologyMockHandler(forCluster(orders, fx.topology) as never),
        getListHistoryMockHandler(forCluster(orders, fx.history) as never),
        getGetTimelineMockHandler((({ params, request }: { params: Record<string, unknown>; request: Request }) =>
          params.name === billing ? timelineFor(new URL(request.url).searchParams) : noFixture()) as never),
        getListDigestsMockHandler(fx.digests),
        getGetDigestMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.id === fx.digest.id ? fx.digest : Promise.reject(problem(fx.digestNotFound))) as never),
        getGetRevisionMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders && params.seq === String(fx.revision.entry.seq)
            ? fx.revision
            : Promise.reject(problem(fx.revisionNotFound))) as never),
        getGetLogTailMockHandler(forCluster(orders, fx.logTail) as never),
        logStreamHandler,
        getListOperationsMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders ? fx.operationsOperator : fx.operationsViewer) as never),
        getRequestOperationMockHandler(fx.operationRequested),
        getGetOperationMockHandler(fx.operationRunning),
        getGetAdminMockHandler(fx.admin),
        getListAccessRequestsMockHandler(fx.accessRequests),
        getCreateAccessRequestMockHandler(fx.accessCreated),
        getApproveAccessRequestMockHandler(fx.accessApproved),
        getDenyAccessRequestMockHandler(fx.accessDenied),
        getRevokeAccessRequestMockHandler(fx.accessRevoked),
        getCreateDatabaseRoleMockHandler(fx.databaseRoleCreated),
        // orders runs the insight pack, billing does not.
        getGetInsightMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders
            ? fx.insightEnabled
            : params.name === billing
              ? fx.insightNotEnabled
              : noFixture()) as never),
        getGetInsightBlockingMockHandler((({ params }: { params: Record<string, unknown> }) =>
          params.name === orders
            ? fx.insightBlocking
            : params.name === billing
              ? insightBlockingNotEnabled
              : noFixture()) as never),
        getGetFleetStatementsMockHandler(fx.fleetStatements),
        ...assistantHandlers(scenario === "mcpOff" ? "off" : "on"),
      ];
  }
}
