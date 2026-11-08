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

import (
	"bufio"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/scrape"
	"github.com/fyannk/pgToolBox/pkg/redact"
)

// Parse bounds. The pack's own LIMITs keep a well-behaved exporter far
// below them (25 tables and 45 indexes per database, 60 statements, 50
// sessions); they exist for the exposition the console does not control:
// an edited pack, a Cluster with hundreds of databases, a hostile pod.
const (
	// MaxRows bounds the rows kept per family; the rest are dropped and
	// the family is named in Reading.Truncated.
	MaxRows = 400
	// maxLabels bounds the labels of one line (the widest family,
	// pgtoolbox_blocking, has ten).
	maxLabels = 16
	// maxLabelValue cuts a label value: PostgreSQL names are at most 63
	// bytes and application_name 63, so anything longer is not the pack's.
	maxLabelValue = 256
	// maxBlockers bounds the pids parsed out of blocked_by (the pack
	// writes at most eight).
	maxBlockers = 8
	// maxQueryErrors bounds the per-query error counters kept.
	maxQueryErrors = 32
)

// rows assembles one family's rows by label set, in first-seen order.
type rows[T any] struct {
	byKey     map[string]*T
	order     []string
	truncated bool
}

func (r *rows[T]) get(key string, init func() T) *T {
	if r.byKey == nil {
		r.byKey = map[string]*T{}
	}
	if row, ok := r.byKey[key]; ok {
		return row
	}
	if len(r.order) >= MaxRows {
		r.truncated = true
		return nil
	}
	row := init()
	r.byKey[key] = &row
	r.order = append(r.order, key)
	return &row
}

func (r *rows[T]) list() []T {
	out := make([]T, 0, len(r.order))
	for _, k := range r.order {
		out = append(out, *r.byKey[k])
	}
	return out
}

func key(l map[string]string, names ...string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = l[n]
	}
	return strings.Join(parts, "\x00")
}

// Parse reads one exporter exposition and keeps the insight pack's
// series. It is the trust-boundary parser of untrusted exporter text
// (fuzzed): lines outside the pack are skipped unread, malformed lines
// and NaN values are dropped, rows per family and label sizes are
// bounded. An error is only a read error; a body without the pack is a
// Reading whose Present is false.
func Parse(r io.Reader) (Reading, error) {
	p := parser{settings: map[string]float64{}, errors: map[string]float64{}, failedDBs: map[string][]string{}}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		p.line(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return Reading{}, redact.NewError("insight parse", redact.CategoryInternal, err)
	}
	return p.reading(), nil
}

// parser accumulates one exposition: the scalar facts straight into the
// Reading, the per-object families as bounded rows, the error counters
// and settings as bounded maps. reading assembles them at the end.
type parser struct {
	out         Reading
	settings    map[string]float64
	databases   rows[Database]
	tables      rows[Table]
	vacuums     rows[VacuumProgress]
	sessions    rows[Session]
	connections rows[ConnectionGroup]
	statements  rows[Statement]
	indexes     rows[Index]
	prepared    rows[PreparedXacts]
	horizons    *Horizons
	backends    *Backends
	info        *StatementsInfo
	errors      map[string]float64
	failedDBs   map[string][]string
	settingsCut bool
}

// line reads one exposition line. Anything that is not a well-formed
// sample of the pack, of its error counter or of the exporter's update
// time is skipped before its labels are even parsed.
func (p *parser) line(line string) {
	name, block, value, ok := scrape.ParseSampleLine(line)
	if !ok {
		return
	}
	if name == lastUpdate {
		if value > 0 {
			t := time.Unix(int64(value), 0).UTC()
			p.out.ExporterUpdatedAt = &t
		}
		return
	}
	if name != userQueryErrors && !strings.HasPrefix(name, metricPrefix) {
		return
	}
	labels, ok := scrape.ParseLabels(block, maxLabels, maxLabelValue)
	if !ok {
		return
	}
	p.sample(name, labels, value)
}

// sample routes one sample to its family by metric-name prefix. The
// order matters where one prefix extends another: statements_info is
// tried before statements.
func (p *parser) sample(name string, labels map[string]string, value float64) {
	switch {
	case name == userQueryErrors:
		p.queryError(labels, value)
	case strings.HasPrefix(name, familyPack):
		switch strings.TrimPrefix(name, familyPack) {
		case "info":
			if value == 1 {
				p.out.Version = labels["version"]
			}
		case "server_version_num":
			p.out.ServerVersionNum = new(value)
		}
	case strings.HasPrefix(name, familySettings):
		p.setting(strings.TrimPrefix(name, familySettings), labels, value)
	case strings.HasPrefix(name, familyXID):
		if strings.TrimPrefix(name, familyXID) == "next_xid" {
			p.out.NextXID = new(value)
		}
	case strings.HasPrefix(name, familyDatabase):
		row := p.databases.get(key(labels, "datname"), func() Database { return Database{Name: labels["datname"]} })
		if row != nil {
			setDatabase(row, strings.TrimPrefix(name, familyDatabase), value)
		}
	case strings.HasPrefix(name, familyTables):
		row := p.tables.get(key(labels, "datname", "schema", "table"), func() Table {
			return Table{
				Database: labels["datname"], Schema: labels["schema"], Name: labels["table"], Kind: labels["kind"],
				ParentSchema: labels["parent_schema"], ParentTable: labels["parent_table"],
			}
		})
		if row != nil {
			setTable(row, strings.TrimPrefix(name, familyTables), value)
		}
	case strings.HasPrefix(name, familyVacuum):
		row := p.vacuums.get(key(labels, "datname", "schema", "table"), func() VacuumProgress {
			return VacuumProgress{Database: labels["datname"], Schema: labels["schema"], Table: labels["table"], Phase: labels["phase"]}
		})
		if row != nil {
			setVacuum(row, strings.TrimPrefix(name, familyVacuum), value)
		}
	case strings.HasPrefix(name, familyBlocking):
		p.blocking(strings.TrimPrefix(name, familyBlocking), labels, value)
	case strings.HasPrefix(name, familyBackends):
		if p.backends == nil {
			p.backends = &Backends{}
		}
		setBackends(p.backends, strings.TrimPrefix(name, familyBackends), value)
	case strings.HasPrefix(name, familyConnections):
		row := p.connections.get(key(labels, "datname", "application_name", "state"), func() ConnectionGroup {
			return ConnectionGroup{Database: labels["datname"], Application: labels["application_name"], State: labels["state"]}
		})
		if row != nil {
			setConnections(row, strings.TrimPrefix(name, familyConnections), value)
		}
	case strings.HasPrefix(name, familyStmtInfo):
		if p.info == nil {
			p.info = &StatementsInfo{}
		}
		setStatementsInfo(p.info, strings.TrimPrefix(name, familyStmtInfo), value)
	case strings.HasPrefix(name, familyStatements):
		p.statement(strings.TrimPrefix(name, familyStatements), labels, value)
	case strings.HasPrefix(name, familyIndexes), strings.HasPrefix(name, familyIndexDups):
		p.index(name, labels, value)
	case strings.HasPrefix(name, familyPrepared):
		row := p.prepared.get(key(labels, "datname"), func() PreparedXacts { return PreparedXacts{Database: labels["datname"]} })
		if row != nil {
			setPrepared(row, strings.TrimPrefix(name, familyPrepared), value)
		}
	case strings.HasPrefix(name, familyXmin):
		if p.horizons == nil {
			p.horizons = &Horizons{}
		}
		setHorizons(p.horizons, strings.TrimPrefix(name, familyXmin), value)
	}
}

// queryError counts a failing pack query and the databases it failed in,
// both bounded; the error text itself is never kept.
func (p *parser) queryError(labels map[string]string, value float64) {
	q, db := errorQuery(labels["errorUserQueries"])
	if q == "" {
		return
	}
	if _, seen := p.errors[q]; !seen && len(p.errors) >= maxQueryErrors {
		return
	}
	p.errors[q] += value
	if db != "" && len(p.failedDBs[q]) < maxQueryErrors && !slices.Contains(p.failedDBs[q], db) {
		p.failedDBs[q] = append(p.failedDBs[q], db)
	}
}

// setting keeps one named setting; past the bound the rest are dropped
// and the family is reported truncated.
func (p *parser) setting(column string, labels map[string]string, value float64) {
	if column != "value" || labels["name"] == "" {
		return
	}
	if _, seen := p.settings[labels["name"]]; !seen && len(p.settings) >= maxLabels*2 {
		p.settingsCut = true
		return
	}
	p.settings[labels["name"]] = value
}

// blocking keeps one session of the lock tree, keyed by its pid; a
// missing or negative pid is not a session.
func (p *parser) blocking(column string, labels map[string]string, value float64) {
	pid, err := strconv.ParseInt(labels["pid"], 10, 64)
	if err != nil || pid < 0 {
		return
	}
	row := p.sessions.get(labels["pid"], func() Session {
		return Session{
			PID: pid, BlockedBy: parsePIDs(labels["blocked_by"]),
			Database: labels["datname"], User: labels["usename"], Application: labels["application_name"],
			State: labels["state"], WaitEventType: labels["wait_event_type"], WaitEvent: labels["wait_event"],
			BackendType: labels["backend_type"], QueryID: labels["queryid"],
		}
	})
	if row == nil {
		return
	}
	switch column {
	case "wait_seconds":
		row.WaitSeconds = new(value)
	case "xact_age_seconds":
		row.XactAgeSeconds = new(value)
	case "state_age_seconds":
		row.StateAgeSeconds = new(value)
	case "blocking_count":
		row.BlockingCount = new(value)
	}
}

// statement keeps one pg_stat_statements entry; a row without a queryid
// cannot be compared over time and is dropped.
func (p *parser) statement(column string, labels map[string]string, value float64) {
	if labels["queryid"] == "" {
		return
	}
	row := p.statements.get(key(labels, "queryid", "datname", "rolname"), func() Statement {
		return Statement{QueryID: labels["queryid"], Database: labels["datname"], Role: labels["rolname"]}
	})
	if row != nil {
		setStatement(row, column, value)
	}
}

// index keeps one index finding. The unused and duplicate families share
// the Index row type; one index can be both unused and a duplicate, so the
// kind is part of the key and it reads as two rows.
func (p *parser) index(name string, labels map[string]string, value float64) {
	column, kind := strings.TrimPrefix(name, familyIndexes), labels["kind"]
	if strings.HasPrefix(name, familyIndexDups) {
		column, kind = strings.TrimPrefix(name, familyIndexDups), "duplicate"
	}
	row := p.indexes.get(labels["datname"]+"\x00"+labels["schema"]+"\x00"+labels["index"]+"\x00"+kind, func() Index {
		return Index{
			Database: labels["datname"], Schema: labels["schema"], Table: labels["table"], Name: labels["index"],
			Kind: kind, DuplicateOf: labels["duplicate_of"],
		}
	})
	if row != nil {
		setIndex(row, column, value)
	}
}

// reading assembles the Reading: maps only when they hold something, the
// rows in first-seen order, presence, and the families cut at a bound.
func (p *parser) reading() Reading {
	out := p.out
	if len(p.settings) > 0 {
		out.Settings = p.settings
	}
	if len(p.errors) > 0 {
		out.QueryErrors = p.errors
	}
	if len(p.failedDBs) > 0 {
		for _, dbs := range p.failedDBs {
			sort.Strings(dbs)
		}
		out.FailedDatabases = p.failedDBs
	}
	out.Databases, out.Tables, out.Vacuums = p.databases.list(), p.tables.list(), p.vacuums.list()
	out.Sessions, out.Connections = p.sessions.list(), p.connections.list()
	out.Statements, out.Indexes = p.statements.list(), p.indexes.list()
	out.Backends, out.StatementsInfo = p.backends, p.info
	out.PreparedXacts, out.Horizons = p.prepared.list(), p.horizons
	// Rows of the pack without pack_info (lost to a cut exposition, say):
	// the pack is loaded, at an unknown version.
	out.Seen = out.Version != "" || out.ServerVersionNum != nil || len(p.settings) > 0 || out.NextXID != nil ||
		len(out.Databases)+len(out.Tables)+len(out.Vacuums)+len(out.Sessions)+len(out.Connections)+
			len(out.Statements)+len(out.Indexes)+len(out.PreparedXacts) > 0 ||
		p.backends != nil || p.info != nil || p.horizons != nil
	for _, cut := range []struct {
		name string
		cut  bool
	}{
		{"pgtoolbox_settings", p.settingsCut},
		{"pgtoolbox_database", p.databases.truncated},
		{"pgtoolbox_tables", p.tables.truncated},
		{"pgtoolbox_vacuum_progress", p.vacuums.truncated},
		{"pgtoolbox_blocking", p.sessions.truncated},
		{"pgtoolbox_connections", p.connections.truncated},
		{"pgtoolbox_statements", p.statements.truncated},
		{"pgtoolbox_indexes", p.indexes.truncated},
		{"pgtoolbox_prepared_xacts", p.prepared.truncated},
	} {
		if cut.cut {
			out.Truncated = append(out.Truncated, cut.name)
		}
	}
	return out
}

func setDatabase(row *Database, column string, value float64) {
	switch column {
	case "xid_age":
		row.XIDAge = new(value)
	case "mxid_age":
		row.MXIDAge = new(value)
	case "min_mxid":
		row.MinMXID = new(value)
	case "stats_reset_time":
		row.StatsResetUnix = new(value)
	case "allow_connections":
		row.AllowConnections = new(value)
	}
}

func setBackends(b *Backends, column string, value float64) {
	switch column {
	case "client":
		b.Client = new(value)
	case "active":
		b.Active = new(value)
	case "idle_in_transaction":
		b.IdleInTransaction = new(value)
	case "idle_in_transaction_max_seconds":
		b.IdleInTransactionMaxSeconds = new(value)
	case "lock_waiting":
		b.LockWaiting = new(value)
	case "max_xact_age_seconds":
		b.MaxXactAgeSeconds = new(value)
	}
}

func setConnections(row *ConnectionGroup, column string, value float64) {
	switch column {
	case "sessions":
		row.Sessions = new(value)
	case "max_state_age_seconds":
		row.MaxStateAgeSeconds = new(value)
	}
}

func setStatementsInfo(info *StatementsInfo, column string, value float64) {
	switch column {
	case "dealloc":
		info.Dealloc = new(value)
	case "stats_reset_time":
		info.StatsResetUnix = new(value)
	case "max":
		info.Max = new(value)
	}
}

func setPrepared(row *PreparedXacts, column string, value float64) {
	switch column {
	case "count":
		row.Count = new(value)
	case "oldest_seconds":
		row.OldestSeconds = new(value)
	case "oldest_xid_age":
		row.OldestXIDAge = new(value)
	}
}

func setHorizons(h *Horizons, column string, value float64) {
	switch column {
	case "slot_xmin_age":
		h.SlotXminAge = new(value)
	case "slot_catalog_xmin_age":
		h.SlotCatalogXminAge = new(value)
	case "backend_xmin_age":
		h.BackendXminAge = new(value)
	case "backend_xid_age":
		h.BackendXIDAge = new(value)
	}
}

// errorQuery extracts the pack query name, and the database when there
// is one, from a cnpg_errors_total label ("<query>: <error>" or "<query>
// on db <db>: <error>"), dropping the error text, which may quote object
// names. Non-pack queries and anything unrecognisable yield "".
func errorQuery(label string) (query, database string) {
	q := label
	if i := strings.IndexAny(q, ": "); i >= 0 {
		q = q[:i]
	}
	if !strings.HasPrefix(q, "pgtoolbox_") || !slices.Contains(Queries, q) {
		return "", ""
	}
	if rest, ok := strings.CutPrefix(label, q+" on db "); ok {
		// The name ends at the first ": "; one that holds ": " itself
		// reads cut, which only shortens the name shown.
		if i := strings.Index(rest, ": "); i > 0 && i <= maxLabelValue {
			database = rest[:i]
		}
	}
	return q, database
}

// parsePIDs reads blocked_by: comma-separated pids, at most maxBlockers,
// sorted and without duplicates; 0 is a prepared transaction, anything
// negative or unreadable is dropped.
func parsePIDs(s string) []int64 {
	if s == "" {
		return nil
	}
	seen := map[int64]bool{}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		if len(out) >= maxBlockers {
			break
		}
		pid, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || pid < 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, pid)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func setTable(row *Table, column string, value float64) {
	switch column {
	case "xid_age":
		row.XIDAge = new(value)
	case "mxid_age":
		row.MXIDAge = new(value)
	case "freeze_max_age":
		row.FreezeMaxAge = new(value)
	case "live_tuples":
		row.LiveTuples = new(value)
	case "dead_tuples":
		row.DeadTuples = new(value)
	case "reltuples":
		row.Reltuples = new(value)
	case "autovacuum_threshold":
		row.AutovacuumThreshold = new(value)
	case "seconds_since_vacuum":
		row.SinceVacuum = new(value)
	case "seconds_since_autovacuum":
		row.SinceAutovacuum = new(value)
	case "seconds_since_analyze":
		row.SinceAnalyze = new(value)
	case "autovacuum_count":
		row.AutovacuumCount = new(value)
	case "autovacuum_enabled":
		row.AutovacuumEnabled = new(value)
	}
}

func setIndex(row *Index, column string, value float64) {
	switch column {
	case "size_bytes":
		row.SizeBytes = new(value)
	case "scans":
		row.Scans = new(value)
	case "unique":
		row.Unique = new(value)
	case "backs_constraint":
		row.BacksConstraint = new(value)
	case "droppable":
		row.Droppable = new(value)
	case "fk_support":
		row.FKSupport = new(value)
	}
}

func setVacuum(row *VacuumProgress, column string, value float64) {
	switch column {
	case "autovacuum":
		row.Autovacuum = new(value)
	case "wraparound":
		row.Wraparound = new(value)
	case "running_seconds":
		row.RunningSeconds = new(value)
	case "heap_blks_total":
		row.HeapBlksTotal = new(value)
	case "heap_blks_scanned":
		row.HeapBlksScanned = new(value)
	case "heap_blks_vacuumed":
		row.HeapBlksVacuumed = new(value)
	case "index_vacuum_count":
		row.IndexVacuumCount = new(value)
	}
}

func setStatement(row *Statement, column string, value float64) {
	switch column {
	case "calls":
		row.Calls = new(value)
	case "total_exec_time_ms":
		row.TotalMillis = new(value)
	case "mean_exec_time_ms":
		row.MeanMillis = new(value)
	case "rows":
		row.Rows = new(value)
	case "shared_blks_hit":
		row.SharedBlksHit = new(value)
	case "shared_blks_read":
		row.SharedBlksRead = new(value)
	}
}
