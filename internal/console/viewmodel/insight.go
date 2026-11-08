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

package viewmodel

import "time"

// Insight is the /insight screens of one Cluster: what its exporters
// report through the pgToolBox insight pack (CloudNativePG custom
// queries the team opts the Cluster into), and what the console derived
// from its own retained samples. The console runs no SQL: every number
// here is the exporter's, read with the metrics sweep or on demand.
type Insight struct {
	Ref         ClusterRef         `json:"ref"`
	GeneratedAt time.Time          `json:"generatedAt"`
	Pack        InsightPack        `json:"pack"`
	Wraparound  InsightWraparound  `json:"wraparound"`
	Statements  InsightStatements  `json:"statements"`
	Indexes     InsightIndexes     `json:"indexes"`
	Connections InsightConnections `json:"connections"`
	Blocking    InsightBlocking    `json:"blocking"`
	Sources     []SourceState      `json:"sources"`
	BlindSpots  []BlindSpot        `json:"blindSpots"`
}

// InsightPack is whether the Cluster runs the pack, and which version.
type InsightPack struct {
	// State is the detection verdict: enabled (every instance read
	// reports this console's version), notEnabled (none reports the
	// pack), outdated (an instance reports an older version), newer (a
	// later one than this console knows), unknown (pack series without
	// their version), truncated (the console cut an exposition at its
	// size bound before the pack's series: whether the pack runs is not
	// known), unsupported (the instances run PostgreSQL 13, which the
	// pack does not cover), noData (no instance read yet), metricsOff
	// (the console does not scrape exporters).
	State  string `json:"state" enum:"enabled,notEnabled,outdated,newer,unknown,truncated,unsupported,noData,metricsOff"`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// Text states the verdict in English.
	Text string `json:"text"`
	// Expected is the pack version this console reads.
	Expected string `json:"expected"`
	// Referenced says whether the Cluster spec lists the pack's
	// ConfigMap in spec.monitoring.customQueriesConfigMap; absent when
	// the Cluster was not read.
	Referenced *bool `json:"referenced,omitempty"`
	// ConfigMap and Key name the pack's ConfigMap entry, and SpecChange
	// is the one-line Cluster change that opts in.
	ConfigMap  string `json:"configMap"`
	Key        string `json:"key"`
	SpecChange string `json:"specChange"`
	// Instances are the versions each instance reports.
	Instances []InsightPackInstance `json:"instances"`
	// QueryErrors are the exporter's recorded failures per pack query.
	QueryErrors []InsightQueryError `json:"queryErrors"`
}

// InsightPackInstance is one instance's pack report.
type InsightPackInstance struct {
	Instance string `json:"instance"`
	Primary  bool   `json:"primary"`
	// Version is the pack version reported; empty when none.
	Version string `json:"version,omitempty"`
	// ExporterUpdatedAt is when the exporter last ran its custom queries
	// (they are cached for the Cluster's metricsQueriesTTL).
	ExporterUpdatedAt *time.Time `json:"exporterUpdatedAt,omitempty"`
	// Truncated names families the console cut at its row bound, and
	// "exposition" when it cut the exporter's response at its size bound.
	Truncated []string `json:"truncated,omitempty"`
	// ServerVersionNum is PostgreSQL's server_version_num as the pack
	// reports it.
	ServerVersionNum *float64   `json:"serverVersionNum,omitempty"`
	Provenance       Provenance `json:"provenance"`
}

// InsightQueryError is cnpg_errors_total for one pack query in the
// exporter's last run; the error text stays in the exporter's log.
type InsightQueryError struct {
	Instance string  `json:"instance"`
	Query    string  `json:"query" untrusted:"true"`
	Count    float64 `json:"count"`
	// Databases are those it failed on, for a per-database query; the
	// other databases' rows are shown.
	Databases []string `json:"databases,omitempty"`
}

// InsightPartial says a panel shows less than the pack reports: a query
// behind it failed on some database or instance, or the console cut
// what it read. Its rows are real; others are missing.
type InsightPartial struct {
	Partial bool `json:"partial"`
	// PartialText says what is missing and why; empty when complete.
	PartialText string `json:"partialText,omitempty"`
}

// InsightSetting is one threshold the derivations use, with where it
// came from.
type InsightSetting struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	// Source is instance (the pack's settings query), spec (the Cluster's
	// spec.postgresql.parameters) or default (PostgreSQL's, assumed).
	Source string `json:"source" enum:"instance,spec,default"`
}

// InsightRate is a consumption rate the console measured from its own
// samples of the primary.
type InsightRate struct {
	PerSecond     float64   `json:"perSecond"`
	WindowSeconds float64   `json:"windowSeconds"`
	From          time.Time `json:"from"`
	To            time.Time `json:"to"`
}

// InsightHorizon is the distance of one age to one threshold.
type InsightHorizon struct {
	Threshold float64 `json:"threshold"`
	Remaining float64 `json:"remaining" doc:"Threshold minus age; negative once past."`
	Past      bool    `json:"past"`
	// InSeconds is the time to reach it at the measured rate; absent
	// without a rate or once past.
	InSeconds *float64 `json:"inSeconds,omitempty"`
}

// InsightForecast measures one age against PostgreSQL's three
// thresholds: the anti-wraparound autovacuum, the vacuum failsafe and
// the stop limit (no new transaction ids).
type InsightForecast struct {
	Age        float64        `json:"age"`
	Autovacuum InsightHorizon `json:"autovacuum"`
	Failsafe   InsightHorizon `json:"failsafe"`
	Stop       InsightHorizon `json:"stop"`
	Health     Health         `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}

// InsightWraparound is the wraparound and vacuum panel, from the
// primary.
type InsightWraparound struct {
	// Available is false when the primary's database query reported
	// nothing; Text then says why.
	Available bool   `json:"available"`
	Text      string `json:"text,omitempty"`
	Instance  string `json:"instance,omitempty"`
	// Limits are the thresholds the forecasts use.
	Limits []InsightSetting `json:"limits"`
	// Assumed lists the settings whose PostgreSQL default was assumed.
	Assumed   []string          `json:"assumed"`
	XIDRate   *InsightRate      `json:"xidRate,omitempty"`
	MXIDRate  *InsightRate      `json:"mxidRate,omitempty"`
	Databases []InsightDatabase `json:"databases"`
	Tables    []InsightTable    `json:"tables"`
	Vacuums   []InsightVacuum   `json:"vacuums"`
	// PreparedXacts are the prepared transactions per database: each
	// holds its locks and the xmin horizon until COMMIT PREPARED or
	// ROLLBACK PREPARED.
	PreparedXacts []InsightPreparedXacts `json:"preparedXacts"`
	// Horizons is what holds the xmin horizon back; absent when not
	// reported.
	Horizons *InsightHorizons `json:"horizons,omitempty"`
	InsightPartial
	// Provenance is the primary's reading; absent when there is none.
	Provenance *Provenance `json:"provenance,omitempty"`
}

// InsightPreparedXacts is one database's prepared transactions; their
// gid (application data) never leaves the database.
type InsightPreparedXacts struct {
	Database      string   `json:"database" untrusted:"true"`
	Count         *float64 `json:"count,omitempty"`
	OldestSeconds *float64 `json:"oldestSeconds,omitempty"`
	OldestXIDAge  *float64 `json:"oldestXidAge,omitempty"`
}

// InsightHorizons is the age of what holds the xmin horizon back: vacuum
// cannot freeze or remove past the oldest of them. A missing field is
// "nothing holds one".
type InsightHorizons struct {
	SlotXminAge        *float64 `json:"slotXminAge,omitempty" doc:"Oldest xmin a replication slot holds."`
	SlotCatalogXminAge *float64 `json:"slotCatalogXminAge,omitempty" doc:"Oldest catalog_xmin a logical replication slot holds."`
	BackendXminAge     *float64 `json:"backendXminAge,omitempty" doc:"Oldest backend_xmin: open snapshots, standby feedback through walsenders included."`
	BackendXIDAge      *float64 `json:"backendXidAge,omitempty" doc:"Oldest transaction id a running backend holds."`
	PreparedXIDAge     *float64 `json:"preparedXidAge,omitempty" doc:"Oldest prepared transaction's id."`
	// Holder names the oldest: slot, catalogSlot, backend, transaction
	// or prepared; empty when nothing is reported.
	Holder string `json:"holder,omitempty" enum:"slot,catalogSlot,backend,transaction,prepared"`
}

// InsightDatabase is one database's wraparound position.
type InsightDatabase struct {
	Name             string           `json:"name" untrusted:"true"`
	AllowConnections *bool            `json:"allowConnections,omitempty"`
	XID              *InsightForecast `json:"xid,omitempty"`
	MXID             *InsightForecast `json:"mxid,omitempty"`
	StatsResetAt     *time.Time       `json:"statsResetAt,omitempty"`
}

// InsightTable is one table the pack singled out: among the oldest
// relfrozenxid or relminmxid, or the most dead rows, of its database.
type InsightTable struct {
	Database string `json:"database" untrusted:"true"`
	Schema   string `json:"schema" untrusted:"true"`
	Name     string `json:"name" untrusted:"true"`
	Kind     string `json:"kind" enum:"table,toast,matview,temporary"`
	// ParentSchema and ParentTable name the table a TOAST table belongs
	// to: the one to VACUUM.
	ParentSchema string `json:"parentSchema,omitempty" untrusted:"true"`
	ParentTable  string `json:"parentTable,omitempty" untrusted:"true"`
	// Reltuples is the row estimate autovacuum scales with; absent while
	// the table was never vacuumed nor analyzed, when the threshold is
	// unknown too.
	Reltuples *float64 `json:"reltuples,omitempty"`
	// XID is the table's own transaction id forecast (its
	// autovacuum_freeze_max_age when lower).
	XID                    *InsightForecast `json:"xid,omitempty"`
	MXIDAge                *float64         `json:"mxidAge,omitempty"`
	LiveTuples             *float64         `json:"liveTuples,omitempty"`
	DeadTuples             *float64         `json:"deadTuples,omitempty"`
	AutovacuumThreshold    *float64         `json:"autovacuumThreshold,omitempty"`
	SinceVacuumSeconds     *float64         `json:"sinceVacuumSeconds,omitempty"`
	SinceAutovacuumSeconds *float64         `json:"sinceAutovacuumSeconds,omitempty"`
	SinceAnalyzeSeconds    *float64         `json:"sinceAnalyzeSeconds,omitempty"`
	AutovacuumEnabled      *bool            `json:"autovacuumEnabled,omitempty"`
	// KeepUp is the console's autovacuum verdict from its own samples.
	KeepUp     string `json:"keepUp" enum:"unknown,keepingUp,notStarted,fallingBehind,disabled,never"`
	KeepUpText string `json:"keepUpText"`
	Health     Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
}

// InsightVacuum is one VACUUM in progress.
type InsightVacuum struct {
	Database         string   `json:"database" untrusted:"true"`
	Schema           string   `json:"schema" untrusted:"true"`
	Table            string   `json:"table" untrusted:"true"`
	Phase            string   `json:"phase"`
	Autovacuum       *bool    `json:"autovacuum,omitempty"`
	Wraparound       *bool    `json:"wraparound,omitempty"`
	RunningSeconds   *float64 `json:"runningSeconds,omitempty"`
	HeapBlksTotal    *float64 `json:"heapBlksTotal,omitempty"`
	HeapBlksScanned  *float64 `json:"heapBlksScanned,omitempty"`
	HeapBlksVacuumed *float64 `json:"heapBlksVacuumed,omitempty"`
	IndexVacuumCount *float64 `json:"indexVacuumCount,omitempty"`
}

// InsightStatements is the query insight of the primary's
// pg_stat_statements: no statement text, only queryid.
type InsightStatements struct {
	Available bool   `json:"available"`
	Text      string `json:"text,omitempty"`
	Instance  string `json:"instance,omitempty"`
	// Top are the statements the pack reports (most total time, highest
	// mean), cumulative since the statistics reset.
	Top []InsightStatement `json:"top"`
	// Comparable says whether a regression comparison can be made;
	// ComparisonText says why not, or what was compared.
	Comparable      bool                `json:"comparable"`
	ComparisonText  string              `json:"comparisonText"`
	CoveredSeconds  float64             `json:"coveredSeconds"`
	NeedsSeconds    float64             `json:"needsSeconds"`
	Regressions     []InsightRegression `json:"regressions"`
	Deallocations   *float64            `json:"deallocations,omitempty"`
	MaxStatements   *float64            `json:"maxStatements,omitempty"`
	StatsResetAt    *time.Time          `json:"statsResetAt,omitempty"`
	TextQuery       string              `json:"textQuery" doc:"The SQL to run in pgAdmin for a statement's text; QUERYID stands for the queryid."`
	Provenance      *Provenance         `json:"provenance,omitempty" doc:"The primary's reading; absent when there is none."`
	RegressionRatio float64             `json:"regressionRatio"`
	// NoBaseline counts the current top statements that were not in the
	// top a day ago: the console keeps the top only, so they are not
	// compared.
	NoBaseline int `json:"noBaseline"`
	// Restarted counts the statements whose counters went back within a
	// window (reset on their own, or evicted and added back): not
	// compared.
	Restarted int `json:"restarted"`
	// Evicted is the entries pg_stat_statements evicted across the
	// windows compared (dealloc growth); absent when not reported.
	Evicted *float64 `json:"evicted,omitempty"`
	InsightPartial
}

// InsightStatement is one pg_stat_statements entry, summed over its
// top-level rows.
type InsightStatement struct {
	QueryID        string   `json:"queryId"`
	Database       string   `json:"database" untrusted:"true"`
	Role           string   `json:"role" untrusted:"true"`
	Calls          *float64 `json:"calls,omitempty"`
	TotalMillis    *float64 `json:"totalMillis,omitempty"`
	MeanMillis     *float64 `json:"meanMillis,omitempty"`
	Rows           *float64 `json:"rows,omitempty"`
	SharedBlksHit  *float64 `json:"sharedBlksHit,omitempty"`
	SharedBlksRead *float64 `json:"sharedBlksRead,omitempty"`
	// HitRatio is hit / (hit + read); absent without blocks.
	HitRatio *float64 `json:"hitRatio,omitempty"`
}

// InsightRegression is a statement whose mean time grew at least
// RegressionRatio times against the same hour a day earlier.
type InsightRegression struct {
	QueryID       string    `json:"queryId"`
	Database      string    `json:"database" untrusted:"true"`
	Role          string    `json:"role" untrusted:"true"`
	Ratio         float64   `json:"ratio"`
	RecentMean    float64   `json:"recentMeanMillis"`
	BaselineMean  float64   `json:"baselineMeanMillis"`
	RecentCalls   float64   `json:"recentCalls"`
	BaselineCalls float64   `json:"baselineCalls"`
	RecentFrom    time.Time `json:"recentFrom"`
	RecentTo      time.Time `json:"recentTo"`
	BaselineFrom  time.Time `json:"baselineFrom"`
	BaselineTo    time.Time `json:"baselineTo"`
	// Uncertain is set when pg_stat_statements evicted entries during a
	// window: an entry evicted and added back restarts its counters.
	Uncertain bool `json:"uncertain,omitempty"`
}

// InsightIndexes is the index hygiene panel, from the primary.
type InsightIndexes struct {
	Available bool           `json:"available"`
	Text      string         `json:"text,omitempty"`
	Instance  string         `json:"instance,omitempty"`
	Unused    []InsightIndex `json:"unused"`
	Invalid   []InsightIndex `json:"invalid"`
	Duplicate []InsightIndex `json:"duplicate"`
	// StatsReset is the oldest statistics reset among the databases;
	// each index carries its own database's.
	StatsResetAt *time.Time `json:"statsResetAt,omitempty"`
	InsightPartial
	Provenance *Provenance `json:"provenance,omitempty" doc:"The primary's reading; absent when there is none."`
}

// InsightIndex is one index the pack flagged.
type InsightIndex struct {
	Database    string   `json:"database" untrusted:"true"`
	Schema      string   `json:"schema" untrusted:"true"`
	Table       string   `json:"table" untrusted:"true"`
	Name        string   `json:"name" untrusted:"true"`
	DuplicateOf string   `json:"duplicateOf,omitempty" untrusted:"true"`
	SizeBytes   *float64 `json:"sizeBytes,omitempty"`
	Scans       *float64 `json:"scans,omitempty"`
	Unique      *bool    `json:"unique,omitempty"`
	// BacksConstraint is set when the index backs a constraint (primary
	// key, unique, exclusion, the target of a foreign key) or is the
	// replica identity.
	BacksConstraint *bool `json:"backsConstraint,omitempty"`
	// Droppable is false for a duplicate pair where both back a
	// constraint: reported, but neither can simply be dropped.
	Droppable *bool `json:"droppable,omitempty"`
	// FKSupport is set when the index's leading columns are a foreign
	// key's: it serves the check when referenced rows change, so few
	// scans are expected.
	FKSupport *bool `json:"fkSupport,omitempty"`
	// StatsResetAt is its database's statistics reset: "unused" means
	// "no scan on this instance since then", and a failover or a crash
	// recovery restarts the counters without setting it.
	StatsResetAt *time.Time `json:"statsResetAt,omitempty"`
}

// InsightConnections is the connection saturation panel, per instance.
type InsightConnections struct {
	Available bool                         `json:"available"`
	Text      string                       `json:"text,omitempty"`
	Instances []InsightInstanceConnections `json:"instances"`
	InsightPartial
}

// InsightInstanceConnections is one instance's connections.
type InsightInstanceConnections struct {
	Instance       string   `json:"instance"`
	Primary        bool     `json:"primary"`
	Client         *float64 `json:"client,omitempty"`
	MaxConnections *float64 `json:"maxConnections,omitempty"`
	// Reserved is superuser_reserved_connections plus
	// reserved_connections (16 and later).
	Reserved *float64 `json:"reserved,omitempty"`
	// Usable is MaxConnections minus Reserved: the slots ordinary roles
	// can take.
	Usable *float64 `json:"usable,omitempty"`
	// Share is Client over Usable.
	Share                       *float64                 `json:"share,omitempty"`
	Active                      *float64                 `json:"active,omitempty"`
	IdleInTransaction           *float64                 `json:"idleInTransaction,omitempty"`
	IdleInTransactionMaxSeconds *float64                 `json:"idleInTransactionMaxSeconds,omitempty"`
	LockWaiting                 *float64                 `json:"lockWaiting,omitempty"`
	MaxXactAgeSeconds           *float64                 `json:"maxXactAgeSeconds,omitempty"`
	IdleTimeoutSeconds          *float64                 `json:"idleInTransactionSessionTimeoutSeconds,omitempty"`
	Groups                      []InsightConnectionGroup `json:"groups"`
	Health                      Health                   `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Provenance                  Provenance               `json:"provenance"`
}

// InsightConnectionGroup is client connections of one database,
// application and state.
type InsightConnectionGroup struct {
	Database           string   `json:"database" untrusted:"true"`
	Application        string   `json:"application"`
	State              string   `json:"state"`
	Sessions           *float64 `json:"sessions,omitempty"`
	MaxStateAgeSeconds *float64 `json:"maxStateAgeSeconds,omitempty"`
}

// InsightBlocking is the lock graph of each instance.
type InsightBlocking struct {
	Available bool   `json:"available"`
	Text      string `json:"text,omitempty"`
	// RefreshedAt is when the console last read the primary on demand;
	// MinRefreshSeconds the least interval between such reads.
	RefreshedAt       *time.Time                `json:"refreshedAt,omitempty"`
	MinRefreshSeconds float64                   `json:"minRefreshSeconds"`
	Instances         []InsightInstanceBlocking `json:"instances"`
	InsightPartial
}

// InsightInstanceBlocking is one instance's lock graph.
type InsightInstanceBlocking struct {
	Instance string `json:"instance"`
	Primary  bool   `json:"primary"`
	// Waiting counts backends waiting on a lock (pgtoolbox_backends).
	Waiting *float64           `json:"waiting,omitempty"`
	Roots   []InsightBlockNode `json:"roots"`
	// DataAgeSeconds is how old the exporter's data was when the console
	// read it (it caches its queries for metricsQueriesTTL).
	DataAgeSeconds float64 `json:"dataAgeSeconds"`
	// Provenance.ObservedAt is when the exporter ran the queries.
	Provenance Provenance `json:"provenance"`
}

// InsightBlockNode is one backend and those waiting on it. The tree is
// bounded by the pack's 50 rows; Missing marks a blocker outside them.
type InsightBlockNode struct {
	// PID is 0 for a prepared transaction (Prepared).
	PID int64 `json:"pid"`
	// Prepared marks a prepared transaction: it holds its locks without
	// a backend, until COMMIT PREPARED or ROLLBACK PREPARED by its gid.
	Prepared        bool               `json:"prepared,omitempty"`
	Missing         bool               `json:"missing,omitempty"`
	Cycle           bool               `json:"cycle,omitempty"`
	Database        string             `json:"database,omitempty" untrusted:"true"`
	User            string             `json:"user,omitempty" untrusted:"true"`
	Application     string             `json:"application,omitempty"`
	State           string             `json:"state,omitempty"`
	WaitEventType   string             `json:"waitEventType,omitempty"`
	WaitEvent       string             `json:"waitEvent,omitempty"`
	BackendType     string             `json:"backendType,omitempty"`
	QueryID         string             `json:"queryId,omitempty"`
	WaitSeconds     *float64           `json:"waitSeconds,omitempty"`
	XactAgeSeconds  *float64           `json:"xactAgeSeconds,omitempty"`
	StateAgeSeconds *float64           `json:"stateAgeSeconds,omitempty"`
	Blocked         int                `json:"blocked"`
	Waiters         []InsightBlockNode `json:"waiters"`
}

// FleetStatements is the team-wide query insight: the top statements
// and the regressions of every Cluster the caller may see.
type FleetStatements struct {
	GeneratedAt time.Time               `json:"generatedAt"`
	Clusters    []FleetClusterStatement `json:"clusters"`
	// Regressions are every Cluster's regressions, the largest ratio
	// first.
	Regressions []FleetRegression `json:"regressions"`
	// Top are the costliest statements by total time, of the Clusters
	// the caller may see, at most 100.
	Top []FleetStatement `json:"top"`
	// TopTotal is how many statements there were before the cut;
	// TopTruncated is set when Top was cut.
	TopTotal     int         `json:"topTotal"`
	TopTruncated bool        `json:"topTruncated"`
	BlindSpots   []BlindSpot `json:"blindSpots"`
}

// FleetClusterStatement is one Cluster's coverage in the fleet view.
type FleetClusterStatement struct {
	Ref ClusterRef `json:"ref"`
	// State is the pack verdict (InsightPack.State).
	State          string  `json:"state" enum:"enabled,notEnabled,outdated,newer,unknown,truncated,unsupported,noData,metricsOff"`
	Statements     bool    `json:"statements" doc:"The primary reports pg_stat_statements."`
	Comparable     bool    `json:"comparable"`
	ComparisonText string  `json:"comparisonText"`
	CoveredSeconds float64 `json:"coveredSeconds"`
}

// FleetRegression is one Cluster's regression.
type FleetRegression struct {
	Ref ClusterRef `json:"ref"`
	InsightRegression
}

// FleetStatement is one Cluster's statement.
type FleetStatement struct {
	Ref ClusterRef `json:"ref"`
	InsightStatement
}
