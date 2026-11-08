---
sidebar_position: 16
title: Tracing
---

# Tracing

The console can emit [OpenTelemetry](https://opentelemetry.io/) traces: one
span per HTTP request, one per Kubernetes API call made while serving it, and
one per [MCP](mcp.md) tool call. They are exported over **OTLP/HTTP** to any
collector or backend that speaks it (Jaeger, Grafana Tempo, LangSmith,
Langfuse, Arize Phoenix): which one is configuration only. Tracing is **off by
default**; nothing is built, started or dialled until an endpoint is set.

## Turning it on

| Where | Setting |
|---|---|
| pgtools chart | `console.tracing.endpoint: http://jaeger-collector.monitoring:4318` |
| `PgToolBoxConsole` | `spec.tracing.endpoint: http://jaeger-collector.monitoring:4318` |
| Environment | `OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger-collector.monitoring:4318` |

| Chart value | `PgToolBoxConsole` field | Variable | Default | What it does |
|---|---|---|---|---|
| `console.tracing.endpoint` | `spec.tracing.endpoint` | `OTEL_EXPORTER_OTLP_ENDPOINT` | empty: off | OTLP/HTTP base URL; the console appends `/v1/traces`. |
| `console.tracing.headersSecret` | `spec.tracing.headersSecretRef` | `TRACING_HEADERS_FILE` | none | A key of a Secret you create, holding the exporter's headers as `key=value,key2=value2` (values percent-encoded). It is mounted as a file and read once at start; the console reads no Secret through the API, so a rotated key takes effect at the next restart. |
| `console.tracing.serviceName` | `spec.tracing.serviceName` | `OTEL_SERVICE_NAME` | `pgtoolbox-console` | The `service.name` of every span. |
| `console.tracing.sampler` | `spec.tracing.sampler` | `OTEL_TRACES_SAMPLER` | `parentbased_always_on` | `always_on`, `always_off`, `traceidratio`, `parentbased_always_on`, `parentbased_always_off` or `parentbased_traceidratio`. |
| `console.tracing.samplerArg` | `spec.tracing.samplerArg` | `OTEL_TRACES_SAMPLER_ARG` | none | The ratio of the `traceidratio` samplers, `0` to `1`. |
| `console.tracing.includePayloads` | `spec.tracing.includePayloads` | `TRACING_INCLUDE_PAYLOADS` | `false` | Add MCP tool arguments and results to the tool spans (see [Privacy](#privacy)). |

The console uses the OpenTelemetry SDK's own environment variables, so the
standard ones work when set directly (for example through the chart's
`console.extraEnv`):

| Variable | Notes |
|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | The full traces URL, used as is (no `/v1/traces` appended); also turns tracing on. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | Headers in the same `key=value,...` format. Prefer `TRACING_HEADERS_FILE` for anything secret: an environment value is visible to whoever can read the Deployment. The file's headers override these, key by key. |
| `OTEL_RESOURCE_ATTRIBUTES` | Extra resource attributes, `key=value,...` (for example `deployment.environment=prod,k8s.cluster.name=east`). |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | Only `http/protobuf` is accepted: the console carries no gRPC exporter. |
| `OTEL_TRACES_EXPORTER` | `otlp` (the default) or `none`, which turns tracing off. |
| `OTEL_SDK_DISABLED` | `true` turns tracing off whatever else is set. |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_INSECURE` | Read by the SDK unchanged. |

A malformed endpoint, header list, sampler or ratio stops the console at start
with the variable's name (never a header's value), like any other setting.

### Network

The console dials the collector itself. With `console.networkPolicy.enabled`,
allow it in `console.networkPolicy.extraEgress`, for example:

```yaml
console:
  networkPolicy:
    extraEgress:
      - to:
          - namespaceSelector:
              matchLabels: {kubernetes.io/metadata.name: monitoring}
            podSelector:
              matchLabels: {app.kubernetes.io/name: jaeger}
        ports: [{port: 4318, protocol: TCP}]
```

A SaaS backend (LangSmith, Langfuse Cloud) needs egress to the internet on
443, usually through the site's egress proxy (`HTTPS_PROXY` in
`console.extraEnv` is honoured). Whether traces may leave the cluster is the
site's decision; the console opens no listener for them.

## What is traced

| Span | Name | Attributes |
|---|---|---|
| HTTP request (server) | `METHOD route`: the API operation's path template (`GET /api/v1/namespaces/{namespace}/clusters/{name}/instances`), `POST /mcp`, or a family (`/api/*`, `/assets/*`, `/auth/*`, `/oauth/*`, `/.well-known/*`, `/*` for the UI) | the OpenTelemetry HTTP attributes: `http.route`, `url.path` (where the namespace and the Cluster name are), status, sizes, `user_agent.original`, `client.address` |
| Kubernetes API call (client) | `kube METHOD template` (`kube POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews`, `kube GET /api/v1/namespaces/{namespace}/pods/{name}/log`) | `url.full`, status, `k8s.namespace.name` |
| MCP tool call (internal) | `execute_tool <tool>` | `gen_ai.operation.name=execute_tool`, `gen_ai.tool.name`, `gen_ai.tool.type=function`, `mcp.method.name=tools/call`, `mcp.protocol.version`, `enduser.id` (the person the token names), `pgtoolbox-console.outcome` (`ok`, a status such as `403`, `timeout`, `error`), `pgtoolbox-console.result.bytes`, `pgtoolbox-console.result.truncated`, `error.type` on failure |
| Other MCP request (internal) | the method: `resources/read`, `prompts/get`, `resources/list`, `tools/list`, `initialize`, ... | `mcp.method.name`, `mcp.resource.uri` (a resource read), `gen_ai.prompt.name` (a prompt), `enduser.id`, `pgtoolbox-console.outcome` |

- **Names carry no values.** A Cluster, namespace or pod name is never part of a
  span name, only of its attributes, so backends group spans by route.
- **Only what a request costs.** API calls are traced only when made while
  serving a traced request (the SubjectAccessReviews of a page, a log read).
  The informers' lists and watches, which run all the time, are not, and
  neither are the live streams (`/api/v1/stream`, log follows), which last a
  session.
- **Trace context.** The console continues a caller's W3C `traceparent`: an MCP
  client (or a gateway) that traces its own run sees the console's spans in
  the same trace. A client may also put `traceparent` in the tool call's
  `params._meta`, as the MCP conventions allow; the tool span then joins that
  trace and links to the request's. The console forwards the context to the
  API server, whose own tracing (when enabled) continues it.
- **Sampling.** The default `parentbased_always_on` follows the caller's
  decision and samples everything else. On a busy console, a ratio such as
  `parentbased_traceidratio` with `0.1` keeps a tenth of the requests that
  arrive without a sampled parent.
- **Metrics.** Independently of tracing, `/metrics` carries
  `pgtoolbox_console_mcp_calls_total{tool,outcome}`,
  `pgtoolbox_console_mcp_call_duration_seconds{tool}` and, for the other requests,
  `pgtoolbox_console_mcp_requests_total{method,outcome}`.

## Privacy

- Spans name the person (`enduser.id`, the Kubernetes user name the API server
  resolved) on tool spans; HTTP spans carry the client address and user agent.
  Treat the trace backend as you treat the console's audit log.
- **No payload by default.** Tool arguments and results stay out of spans. With
  `includePayloads`, the arguments (`gen_ai.tool.call.arguments`) and the
  result (`gen_ai.tool.call.result`) are added, each capped at 16 KiB and
  redacted first: values under keys that name a credential (token, secret,
  password, authorization, cookie, credential, API key) and credential-shaped
  text anywhere (JWTs, `Bearer` and `Basic` values, user information in URLs)
  are replaced with `[redacted]`. Results are what the person could read on the
  screens (the console reads no Secret), but they include text written by
  database users (log lines, statements, application names): turn payloads on
  for a backend you would trust with the console's screens.
- Tokens are never traced: the bearer of an MCP request or of an API call is a
  header, and no header is recorded. Exporter headers (an API key) are read
  from the mounted file and never logged.

## Examples

### Jaeger or Grafana Tempo (in the cluster)

```yaml
console:
  tracing:
    endpoint: http://jaeger-collector.monitoring:4318   # Tempo: http://tempo.monitoring:4318
```

Jaeger's all-in-one image accepts OTLP/HTTP on 4318 by default. With an
OpenTelemetry Collector in front, point `endpoint` at the collector instead.

### LangSmith

LangSmith ingests OTLP at `https://api.smith.langchain.com/otel` (EU:
`https://eu.api.smith.langchain.com/otel`; self-hosted: `<your LangSmith
URL>/api/v1/otel`) with the API key in `x-api-key` and the project in
`Langsmith-Project`. Tool spans carry `gen_ai.operation.name=execute_tool` and
show as tool runs.

```bash
kubectl -n team-a create secret generic langsmith-otlp \
  --from-literal=headers='x-api-key=lsv2_...,Langsmith-Project=pgtoolbox'
```

```yaml
console:
  tracing:
    endpoint: https://api.smith.langchain.com/otel
    headersSecret: {existingSecret: langsmith-otlp, key: headers}
```

```yaml
# PgToolBoxConsole
spec:
  tracing:
    endpoint: https://api.smith.langchain.com/otel
    headersSecretRef: {name: langsmith-otlp, key: headers}
```

### Langfuse

Langfuse takes OTLP/HTTP at `https://cloud.langfuse.com/api/public/otel` (or
its US region host, or `<your Langfuse URL>/api/public/otel` self-hosted),
authenticated with HTTP Basic over the project's public and secret keys:

```bash
auth=$(printf '%s' 'pk-lf-...:sk-lf-...' | base64 -w0)
kubectl -n team-a create secret generic langfuse-otlp \
  --from-literal=headers="Authorization=Basic%20${auth}"
```

```yaml
console:
  tracing:
    endpoint: https://cloud.langfuse.com/api/public/otel
    headersSecret: {existingSecret: langfuse-otlp, key: headers}
```

The space after `Basic` is percent-encoded (`%20`), as the header format asks.

### Arize Phoenix

A Phoenix server takes OTLP/HTTP on its web port, 6006:

```yaml
console:
  tracing:
    endpoint: http://phoenix.monitoring:6006
```

Phoenix Cloud needs its space URL as `endpoint` and an API key header
(`api_key=...` or `Authorization=Bearer%20...`, per its documentation) in the
headers Secret.

## Troubleshooting

- The console logs `tracing on` at start with the endpoint (without user
  information or query), the sampler and the headers file's path.
- An unreachable or refusing collector costs nothing but a warning,
  `tracing export failed`, at most once a minute, with how many were
  suppressed; spans are dropped, requests are not slowed.
- `pgtoolbox_console_config_info{name="TRACING"}` says whether tracing is on.
