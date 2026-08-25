---
name: code-review
description: Review pgToolBox changes against the repository's hard invariants — the CNPG Cluster is never written, no secret material reaches status or logs, rendering is byte-stable, apps stay auth-free behind the proxy, the condition and label surface is frozen, and one version is said everywhere. Use this when reviewing any pull request in this repository.
license: Apache-2.0
---

pgToolBox is a Kubernetes operator for CloudNativePG. It reconciles
objects in a cluster an operator depends on, and it renders RBAC that
decides what its own components may do. Most of what matters in a review
here is not style: it is whether a change writes something it must not,
leaks something into a status a user can read, or makes a render
non-deterministic so that a no-op reconcile starts churning.

`make lint` already runs `golangci-lint` and the repository checks. Do
not spend review on what it fails on its own.

## The canonical source

The numbered list in
[`CONTRIBUTING.md`](../../../CONTRIBUTING.md#repository-invariants) is the
one to quote — **read the invariant there before asserting a change
violates it**, and cite its number.
[`AGENTS.md`](../../../AGENTS.md) restates most of them as prose for
agents and adds build and verification guidance. Where the two disagree,
the numbered list wins and the summary is the defect; report the drift
when you find it.

## What to look for

### 1. CNPG 1.30+ only

The `DatabaseRole` CRD always exists. A version check, a fallback path,
or an "if the CRD is present" branch is a finding — those paths cannot be
tested honestly and rot into the thing that breaks on an upgrade.

### 2. Never write to the CNPG `Cluster` object

The operator reads `Cluster` and writes its own objects. Any
`Update`, `Patch`, or `Apply` whose target is a `Cluster` is a violation
however small the field, and the tests assert `resourceVersion` is
unchanged for exactly this reason. A change that touches those assertions
deserves the same scrutiny as one that touches the write itself.

### 3. No secret material in status, logs, or events

Passwords, connection strings, tokens, and rendered credentials never
reach a `.status`, a log line, or an Event — all three are readable by
anyone with get on the object. **CEL rules are ASCII-only**, which is not
cosmetic: a non-ASCII rule can fail validation in ways that differ by
apiserver version.

Look for a new status field carrying a rendered value, an error wrapped
with `%w` that embeds a Secret's content, and an Event message
interpolating user data.

### 4. Deterministic byte-stable rendering

**A no-op reconcile produces zero object updates.** This is the invariant
most often broken by accident, because the broken version looks
harmless: a map iterated without sorting, a timestamp taken from the
wall clock instead of an injected one, a slice appended in
non-deterministic order, a `resource.Quantity` formatted differently than
it was parsed. Each produces a diff on every reconcile, and the operator
starts writing forever.

If a change adds a field to a rendered object, ask what orders it.

### 5. Apps stay auth-free

The proxy is the only authentication and coarse authorization boundary,
and the generated `NetworkPolicy` is what ensures nothing bypasses it. A
change that adds an auth check inside an app, or that widens the
NetworkPolicy, is a security change rather than a feature — the boundary
is structural, not defence in depth.

### 6. License boilerplate

`hack/boilerplate.go.txt` on all new Go files.

### 7. Frozen API surface

Condition types and reasons in `api/v1alpha1/conditions.go`, and label
keys in `common_types.go`, are **frozen: extend, never rename**. A rename
is invisible in Go — it compiles — and silently breaks every consumer
selecting on the old key and every alert matching the old reason. Treat a
rename as an API break even when the pull request calls it a typo fix.

### 8. One version, said everywhere

`make validate-packaging` is the arbiter: the Helm chart, the OLM bundle,
and the catalog must agree. A change that bumps one and not the others
fails it, and a change that edits generated packaging by hand instead of
through `make sync-manifests` will drift the next time it is regenerated.

## What the unit tests cannot catch

The unit suite uses a fake client, which cannot fail the two ways a real
cluster can:

- **RBAC escalation prevention** refusing a Role rule the operator does
  not itself hold;
- **a component container rejecting the environment rendered for it.**

So a change to the generated Roles, or to anything the component
containers read, needs `make test-e2e` (kind + CNPG + the published
images) and not just a green `go test`. If such a change arrives with
unit tests only, say so — that is an incomplete change, not a style
preference.

## Documentation is not specification

The code and tests are the source of truth. When prose and code disagree,
**the code is right and the prose is the defect** — do not report a code
change as wrong because a doc page or a README says otherwise. Report the
stale prose instead.

Apply the same standard to prose *about* configuration. If a comment or
document paraphrases a condition, a filename, or a flag, ask for the
exact string so a reader can grep for it. A paraphrase that cannot be
verified is the same class of defect as a stale one, and a document that
restates a version the build owns will be falsified by the next bump.

## How to report

Lead with the invariant number and what the change does to it. Prefer one
well-evidenced finding over several speculative ones — quote the line and
say what input reaches it. If the reasoning depends on a file you have
not read, read it or say the finding is uncertain. State plainly when a
change is clean; "no findings" is a useful review here.
