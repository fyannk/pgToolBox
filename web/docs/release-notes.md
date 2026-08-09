# Release notes

## v0.1.0 (unreleased)

Initial release of the pgToolBox rewrite.

- `PgConsole` controller: proxy + pgconsole + embedded pgAdmin + optional
  evidence sidecar, exposure (Route/Ingress/Gateway HTTPRoute/ClusterIP),
  generated RBAC and NetworkPolicy.
- `pgtoolbox-proxy`: OIDC (PKCE S256), OpenShift service-account OAuth, and
  local bcrypt modes; level authorization; access-request flow with CSRF.
- Embedded pgAdmin user/server sync through the in-pod mTLS admin-sync API.
- `PgToolBoxAccessRequest` controller: materializes `PgToolBoxUser` on
  approval.
- Packaging: Helm chart, OLM bundle and file-based catalog, kustomize
  development overlay, multi-target Dockerfile (manager + proxy).
