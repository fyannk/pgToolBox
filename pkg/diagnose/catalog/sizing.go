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

package catalog

import "github.com/fyannk/pgToolBox/pkg/diagnose"

// sizingRules are the configuration advisor: PostgreSQL's declared
// memory settings weighed against the container's resources. PostgreSQL
// sizes itself from its parameters and has no idea of the cgroup around
// it; a DBA new to Kubernetes does not think of the cgroup either.
//
// They read spec.postgresql.parameters and spec.resources, both stable
// across every CloudNativePG this console meets, and PostgreSQL's unit
// rules, stable since PostgreSQL 12; so they are unpinned. Every number
// is declared intent: /pg/status reports no setting, so a change still
// waiting for a restart is judged as if applied.
func sizingRules() []diagnose.Rule {
	return []diagnose.Rule{
		{
			ID:        "pg-memory-over-limit",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityWarning,
			Describes: "worst-case PostgreSQL memory (shared_buffers + wal_buffers + max_connections × work_mem + " +
				"autovacuum_max_workers × autovacuum memory) above the postgres container's memory limit",
			Summary: "PostgreSQL's worst-case memory exceeds the container's memory limit: the instance risks an OOM kill.",
			Detail: "PostgreSQL sizes itself from its parameters and cannot see the container's memory limit. " +
				"When every connection sorts or hashes at once and autovacuum runs, the processes together can " +
				"pass the limit, and the kernel kills the container; CloudNativePG restarts the instance, and a " +
				"killed primary is a failover. The estimate is the conventional one, not a bound: a single query " +
				"may use work_mem several times (one per sort or hash node, hashes up to hash_mem_multiplier " +
				"times, per parallel worker), while most connections use far less. Without a memory limit the " +
				"check has nothing to compare and stays clear.",
			When: diagnose.MemoryOverLimit{},
			NextSteps: "Lower work_mem or max_connections (a pooler in front lets fewer server connections serve " +
				"more clients), or raise resources.limits.memory. Set work_mem higher per role or per session for " +
				"the few queries that need it rather than for every connection.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
		{
			ID:        "pg-shared-buffers-over-limit",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityHigh,
			Describes: "shared_buffers + wal_buffers at or above the postgres container's memory limit",
			Summary:   "shared_buffers alone does not fit the container's memory limit.",
			Detail: "The shared segment is charged to the container as PostgreSQL touches its pages, so as the " +
				"buffer cache fills the container reaches its limit with nothing left for connections, and is " +
				"OOM-killed: the instance restarts, and a primary's restart is a failover. It is left out when " +
				"the segment comes from a large enough huge page reservation, which Kubernetes accounts apart.",
			When: diagnose.SharedBuffersOverLimit{},
			NextSteps: "Bring shared_buffers to a fraction of the limit (a quarter is the usual start) or raise " +
				"resources.limits.memory; both need a restart, which CloudNativePG rolls out replica first.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
		{
			ID:        "pg-effective-cache-size-over-limit",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityNote,
			Describes: "effective_cache_size above the postgres container's memory limit",
			Summary:   "effective_cache_size promises the planner more cache than the container can hold.",
			Detail: "effective_cache_size allocates nothing; it tells the planner how much of the data the " +
				"operating system is likely to hold in its cache. Inside a container that cache is charged to " +
				"the container's memory limit, so a value above the limit (the default is 4 GB) makes the " +
				"planner favour index scans that then read from disk.",
			When:      diagnose.EffectiveCacheSizeOverLimit{},
			NextSteps: "Set effective_cache_size to what the limit leaves after shared_buffers and the connections' memory; it applies on reload.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
		{
			ID:        "pg-huge-pages-missing",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityHigh,
			Describes: "huge_pages = on without a hugepages-* reservation large enough for shared_buffers + wal_buffers",
			Summary:   "huge_pages is on, but the container does not reserve enough huge pages: the next start of an instance fails.",
			Detail: "With huge_pages = on PostgreSQL refuses to start when it cannot map its shared segment in " +
				"huge pages, and a container only gets the huge pages its hugepages-2Mi or hugepages-1Gi " +
				"resource reserves. Instances running now keep running; the next restart, failover or new " +
				"replica does not come up. The real segment is a little larger than shared_buffers + " +
				"wal_buffers. Whether the nodes have free huge pages is not visible to the console, which reads " +
				"no Node; a reservation the nodes cannot meet leaves the pod Pending.",
			When: diagnose.HugePages{Problem: diagnose.HugePagesMissing},
			NextSteps: "Reserve huge pages in resources (requests equal to limits, e.g. hugepages-2Mi) above " +
				"shared_buffers with some margin, on nodes that have them, or set huge_pages to try.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
		{
			ID:        "pg-huge-pages-unused",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityNote,
			Describes: "a hugepages-* reservation PostgreSQL will not use (huge_pages off, or try with too small a reservation)",
			Summary:   "The container reserves huge pages that PostgreSQL will not use.",
			Detail: "Huge pages reserved by a pod are taken from the node whether used or not. With huge_pages " +
				"off, or with try and a reservation smaller than the shared segment, PostgreSQL silently uses " +
				"normal pages, and its shared segment then counts against the memory limit instead.",
			When:      diagnose.HugePages{Problem: diagnose.HugePagesUnused},
			NextSteps: "Either size the reservation above shared_buffers with some margin and set huge_pages to try or on, or drop the reservation.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
		{
			ID:        "pg-memory-parameter-unparsable",
			Component: diagnose.ComponentPostgreSQL,
			Layer:     diagnose.LayerPostgreSQL,
			Severity:  diagnose.SeverityWarning,
			Describes: "a memory parameter in spec.postgresql.parameters that does not parse as PostgreSQL reads it",
			Summary:   "A declared memory parameter is not a value PostgreSQL accepts.",
			Detail: "PostgreSQL's memory units are B, kB, MB, GB and TB, case-sensitive (\"1gb\" and \"1 KB\" " +
				"are refused), and a bare number counts in the parameter's own unit (8 kB pages for " +
				"shared_buffers). A refused value makes the instance keep its previous setting with an error " +
				"in its log, or fail to start when it is read at start-up; the console cannot size what it " +
				"cannot read, so the memory checks say they could not run.",
			When:      diagnose.ParametersUnparsable{},
			NextSteps: "Correct the value in spec.postgresql.parameters, then read the instances' logs for the configuration errors PostgreSQL reported.",
			Link:      "/diagnostics",
			LinkLabel: "Diagnostics",
		},
	}
}
