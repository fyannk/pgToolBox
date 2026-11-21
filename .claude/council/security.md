# Checklist: security (the whole product)

`k8s-security-reviewer` goes deep on the cluster side: RBAC, NetworkPolicy,
Pod Security, SAR and impersonation, webhooks and CEL. You hold the rest:
every place something from outside enters a component, and every place a
secret could leave one. Threats: an authenticated low-privilege user, an
unauthenticated peer in the cluster, a hostile object-store or CNPG object,
a hostile site answering a fetch, a page or prompt crafted to make an AI
client act, a stale identity. Read the trust boundaries first:
`docs/site/docs/architecture/trust-boundaries.md` and
`docs/site/docs/security.md` (its known-limits table is deliberate).

- **Who may call it.** Every console route and MCP tool authorizes with a
  SubjectAccessReview for the caller (invariant 1); what does a route
  nobody listed get? Does a handler check the object it touches (its
  namespace, its name), not only that someone is signed in? Sign-in and
  sessions in `internal/console/auth` (cookies, CSRF, redirects, rate
  limits, the trusted-proxy client address), the MCP bearer and OAuth
  paths in `internal/console/mcpauth` and `mcpoauth`, the grant flow.
- **What comes from outside.** Every parser of untrusted bytes is bounded
  and fuzzed (invariant 3): backup metadata in
  `internal/evidence/formats`, instance `/pg/status` and metrics in
  `pkg/cnpgstatus`, logs, PostgreSQL configuration, the evidence wire
  contract between agent and console. A URL fetched from configuration
  or a client metadata document is SSRF-guarded (every resolved address,
  every redirect); a peer's answer is size-capped; a path from a request
  or an object-store key never escapes its prefix.
- **What goes back out.** Redaction at every boundary (`pkg/redact`,
  `internal/evidence/redact`, invariant 16): API responses, SSE, MCP tool
  results, digests, the assistant's generated files
  (`internal/console/assistant`), logs and events (invariant 2). A
  string from a cluster object rendered as HTML (`html/template` only on
  the Go side; in `ui/src` no `dangerouslySetInnerHTML`, no unchecked
  `href`/`src` scheme), the CSP on every response.
- **Content that steers an AI.** Text from cluster objects, logs or
  backup metadata returned by an MCP tool or written into an assistant
  file: is it marked as data, length-capped, and unable to widen what the
  client may do?
- **Secrets.** Object-store credentials never enter the console
  (invariant 14); the evidence agent's keys are opaque and never echoed
  (invariant 19); pgAdmin's credential files written by
  `internal/operator/adminsync`; error messages that echo a secret back;
  a token in a URL or a log line.
- **Dependencies and images.** A new Go module or npm package: maintained,
  needed, scanned (govulncheck, osv-scanner, npm audit); a new base image
  pinned by digest and scanned by grype.
