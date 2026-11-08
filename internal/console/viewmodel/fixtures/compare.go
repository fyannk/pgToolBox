// Copyright 2026 The pgToolBox Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fixtures

import (
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func cv(v string) viewmodel.CompareValue {
	if v == "" {
		return viewmodel.CompareValue{State: "unset"}
	}
	return viewmodel.CompareValue{State: "set", Value: v}
}

var unknownCV = viewmodel.CompareValue{State: "unknown"}

func crow(key string, l, r viewmodel.CompareValue) viewmodel.CompareRow {
	return viewmodel.CompareRow{Key: key, Left: l, Right: r, Same: l.State != "unknown" && r.State != "unknown" && l == r}
}

func csection(id, source string, rows ...viewmodel.CompareRow) viewmodel.CompareSection {
	s := viewmodel.CompareSection{ID: id, Origin: viewmodel.OriginOperator, Source: source, Rows: rows}
	for _, r := range rows {
		if !r.Same {
			s.Differences++
		}
	}
	return s
}

// Comparison is orders (healthy) against billing (failing over): the
// differences a DBA would look for first — a smaller memory limit with
// a larger work_mem, no backups, no synchronous replication — and one
// parameter spelled differently but equal.
func Comparison() viewmodel.Comparison {
	sharedBuffers := crow("shared_buffers", cv("2GB"), cv("2048MB"))
	sharedBuffers.Same, sharedBuffers.Note = true, "the same value, spelled differently"
	sections := []viewmodel.CompareSection{
		csection("version", "Cluster.status.image, pgDataImageInfo; Cluster.spec.imageName, imageCatalogRef; detected operator version",
			crow("PostgreSQL major", cv("18"), cv("18")),
			crow("status.image", cv("ghcr.io/cloudnative-pg/postgresql:18.1"), cv("ghcr.io/cloudnative-pg/postgresql:18.0")),
			crow("spec.imageName", cv("ghcr.io/cloudnative-pg/postgresql:18.1"), cv("ghcr.io/cloudnative-pg/postgresql:18.0")),
			crow("spec.imageCatalogRef", cv(""), cv("")),
			crow("CloudNativePG (detected)", cv("1.30.1"), unknownCV),
		),
		csection("instances", "Cluster.spec, Cluster.status.readyInstances",
			crow("spec.instances", cv("3"), cv("2")),
			crow("status.readyInstances", cv("3"), cv("1")),
			crow("spec.primaryUpdateStrategy", cv("unsupervised"), cv("unsupervised")),
			crow("spec.primaryUpdateMethod", cv("switchover"), cv("restart")),
			crow("spec.failoverDelay", cv(""), cv("")),
			crow("spec.switchoverDelay", cv(""), cv("")),
			crow("spec.replica.enabled", cv(""), cv("")),
		),
		csection("parameters", "Cluster.spec.postgresql.parameters, shared_preload_libraries",
			crow("max_connections", cv("200"), cv("400")),
			sharedBuffers,
			crow("work_mem", cv("16MB"), cv("64MB")),
			crow("shared_preload_libraries", cv("pg_stat_statements"), cv("")),
		),
		csection("resources", "Cluster.spec.resources (the postgres container)",
			crow("requests.cpu", cv("2"), cv("1")),
			crow("limits.cpu", cv("2"), cv("")),
			crow("requests.memory", cv("8Gi"), cv("4Gi")),
			crow("limits.memory", cv("8Gi"), cv("4Gi")),
		),
		csection("storage", "Cluster.spec.storage, walStorage, tablespaces",
			crow("storage.size", cv("100Gi"), cv("100Gi")),
			crow("storage.storageClass", cv("fast-ssd"), cv("fast-ssd")),
			crow("walStorage.size", cv("20Gi"), cv("")),
			crow("walStorage.storageClass", cv("fast-ssd"), cv("")),
			crow("tablespaces", cv(""), cv("")),
		),
		csection("backup", "Cluster.spec.backup, spec.plugins, ObjectStore.spec, ScheduledBackups",
			crow("method", cv("plugin barman-cloud.cloudnative-pg.io"), cv("none")),
			crow("objectStore", cv("orders-store"), cv("")),
			crow("retentionPolicy", unknownCV, cv("")),
			crow("backup.target", cv(""), cv("")),
			crow("schedules", cv("0 0 7 * * *"), cv("")),
		),
		csection("topology", "Cluster.spec.postgresql.synchronous, spec.affinity, Poolers",
			crow("synchronous", cv("any 1, dataDurability required, failoverQuorum"), cv("none")),
			crow("affinity.enablePodAntiAffinity", cv(""), cv("")),
			crow("affinity.podAntiAffinityType", cv("required"), cv("")),
			crow("affinity.topologyKey", cv("topology.kubernetes.io/zone"), cv("")),
			crow("affinity.nodeSelector", cv(""), cv("")),
			crow("affinity.tolerations", cv(""), cv("")),
			crow("poolers", cv("rw ×2"), unknownCV),
		),
	}
	out := viewmodel.Comparison{
		GeneratedAt: Now,
		Left: viewmodel.ComparedCluster{
			Ref: ordersRef, Health: viewmodel.HealthOK, Phase: "Cluster in healthy state",
			Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Cluster", ObservedAt: ago(2 * time.Second)},
		},
		Right: viewmodel.ComparedCluster{
			Ref:    viewmodel.ClusterRef{Namespace: "team-a", Name: "billing", UID: "4d2f1e0a-1b2c-4d3e-8f90-a1b2c3d4e5f6"},
			Health: viewmodel.HealthCritical, Phase: "Failing over",
			Provenance: viewmodel.Provenance{Origin: viewmodel.OriginOperator, Source: "Cluster", ObservedAt: ago(2 * time.Second)},
		},
		Sections: sections,
		BlindSpots: []viewmodel.BlindSpot{{
			ID: "compare.declared", Panel: "compare",
			Text: "Parameters and resources are what each Cluster declares; what PostgreSQL applied is not reported (a change waiting for a restart reads as applied).",
		}},
	}
	for _, s := range sections {
		out.Differences += s.Differences
	}
	return out
}
