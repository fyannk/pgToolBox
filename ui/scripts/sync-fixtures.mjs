// Copyright © contributors to the pgtoolbox project.
// SPDX-License-Identifier: Apache-2.0
//
// The MSW fixtures are the API's own fixture dump, never hand-written data:
// internal/console/api/testdata/fixtures, listed by its
// manifest.json with the operation, path and status each body answers.
//
//   node scripts/sync-fixtures.mjs           copy them and regenerate scenarios.gen.ts
//   node scripts/sync-fixtures.mjs --check   fail when either is stale (pnpm check, CI)
//
// scenarios.gen.ts declares each body `satisfies` the OpenAPI type of its
// operation and status, so tsc fails the moment the fixtures and the document
// disagree.

import { existsSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const source = here("../../internal/console/api/testdata/fixtures/");
const jsonDir = here("../mocks/fixtures/json/");
// "Connect an assistant" is rendered by internal/console/assistant's real
// templates; its golden files (one per console configuration, and the prompt
// pack) are the fixtures of getAssistantSetup and of the skill/agents downloads.
const assistantSource = here("../../internal/console/assistant/testdata/");
const assistantDir = here("../mocks/fixtures/assistant/");
const tsPath = here("../mocks/fixtures/scenarios.gen.ts");
const check = process.argv.includes("--check");

/** operationId → the schema of its 200 body. Non-2xx bodies are ErrorModel. */
const BODY = {
  listClusters: "ClusterListView",
  getCluster: "ClusterOverviewView",
  listInstances: "InstanceList",
  listEvents: "EventList",
  getDiagnostics: "Diagnostics",
  getTriage: "Triage",
  getSession: "SessionView",
  getVocabulary: "Vocabulary",
  getAdmin: "Admin",
  getBackups: "Backups",
  getRestoreCheck: "RestoreCheck",
  getReplication: "Replication",
  compareClusters: "Comparison",
  getStorage: "Storage",
  getObjects: "Objects",
  getMetrics: "Metrics",
  getTopology: "Topology",
  listHistory: "History",
  getRevision: "Revision",
  getLogTail: "LogTail",
  listOperations: "Operations",
  requestOperation: "OperationAccepted",
  getOperation: "Operation",
  listAccessRequests: "AccessRequestList",
  getAccessRequest: "AccessRequest",
  createAccessRequest: "AccessRequest",
  approveAccessRequest: "AccessRequest",
  denyAccessRequest: "AccessRequest",
  revokeAccessRequest: "AccessRequest",
  createDatabaseRole: "PersonalDatabaseRole",
  getInsight: "InsightView",
  getInsightBlocking: "InsightBlockingView",
  getFleetStatements: "FleetStatementsView",
  getTimeline: "Timeline",
  listDigests: "DigestList",
  getDigest: "Digest",
  listGrants: "GrantList",
  getAssistantSetup: "AssistantSetupView",
};

let failed = false;
const fail = (msg) => {
  process.stderr.write(`${msg}\n`);
  failed = true;
};
// The current copy is read, not tested for first: a missing file is the
// read's ENOENT, so nothing changes between a check and the write.
const readIfPresent = (path) => {
  try {
    return readFileSync(path, "utf8");
  } catch (err) {
    if (err?.code === "ENOENT") return null;
    throw err;
  }
};
const write = (path, body, what) => {
  const current = readIfPresent(path);
  if (current === body) return;
  if (check) fail(`${what} is stale; run pnpm fixtures`);
  else writeFileSync(path, body);
};

if (existsSync(source)) {
  const wanted = new Set(readdirSync(source).filter((f) => f.endsWith(".json")));
  for (const f of wanted) write(jsonDir + f, readFileSync(source + f, "utf8"), `mocks/fixtures/json/${f}`);
  for (const f of readdirSync(jsonDir)) {
    if (wanted.has(f)) continue;
    if (check) fail(`mocks/fixtures/json/${f} is not in the API's fixtures any more`);
    else rmSync(jsonDir + f);
  }
} else {
  process.stdout.write(`notice: ${source} is absent; checking the committed copies only\n`);
}

if (existsSync(assistantSource)) {
  const golden = /^(setup-.*\.json|(skill|agents)-.*\.md)$/;
  const wanted = new Set(readdirSync(assistantSource).filter((f) => golden.test(f)));
  for (const f of wanted)
    write(assistantDir + f, readFileSync(assistantSource + f, "utf8"), `mocks/fixtures/assistant/${f}`);
  for (const f of readdirSync(assistantDir)) {
    if (wanted.has(f)) continue;
    if (check) fail(`mocks/fixtures/assistant/${f} is not in the assistant's golden files any more`);
    else rmSync(assistantDir + f);
  }
}

const manifest = JSON.parse(readFileSync(`${jsonDir}manifest.json`, "utf8"));
const camel = (file) =>
  file
    .replace(/\.json$/, "")
    .split(/[.-]/)
    .map((w, i) => (i === 0 ? w : w[0].toUpperCase() + w.slice(1)))
    .join("");
const types = new Set(["ErrorModel"]);
let body = "";
for (const entry of manifest) {
  // Event sequences, read by the SSE handlers as data.
  if (entry.operationId === "stream" || entry.operationId === "streamLogs") continue;
  const type = entry.status >= 400 ? "ErrorModel" : BODY[entry.operationId];
  if (!type) {
    fail(`manifest: no schema known for ${entry.operationId}; add it to BODY`);
    continue;
  }
  types.add(type);
  const json = readFileSync(jsonDir + entry.file, "utf8").trim();
  body += `\n/** ${entry.method} ${entry.path} → ${entry.status}: ${entry.scenario} */\nexport const ${camel(entry.file)} = ${json} satisfies ${type};\n`;
}
// The assistant goldens: setup-<name>.json → assistantSetup<Name>; the prompt pack as strings.
for (const f of readdirSync(assistantDir).sort()) {
  const name = camel(`assistant-${f.replace(/\.md$/, "")}`);
  const text = readFileSync(assistantDir + f, "utf8");
  if (f.endsWith(".json")) {
    types.add("AssistantSetupView");
    body += `\n/** internal/console/assistant/testdata/${f}: GET /api/v1/assistant-setup → 200 */\nexport const ${name} = ${text.trim()} satisfies AssistantSetupView;\n`;
  } else {
    body += `\n/** internal/console/assistant/testdata/${f}: the prompt pack (text/markdown) */\nexport const ${name}: string = ${JSON.stringify(text)};\n`;
  }
}
const ts = `// Generated by scripts/sync-fixtures.mjs from internal/console/api/testdata/fixtures
// and internal/console/assistant/testdata; do not edit.
// SPDX-License-Identifier: Apache-2.0

import type { ${[...types].sort().join(", ")} } from "../../src/api/generated/model";
${body}`;
write(tsPath, ts, "mocks/fixtures/scenarios.gen.ts");
if (failed) process.exit(1);
