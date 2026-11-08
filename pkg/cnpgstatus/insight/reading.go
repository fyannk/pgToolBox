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

package insight

import "time"

// Reading is one instance's insight-pack series from one exposition. A
// nil pointer or an absent row is "not reported", never zero: the pack
// writes NULL (exported as NaN) for "never" and "not applicable", and a
// query that did not run (primary-only on a replica, a guard that said
// no, an error) leaves its rows out entirely.
type Reading struct {
	// Version is the pack version the instance reports; empty when the
	// pack is not loaded, or when pack_info was not read although other
	// pack series were (see Seen).
	Version string
	// Seen reports that at least one pack series was read: with an empty
	// Version, the pack is loaded at an unknown version (pack_info lost,
	// e.g. in a cut exposition).
	Seen bool
	// ServerVersionNum is server_version_num as the pack reports it;
	// below 140000 every other pack query is skipped.
	ServerVersionNum *float64
	// Settings are pgtoolbox_settings, by setting name, in base units.
	Settings map[string]float64
	// NextXID is the next transaction id (64-bit), primary only.
	NextXID *float64
	// Databases are pgtoolbox_database, primary only.
	Databases []Database
	// Tables are pgtoolbox_tables (top-N per database), primary only.
	Tables []Table
	// Vacuums are pgtoolbox_vacuum_progress, primary only.
	Vacuums []VacuumProgress
	// Sessions are pgtoolbox_blocking: backends waiting on a lock and
	// those they wait for.
	Sessions []Session
	// Backends is pgtoolbox_backends; nil when not reported.
	Backends *Backends
	// Connections are pgtoolbox_connections (the largest groups).
	Connections []ConnectionGroup
	// Statements are pgtoolbox_statements, primary only, present only
	// when pg_stat_statements is loaded and installed in public.
	Statements []Statement
	// StatementsInfo is pgtoolbox_statements_info; nil when not reported.
	StatementsInfo *StatementsInfo
	// Indexes are pgtoolbox_indexes, primary only.
	Indexes []Index
	// PreparedXacts are pgtoolbox_prepared_xacts, per database, primary
	// only.
	PreparedXacts []PreparedXacts
	// Horizons is pgtoolbox_xmin, primary only; nil when not reported.
	Horizons *Horizons
	// ExporterUpdatedAt is cnpg_last_update_timestamp: when the exporter
	// last ran its custom queries (it caches them for metricsQueriesTTL).
	ExporterUpdatedAt *time.Time
	// QueryErrors counts the exporter's recorded failures per pack query
	// (cnpg_errors_total), keyed by query name; error texts are dropped.
	// CloudNativePG resets the counter at every run, so these are the
	// failures of the run the reading comes from.
	QueryErrors map[string]float64
	// FailedDatabases names, per pack query, the databases it failed on
	// ("<query> on db <db>: ..."), sorted; a failure without a database
	// (the query as a whole) is not listed here.
	FailedDatabases map[string][]string
	// Truncated names the families cut at the parser's row bound, and
	// TruncatedExposition when the console cut the exposition itself
	// (every family after the cut is missing, pack_info included).
	Truncated []string
}

// TruncatedExposition in Reading.Truncated marks an exposition the
// console cut at its size bound.
const TruncatedExposition = "exposition"

// ExpositionCut reports whether the console cut the exposition: what is
// absent may only be past the cut.
func (r Reading) ExpositionCut() bool {
	for _, t := range r.Truncated {
		if t == TruncatedExposition {
			return true
		}
	}
	return false
}

// Present reports whether the instance reports the pack at all, at a
// known version or not.
func (r Reading) Present() bool { return r.Version != "" || r.Seen }

// Database is one row of pgtoolbox_database.
type Database struct {
	Name             string
	XIDAge           *float64
	MXIDAge          *float64
	MinMXID          *float64
	StatsResetUnix   *float64
	AllowConnections *float64
}

// Table is one row of pgtoolbox_tables.
type Table struct {
	Database, Schema, Name string
	// Kind is table, toast, matview or temporary.
	Kind string
	// ParentSchema and ParentTable name the table a TOAST table belongs
	// to: the one to VACUUM. Empty for any other kind.
	ParentSchema, ParentTable string
	// Reltuples is pg_class.reltuples; nil while -1 (never vacuumed nor
	// analyzed), when AutovacuumThreshold is nil too.
	Reltuples           *float64
	XIDAge              *float64
	MXIDAge             *float64
	FreezeMaxAge        *float64
	LiveTuples          *float64
	DeadTuples          *float64
	AutovacuumThreshold *float64
	SinceVacuum         *float64
	SinceAutovacuum     *float64
	SinceAnalyze        *float64
	AutovacuumCount     *float64
	AutovacuumEnabled   *float64
}

// Key identifies a table across samples.
func (t Table) Key() string { return t.Database + "\x00" + t.Schema + "\x00" + t.Name }

// VacuumProgress is one row of pgtoolbox_vacuum_progress.
type VacuumProgress struct {
	Database, Schema, Table, Phase string
	Autovacuum                     *float64
	Wraparound                     *float64
	RunningSeconds                 *float64
	HeapBlksTotal                  *float64
	HeapBlksScanned                *float64
	HeapBlksVacuumed               *float64
	IndexVacuumCount               *float64
}

// Session is one row of pgtoolbox_blocking.
type Session struct {
	// PID is the backend's pid; 0 is how pg_blocking_pids names a
	// prepared transaction, which has no backend (see Prepared).
	PID int64
	// BlockedBy are the pids this backend waits for (at most eight, 0
	// for a prepared transaction); empty for a backend that only blocks
	// others.
	BlockedBy                             []int64
	Database, User, Application, State    string
	WaitEventType, WaitEvent, BackendType string
	// QueryID is pg_stat_activity.query_id, empty without compute_query_id.
	QueryID         string
	WaitSeconds     *float64
	XactAgeSeconds  *float64
	StateAgeSeconds *float64
	BlockingCount   *float64
}

// Prepared reports a prepared transaction: pid 0, which holds locks
// without a backend until COMMIT PREPARED or ROLLBACK PREPARED.
func (s Session) Prepared() bool { return s.PID == 0 }

// Backends is pgtoolbox_backends.
type Backends struct {
	Client                      *float64
	Active                      *float64
	IdleInTransaction           *float64
	IdleInTransactionMaxSeconds *float64
	LockWaiting                 *float64
	MaxXactAgeSeconds           *float64
}

// ConnectionGroup is one row of pgtoolbox_connections.
type ConnectionGroup struct {
	Database, Application, State string
	Sessions                     *float64
	MaxStateAgeSeconds           *float64
}

// Statement is one row of pgtoolbox_statements; cumulative since the
// statistics reset.
type Statement struct {
	QueryID, Database, Role string
	Calls                   *float64
	TotalMillis             *float64
	MeanMillis              *float64
	Rows                    *float64
	SharedBlksHit           *float64
	SharedBlksRead          *float64
}

// Key identifies a statement across samples.
func (s Statement) Key() string { return s.QueryID + "\x00" + s.Database + "\x00" + s.Role }

// StatementsInfo is pgtoolbox_statements_info.
type StatementsInfo struct {
	Dealloc        *float64
	StatsResetUnix *float64
	Max            *float64
}

// Index is one row of pgtoolbox_indexes.
type Index struct {
	Database, Schema, Table, Name string
	// Kind is unused, invalid or duplicate.
	Kind string
	// DuplicateOf names the index kept for a duplicate.
	DuplicateOf string
	SizeBytes   *float64
	Scans       *float64
	Unique      *float64
	// BacksConstraint is 1 when the index backs a constraint (primary
	// key, unique, exclusion, the target of a foreign key) or is the
	// replica identity.
	BacksConstraint *float64
	// Droppable is 0 when dropping the index would drop or break a
	// constraint: a duplicate pair where both back one.
	Droppable *float64
	// FKSupport is 1 when the index's leading columns are a foreign
	// key's: it serves the check when referenced rows change, so few
	// scans are expected.
	FKSupport *float64
}

// Key identifies an index finding: one index can be both unused and a
// duplicate, two distinct rows.
func (i Index) Key() string {
	return i.Database + "\x00" + i.Schema + "\x00" + i.Name + "\x00" + i.Kind
}

// PreparedXacts is one row of pgtoolbox_prepared_xacts. The gid is
// never exported: it is application data.
type PreparedXacts struct {
	Database      string
	Count         *float64
	OldestSeconds *float64
	OldestXIDAge  *float64
}

// Horizons is pgtoolbox_xmin: what holds the xmin horizon back. A nil
// field is "nothing holds one" (the query writes NULL).
type Horizons struct {
	SlotXminAge        *float64
	SlotCatalogXminAge *float64
	BackendXminAge     *float64
	BackendXIDAge      *float64
}
