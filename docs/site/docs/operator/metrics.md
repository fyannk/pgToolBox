---
sidebar_position: 7
title: Operator metrics
---

# Operator metrics

The operator serves `/metrics` over **HTTPS** on `--metrics-bind-address`
(`:8080`), HTTP/2 off, TLS 1.2 or newer. **Every scrape needs a token**: the
endpoint authenticates it with a TokenReview and authorizes it with a
SubjectAccessReview for `get` on the non-resource URL `/metrics`. The scraper's
ServiceAccount therefore needs the metrics-reader ClusterRole.

| Chart value | Effect |
|---|---|
| `operator.metrics.readerClusterRole`, `readerSubjects` | Renders `<release>-metrics-reader` (`get /metrics`), bound to the listed subjects only. |
| `operator.metrics.tls.certSecret` | A `kubernetes.io/tls` Secret (cert-manager, or on OpenShift the service CA through the metrics Service's `service.beta.openshift.io/serving-cert-secret-name`), mounted and passed as `--metrics-cert-dir`. |
| `operator.metrics.tls.caSecret`, `serverName` | The CA the monitors verify with. Empty: the monitors skip verification (`insecureSkipVerify`); the token still decides who may read. |
| `operator.metrics.serviceMonitor` | A metrics Service and a ServiceMonitor that sends the Prometheus pod's ServiceAccount token. |
| `operator.metrics.podMonitor` | A PodMonitor; it cannot send a token itself, so it needs `bearerTokenSecret`. |

Without `certSecret` the operator generates a self-signed certificate at start.

## What it exports

Besides controller-runtime's standard `controller_runtime_*`, `workqueue_*` and Go
runtime metrics:

| Metric | Labels | Meaning |
|---|---|---|
| `pgtoolbox_config_info` | `name`, `value`, `source` | The effective [configuration](index.md#configuration) and where each value came from; Secret-sourced values are redacted. |
| `pgtoolbox_cached_objects` | `kind`, `managed` | Objects held in the operator's cache per kind, split on whether pgToolBox manages them. |
| `pgtoolbox_build_info` | | The operator's build. |
| `pgtoolbox_pgadmin_info` | `namespace`, `name`, `clusters`, `image` | One series per `PgAdmin`; `clusters` is the number of Clusters it registers, not their names. |
| `pgtoolbox_pgadmin_condition` | `namespace`, `name`, `type`, `status` | 1 for the current status of each condition. |
| `pgtoolbox_evidenceagent_info` | `namespace`, `name`, `cluster`, `issuer`, `image` | One series per `EvidenceAgent`. |
| `pgtoolbox_evidenceagent_condition` | `namespace`, `name`, `type`, `status` | 1 for the current status of each condition. |
| `pgtoolbox_evidenceagent_producer_epoch` | `namespace`, `name` | `status.producerEpoch`. |
| `pgtoolbox_watch_namespaces_dropped` | `reason` | Namespaces of `WATCH_NAMESPACES` left out of the watch at start, per cause (a fixed set, every series exported, 0 when none): `clusters-unlistable`, the operator may not list CloudNativePG Clusters there (deleted since the install, or not bound); `permissions`, it lacks another namespaced permission of its manager role there (a narrower Role bound in place of the chart's; the log names the permission); `review-failed`, its permissions there could not be reviewed (an API server error or a reset or refused connection, after the retries, or no answer in time while the API server answered nothing else either; reviewed again every minute, and the operator restarts to watch the namespace once it passed twice in a row and the process has run ten minutes); `review-too-large`, its permission review answered more than 256 KiB (its RoleBindings grant the operator far more rules than its manager role; the log says the size seen); `review-too-slow`, its permission review got no answer in time at both attempts, a late server or gateway timeout included, while the API server answered a check sent meanwhile (most likely the same cause; neither is reviewed again before the next start). A `review-failed` namespace whose background review turns too large or too slow moves to that reason, without a restart. An optional API refused never counts here. Their objects are not reconciled until the namespace is back and the operator restarts ([dropped namespaces](index.md#namespaces-dropped-from-the-watch)). |
| `pgtoolbox_optional_api_refused_namespaces` | `group`, `kind` | Per optional API, the watched namespaces whose Role refuses it while others allow it: the feature is off in those namespaces only (no namespace is left out for it). 0 when none does, or when the API is off everywhere. |

These are computed from the cache at scrape time, kube-state-metrics style. A
failed list yields no series: *absent*, not zero. The two start-up gauges
(`pgtoolbox_watch_namespaces_dropped`, `pgtoolbox_optional_api_refused_namespaces`)
are set once at start and change only with a restart; every replica exports
its own.

## Alerts

`operator.metrics.prometheusRule.enabled` renders `<release>-operator`, a
PrometheusRule with the operator's alerts (`files/alerts/operator.rules.yaml`
in the chart, unit-tested with promtool by `make rules-test`); `labels` adds
the labels your Prometheus `ruleSelector` needs.

| Alert | Fires when | Severity |
|---|---|---|
| `PgToolBoxOperatorNamespacesDropped` | `max by (namespace, reason) (pgtoolbox_watch_namespaces_dropped) > 0` for 5 minutes: some tenant's objects are not reconciled. The `reason` label tells the causes apart, and the description says what to do for each (`review-failed` restarts the operator on its own; the others, `review-too-large` and `review-too-slow` included, need a fix, then a restart); the operator log names each namespace. Runbook: [namespaces dropped from the watch](index.md#namespaces-dropped-from-the-watch). | warning |

