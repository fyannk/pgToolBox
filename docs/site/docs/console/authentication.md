---
sidebar_position: 2
title: Authentication
---

# Authentication

The console never decides who someone is. After sign-in it holds one thing for
the person: **a token the Kubernetes API server accepts**. Every identity it
shows or uses (username and groups) comes from a `SelfSubjectReview` the API
server answered for that token, so the API server's own authenticator is the only
source of names. No level, role or group is stored in a cookie, and no group
claim is read from an ID token (`OIDC_GROUPS_CLAIM` does not exist).

Consequence for RBAC: a RoleBinding subject must be spelled exactly as the API
server reports the person. Sign in once and read `GET /api/v1/session` (or the
user menu) to see the username and groups the console got; that is what to bind.

## Providers

`console.auth.provider` (the `AUTH_PROVIDER` variable) is one of:

| Provider | Person does | Needs |
|---|---|---|
| `token` | Pastes an API-server token into the sign-in form. | Nothing. Works on every cluster. |
| `oidc` | Signs in at the identity provider (authorization-code flow with PKCE and a nonce). | The **API server must trust the same issuer**: `--oidc-*` flags or an `AuthenticationConfiguration`, because the ID token the console keeps is sent to the API server as the bearer token. |
| `openshift` | Signs in through OpenShift's OAuth server. | OpenShift. The console's ServiceAccount is the OAuth client. See [OpenShift](openshift.md). |

The pasted-token form stays offered next to an SSO provider as the break-glass
path. Remove it with `console.auth.tokenLogin=false` (`AUTH_TOKEN_LOGIN=false`).

On a managed control plane that cannot be told to trust your identity provider,
SSO with `oidc` does not work: the console cannot forward a token the API server
would reject. Use `token`, or an external proxy that forwards an API-server token.
Impersonation (the console calling the API server with its own token plus
`Impersonate-User`) is not offered; see the [security model](../security.md).

### The `oidc` provider

| Setting | Chart value | Notes |
|---|---|---|
| `OIDC_ISSUER_URL` | `console.auth.oidc.issuerURL` | https only. |
| `OIDC_CLIENT_ID` | `console.auth.oidc.clientID` | |
| `OIDC_CLIENT_SECRET_FILE` | `console.auth.oidc.clientSecret.existingSecret` and `.key` | A Secret you create; the chart mounts it as a file. The console reads no Secret through the API. |
| `OIDC_REDIRECT_URL` | `console.auth.oidc.redirectURL` | Register `https://<host>/auth/oidc/callback` with the provider. |
| `OIDC_SCOPES` | `console.auth.oidc.scopes` | Must contain `openid`. The default is `openid email profile offline_access`; `offline_access` is what lets the console refresh a token. Without it a session lasts as long as the ID token. |
| `OIDC_CA_FILE` | `console.auth.oidc.caConfigMap` | A ConfigMap with `ca.crt`, for a private provider. |
| `OIDC_REQUIRE_EMAIL_VERIFIED` | `console.auth.oidc.requireEmailVerified` | Default `true`: a login whose ID token carries an `email` claim without `email_verified=true` is refused (a token with no `email` claim passes). Set `false` only for a provider that omits `email_verified`. |
| `OIDC_GRANT_CLIENT_ID`, `OIDC_GRANT_CLIENT_SECRET_FILE` | `console.auth.oidc.grantClient.clientID` and `.clientSecret` | Optional, off by default: a second client of the same issuer for the identity provider sign-ins of [MCP clients' grants](mcp.md#the-identity-provider-sign-in-of-a-grant). Register it with the same redirect URI, and make the API server accept its audience too (below). |

A sign-in with `oidc`, from the click to the session cookie:

```mermaid
sequenceDiagram
    actor B as Browser
    participant C as Console
    participant I as Identity provider
    participant K as API server
    B->>C: GET /auth/oidc/login?rd=page
    Note over C: state, nonce, PKCE verifier<br/>sealed in a 10-minute flow cookie
    C-->>B: 302 to the authorization endpoint (S256)
    B->>I: sign in
    I-->>B: 302 /auth/oidc/callback?code&state
    B->>C: callback, with the flow cookie
    Note over C: flow cookie cleared, state compared,<br/>replay refused, then rate limit
    C->>I: exchange code + verifier
    I-->>C: ID token, refresh token
    Note over C: signature, nonce, email_verified
    C->>K: SelfSubjectReview with the ID token
    K-->>C: username and groups
    C-->>B: sealed session cookie, 302 to page
```

With an OIDC grant client (`OIDC_GRANT_CLIENT_ID`), the API server must accept
both audiences, for example:

```yaml
apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
jwt:
  - issuer:
      url: https://idp.example.com
      audiences: [pgtoolbox-console, pgtoolbox-console-grants]
      audienceMatchPolicy: MatchAny
    claimMappings:
      username: {claim: email, prefix: "oidc:"}
```

The `openshift` provider follows the same flow without the nonce and the ID
token: the session keeps OpenShift's access token. If the API server does not
accept the token (it does not trust the issuer), the sign-in fails there with a
message that says so, and no cookie is set.

### The `token` provider (break-glass)

`POST /auth/token` takes the pasted token (an optional `Bearer ` prefix is
dropped), spends one sign-in attempt, verifies it with a `SelfSubjectReview` and
keeps it in the sealed session; the session ends no later than the token's own
expiry when the token says it (a `kubectl create token` token does). The form is
accepted only from the console's own page (same origin). Every action is then audited by the API server as the
ServiceAccount or user the token belongs to; revoking access is deleting the
binding. The product stores no password.

`console.breakGlass.enabled=true` (off by default) renders the identity behind the
form: a ServiceAccount `pgtoolbox-console-breakglass` that holds no token and mounts
nothing, and in each watched namespace a RoleBinding to the `pgtoolbox-console-operator`
ClusterRole, which must exist (`operator.userRoles.create`, or `config/rbac`). When
it is needed, mint a short-lived token and paste it into the form:

```bash
kubectl -n team-a create token pgtoolbox-console-breakglass --duration=1h
```

Whoever can create that token holds `pgtoolbox-console-operator` rights in the watched
namespaces, so leave it off until you need it, or create the binding by hand and
delete it afterwards.

#### A local test with a ServiceAccount token

To script against a console reached with `kubectl port-forward`, sign in once with
the form and keep the session cookie. The form wants the console's own origin
(`http://localhost:8080` here; the public URL when one is set), and
`/api` reads only the session cookie: by design it ignores an `Authorization:
Bearer` header and answers 401 (only `/mcp` takes a bearer).

```bash
kubectl -n team-a port-forward svc/console 8080:80 &   # the command the chart's NOTES print
TOKEN=$(kubectl -n team-a create token <serviceaccount> --duration=1h)
curl -s -c jar -o /dev/null -w '%{http_code}\n' -H 'Origin: http://localhost:8080' \
  --data-urlencode "token=${TOKEN}" http://localhost:8080/auth/token   # 302
curl -s -b jar http://localhost:8080/api/v1/clusters
```

The ServiceAccount sees what its RBAC allows, like a person. Unsafe API methods
also need the `X-PgToolBox-Request: 1` header.

## Sessions

- The session cookie holds the person's token **sealed** with AES-256-GCM under
  keys derived with HKDF-SHA256 from the session secret. The sealed token is
  useless against the API server by itself (it is ciphertext) and the console
  never writes a level or a group into it. The cookie is split into at most two
  chunks, `HttpOnly`, `SameSite=Strict`, and named `__Host-pgtoolbox-console-0`/`-1` when
  `COOKIE_SECURE` is true (the default). The login flow uses a separate short
  `SameSite=Lax` cookie for the round trip to the provider (ten minutes at most).
- **Session secret.** `SESSION_KEY_FILE` points at a file with one secret per
  line, each at least 32 bytes. The chart generates one Secret and keeps it
  across upgrades (`console.auth.sessionKey`). The first line seals; every line
  opens: to rotate, put a new secret first and keep the old line for one session
  lifetime.
- **Lifetime.** `SESSION_IDLE_TIMEOUT` (default `1h`) and `SESSION_MAX_AGE` (default `12h`, never less than the idle
  timeout). A session ends at the first of: sign-in plus `SESSION_MAX_AGE`, last
  activity plus `SESSION_IDLE_TIMEOUT`, and, for a token that cannot be
  refreshed, the token's expiry. Activity slides the idle timer at most once a
  minute, and only for requests from the console's own pages. An `oidc` session
  with a refresh token renews its ID token when the UI calls `GET
  /api/v1/session` within two minutes of the token's expiry; the event stream
  asks the UI to do so 90 seconds before the session would end. A failed refresh
  ends the session when the token expires. `IDP_TIMEOUT` (default `10s`) bounds
  calls to the provider.
- **Identity cache.** The console remembers a positive `SelfSubjectReview` answer
  for 30 seconds. A token revoked at the API server stops working for *writes*
  immediately (they carry the token) and for views within that time.
- **CSRF.** Every unsafe method needs a same-origin request (`Sec-Fetch-Site:
  same-origin`, or an `Origin` equal to the console's), and an API call
  (`/api/...`) also the header `X-PgToolBox-Request: 1`; anything else is a 403
  before the request reaches the session check.
  `SameSite` alone does not protect on OpenShift, where every Route under
  `*.apps.<domain>` is the same site.
- **Replayed flows.** The callback clears the flow cookie; a client that
  replays it gains nothing a new flow does not: the IdP redeems a code once,
  bound to the flow's PKCE verifier, and every callback is charged the budgets
  below. Starting a flow stores nothing on the server (the flow lives in its
  sealed cookie), so starting many costs only the sealing.
- **Rate limit.** Sign-in attempts are limited per client address and per
  replica. The per-replica bounds protect the API server and the identity
  provider; they are not meant to ration people, so the ones an
  unauthenticated sender can reach are large, and the one a known MCP bearer
  needs is separate, so junk never reaches it.
  - An **OAuth callback** is charged only once its flow cookie and state match
    (a callback without them costs the console nothing). It then spends its
    client address's budget (ten a minute, a burst of ten) and the replica's
    code-exchange bound (100 a second, a burst of 200), which caps the requests
    junk codes make the console send to the IdP; the IdP's own rate limit is the
    real bound on what it accepts. Only a **successful** exchange leads to a
    SelfSubjectReview, so the code-exchange bound caps those too: a junk code
    costs one refused exchange and nothing else.
  - A **pasted token** is charged two budgets in turn, and a refusal by the
    first spends none of the second: the sender's address (twenty a minute, a
    budget of the token form only, so junk tokens never lock the OAuth
    sign-in), so junk tokens from one address stop there; and the replica's
    bound on reviews of tokens it has never accepted (100 a
    second, a burst of 200), which caps what a flood from many addresses makes
    the API server authenticate.
  - Over any budget the console answers 429 without asking anyone, and counts it
    in `pgtoolbox_console_throttled_requests_total{bucket}`
    ([metrics](metrics.md)).

  **When the client address is shared** (hops without CIDRs, or hops 0 behind
  a public URL, below), no per-address budget is charged, on either path: every
  client behind the proxy has its address, and anyone could spend that budget
  for everyone. Callbacks and pasted tokens are then bounded by the
  per-replica bounds alone. **What a flood can still do**: one sender making
  more than 100 junk callbacks a second refuses new OAuth sign-ins on that replica while it lasts,
  and makes the console send up to 100 refused token requests a second to the
  IdP; more than 100 junk tokens a second refuses new token sign-ins. Sessions
  already open, MCP clients already connected and grants already issued keep
  working. Name the proxies, so each client keeps its own budget.
  Each key space has a table of its own, so a spray that fills one (10,000
  keys) resets only that one.
  Raw API-server tokens presented as MCP bearers spend the token form's bound
  the first time, after their address's (or, behind a shared address, their
  own) budget. A bearer this replica accepted before is remembered by a digest
  (at most 4,096, until it expires, an hour at most; when the set is full the
  expired go, and the whole set when none has, so a forgotten bearer spends the
  token form's bound once more) and its re-review, once
  its 30-second cached answer ends, spends a bound of its own (20 a second, a
  burst of 40): a flood of junk bearers cannot fail a connected client.
- **Client address.** Every per-address budget and the audit trail's
  `client_ip` use the direct peer, unless the peer is a trusted proxy.
  `TRUSTED_PROXY_HOPS` (0 to 8) is how many proxies append to
  `X-Forwarded-For`; `TRUSTED_PROXY_CIDRS` (comma-separated CIDRs, at most 32)
  is who they are. The header is read only from a peer inside one of those
  CIDRs, walked from the right for at most the hops, and the first address that
  is not a trusted proxy is the client. **Hops without CIDRs trust no peer and
  the header is ignored**: any Pod that reaches the console's port directly
  could otherwise name a new address on every request, escape every
  per-address budget, drain the shared review bound for everyone and forge
  `client_ip` in the audit trail. `0.0.0.0/0,::/0` trusts every peer and is
  safe only where a NetworkPolicy admits nothing but the proxies; the operator
  and the chart write it only then. With hops but no CIDRs the console knows
  the address is shared and drops the per-address budgets (above). With hops 0
  and a `PUBLIC_URL` that is not loopback it cannot know either (a clusterIP
  exposure behind an Ingress of your own, a load balancer that rewrites the
  source address), so it treats the peer's address as shared in the same way.
  Per-address budgets apply only with trusted CIDRs, or with hops 0 on a
  loopback `PUBLIC_URL` (or none). The console warns at start in both shared
  cases. The `pgtools` chart sets one hop when `console.exposure.type` is
  not `none` (`console.trustedProxyHops`, `console.trustedProxyCIDRs`); the
  operator [follows the published exposure](../operator/console.md#trusted-proxies-and-the-ingress-networkpolicy).
- **Sign-out** is `POST /logout` (`GET /logout` shows the confirmation page). It
  clears the cookie, drops the cached identity and refuses that session until its
  absolute expiry: on this replica at once, and on the others within about a
  second through the [state ConfigMap](configuration.md#what-survives-a-restart),
  which holds 128 bits of the SHA-256 of the session ID, when it was
  recorded, and a tag of the person: an HMAC of their name under the
  session secret, sealed in the cookie at sign-in, so reading the ConfigMap
  cannot test a guessed name and a token revoked before signing out still
  counts against its owner. It never holds the session. Without that
  ConfigMap (missing or forbidden) the refusal stays on this replica, and a
  copied cookie stays usable on another until it expires or is put on the
  [revocation list](#revoking-sessions). The ConfigMap keeps at most 16
  sign-outs a person (32 entries) and 1,536 entries in all. A person past
  their 16 keeps their **latest** sign-outs: each new one takes the place
  of their own sign-out that ends first (the newest is the one a freshly
  copied cookie needs), and nobody else's entry moves. The sessions sealed
  without a person's tag share 128 entries; past them a **new** one is
  refused on its own replica only (a copy of that cookie works on the
  other replicas until it expires). Once the document is full it is shared
  fairly, so that some fifty identities the API server accepts (a
  namespace editor can mint ServiceAccounts) cannot lock everyone else
  out: a newcomer's sign-out takes the place of an entry of the people
  holding the most, provided they hold more than the newcomer; among
  several holding as many, the entry that goes is the one recorded
  longest ago (never chosen by who they are), so the result is the same
  on every replica. Nobody's entry is ever taken for someone who holds as
  many or more, and a person holding as much as the most replaces their
  own instead. Everyone keeps their **latest** sign-outs: a person's
  newest sign-out goes only once the document holds more than 1,536
  people and at least 1,536 of them signed out more recently, so a
  person who signs out again after a theft, while many identities fill
  the document, keeps that sign-out shared; one exception: the new
  sign-out of a person at their own bound of 16 takes the place, and the
  rank, of the sign-out it replaces, and may then be the first to go.
  Each is counted in
  `pgtoolbox_console_shared_state_signouts_refused_total{reason}`
  (`replaced`, `unnamed`, `evicted` for an entry taken for a newcomer,
  `budget` for a new one refused) and the full document is logged once.
  Two replicas taking the document's last place at the same moment, or
  both replacing the same entry of a person at their bound, each accept
  their sign-out, and merging keeps one entry fewer: it is counted under
  `reason="merge"` (by every replica that merges it), logged once, and
  still refused by the replica that recorded it. Every sign-out of
  a session sealed with its person's tag is shared, even when the API
  server refuses the token at sign-out: OpenShift's OAuth API server and
  the webhook authenticators of EKS and GKE answer 401 while they are
  down, and the token works again once they are back. Only a session
  without that tag, whose token is refused and cannot be renewed (a pasted
  token, an OIDC session without a refresh token), is kept on its own
  replica. Each replica also bounds its own
  list (64 sign-outs a person, 65,536 in all, the earliest-ending
  forgotten first), so sign-outs never grow memory or the ConfigMap past
  what the API server accepts. It does **not** revoke the token at the
  identity provider or at OpenShift's OAuth server.

  When the sessions document is contended (a steady
  `signouts_refused_total{reason="evicted"}` or any `budget`), a sign-out
  may be kept only on the replica that recorded it. The durable answer to
  a stolen cookie is then the [revocation list](#revoking-sessions): a
  `users` line (`<username> <now>`) refuses every session that person
  signed in before it, on every replica and after every restart, and it
  is bounded by nothing an identity can fill.

What the console checks on every request after sign-in:

```mermaid
flowchart TB
    R[Request] --> X{unsafe method:<br/>same origin, API header?}
    X -- no --> F[403]
    X -- yes --> S{session cookie opens,<br/>not expired?}
    S -- no cookie --> U[401 or sign-in page]
    S -- expired --> U
    S -- yes --> L{signed out on this<br/>or another replica?}
    L -- yes --> U
    L -- no --> T[refresh the token, only on<br/>GET /api/v1/session near expiry]
    T --> V{SelfSubjectReview,<br/>positive cached 30 s}
    V -- rejected --> U
    V -- API server unreachable --> E[503]
    V -- user --> RV{on the revocation list?}
    RV -- yes --> U
    RV -- no --> OK[served as that user]
```

A refused session is cleared, and the sign-in page says why (`expired`,
`rejected`, `revoked`). An API call gets a 401 with the sign-in URL in its body
instead of a redirect.

`PUBLIC_URL` (the chart derives `https://<exposure.host>`) is the origin people
type. It is used for cookies, redirects and the CSRF check, and must be `https`
while `COOKIE_SECURE` is true.

## Revoking sessions

The console has no session database. Two replicas agree through a ConfigMap
(`console.revocations.configMapName`; the chart creates it empty) that every
replica watches and never writes. An admin edits its keys:

| Key | Value | Effect |
|---|---|---|
| `notBefore` | an RFC 3339 time | Every session signed in earlier is revoked. |
| `users` | one `<username> <RFC 3339 time>` per line | That user's sessions signed in earlier are revoked. |
| `sessions` | one session ID or bearer reference per line | That session is revoked. The ID is in the `login.success` audit line. A `bearer-<16 hex>` reference (the `session` of an `mcp.call` audit line) refuses that one token on the [MCP endpoint](mcp.md). |

Blank lines and lines starting with `#` are ignored. A list that does not parse
is refused as a whole and **the last good list stays in force**, so a typo neither
revokes everyone nor un-revokes anyone; the metric `pgtoolbox_console_revocation_list_valid`
goes to `0`. The console's Role may `get` and `watch` that one ConfigMap by name
and nothing else of its kind.

## Audit lines

The console logs one line per event with `audit=<event>`, the client address and
the user agent: `login.success`, `login.failure`, `access.denied`, `logout`,
`session.revoked`. The Kubernetes audit log remains the complete record of what
people *did*, because writes carry their own token.

## Environment variables

The chart sets these; the binary reads them. Unlisted settings are in
[configuration](configuration.md).

| Variable | Default | |
|---|---|---|
| `AUTH_PROVIDER` | none (required) | Comma list of `token`, `oidc`, `openshift`. |
| `AUTH_TOKEN_LOGIN` | `true` | Offer the token form beside SSO. |
| `SESSION_KEY_FILE` | none (required) | See above. |
| `SESSION_IDLE_TIMEOUT`, `SESSION_MAX_AGE` | `1h`, `12h` | |
| `PUBLIC_URL`, `COOKIE_SECURE`, `TRUSTED_PROXY_HOPS` | empty, `true`, `0` | |
| `TRUSTED_PROXY_CIDRS` | empty | The proxies whose `X-Forwarded-For` is read; empty trusts none ([client address](#sessions)). |
| `IDP_TIMEOUT` | `10s` | |
| `REVOCATION_CONFIGMAP`, `POD_NAMESPACE` | | The ConfigMap name and the console's namespace. |
| `OIDC_*`, `OPENSHIFT_*` | | As above and on the [OpenShift page](openshift.md). |
| `TLS_CERT_FILE`, `TLS_KEY_FILE` | | Serve HTTPS from mounted files (`console.tls.secretName`). |
