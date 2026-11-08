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

These are computed from the cache at scrape time, kube-state-metrics style. A
failed list yields no series: *absent*, not zero.

