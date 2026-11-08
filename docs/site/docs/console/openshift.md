---
sidebar_position: 3
title: OpenShift
---

# OpenShift

On OpenShift the built-in OAuth server issues API-server tokens for whatever
identity providers the cluster has configured, so SSO needs no cluster
prerequisite. With `console.auth.provider=openshift` the console's ServiceAccount
is the OAuth client.

## Configure

```bash
helm install console oci://ghcr.io/fyannk/charts/pgtools \
  --version <X.Y.Z> --namespace team-a \
  --set console.auth.provider=openshift \
  --set console.exposure.type=route \
  --set console.exposure.host=console-team-a.apps.example.com \
  --set console.auth.openshift.redirectURI=https://console-team-a.apps.example.com/auth/openshift/callback
```

| Chart value | Variable | Meaning |
|---|---|---|
| `auth.openshift.redirectURI` | (annotation) | **Required.** The public callback URL. The chart puts it on the ServiceAccount as `serviceaccounts.openshift.io/oauth-redirecturi.console`; OpenShift redirects only to URIs the ServiceAccount lists. |
| `auth.openshift.clientID` | `OPENSHIFT_CLIENT_ID` | Default: read from the console's own projected token, `system:serviceaccount:<ns>:<sa>`. |
| `auth.openshift.clientSecret.existingSecret` | `OPENSHIFT_CLIENT_SECRET_FILE` | Empty means the ServiceAccount's projected token is presented as the client secret. **Not yet verified on a live OpenShift** (see the checklist, step 5). |
| `auth.openshift.discoveryURL` | `OPENSHIFT_DISCOVERY_URL` | https only. Default: `https://openshift.default.svc/.well-known/oauth-authorization-server`. |
| `auth.openshift.scopes` | `OPENSHIFT_SCOPES` | Default `user:info,user:check-access,user:full`. |
| `auth.openshift.caConfigMap` | `OPENSHIFT_CA_FILE` | Default: the `openshift-service-ca.crt` ConfigMap of the namespace, projected next to the API token. |

`PUBLIC_URL` defaults to `https://<exposure.host>`. The Route gets the HAProxy
timeout annotation the live stream needs (`console.exposure.sse.timeout`).

For a NetworkPolicy on OpenShift set `console.exposure.routerNamespace` to
`openshift-ingress` (the default) and `console.networkPolicy.dns` to the
`openshift-dns` namespace on port `5353`; see [NetworkPolicy](networkpolicy.md).

## Manual checklist

The OpenShift OAuth flow cannot run on kind, and there is no OpenShift CI target.
This checklist is run on a real OpenShift 4.x cluster, with the console chart
installed as above, before each release.

1. The console ServiceAccount carries
   `serviceaccounts.openshift.io/oauth-redirecturi.console=<redirectURI>`
   (`oc get sa <release>-console -o yaml`).
2. The Pod is Ready under the `restricted-v2` SCC with an arbitrary UID; its log
   shows `listening` and no OAuth discovery error. `OPENSHIFT_CLIENT_ID` is
   `system:serviceaccount:<ns>:<sa>`.
3. Open `https://<host>/`: you land on the sign-in page (the token form is
   offered beside "Sign in with OpenShift"). Choose OpenShift, sign in, and
   approve the "Authorize access" page that names the ServiceAccount.
4. Back on the console, `GET /api/v1/session` reports provider `openshift`, and the
   user and groups as `oc whoami` and `oc get user` report them.
5. **The unverified point.** The sign-in worked with the console's projected
   (bound) token as the client secret. If the token endpoint answers
   `invalid_client`, create a long-lived token Secret for the ServiceAccount, set
   `console.auth.openshift.clientSecret.existingSecret`, and record the result.
6. The cluster list shows exactly the Clusters the user may `get`
   (compare with `oc auth can-i get clusters.postgresql.cnpg.io/<name> -n <ns> --as <user>`
   for a few), and a Cluster they may not get answers 403.
7. Sign out (`POST /logout`); the session API answers 401. The console does not
   revoke the OpenShift OAuth token; say so in the report.
8. Revoke a session through the [revocation ConfigMap](authentication.md#revoking-sessions)
   (`notBefore`) and observe 401 on all replicas within a minute.
9. With `networkPolicy.enabled`, steps 3 to 6 still work from the router namespace
   `openshift-ingress`.
10. With a `ServiceMonitor` or `PodMonitor`, the user-workload Prometheus scrapes
    `/metrics` on the probe port with a token bound to the metrics-reader
    ClusterRole ([metrics](metrics.md)).

## pgAdmin on OpenShift

A `PgAdmin` on OpenShift uses an `oauth-proxy` sidecar instead of pgAdmin's own
OAuth2; see [pgAdmin](../operator/pgadmin.md#sign-in). Its Route and the
manual checks for it (Route, OAuth login, oauth-proxy) follow the same list as
the console's.
