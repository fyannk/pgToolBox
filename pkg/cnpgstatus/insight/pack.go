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

// Package insight reads the pgToolBox insight pack: a set of
// CloudNativePG custom queries (charts/pgtools/files/insight-pack) that a
// team opts a Cluster into with spec.monitoring.customQueriesConfigMap.
// The instance exporter runs them; the console reads their series from
// the same metrics endpoint it already scrapes, and never connects to
// PostgreSQL itself (AGENTS.md: no SQL from the console).
//
// The package has three parts, all pure and clock-injected:
//
//   - Parse turns one exporter exposition into a Reading: the pack's rows,
//     bounded in count and label size, everything else skipped unread.
//   - Tracker keeps, per Cluster, the latest Reading of each instance and
//     the small histories the derivations need (transaction id samples,
//     dead tuples per table, hourly statement snapshots), bounded by
//     construction.
//   - The derivations (wraparound forecasts, autovacuum keep-up, the
//     blocking tree, statement regressions) state what they rest on and
//     when they cannot conclude, rather than guessing.
//
// Every number here is the instance's claim, carried with the time the
// console read it; the derivations are the console's inference and are
// labelled so by the screens.
package insight

// PackVersion is the version of the insight pack this console reads. The
// pack's ConfigMap carries it as VersionLabel and the exporter reports it
// as cnpg_pgtoolbox_pack_info{version="..."}. A Cluster reporting an older
// version is told so; the console still reads what the older pack has.
const PackVersion = 1

// ConfigMapName is the name the chart gives the pack's ConfigMap, and
// ConfigMapKey the key holding the queries; a Cluster opts in with
//
//	spec.monitoring.customQueriesConfigMap: [{name: pgtoolbox-insight-pack, key: queries}]
const (
	ConfigMapName = "pgtoolbox-insight-pack"
	ConfigMapKey  = "queries"
	// VersionLabel is the label (and annotation) carrying PackVersion on
	// the ConfigMap.
	VersionLabel = "pgtoolbox.fyannk.dev/insight-pack-version"
)

// SpecChange is the one-line change that opts a Cluster in, shown where
// the pack is missing.
const SpecChange = `spec.monitoring.customQueriesConfigMap: [{name: ` + ConfigMapName + `, key: ` + ConfigMapKey + `}]`

// metricPrefix starts every pack series. The exporter names a custom
// query's metrics cnpg_<query>_<column>, and every pack query starts with
// pgtoolbox_.
const metricPrefix = "cnpg_pgtoolbox_"

// Families of the pack, as the exporter names them (cnpg_ + query name).
const (
	familyPack        = "cnpg_pgtoolbox_pack_"
	familySettings    = "cnpg_pgtoolbox_settings_"
	familyXID         = "cnpg_pgtoolbox_xid_"
	familyDatabase    = "cnpg_pgtoolbox_database_"
	familyTables      = "cnpg_pgtoolbox_tables_"
	familyVacuum      = "cnpg_pgtoolbox_vacuum_progress_"
	familyBlocking    = "cnpg_pgtoolbox_blocking_"
	familyBackends    = "cnpg_pgtoolbox_backends_"
	familyConnections = "cnpg_pgtoolbox_connections_"
	familyStatements  = "cnpg_pgtoolbox_statements_"
	familyStmtInfo    = "cnpg_pgtoolbox_statements_info_"
	familyIndexes     = "cnpg_pgtoolbox_indexes_"
	familyIndexDups   = "cnpg_pgtoolbox_index_duplicates_"
	familyPrepared    = "cnpg_pgtoolbox_prepared_xacts_"
	familyXmin        = "cnpg_pgtoolbox_xmin_"
)

// Exporter series outside the pack the parser also reads.
const (
	// lastUpdate is the exporter's query-cache timestamp (CloudNativePG
	// 1.28 and later): how old the custom queries' results are, since
	// the exporter answers from a cache for metricsQueriesTTL (30 s by
	// default).
	lastUpdate = "cnpg_last_update_timestamp"
	// userQueryErrors counts failed custom queries, one label per query
	// name and error text.
	userQueryErrors = "cnpg_errors_total"
)

// Queries are the pack's query names, in the order the documentation
// lists them; the pack test holds the YAML to this list.
var Queries = []string{
	"pgtoolbox_pack", "pgtoolbox_settings", "pgtoolbox_xid", "pgtoolbox_database",
	"pgtoolbox_prepared_xacts", "pgtoolbox_xmin",
	"pgtoolbox_tables", "pgtoolbox_vacuum_progress", "pgtoolbox_blocking", "pgtoolbox_backends",
	"pgtoolbox_connections", "pgtoolbox_statements", "pgtoolbox_statements_info", "pgtoolbox_indexes",
	"pgtoolbox_index_duplicates",
}
