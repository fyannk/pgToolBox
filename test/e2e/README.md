# End-to-end tests

What proves the product's behaviour on a real API server. Everything here runs
on CI only (the kind jobs on a Docker host, the envtest job in a container);
nothing is run against a developer's own cluster.

| Suite | Where | Runs as |
|---|---|---|
| `test/e2e/chart` (`e2echart` tag) | kind via `hack/e2e-chart.sh scope` and `netpol` | ci.yml `e2e-chart-scope`, `e2e-chart-netpol` |
| `test/e2e/team/run.sh` | kind, a real CloudNativePG + barman-cloud + MinIO, **no operator**: `charts/pgtools` installed by a user holding only the namespace's `admin` role (impersonated), console + evidence agent + pgAdmin, evidence `live`, `server credential verified`, token sign-in | ci.yml `e2e-team` |
| `test/e2e/pgbackrest/run.sh` (assertions: `check_test.go`, tag `e2epgbackrest`) | kind, CloudNativePG 1.30, **both pgBackRest CNPG-I plugins** (Dalibo v0.0.3, Opera v0.9.0, images by digest) and MinIO over TLS with a CA created for the test; one cell per plugin: a real full and differential backup, switched WAL, an operator `EvidenceAgent` read by a `PgToolBoxConsole` and a self-resolving `pgtools` agent read by the chart's console; asserts the published `pgbackrest/v1alpha1` evidence, the console's backups API (stanza, window, joined Backups), a restore drill, and that a bad CA and a removed WAL segment are reported; prints wall time by phase and the node's peak memory | ci.yml `e2e-pgbackrest` (heavy tier only) |
| `test/e2e/console` (`e2econsole` tag) | envtest, `make test-e2e-console-authz` | ci.yml |
| `test/e2e/pgadmin`, `evidenceagent.sh`, `e2e_test.go` | kind, operator scenarios | ci.yml |
| `hack/console/test-e2e.sh` | kind, the console binary deployed by hand: boot, token sign-in, SAR filter, CSRF header, `/metrics`, sign-out | ci.yml console job |

## Console coverage

What the console must prove: a DBA signs in with SSO and sees every cluster they may `get`
in the watched namespaces; the overview is honest; two team consoles hold no
cluster-scoped read but SubjectAccessReview creation and the opt-in
ClusterImageCatalog read; on a policy-enforcing CNI the console works and cannot
reach the other namespace's instance pods or evidence agent.

| Criterion | Scenario | Suite |
|---|---|---|
| SSO sign-in on a live cluster (authorization-code flow, PKCE, nonce, API server trusting the issuer) | `TestTeamConsoles/SSO: a DBA sees exactly...`: a browser-less client follows the redirects to an in-cluster dex (pinned by digest, optionally pulled through a registry mirror), posts its password form, lands on the console with a session cookie; the API server accepts dex ID tokens through a structured `AuthenticationConfiguration` | chart / scope |
| Exactly the clusters the user may `get` | dba, ops and stranger on two consoles; RBAC includes a `resourceNames` grant; the list matches the expected set per user and console | chart / scope |
| 403 for a Cluster the caller may not get, existing or not; no existence oracle; unwatched namespace not served; CSRF header | `a Cluster the caller may not get is a 403...` | chart / scope |
| Honest overview | a Cluster whose namespace has no instance CA bundle reads `notVerifiable` with a reason and is not green; a fenced Cluster reports `fencing.state=all`; blind spots and sources are present | chart / scope |
| Break-glass token form beside SSO; `/metrics` identity (401 / 403 / 200) | `the break-glass token form...`, `/metrics asks the API server...` | chart / scope |
| Revocation ConfigMap (`sessions`, `users`, `notBefore`), on every replica; a malformed list leaves the last good one in force and the console Ready | `the revocation ConfigMap ends sessions on every replica` | chart / scope |
| Sign-out | `sign-out ends the session` (the copy of the cookie is refused on the signing replica at once and on the others through the revocation list) | chart / scope |
| No cluster-scoped read beyond SAR create and the opt-in ClusterImageCatalog read | SubjectAccessReview matrix over all API resources for each console's ServiceAccount (`rbac_test.go`), exact ClusterRoles and bindings, uninstall leaves nothing | chart / scope |
| Availability rule (two replicas rolling; persistent history: one replica, Recreate) | `persistent history forces one replica and Recreate` | chart / scope |
| Console works and cannot reach the other namespace's instance pods or evidence agent | probes from an ephemeral container in the console Pod, before and after `networkPolicy.enabled`; a user signs in through the router namespace and lists their Cluster under the policy | chart / netpol (Calico) |
| RBAC / SAR / SelfSubjectReview semantics, real authorizer | `TestSubjectAccessReviewSemantics`, `TestRevocationTakesEffect` (writes denied at once, views within the 10 s SAR cache TTL, a new grant seen at once), `TestIdentityBehindTheToken`, `TestMetricsIdentity` | console / envtest |

The console binary under test is the one built from the tree (`cmd/console`) with
`charts/pgtools` values; the chart's environment contract is therefore exercised
for the oidc, token and instance-CA paths.

## How the SSO scenario is wired

`hack/e2e-chart.sh scope` creates the CA, dex's serving certificate and an
`AuthenticationConfiguration` before the cluster exists, and starts kind with the
API server pointed at it (kubeadm `extraArgs` (map form) and `extraVolumes`). The issuer
is `https://10.96.200.10:5556/dex`: the dex Service has that fixed ClusterIP,
because the API server runs on the node and cannot resolve cluster DNS. Usernames
are `oidc:<email>`. The test dials the console's public host (`pgc-a.e2e.test`)
and dex's issuer address at NodePorts of the node, so the redirect URIs and the
issuer in the token are the real ones. Sign-ins are paced: the console budgets
10 a minute per replica per client address and the test waits out a 429.

## Blind spots: what these tests cannot see

- **OpenShift.** The OpenShift OAuth flow cannot run on kind. It is checked by hand
  (checklist below). Whether OpenShift accepts a bound projected ServiceAccount
  token as the OAuth client secret is unverified until that checklist is run.
- **TLS on `/metrics`.** The probe listener is plain HTTP; TLS on it is stubbed in the
  binary, so there is nothing to test.
- **ServiceMonitor / PodMonitor.** Rendered and unit-tested (`helm-unittest`,
  kubeconform), never scraped: no Prometheus Operator in the clusters.
- **Ingress / Route / HTTPRoute.** An Ingress object is created for the policy test,
  but no controller serves it; edge TLS, SSE timeouts and `TRUSTED_PROXY_HOPS` behind
  a real router are untested. Cookies are not `Secure` (plain HTTP to a NodePort), so
  the `__Host-` cookie names are covered by unit tests only.
- **A real CloudNativePG operator.** The clusters have only the CRDs; Cluster status
  is written by the test and the instance Pod is a stand-in. Live `/pg/status` reads
  (`ok`, `unreachable`, `tlsError`), staleness after a source outage and per-minor
  differences are covered by the sweeper and read-model unit tests and the per-minor
  envtest, not by kind.
- **A token revoked at the identity provider.** A deleted ServiceAccount's token is
  still accepted by the envtest API server, and a console caches a positive identity
  for 30 s (`DefaultIdentityTTL`): only writes (sent with the user's token) are
  immediate. Revocation of a console session is the revocation ConfigMap.
- **Sign-out across replicas.** Sign-out is immediate on the replica that handled it;
  the others refuse a copied cookie only once the session is in the revocation list.
  The test asserts the list's effect, not the gap itself.
- **OIDC refresh.** The scopes omit `offline_access`; refresh-token rotation (and the
  documented double-redemption with two replicas) is not exercised.
- **Browsers.** The flow is a Go HTTP client; SPA behaviour is the Playwright suite's
  (`ui/`).

## OpenShift sign-in: manual checklist

Run on an OpenShift 4.x cluster with the console chart installed
(`console.auth.provider=openshift`, `console.publicURL=https://<host>`,
`console.auth.openshift.redirectURI=https://<host>/auth/openshift/callback`).

1. The console ServiceAccount carries
   `serviceaccounts.openshift.io/oauth-redirecturi.console=<redirectURI>`
   (`oc get sa <release>-console -o yaml`).
2. The Pod is Ready under the restricted-v2 SCC with an arbitrary UID; the log shows
   `listening`, no OAuth discovery error. `OPENSHIFT_CLIENT_ID` is
   `system:serviceaccount:<ns>:<sa>` (set by the chart or read from the token).
3. Open `https://<host>/`: redirected to the login page (the token form is offered
   beside "Sign in with OpenShift"). Choose OpenShift: the OpenShift login, then the
   "Authorize access" page naming the ServiceAccount; approve.
4. Back on the console: `GET /api/v1/session` shows provider `openshift`, the user
   and groups as `oc whoami` / `oc get user` report them.
5. **The unverified point**: the sign-in succeeded with the console's projected
   (bound) token as the client secret. If the token endpoint answers
   `invalid_client`, create a long-lived token Secret for the ServiceAccount, set
   `console.auth.openshift.clientSecret.existingSecret`, and record the result in the
   chart README.
6. The cluster list shows exactly the Clusters the user may `get`
   (`oc auth can-i get clusters.postgresql.cnpg.io/<name> -n <ns> --as <user>` for
   a few), and a Cluster they may not get answers 403.
7. Sign out (`POST /logout`); the session API answers 401; the OpenShift OAuth token
   is not revoked by the console (state this in the report).
8. Revoke a session through the revocation ConfigMap (`notBefore`) and observe 401
   on all replicas within a minute.
9. With `networkPolicy.enabled`, steps 3 to 6 still work from the router namespace
   `openshift-ingress`; DNS port is `5353` (`networkPolicy.dns.ports`).
10. `ServiceMonitor` / `PodMonitor`: the user-workload Prometheus scrapes `/metrics`
    on the probe port with a token bound to the reader ClusterRole.
