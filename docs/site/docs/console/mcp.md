---
sidebar_position: 15
title: MCP endpoint
---

# MCP endpoint

The console can serve the [Model Context Protocol](https://modelcontextprotocol.io/)
at `/mcp`, so an AI agent or an IDE assistant can read the same facts a person
reads on the screens: the fleet table, a Cluster's status, the triage, the
diagnostics with their reasons, replication, backups, storage, the insight
pack, the timeline and the history. The endpoint is **off by default** and
**read-only** unless its [write tools](#write-tools) are turned on as well.

Every tool calls the same code as the REST endpoint behind the screen it
mirrors, **as the token's owner**: the API server decides (SubjectAccessReview)
which Clusters the agent sees, exactly as it decides for the person. An agent
holds no more rights than the identity whose token it was given.

## Turning it on

| Where | Setting |
|---|---|
| pgtools chart | `console.mcp.enabled: true` |
| `PgToolBoxConsole` | `spec.mcp.enabled: true` |
| Environment | `MCP_ENABLED=true` |

The endpoint needs the console's public URL (`PUBLIC_URL`; the chart's
`console.publicURL` or `console.exposure.host`, the `PgToolBoxConsole`'s `publicURL` or
`exposure.hostname`): the challenge and the metadata name only that origin,
never one taken from a request's `Host` or `X-Forwarded-Proto`. Without it the
console refuses to start, the chart refuses to render and the API server
refuses the `PgToolBoxConsole`.

| Variable | Default | What it does |
|---|---|---|
| `MCP_ENABLED` | `false` | Serve the endpoint at `/mcp` and its metadata at `/.well-known/oauth-protected-resource`. |
| `MCP_WRITE_TOOLS` | `false` | Add the [write tools](#write-tools) `backup`, `reload`, `restart` and `promote` (chart `console.mcp.writeTools`, `spec.mcp.writeTools`). It requires `MCP_ENABLED`, and the tools exist only while operations are served (`OPERATIONS_ENABLED`). |
| `MCP_MAX_RESULT_BYTES` | `262144` (256 KiB) | The cap on one tool result's JSON, from 16 KiB to 4 MiB. A larger result is shortened and says so. |
| `MCP_CALL_TIMEOUT` | `20s` | The time limit of one tool call, from 1s to 50s. |
| `MCP_OAUTH_ACCESS_TTL` | `1h` | The lifetime of an access token the console issues to an MCP client, from 10s to 1h. The client renews it with its refresh token. |
| `MCP_OAUTH_CLIENT_METADATA_DOCUMENTS` | `false` | Accept [client ID metadata documents](#client-registration): an HTTPS URL as `client_id`, which the console fetches. Off by default, since it makes the console reach out to the Internet. |
| `MCP_OAUTH_CLIENT_METADATA_HOSTS` | empty | With metadata documents on, fetch them only from these hosts (a comma-separated list of DNS names, for example `claude.ai,chatgpt.com`). Empty: any public address. |

The chart and the `PgToolBoxConsole` set the tuning variables too. Each is rendered
only while the endpoint is on and only when set; left empty, the console's
default above applies. The API server refuses a `PgToolBoxConsole` value outside the
ranges above. The chart's schema checks the byte range and the shape of the
durations and host names; a duration out of range stops the console at start
with a message naming the variable.

| Pgtools chart | `PgToolBoxConsole` | Environment |
|---|---|---|
| `console.mcp.maxResultBytes: 1048576` | `spec.mcp.maxResultBytes: 1048576` | `MCP_MAX_RESULT_BYTES` |
| `console.mcp.callTimeout: 45s` | `spec.mcp.callTimeout: 45s` | `MCP_CALL_TIMEOUT` |
| `console.mcp.oauthAccessTTL: 15m` | `spec.mcp.oauthAccessTTL: 15m` | `MCP_OAUTH_ACCESS_TTL` |
| `console.mcp.oauthClientMetadataDocuments: true` | `spec.mcp.oauthClientMetadataDocuments: true` | `MCP_OAUTH_CLIENT_METADATA_DOCUMENTS` |
| `console.mcp.oauthClientMetadataHosts: [claude.ai]` | `spec.mcp.oauthClientMetadataHosts: [claude.ai]` | `MCP_OAUTH_CLIENT_METADATA_HOSTS` |

The host list takes effect only with `oauthClientMetadataDocuments`; set
without it, it is rendered and ignored (nothing refuses it). Before turning
the documents on, read [the trade-off](#client-id-metadata-documents-and-egress).

The console is the MCP clients' OAuth authorization server only when its public
URL can be an OAuth issuer: `https`, with no user information, query or
fragment (plain `http` only on `localhost`, `127.0.0.1` or `[::1]`, a console on
your own computer). Otherwise it logs a warning at start and the endpoint
accepts [raw API-server tokens](#raw-api-server-tokens-break-glass) only.

The transport is MCP Streamable HTTP in its stateless form: every request is
authenticated on its own, any replica answers, and no session is kept. A
request carries one JSON-RPC message: batches are refused with `400`.

## Authentication

A client sends `Authorization: Bearer <token>` with every request. The session
cookie is never read on `/mcp`, so a browser's sign-in grants nothing there.
Two kinds of token are accepted:

- a **console grant**, which the console issues as the MCP clients' OAuth
  authorization server once the person has signed in and approved the client
  ([below](#connecting-an-assistant)): the normal way;
- a **raw API-server token** (a ServiceAccount token or an OIDC id_token the
  API server trusts), only while [token sign-in](authentication.md) is on
  (`console.auth.tokenLogin`, `AUTH_TOKEN_LOGIN`): the break-glass way.

Without a token the endpoint answers `401` with
`WWW-Authenticate: Bearer resource_metadata="<public URL>/.well-known/oauth-protected-resource/mcp"`,
and serves that [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728) document
(also at `/.well-known/oauth-protected-resource`). It names the console itself
in `authorization_servers`, and the console serves its
[RFC 8414](https://www.rfc-editor.org/rfc/rfc8414) metadata at
`/.well-known/oauth-authorization-server`. An MCP client follows these by
itself: give it the endpoint's URL and it signs in.

A request that carries an `Origin` header must come from the console's own
origin; MCP clients send none.

### Connecting an assistant

1. Add the console to the client. The console's **Assistants** page
   (`/assistants`: **Assistants** in the user menu, or **Connect an
   assistant** in the ⌘K palette) gives the exact command and configuration
   for each client, see [below](#setup-snippets). For Claude Code:

   ```bash
   claude mcp add --transport http --scope user pgtoolbox https://console.example.com/mcp
   ```

2. On first use (Claude Code: `/mcp`, Codex: `codex mcp login pgtoolbox`,
   OpenCode: `opencode mcp auth pgtoolbox`) the client registers itself and
   opens the console in the browser. Sign in as usual if you are not.
3. The console shows a consent page: the application's name (marked
   *unverified*: it is what the application says about itself), the address it
   will receive the answer at (always on your own computer: the page shows its
   port and path, check them against the program you started), this console,
   the access and how long the connection lasts. **Approve** (it becomes
   clickable after a moment, so a click meant for another page cannot land on
   it) or **Deny**. When the client asked for `mcp:operate` and the write tools
   are on, you choose *read-only* (the default) or *read and operate*.
4. With single sign-on (OIDC), the browser then goes to your identity provider
   once more: the connection gets a sign-in of its own (see
   [below](#the-identity-provider-sign-in-of-a-grant)). An identity provider
   that keeps a browser session answers at once; one that does not (dex, for
   example) asks for your password again.
5. The client receives a code, exchanges it for an access token (one hour by
   default, `MCP_OAUTH_ACCESS_TTL`) and a refresh token, and calls the tools as
   you.

A consent page is good for one answer within 3 minutes, and a person's
answers are limited to 10 a minute. Enter on the page denies; Approve takes a
click once it is armed. Nothing about the request is read (its client ID
resolved, a document fetched) before the console knows who you are, and a
request in error is answered with a page, never sent back to the
application: without your consent, the application learns nothing, not even
that you are signed in. Signing out of the console between Approve and the
identity provider's answer ends the connection's sign-in.

### What a console grant is

A grant acts as the person who approved it, with their identity provider's
token: every tool call is authorized by the API server as that person, exactly
like the screens, and an assistant never holds more rights than its person.

| | |
|---|---|
| Clients | Public clients only (no client secret: a program on a laptop cannot keep one), with loopback redirect URIs: `http://127.0.0.1:<port>/...`, `http://localhost:<port>/...` or `http://[::1]:<port>/...`, any port. A custom URI scheme or an `https` redirect is refused. `localhost` is admitted although [RFC 8252 §8.3](https://www.rfc-editor.org/rfc/rfc8252#section-8.3) advises the IP literals: Claude Code registers `http://localhost:<port>/callback`. |
| Proof | PKCE with `S256` is required; `state` is required; the `resource` must be this console's `/mcp`; the answer carries `iss` ([RFC 9207](https://www.rfc-editor.org/rfc/rfc9207)). |
| Scope | `mcp:read` (also when the client asks for nothing, or only for OpenID scopes), plus `mcp:operate` only when the client asks for it, the write tools are on and you chose it. Any other scope is refused (`invalid_scope`). Each write tool requires `mcp:operate` on top of your RBAC; a raw API-server token is bounded by RBAC alone. |
| Code | Sealed, single use, 60 seconds, bound to the client, the redirect URI, the PKCE challenge and the resource. A code redeemed twice revokes the grant it bought. |
| Access token | Sealed with a key derived from the console's session key for this purpose only (the same on every replica): the grant ID, the client, the scope, the person, the grant's identity provider token, one hour at most and never past that token's expiry. It opens only on a console holding the session key. |
| Refresh token | Sealed, rotated at every use; a refresh token used twice revokes the grant (one of the two holders is not the client). Refreshing renews the grant's identity provider token when it is about to expire (OIDC refresh, with the grant's own refresh token); a grant whose identity provider sign-in cannot be renewed ends with it. |
| Lifetime | The session's maximum age (`AUTH_MAX_AGE`) from the approval, whatever the refreshes. |
| Bounds | 16 grants per person and 1600 per console in the index, counting the revoked ones it still remembers (10 minutes after their revocation, or until they would have ended); past either the console refuses new connections rather than forget a revocation. 16 spent codes and consent answers per person, 1600 per console, each remembered until it expires (about 2 and 3.5 minutes). Past 80 % of either total (1280), a person holding 4 entries or more, or at least the mean share of the people the table holds, is refused new work (the token request answers `invalid_grant`, "too many connected assistants"; a sign-in or a confirmation, "retry in a minute"); a newcomer is admitted while the table has room. Whatever order they arrive in, N identities (N ≥ 80) hold at most 1280 + 4 × (N − 80) places of a table (plus, in the index, one revocation placeholder each): a hundred identities each taking all they may hold at most 1360 codes and 1460 grants, and the places left go to newcomers. Filling a table takes 160 identities (128 for the index). A confirmation of a write tool is one entry whatever steps it records (the page's answer, then its approval and the operation's run are marks on that entry, which no bound refuses), so a person may have 4 confirmations in flight past 80 % and a confirmation admitted at its first step is never refused at a later one. Two writes are not new work and only the hard bounds above refuse them: a refresh re-recording a grant another replica admitted, and a person's first standing revocation placeholder (a replayed code's). Each replica decides from what it holds and the index stays a union, so the replicas agree (admissions on several replicas within the second a write takes to arrive can each take a place). Below 80 % everyone may reach their own bound; past it, legitimate people who already hold 4 connections or 4 sign-ins in progress wait for one to end. The totals are sized for a hundred people at their own bound, and the full index takes about 605 KiB of the shared state ConfigMap's 768 KiB budget (client names are cut to 128 bytes once JSON-escaped). |

On every call the console checks, in order: the token opens and has not
expired; it was issued for this endpoint; the grant index is usable; the index
holds the grant and it is not revoked; the revocation list does not name it;
and the API server still accepts the identity provider's token for the same
person. A grant the index does not hold is refused, never trusted on the token
alone: only for 30 seconds after a token was issued, while the index of
another replica catches up, is a shared index's silence accepted. While the
index cannot be trusted (the shared state ConfigMap unreachable for more than
30 seconds, its watch refused or broken for more than 30 seconds, so that the
replica would not see a revocation written by another, or a grants document the
console cannot read) every grant is refused with `503` and no new one is issued.
A write that lands does not end that state on its own: only an open watch does. A `STATE_CONFIGMAP` that is
missing, or that the console's ServiceAccount may not use, refuses grants the
same way, and the console says so at start with what it needs: the ConfigMap
in its namespace and a Role with `get`, `watch` and `update` on it by name (the
chart renders both unless `console.rbac.create` is false, and its notes then
give the commands).

### The identity provider sign-in of a grant

A grant does not share your browser session's identity provider refresh token.
Identity providers that rotate refresh tokens (dex, Keycloak, Entra ID, Okta)
refuse, or treat as theft, the second use of one: the session and the
assistant would end each other at their next renewal. So, with OIDC, approving
a connection runs a second sign-in at the identity provider whose tokens go to
that grant alone, never to a cookie; each grant renews its own.

:::note dex keeps one refresh token per person and client
Each sign-in at dex replaces the previous refresh token of the same client.
With one client, only the newest of your browser session and your connected
assistants can renew its identity provider token; the others end when their ID
token does (the browser asks you to sign in again, an assistant to connect
again). Give grants a client of their own, `OIDC_GRANT_CLIENT_ID` (chart
`console.auth.oidc.grantClient`, `PgToolBoxConsole` `spec.auth.oidc.grantClient`),
and the browser session keeps renewing whatever assistants connect; among the
assistants themselves, the newest still wins. Without one, keep dex's
`expiry.idTokens` (24 hours by default) at or above `AUTH_MAX_AGE` (12 hours),
so that nothing needs renewing.
:::

The grant client is a second client of the same issuer, registered with the
same redirect URI (`/auth/oidc/callback`). The API server must accept its ID
tokens as it accepts the console's: add its ID to the audiences of the JWT
authenticator (`audiences` and `audienceMatchPolicy: MatchAny` in a structured
`AuthenticationConfiguration`; the legacy `--oidc-client-id` flag takes one
audience only). Grants signed in before it was configured keep renewing with
the console's client.

With a sign-in
that renews nothing (OpenShift, a pasted token), the grant carries the
session's token, no identity provider refresh token, and ends when that token
does; the consent page says so.

### Your connected assistants

The **Assistants** page (`/assistants`, in the user menu) lists your grants
(application name, marked unverified, scope, when you approved it, its last use
to within about five minutes, when it ends) and disconnects one, or all:
`GET /api/v1/session/grants`, `DELETE /api/v1/session/grants/{id}`,
`DELETE /api/v1/session/grants`. A disconnected grant is refused by every
replica within about a second (the grant index is in the shared state
ConfigMap, `STATE_CONFIGMAP`, see [sessions](authentication.md)): the
disconnection is written to the ConfigMap before the console answers. When
that write cannot be made (the API server refuses or does not answer), the
answer is `503` with `Retry-After` and "retry": the replica that answered
refuses the grant at once and writes the disconnection with its next write, but
the others may accept it until then. Retry, or use **Disconnect all**, which
writes again what an earlier attempt left unwritten. The Assistants page closes
the dialog on that `503` and says the assistant is disconnected on the replica
that answered while the others may accept it for a few more seconds; the
assistant stays listed as "being written to the other replicas" with a
**Retry** button, and **Disconnect all** is offered as long as one is pending,
even with a single assistant. A retry that answers `404` means nothing was left
to write: the page reports the assistant disconnected. A replica that has not
read the shared state yet (a pod just started) answers `503` instead, to a
disconnection of a grant it does not know and to **Disconnect all** with nothing
to revoke there: it knows none of the grants the others recorded. With MCP
OAuth on, `/readyz` waits for that first read, so such a pod takes no traffic
first. While the ConfigMap does
not exist (deleted, or not created yet), a disconnection is answered done, and
logged: every replica refuses every grant until the ConfigMap exists, and the
replica that answered writes the disconnection into it at its next read of the
ConfigMap, within two seconds of its return (a missing ConfigMap is read every
one to two seconds). Meanwhile another replica that reads the new ConfigMap
first and recorded the grant less than eight minutes earlier may serve it for
those seconds. A restart of the replica that answered before then loses the
disconnection (that other replica then keeps the grant). A ConfigMap the console may not write is not so: a replica whose
watch opened before the permission was withdrawn still serves grants until its
next write, so that answer stays `503`. A sign-out with
**disconnect my assistants** goes on even then (the console log says so). Without the
shared state the index is in each replica's memory: a restart disconnects every
assistant, and with more than one replica each refuses the grants of the
others. Run one replica, or set `STATE_CONFIGMAP`.

The [revocation list](authentication.md#revoking-sessions) applies to grants:

- a grant ID (`grant-<32 hex>`, in the audit log and on the Assistants page)
  under `sessions` refuses that grant;
- the ID of the console session the approval was given in, under `sessions`,
  refuses every grant approved in it;
- a `users` or `notBefore` entry refuses grants approved before its time:
  this is "sign out everywhere".

Signing out of the console does **not** end the grants approved in that
session, unless you choose **Sign out and disconnect my assistants** on the
sign-out page: an assistant keeps working until you disconnect it, it reaches
its lifetime, or your identity provider refuses to renew its sign-in.

### Client registration

- **Dynamic registration** ([RFC 7591](https://www.rfc-editor.org/rfc/rfc7591),
  `POST /oauth/register`), always on. The console stores nothing: the
  `client_id` it returns is the registration itself, sealed with the session
  key. A registration that also lists URIs the console does not admit (VS Code
  adds `https://vscode.dev/redirect` beside its loopback one) is kept with its
  loopback URIs only; one with none is refused. 10 registrations a minute per
  address, and 600 a minute (bursts of 120) per replica: registration stores
  nothing, so that bound only caps the replica's work and audit lines. A client
  registers once and keeps its `client_id`, so a flood of registrations delays
  only new ones.
- **Client ID metadata documents** (an HTTPS URL as `client_id`), with
  `MCP_OAUTH_CLIENT_METADATA_DOCUMENTS=true`. The console then fetches the
  document at that URL, fenced: `https` only, never to a private, loopback,
  link-local or otherwise non-public address (checked on the address actually
  dialed, after DNS), no proxy, no redirect, 5 seconds, 32 KiB, a JSON content
  type; the document must name itself and pass the registration rules; answers
  are cached from five minutes to a day, refusals a minute. Concurrent
  requests for one document share one fetch, and a caller that gives up does
  not cut it short for the others. Only a signed-in person's authorization
  request makes the console fetch. `MCP_OAUTH_CLIENT_METADATA_HOSTS` restricts
  the hosts. The console needs egress to the Internet for it, which is why it is
  off: where egress is policed, allow the console's pods HTTPS (443) to those
  hosts only. Clients that prefer these documents (Codex, Claude Code) fall
  back to dynamic registration when the console does not advertise them.

### Client ID metadata documents and egress

| Pgtools chart | `PgToolBoxConsole` |
|---|---|
| `console.mcp.oauthClientMetadataDocuments: true` | `spec.mcp.oauthClientMetadataDocuments: true` |

Both are off by default and rendered only with the endpoint on. **The
trade-off:** with them on, a URL chosen by whoever starts a sign-in becomes a
request your console makes, from inside your cluster. The fence above keeps
that request off private, loopback and link-local addresses, and the host list
narrows it to the clients you name, so leave the documents off unless a client
you use cannot register dynamically, and when you turn them on, set
`oauthClientMetadataHosts` (for example `[claude.ai, chatgpt.com]`).

The console then needs HTTPS (TCP 443) to the Internet. Neither install path
opens it for you, as for the identity provider, a trace collector or the
digest's servers:

- **pgtools chart**: with `console.networkPolicy.enabled`, add the rule to
  `console.networkPolicy.extraEgress`; the chart's notes remind you.
- **`PgToolBoxConsole`**: the operator renders no NetworkPolicy around the console;
  open it in your site's own policy, if it polices egress.

A NetworkPolicy cannot name hosts, only addresses, so the rule opens port 443
to public addresses and the host list stays the console's own check:

```yaml
console:
  networkPolicy:
    extraEgress:
      - to:
          - ipBlock:
              cidr: 0.0.0.0/0
              except: [10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16]
        ports:
          - {port: 443, protocol: TCP}
```

Without that egress, the console still advertises the documents, and a client
that uses them cannot sign in (its fetch times out after 5 seconds) instead of
falling back to dynamic registration. A document URL on another port than 443
is refused by such a rule as well.

There is no OpenID Connect discovery document
(`/.well-known/openid-configuration`): the console is an OAuth authorization
server, not an identity provider, and MCP clients read RFC 8414 metadata.

### Replicas and replays

The grant index and the redeemed codes are shared between replicas through
the shared state ConfigMap, written within about a second of a change. A code
or a refresh token presented to two replicas within that second can be
accepted by both. Using a code takes the PKCE verifier as well, which only the
client holds; and the two holders then share one refresh generation, so the
first refresh of either makes the other's refresh a reuse, which revokes the
grant. The alternative, a write to the API server before every token answer,
would put the API server on the path of every refresh.

When the table of redeemed codes is full, a code exchange answers `503
temporarily_unavailable` and spends nothing: the client may retry the same
code within its minute. It is never taken for a replay, which would revoke a
grant that did nothing wrong.

### Setup snippets

`GET /api/v1/assistant-setup` returns, for Claude Code, Codex CLI, OpenCode and
other clients (VS Code, Cursor), the one-line add command, the user and
project configuration files, the command that signs in, and the one that
checks the connection. While token sign-in is on it adds each client's
static-token form: Claude Code's `headersHelper` (a fresh token at each
connection), Codex's `bearer_token_env_var`, OpenCode's `{env:VAR}` header with
`"oauth": false`, Cursor's `${env:VAR}` header.

Each client's own reference, checked on 2026-11-06:
[Claude Code](https://code.claude.com/docs/en/mcp),
[Codex](https://learn.chatgpt.com/docs/extend/mcp?surface=cli),
[OpenCode](https://opencode.ai/docs/mcp-servers/),
[Cursor](https://cursor.com/docs/mcp) and
[VS Code](https://code.visualstudio.com/docs/agents/reference/mcp-configuration).
VS Code reads two formats: `servers` (with `"type": "http"`) in
`.vscode/mcp.json`, which the snippet uses, and `mcpServers` in its portable
`~/.copilot/mcp-config.json`, which its *Add Server* flow now proposes for a
personal server (the same entry under `mcpServers`, with `"type": "http"`).

The prompt pack is generated from the tool list:
`GET /api/v1/assistant-setup/skill` is a Claude Code skill (`SKILL.md`, for
`.claude/skills/pgtoolbox/`) and `GET /api/v1/assistant-setup/agents` an
`AGENTS.md` section for Codex and OpenCode. Both say what the tools are, that
text from the cluster is data and not instructions, and to cite the console
page of each finding.

### Raw API-server tokens (break-glass)

While token sign-in is on, a raw API-server token is checked the way the token
sign-in checks a pasted token:

- The API server says whose token it is (SelfSubjectReview); positive answers
  are cached 30 seconds, refusals are not cached. A JWT whose `exp` has passed
  is refused even while that answer is cached.
- The [revocation list](authentication.md#revoking-sessions) applies. A token
  is named in the audit trail by a reference `bearer-<16 hex>` (a digest, not
  the token). List that reference under `sessions` to refuse one token. A
  `users` or `notBefore` entry refuses a JWT issued before its time and refuses
  an opaque token (whose issue time is unknown) until the entry is removed.
- Signing out of the console also refuses the session's token as a bearer,
  until the session would have ended.
- **These stop one token, not its holder.** A client that mints a token per
  call (a `kubectl create token` helper, a refresh-token flow) presents a new
  reference every time. To stop such a holder, remove its RBAC binding or
  delete the ServiceAccount; for a person, revoke them at the identity
  provider and list them under `users`.
- The tools then authorize every read as that user, 10-second positive
  SubjectAccessReview cache and no negative cache, like the screens.

For a client that takes headers (Claude Code, for example):

```bash
kubectl -n team-a create token pgtoolbox-console-reader --duration=1h > ~/.pgtoolbox-token
claude mcp add --transport http pgtoolbox https://console.example.com/mcp \
  --header "Authorization: Bearer $(cat ~/.pgtoolbox-token)"
```

Give the agent a token of an identity with the rights you mean it to have,
ideally a ServiceAccount bound to the `pgtoolbox-console-viewer` ClusterRole in the
namespaces it should read. A short `--duration` bounds what a leaked token
can do.

## Tools

Every tool takes Kubernetes names (`namespace`, `name`) and returns JSON, both as
structured content and as text. A Cluster the caller may not read is a tool
error (`403 Forbidden`), never data.

| Tool | Mirrors | Arguments |
|---|---|---|
| `list_clusters` | the fleet table | none |
| `cluster_status` | a Cluster's overview | `namespace`, `name` |
| `triage` | the triage panel | `namespace`, `name` |
| `diagnostics` | the diagnostics, with each check's reason text from the [checks reference](../reference/checks.md) | `namespace`, `name` |
| `replication` | the replication screen | `namespace`, `name` |
| `backups` | the backups screen | `namespace`, `name` |
| `restore_check` | the restore-point check | `namespace`, `name`, optional `at` (RFC 3339; default now) |
| `storage` | the storage screen | `namespace`, `name` |
| `insight_statements` | the [insight pack](insight-pack.md) statements; without a Cluster, the fleet's | optional `namespace` and `name` |
| `insight_blocking` | the blocking-sessions view (the last reading, not a new one) | `namespace`, `name` |
| `timeline` | the [timeline](timeline-and-digest.md) | `namespace`, `name`, optional `from`, `to` (31 days at most), `kinds`, `limit` |
| `history` | the history screen | `namespace`, `name` |
| `events` | the Kubernetes events of the Cluster | `namespace`, `name` |
| `compare` | the comparison of two Clusters | `leftNamespace`, `leftName`, `rightNamespace`, `rightName` |
| `digests` | the weekly digests | optional `id` |
| `instance_logs` | the bounded log tail (never a stream); only when logs are served | `namespace`, `name`, `pod`, optional `container`, `previous`, `lines` (default 200, at most 500) |
| `operation_status` | an operation's status; only when operations are served | `operationId` |
| `search_docs` | this documentation, at the console's own version (BM25 over its sections) | `query`, optional `limit` (default 8, at most 25) |
| `read_doc` | one page of it, as Markdown | `path` (from `search_docs`) |

With `MCP_WRITE_TOOLS` on, the [write tools](#write-tools) are listed too.

Resources: `pgtoolbox://clusters/{namespace}/{name}` (the overview),
`pgtoolbox://vocabulary` (the states, reasons and checks the results use), and
every documentation page as `docs://<version>/<path>`. The documentation is the
one embedded in the console image, so an agent reads the text of the release it
talks to, without network access.

Prompts: `triage_cluster`, `explain_replication_lag`, `last_backup_restorable`,
`switchover_checklist` (each takes `namespace` and `name`) and
`weekly_digest_review`.

## Write tools

Two switches, both off by default: `mcp.enabled` serves the endpoint, read-only;
`mcp.writeTools` (accepted only with `mcp.enabled`) adds four tools that change
a Cluster. Nothing finer: what a person may do is RBAC's business, and every
write is confirmed by the person.

| Pgtools chart | `PgToolBoxConsole` | Environment |
|---|---|---|
| `console.mcp.enabled: true` | `spec.mcp.enabled: true` | `MCP_ENABLED=true` |
| `console.mcp.writeTools: true` | `spec.mcp.writeTools: true` | `MCP_WRITE_TOOLS=true` |

| Tool | Operation | Arguments | Hints | Confirmed |
|---|---|---|---|---|
| `backup` | an on-demand Backup (`create backups`) | `namespace`, `name` | not destructive, not idempotent (each call is a new backup) | in the client (form) |
| `reload` | reload the configuration (`patch clusters`) | `namespace`, `name` | not destructive, idempotent | in the client (form) |
| `restart` | roll every instance (`patch clusters`) | `namespace`, `name` | destructive, not idempotent | on a console page (URL) |
| `promote` | switchover to a replica (`patch clusters/status`) | `namespace`, `name`, `target` | destructive, not idempotent | on a console page (URL) |

Each tool is the console's own operation, the one the Operations page runs:
the same function, not a copy. The call

1. refuses a [console grant](#what-a-console-grant-is) that was not consented for
   *read and operate* (`403`): a grant given while the write tools were off stays
   read-only after they are turned on; the person reconnects the assistant to
   grant more;
2. reads the Cluster as the person (a Cluster they may not get is `403`) and the
   API server's answer for the operation's verbs: without them, `403` before the
   person is asked anything; a precondition not met now (hibernated, a
   switchover in progress, a rollout in progress for `restart`, an ineligible
   replica) is `409`;
3. asks the person to confirm (below), at most five operations a minute per
   person **and replica**, charged when a confirmation is first asked (`429`
   past it; see [Budgets](#budgets));
4. spends the confirmation (one confirmation runs one operation), then runs the
   operation: the typed confirmation, a live read and the preconditions on it,
   a check that the Cluster is still the one the person confirmed on (`409`
   *the Cluster changed since the person confirmed* when it was recreated, or
   for `restart` its spec, phase or primary changed, for `promote` its
   primary). The question was built on the console's cache, which can be a
   moment behind the live read: on the first such difference nothing is done
   and the person is **asked again**, on the Cluster as the live read found
   it; a second difference is the `409`. Then the write is made **with the
   person's own token** (the API server
   authorizes it and audits it under their name), the console's `operation`
   audit line and the tracker. The result carries the `operationId`; follow it
   with `operation_status`.

The server never retries a write and there is no batch tool. A failure of the
console or the API server is `503`, `timeout` or `error`, never a refusal: a
`403` always means the person's rights or the grant's scope, and a refusal by
an admission webhook, a policy, a quota or a terminating namespace is `422`. A
write that runs out of time answers that it **may have been applied**: check
`operation_status` (or the Operations page) and do not retry it. The write
itself has its own deadline, so a client that gives up does not cut it short.

### Confirmation

The call returns a question, as an input request of the protocol's multi
round-trip (version 2026-07-28, MCP elicitation), and the client sends the call
again with the answer. The question names the operation, the Cluster (and for
`promote` the instance), the person it runs as, and what it does and costs.
Each question carries a random single-use value, sealed with the person, the
credential (the grant or the raw token), the tool, the target and the Cluster's
state, and good for **two minutes**. The answer is spent in the shared index
before anything runs, so a replayed answer is refused (`409`, *already used*).
With `STATE_CONFIGMAP` the spend is written to the ConfigMap (an update under
the API server's optimistic concurrency) before the operation runs, so a
replay sent to another replica is refused too, even before that replica's
watch has delivered the write; without it, the spend is in the replica's
memory (run one replica). When the spend cannot be recorded (the ConfigMap is
not writable or keeps conflicting, or too many confirmations are pending), the
tool refuses with `503` and does nothing. The page's answer and its approval
or refusal are written the same way.

What each kind of confirmation guarantees:

| Kind | Tools | How the person answers | What it proves |
|---|---|---|---|
| Form | `backup`, `reload` | types the Cluster name in the client's dialog | that the client asked. The client relays the answer, so a client (or a model driving one) can answer by itself; it is kept for the two operations that interrupt nobody. Grant *read and operate* only to a client you trust with them. |
| URL | `restart`, `promote` | opens the console's page `/mcp/confirm`, signed in to the console **in their own browser**, checks what asked (a console grant: its client's declared name and the callback host its consent sent the code to; or a raw API-server token), when, and for `promote` the primary now and after, types the Cluster (or instance) name, approves | that the person approved. The page needs the person's browser session (an MCP client holds no such cookie), posts only from the console's own origin (the CSRF rules), is bound to the person the tool asked for, and takes one answer. The tool runs only once the page recorded the approval. |

After approving on the page, the person returns to the client and continues;
the client calls the tool again. With several replicas, an answer reaches the
other replicas in about a second: a call that arrives before is asked again
(at most three times), then refused.

A client that cannot ask is **refused**, and nothing is done: without form
elicitation for `backup` and `reload`, without **URL** elicitation for
`restart` and `promote` (the tool says so and points to the Operations page).
There is deliberately no `confirm` argument: the model would fill it in itself,
and a tool call chosen by a model, perhaps after reading text an application
wrote into a log, is not the person agreeing to it.

Client support for URL elicitation on protocol 2026-07-28, as far as it could
be checked on 2026-11-06 (verify against your client's release notes):

| Client | Form | URL |
|---|---|---|
| Claude Code | yes (since 2.1.76) | yes since 2.1.281, reported in its changelog and issue tracker; earlier versions are refused `restart` and `promote` |
| Codex CLI | yes | not confirmed: an open request upstream (openai/codex#29344) |
| OpenCode | not confirmed | not confirmed |

Not tested against these clients; the console's own tests use the Go SDK's
client.

### Who may operate

| Token | Write tools |
|---|---|
| A console grant consented for *read and operate* | yes, with the person's rights |
| A console grant consented for *read-only*, or before the write tools were on | no (`403`) |
| A raw API-server token (break-glass, `tokenLogin`) | yes, with its owner's rights: it is the person's own token, so the API server is the gate |

In every case `MCP_WRITE_TOOLS` must be on, and the API server decides whether
the write is allowed. A read-only grant is not shown the write tools in
`tools/list` (served with `cacheScope: private`).

The consent page of a grant names the client by the name it declared and warns,
when it offers *read and operate*, what that lets the client ask for.

### Audit

Each round is an `mcp.call` line: `confirmation_requested` when the question
is sent, then `ok`, `declined` (the person declined or cancelled, in the client
or on the page), `unconfirmed` (the client cannot ask, or the page was not
answered in time), or a status. The page's answer is an `mcp.confirm` line
(`approved` or `declined`). The operation itself writes the console's
`operation` line with `via=mcp`, the client's declared name (`assistant`,
unverified, without control or format characters) and the grant ID (`grant`,
empty for a raw token); an operation from the console's screens says
`via=api`. The `mcp.call`, `mcp.confirm` and `operation` lines of one
confirmation carry the same `confirmation` ID (a hash of its single-use value,
not the value). The confirmation link is never logged.

## Budgets

| Budget | Value |
|---|---|
| Requests per user | 120 a minute, bursts of 60; past it, `429` with `Retry-After`. |
| Requests in flight per user | 4; past it, `429`. The console advertises no list-change notifications (its lists do not change during a session), so an assistant keeps no request open between calls. A `subscriptions/listen` request a client opens anyway does not count toward the 4 in flight, but has its own cap of 16 per person and replica, past which a new one is refused with `429`. A `GET` is answered `405` at once (the endpoint is stateless). |
| Unrecognised tokens per client address | The sign-in form's budget, shared with it: 10 a minute per replica. A token accepted in the last 30 seconds does not count. The client address is the peer's unless it is a trusted proxy ([client address](authentication.md#sessions)). |
| OAuth endpoints | Authorization requests and token requests: 60 a minute per client address, bursts of 30. Token requests also per replica, once the code or refresh token has been opened and checked: one this console sealed that is still good (unexpired, this client's, PKCE verified, of a grant and person not revoked) spends its grant's bucket (12 a minute, bursts of 6), then its person's (the name sealed at consent: 24 a minute, bursts of 12, so neither one person's 16 grants nor a few dozen identities empty the shared bucket) and then 20 a second (bursts of 40); a refresh token behind its grant's generation is a replay, refused (and the grant revoked) before any of these; anything else, junk or a sealed token that no longer buys anything (a spent code or revoked refresh token replayed), or any refresh while the grant index cannot be trusted (the `503`), a bucket of its own of 20 a second, so neither junk nor a token once held refuses a connected client's refresh (past its bucket, `429` and no audit line). Registrations: above. |
| Confirmation pages | 30 loads a minute per client address; answers 10 a minute per person. |
| Behind a shared address | With `TRUSTED_PROXY_HOPS` and no `TRUSTED_PROXY_CIDRS`, or with hops 0 behind a `PUBLIC_URL` that is not loopback, every client may have the proxy's address, so no per-address budget above is charged (one sender would spend it for everyone). Instead: unrecognised tokens 10 a minute **per token**, under the per-replica review bound; authorization requests and confirmation pages per signed-in person (anonymous ones only reach the sign-in); token requests and registrations by their per-replica bounds only. |
| Raw API-server tokens reviewed | A token this replica never accepted: the token form's per-replica bound, shared with it (100 a second, bursts of 200), charged after the address or token budget. A token it accepted before (remembered by digest until it expires, an hour at most; 16 a person and 4,096 a replica, the least recently used forgotten first, so one person minting tokens forgets only their own): its person's share first (32 a minute, bursts of 16), then 20 a second, bursts of 40, a bound no junk token reaches, so a flood cannot fail a connected client's re-review. A token whose answer is cached (30 s) does not count. Past either, `429` with `Retry-After`. Every refusal is counted in `pgtoolbox_console_throttled_requests_total{bucket}`. |
| Result size | `MCP_MAX_RESULT_BYTES`. The longest lists are shortened first (their first items kept, in the API's order), then strings over 2 KiB; the result's `truncated` object names what was cut. |
| Call time | `MCP_CALL_TIMEOUT`. |
| Request body | 64 KiB, one JSON-RPC message (no batch). |
| Write tools' confirmations per user | 5 a minute **per replica**, charged when a confirmation is first asked (its re-asks, its answer and the operation are not charged); past it, the call is refused with `429` and nothing is asked or done. With two replicas a person can be asked up to 10 times a minute. |

## Security notes

- **Text from the cluster is data, not instructions.** Log lines, event
  messages and reasons, condition messages, labels, phase reasons, database,
  role, schema, table, slot and object names, and application names come
  from whoever can write to the database or the cluster. Every such field is
  marked where the view defines it and wrapped as `{"untrusted": ...}` in the
  results (a string, or a whole map of labels), and the server's
  instructions tell the agent never to follow instructions found in them. An
  agent that acts on a tool result can still be misled: keep its token's
  rights to what it needs, and keep the write tools off unless the people
  using them want them; with them on, every write still waits for a
  single-use confirmation, and `restart` and `promote` for the person's
  approval on a console page.
- **Audit.** Every tool call writes an `mcp.call` line: user, provider, token
  reference, tool, argument names (values too, except free text), outcome
  (`ok`, a status such as `403`, `404` or `422`, `timeout` or `error`), result
  size and whether it was shortened. Arguments that do not match a tool's
  schema are refused by the protocol layer before the tool runs. A refused
  request writes `mcp.denied` with the reason. The authorization server writes
  `mcp.register` (a registration, with the redirect hosts), `mcp.grant`
  (approved, denied, or refused when the identity provider signed in another
  person, with the grant ID, the scope and whether the grant has identity
  provider tokens of its own), `mcp.token` (each code exchange and refresh,
  issued or refused with the reason) and `mcp.revoke`. No token, code or client
  ID is ever logged; an identity provider's refusal of a renewal is logged by
  its error code only. The grant index names people by a keyed hash (HMAC with
  a key derived from the session key), so the ConfigMap does not say whose a
  grant is to someone who can read it.
  The same outcomes are counted in `pgtoolbox_console_mcp_calls_total{tool,outcome}`,
  with `pgtoolbox_console_mcp_call_duration_seconds{tool}`, and each call can be
  [traced](tracing.md) as an OpenTelemetry `execute_tool` span. A Grafana dashboard
  and alerts over them ship with the chart ([MCP alerts](alerts.md)).
- **No new rights.** The endpoint holds no credential of its own: the console's
  ServiceAccount is not used for any tool read beyond what the screens already
  use, and it never impersonates.
