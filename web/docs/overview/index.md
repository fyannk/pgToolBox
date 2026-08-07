---
sidebar_position: 1
title: Introduction
slug: /
---

# Overview

pgToolBox runs a complete, dedicated access stack for every CloudNativePG
cluster you point it at. One `PgConsole` object produces one pod: a
`pgtoolbox-proxy` authentication boundary, the `pgconsole` observation UI,
an optional embedded pgAdmin dedicated to that cluster (enabled by
default), and an optional `objectstoreviewer` evidence sidecar.

## Why dedicated-per-cluster

pgAdmin's own multi-server model trades isolation for convenience: one
compromised pgAdmin sees every server's password. pgToolBox makes the
opposite trade — every console is dedicated to exactly one cluster, in the
same namespace, with its own proxy, its own pgpass, and its own generated
RBAC and NetworkPolicy. Security isolation beats resource sharing.

## What you get per console

- **Authentication and coarse authorization** at the proxy: OIDC (PKCE
  S256), OpenShift service-account OAuth, or local bcrypt accounts.
- **Observation** through pgconsole: cluster status, backups, pods, events,
  and bounded logs — Kubernetes API only, never SQL.
- **A ready-to-use pgAdmin**: per `PgToolBoxUser`, the operator syncs a
  pgAdmin account and a shared server definition authenticated with the
  saved password of the user's postgres role.
- **Repository evidence** (optional): the `objectstoreviewer` sidecar
  publishes backup/WAL destination fingerprints to the console over a
  pod-private Unix socket.

## Declarative access

```yaml
kind: PgToolBoxRole   # level + postgres backing
kind: PgToolBoxUser   # identity + roleRef
kind: PgToolBoxAccessRequest  # unknown user asks a dba for access
```

The proxy's 403 page lets an authenticated-but-unknown user file a
`PgToolBoxAccessRequest`. A `dba` approves it in the console; the operator
materializes the `PgToolBoxUser` and the proxy starts letting them in.
