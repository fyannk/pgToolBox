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

// Package insightview converts a Cluster's insight-pack snapshot (the
// sweep's pkg/cnpgstatus/insight state) into the /insight view models.
// It is pure and clock-injected, shared by the read model and the API
// fixtures so both draw from one conversion. Nothing here reads
// PostgreSQL: every number is the exporter's, or the console's own
// derivation over its retained samples, and is labelled as such.
package insightview

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

// StatementTextQuery is what the screens tell a DBA to run in pgAdmin
// for a statement's text: the text never leaves the database.
const StatementTextQuery = "SELECT query FROM pg_stat_statements WHERE queryid = QUERYID LIMIT 1;"

// FleetTopStatements bounds the team-wide statement list.
const FleetTopStatements = 100

// Panel queries: the pack queries each panel is drawn from. A failure or
// a cut of one of them makes the panel partial.
var (
	wraparoundQueries = []string{"pgtoolbox_settings", "pgtoolbox_xid", "pgtoolbox_database", "pgtoolbox_tables", "pgtoolbox_vacuum_progress", "pgtoolbox_prepared_xacts", "pgtoolbox_xmin"}
	statementQueries  = []string{"pgtoolbox_statements", "pgtoolbox_statements_info"}
	indexQueries      = []string{"pgtoolbox_indexes", "pgtoolbox_index_duplicates"}
	connectionQueries = []string{"pgtoolbox_settings", "pgtoolbox_backends", "pgtoolbox_connections"}
	blockingQueries   = []string{"pgtoolbox_blocking", "pgtoolbox_backends"}
)

// partial says whether the readings' panel is complete: a query behind
// it failed (on some database, or as a whole), the parser cut its rows,
// or the console cut the exposition.
func partial(readings []insight.InstanceReading, queries []string) viewmodel.InsightPartial {
	var reasons []string
	for _, r := range readings {
		if r.Reading.ExpositionCut() {
			reasons = append(reasons, r.Instance+"'s exporter response was cut at the console's size bound: the series after the cut are missing")
		}
		for _, q := range queries {
			if dbs := r.Reading.FailedDatabases[q]; len(dbs) > 0 {
				reasons = append(reasons, q+" failed on database "+strings.Join(dbs, ", ")+" on "+r.Instance+": its rows are missing")
			} else if r.Reading.QueryErrors[q] > 0 {
				reasons = append(reasons, q+" failed on "+r.Instance+": its rows are missing")
			}
			if slices.Contains(r.Reading.Truncated, q) {
				reasons = append(reasons, q+" on "+r.Instance+" was cut at the console's row bound")
			}
		}
	}
	if len(reasons) == 0 {
		return viewmodel.InsightPartial{}
	}
	return viewmodel.InsightPartial{Partial: true, PartialText: strings.Join(reasons, "; ") + " (see the exporter's log)."}
}

// dataAt is when the exporter measured a reading; the read time for a
// reading recorded without it.
func dataAt(r insight.InstanceReading) time.Time {
	if r.DataAt.IsZero() {
		return insight.DataTime(r.Reading, r.At)
	}
	return r.DataAt
}

// unsupported reports a reading of PostgreSQL 13 or earlier, where the
// pack runs its version query only.
func unsupported(r insight.Reading) bool {
	return r.ServerVersionNum != nil && *r.ServerVersionNum < 140000
}

// Build converts one Cluster's insight snapshot. cluster may be
// nil (the fleet view does not read it); has is false when exporters
// are not scraped; interval is the sweep cadence, which decides when a
// reading is stale.
func Build(cluster *cnpgv1.Cluster, snap insight.Snapshot, has bool, interval time.Duration, now time.Time) viewmodel.Insight {
	out := viewmodel.Insight{
		GeneratedAt: now,
		Wraparound:  viewmodel.InsightWraparound{Limits: []viewmodel.InsightSetting{}, Assumed: []string{}, Databases: []viewmodel.InsightDatabase{}, Tables: []viewmodel.InsightTable{}, Vacuums: []viewmodel.InsightVacuum{}, PreparedXacts: []viewmodel.InsightPreparedXacts{}},
		Statements: viewmodel.InsightStatements{
			Top: []viewmodel.InsightStatement{}, Regressions: []viewmodel.InsightRegression{},
			TextQuery: StatementTextQuery, NeedsSeconds: insight.RegressionNeeds.Seconds(), RegressionRatio: insight.RegressionRatio,
		},
		Indexes:     viewmodel.InsightIndexes{Unused: []viewmodel.InsightIndex{}, Invalid: []viewmodel.InsightIndex{}, Duplicate: []viewmodel.InsightIndex{}},
		Connections: viewmodel.InsightConnections{Instances: []viewmodel.InsightInstanceConnections{}},
		Blocking:    viewmodel.InsightBlocking{Instances: []viewmodel.InsightInstanceBlocking{}, MinRefreshSeconds: 1},
		Sources:     []viewmodel.SourceState{},
		BlindSpots:  BlindSpots("insight"),
	}
	var params map[string]string
	if cluster != nil {
		out.Ref = viewmodel.ClusterRef{Namespace: cluster.Namespace, Name: cluster.Name, UID: string(cluster.UID)}
		params = cluster.Spec.PostgresConfiguration.Parameters
	}
	out.Pack = packState(cluster, snap, has)
	if out.Pack.State == "notEnabled" {
		out.BlindSpots = append(out.BlindSpots, viewmodel.BlindSpot{
			ID: "insight.notEnabled", Panel: "insight",
			Text: "The insight pack is not enabled on this Cluster, so none of these panels has data. Opt in with " + insight.SpecChange + ".",
		})
	}
	fresh := 3 * interval
	if fresh < 2*time.Minute {
		fresh = 2 * time.Minute
	}
	// ObservedAt is when the exporter ran the queries: it answers from a
	// cache for metricsQueriesTTL, so that can be well before the read.
	prov := func(r insight.InstanceReading) viewmodel.Provenance {
		at := dataAt(r).UTC()
		p := viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "insight pack, " + r.Instance + ":9187/metrics", ObservedAt: &at}
		switch {
		case now.Sub(r.At) > fresh:
			p.Stale, p.StaleReason = true, "the exporter was last read "+now.Sub(r.At).Round(time.Second).String()+" ago"
		case now.Sub(at) > fresh:
			p.Stale, p.StaleReason = true, "the exporter's data is "+now.Sub(at).Round(time.Second).String()+" old (its metricsQueriesTTL cache)"
		}
		return p
	}
	readings := presentReadings(snap)
	primary, hasPrimary := snap.PrimaryReading()
	hasPrimary = hasPrimary && primary.Reading.Present()
	noPrimary := "No reading of the primary's exporter reports the pack."
	if !has || len(readings) == 0 {
		noPrimary = out.Pack.Text
	}
	if hasPrimary && unsupported(primary.Reading) {
		hasPrimary, noPrimary = false, out.Pack.Text
	}
	var primaryOnly []insight.InstanceReading
	if hasPrimary {
		primaryOnly = []insight.InstanceReading{primary}
	}

	// Wraparound and vacuum.
	w := &out.Wraparound
	switch {
	case !hasPrimary:
		w.Text = noPrimary
	case len(primary.Reading.Databases) == 0:
		w.Text = "The primary's exporter reports no database ages: the pack's database query failed (see the query errors), or the exposition was cut before it."
	default:
		w.Available = true
	}
	w.InsightPartial = partial(primaryOnly, wraparoundQueries)
	if hasPrimary {
		buildWraparound(w, primary, snap, params)
		p := prov(primary)
		w.Provenance = &p
	}

	// Statements.
	st := &out.Statements
	switch {
	case !hasPrimary:
		st.Text = noPrimary
	case len(primary.Reading.Statements) == 0:
		st.Text = "The primary reports no statements: pg_stat_statements is not in shared_preload_libraries, " +
			"not created in the public schema of the database the exporter connects to, older than 1.9, or the query failed. " +
			"CloudNativePG creates the extension when a pg_stat_statements.* parameter is set in the Cluster spec."
	default:
		st.Available = true
	}
	st.InsightPartial = partial(primaryOnly, statementQueries)
	if hasPrimary {
		buildStatements(st, primary, snap)
		p := prov(primary)
		st.Provenance = &p
	} else {
		st.ComparisonText = st.Text
	}

	// Indexes.
	ix := &out.Indexes
	switch {
	case !hasPrimary:
		ix.Text = noPrimary
	case len(primary.Reading.Indexes) == 0 && primary.Reading.QueryErrors["pgtoolbox_indexes"] > 0 && primary.Reading.QueryErrors["pgtoolbox_index_duplicates"] > 0:
		ix.Text = "The pack's index queries failed on the primary (see the query errors)."
	default:
		ix.Available = true
	}
	ix.InsightPartial = partial(primaryOnly, indexQueries)
	if hasPrimary {
		buildIndexes(ix, primary)
		p := prov(primary)
		ix.Provenance = &p
	}

	// Connections and blocking, every instance.
	var supported []insight.InstanceReading
	for _, r := range readings {
		if !unsupported(r.Reading) {
			supported = append(supported, r)
		}
	}
	out.Connections.InsightPartial = partial(supported, connectionQueries)
	out.Blocking.InsightPartial = partial(supported, blockingQueries)
	for _, r := range supported {
		out.Connections.Instances = append(out.Connections.Instances, buildConnections(r, prov(r)))
		b := viewmodel.InsightInstanceBlocking{
			Instance: r.Instance, Primary: r.Primary, Roots: []viewmodel.InsightBlockNode{}, Provenance: prov(r),
			DataAgeSeconds: math.Max(0, r.At.Sub(dataAt(r)).Seconds()),
		}
		if r.Reading.Backends != nil {
			b.Waiting = r.Reading.Backends.LockWaiting
		}
		for _, n := range insight.BlockingTree(r.Reading.Sessions) {
			b.Roots = append(b.Roots, blockNode(n))
		}
		out.Blocking.Instances = append(out.Blocking.Instances, b)
	}
	out.Connections.Available = len(out.Connections.Instances) > 0
	out.Blocking.Available = len(out.Blocking.Instances) > 0
	if !out.Connections.Available {
		out.Connections.Text, out.Blocking.Text = out.Pack.Text, out.Pack.Text
	}
	return out
}

// presentReadings are the instances reporting the pack, primary first.
func presentReadings(snap insight.Snapshot) []insight.InstanceReading {
	var out []insight.InstanceReading
	for _, r := range snap.Instances {
		if r.Reading.Present() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Primary != out[j].Primary {
			return out[i].Primary
		}
		return out[i].Instance < out[j].Instance
	})
	return out
}

// packState is the detection verdict.
func packState(cluster *cnpgv1.Cluster, snap insight.Snapshot, has bool) viewmodel.InsightPack {
	p := viewmodel.InsightPack{
		Expected: strconv.Itoa(insight.PackVersion), ConfigMap: insight.ConfigMapName, Key: insight.ConfigMapKey,
		SpecChange: insight.SpecChange, Instances: []viewmodel.InsightPackInstance{}, QueryErrors: []viewmodel.InsightQueryError{},
	}
	if cluster != nil {
		referenced := false
		if m := cluster.Spec.Monitoring; m != nil {
			for _, c := range m.CustomQueriesConfigMap {
				if c.Name == insight.ConfigMapName && c.Key == insight.ConfigMapKey {
					referenced = true
				}
			}
		}
		p.Referenced = &referenced
	}
	if !has {
		p.State, p.Health = "metricsOff", viewmodel.HealthInactive
		p.Text = "The console does not scrape the instances' exporters (METRICS_ENABLED is false), so it cannot read the insight pack."
		return p
	}
	names := make([]string, 0, len(snap.Instances))
	for name := range snap.Instances {
		names = append(names, name)
	}
	sort.Strings(names)
	older, newer, present, cut, unknown, old := false, false, 0, false, false, 0
	for _, name := range names {
		r := snap.Instances[name]
		row := viewmodel.InsightPackInstance{Instance: name, Primary: r.Primary, Version: r.Reading.Version, Truncated: r.Reading.Truncated, ServerVersionNum: r.Reading.ServerVersionNum}
		at := r.At.UTC()
		row.Provenance = viewmodel.Provenance{Origin: viewmodel.OriginInstance, Source: "cnpg_pgtoolbox_pack_info, " + name + ":9187/metrics", ObservedAt: &at}
		if t := r.Reading.ExporterUpdatedAt; t != nil {
			u := t.UTC()
			row.ExporterUpdatedAt = &u
		}
		p.Instances = append(p.Instances, row)
		for _, q := range insight.Queries {
			if c, ok := r.Reading.QueryErrors[q]; ok && c > 0 {
				p.QueryErrors = append(p.QueryErrors, viewmodel.InsightQueryError{Instance: name, Query: q, Count: c, Databases: r.Reading.FailedDatabases[q]})
			}
		}
		if r.Reading.ExpositionCut() {
			cut = true
		}
		if !r.Reading.Present() {
			continue
		}
		present++
		if unsupported(r.Reading) {
			old++
		}
		if r.Reading.Version == "" {
			unknown = true
			continue
		}
		v, err := strconv.Atoi(r.Reading.Version)
		switch {
		case err != nil || v > insight.PackVersion:
			newer = true
		case v < insight.PackVersion:
			older = true
		}
	}
	switch {
	case len(names) == 0:
		p.State, p.Health = "noData", viewmodel.HealthUnknown
		p.Text = "No instance's exporter has been read yet."
	case present == 0 && cut:
		// A cut exposition can lose every pack series: never read that
		// as "not enabled".
		p.State, p.Health = "truncated", viewmodel.HealthWarning
		p.Text = "The console cut an exporter's response at its size bound before the pack's series, so whether the pack runs is not known: " +
			"the instance exports more than the console reads (many databases or custom queries)."
	case present == 0 && p.Referenced != nil && *p.Referenced:
		p.State, p.Health = "notEnabled", viewmodel.HealthWarning
		p.Text = "The Cluster lists the pack's ConfigMap but no instance reports it: the ConfigMap " + insight.ConfigMapName +
			" may be missing from this namespace, or its key is not " + insight.ConfigMapKey + "."
	case present == 0:
		p.State, p.Health = "notEnabled", viewmodel.HealthInfo
		p.Text = "The insight pack is not enabled on this Cluster. Opt in with " + insight.SpecChange + "."
	case old == present:
		p.State, p.Health = "unsupported", viewmodel.HealthInfo
		p.Text = "The instances run PostgreSQL 13 or earlier: the insight pack supports PostgreSQL 14 and later, and reports only its version here."
	case older:
		p.State, p.Health = "outdated", viewmodel.HealthWarning
		p.Text = "An instance reports an older insight pack than this console reads (version " + p.Expected +
			"): upgrade the pack's ConfigMap; what the older pack reports is still shown."
	case newer:
		p.State, p.Health = "newer", viewmodel.HealthInfo
		p.Text = "An instance reports a newer insight pack than this console knows (version " + p.Expected +
			"): upgrade the console to read all of it."
	case unknown && cut:
		p.State, p.Health = "truncated", viewmodel.HealthWarning
		p.Text = "The console cut an exporter's response at its size bound, past some of the pack's series but before its version: what was read is shown, the rest is missing."
	case unknown:
		p.State, p.Health = "unknown", viewmodel.HealthWarning
		p.Text = "An instance reports the pack's series without its version (cnpg_pgtoolbox_pack_info): what it reports is shown, read as version " + p.Expected + "."
	default:
		p.State, p.Health = "enabled", viewmodel.HealthOK
		p.Text = "The insight pack version " + p.Expected + " is enabled."
	}
	if cut && p.State != "truncated" && present > 0 {
		p.Health = worseHealth(p.Health, viewmodel.HealthWarning)
		p.Text += " An exporter's response was cut at the console's size bound: the panels drawn from what was lost say so."
	}
	if len(p.QueryErrors) > 0 && p.Health == viewmodel.HealthOK {
		p.Health = viewmodel.HealthWarning
		p.Text += " Some of its queries fail on the exporter: see the query errors and the instance's log."
	}
	return p
}

// BlindSpots are what the insight screens cannot see, for one panel.
func BlindSpots(panel string) []viewmodel.BlindSpot {
	out := []viewmodel.BlindSpot{
		{ID: "insight.primaryOnly", Panel: panel, Text: "Wraparound, tables, indexes and statements are read on the primary only: an index read as unused counts primary scans, and the replicas' pg_stat_statements are not shown."},
		{ID: "insight.noStatementText", Panel: panel, Text: "Statement text never leaves the database: the console shows the queryid, and pgAdmin shows the text."},
		{ID: "insight.inMemoryHistory", Panel: panel, Text: "Consumption rates, autovacuum verdicts and regressions rest on samples the console keeps in memory: they start again when the console restarts."},
	}
	out = append(out, viewmodel.BlindSpot{ID: "insight.topStatementsOnly", Panel: panel, Text: "The console keeps each hour's costliest statements only: a statement that entered them lately has no baseline a day earlier and is not compared."})
	if panel == "insight" {
		out = append(out,
			viewmodel.BlindSpot{ID: "insight.exporterCache", Panel: panel, Text: "The exporter caches its custom queries for the Cluster's metricsQueriesTTL (30 s by default): a refreshed lock graph can be that old."},
			viewmodel.BlindSpot{ID: "insight.multixactMembers", Panel: panel, Text: "Multixact member space is not read: it can run out before multixact ids do, and autovacuum then freezes multixacts earlier than autovacuum_multixact_freeze_max_age says."},
			viewmodel.BlindSpot{ID: "insight.dynamicFreezeAge", Panel: panel, Text: "The forecasts use the configured freeze ages: PostgreSQL lowers the effective multixact freeze age on its own when member space runs short, which they do not show."},
			viewmodel.BlindSpot{ID: "insight.countersReset", Panel: panel, Text: "A failover or a crash recovery resets the scan and vacuum counters without setting stats_reset: an index read as unused may have been scanned before it."},
			viewmodel.BlindSpot{ID: "insight.fkIndexScans", Panel: panel, Text: "An index that serves a foreign key's checks often shows no scans: it is used when referenced rows are deleted or updated, which may be rare and still need it."},
		)
	}
	return out
}

func buildWraparound(w *viewmodel.InsightWraparound, r insight.InstanceReading, snap insight.Snapshot, params map[string]string) {
	w.Instance = r.Instance
	limits := insight.LimitsFrom(r.Reading.Settings, params)
	for _, l := range []struct {
		name  string
		value float64
	}{
		{"autovacuum_freeze_max_age", limits.FreezeMaxAge},
		{"vacuum_failsafe_age", limits.FailsafeAge},
		{"autovacuum_multixact_freeze_max_age", limits.MultixactFreezeMaxAge},
		{"vacuum_multixact_failsafe_age", limits.MultixactFailsafeAge},
	} {
		w.Limits = append(w.Limits, viewmodel.InsightSetting{Name: l.name, Value: l.value, Source: limits.Source[l.name]})
	}
	w.Assumed = append(w.Assumed, limits.Assumed()...)
	var xidRate, mxidRate *float64
	if snap.XIDRate != nil {
		w.XIDRate, xidRate = rate(*snap.XIDRate), &snap.XIDRate.PerSecond
	}
	if snap.MXIDRate != nil {
		w.MXIDRate, mxidRate = rate(*snap.MXIDRate), &snap.MXIDRate.PerSecond
	}
	for _, db := range r.Reading.Databases {
		row := viewmodel.InsightDatabase{Name: db.Name, AllowConnections: boolOf(db.AllowConnections), StatsResetAt: unix(db.StatsResetUnix)}
		if db.XIDAge != nil {
			row.XID = forecast(insight.ForecastXID(*db.XIDAge, limits, 0, xidRate), limits.FreezeMaxAge)
		}
		if db.MXIDAge != nil {
			row.MXID = forecast(insight.ForecastMXID(*db.MXIDAge, limits, mxidRate), limits.MultixactFreezeMaxAge)
		}
		w.Databases = append(w.Databases, row)
	}
	sort.SliceStable(w.Databases, func(i, j int) bool { return forecastAge(w.Databases[i].XID) > forecastAge(w.Databases[j].XID) })
	for _, t := range r.Reading.Tables {
		row := viewmodel.InsightTable{
			Database: t.Database, Schema: t.Schema, Name: t.Name, Kind: t.Kind,
			ParentSchema: t.ParentSchema, ParentTable: t.ParentTable, Reltuples: t.Reltuples,
			MXIDAge: t.MXIDAge, LiveTuples: t.LiveTuples, DeadTuples: t.DeadTuples, AutovacuumThreshold: t.AutovacuumThreshold,
			SinceVacuumSeconds: t.SinceVacuum, SinceAutovacuumSeconds: t.SinceAutovacuum, SinceAnalyzeSeconds: t.SinceAnalyze,
			AutovacuumEnabled: boolOf(t.AutovacuumEnabled), KeepUp: string(insight.KeepUpUnknown), Health: viewmodel.HealthOK,
		}
		if t.XIDAge != nil {
			freeze := limits.FreezeMaxAge
			if t.FreezeMaxAge != nil && *t.FreezeMaxAge > 0 && *t.FreezeMaxAge < freeze {
				freeze = *t.FreezeMaxAge
			}
			row.XID = forecast(insight.ForecastXID(*t.XIDAge, limits, freeze, xidRate), freeze)
			row.Health = row.XID.Health
		}
		if tr, ok := snap.Trends[t.Key()]; ok {
			row.KeepUp = string(tr.Verdict)
		}
		row.KeepUpText = keepUpText(insight.KeepUp(row.KeepUp))
		switch insight.KeepUp(row.KeepUp) {
		case insight.KeepUpNotStarted, insight.KeepUpFallingBehind, insight.KeepUpDisabled:
			row.Health = worseHealth(row.Health, viewmodel.HealthWarning)
		case insight.KeepUpUnknown, insight.KeepUpOK, insight.KeepUpNever:
			// Not judged yet, keeping up, or never autovacuumed by design
			// (a temporary table): no warning.
		}
		w.Tables = append(w.Tables, row)
	}
	for _, x := range r.Reading.PreparedXacts {
		w.PreparedXacts = append(w.PreparedXacts, viewmodel.InsightPreparedXacts{Database: x.Database, Count: x.Count, OldestSeconds: x.OldestSeconds, OldestXIDAge: x.OldestXIDAge})
	}
	sort.SliceStable(w.PreparedXacts, func(i, j int) bool {
		return val(w.PreparedXacts[i].OldestXIDAge) > val(w.PreparedXacts[j].OldestXIDAge)
	})
	w.Horizons = horizons(r.Reading)
	for _, v := range r.Reading.Vacuums {
		w.Vacuums = append(w.Vacuums, viewmodel.InsightVacuum{
			Database: v.Database, Schema: v.Schema, Table: v.Table, Phase: v.Phase,
			Autovacuum: boolOf(v.Autovacuum), Wraparound: boolOf(v.Wraparound), RunningSeconds: v.RunningSeconds,
			HeapBlksTotal: v.HeapBlksTotal, HeapBlksScanned: v.HeapBlksScanned, HeapBlksVacuumed: v.HeapBlksVacuumed,
			IndexVacuumCount: v.IndexVacuumCount,
		})
	}
}

// horizons converts pgtoolbox_xmin and the oldest prepared transaction;
// Holder is the oldest of them.
func horizons(r insight.Reading) *viewmodel.InsightHorizons {
	var out viewmodel.InsightHorizons
	if h := r.Horizons; h != nil {
		out.SlotXminAge, out.SlotCatalogXminAge, out.BackendXminAge, out.BackendXIDAge = h.SlotXminAge, h.SlotCatalogXminAge, h.BackendXminAge, h.BackendXIDAge
	}
	for _, x := range r.PreparedXacts {
		if x.OldestXIDAge != nil && (out.PreparedXIDAge == nil || *x.OldestXIDAge > *out.PreparedXIDAge) {
			v := *x.OldestXIDAge
			out.PreparedXIDAge = &v
		}
	}
	if r.Horizons == nil && out.PreparedXIDAge == nil {
		return nil
	}
	oldest := -1.0
	for _, c := range []struct {
		holder string
		age    *float64
	}{
		{"slot", out.SlotXminAge},
		{"catalogSlot", out.SlotCatalogXminAge},
		{"backend", out.BackendXminAge},
		{"transaction", out.BackendXIDAge},
		{"prepared", out.PreparedXIDAge},
	} {
		if c.age != nil && *c.age > oldest {
			oldest, out.Holder = *c.age, c.holder
		}
	}
	return &out
}

func keepUpText(k insight.KeepUp) string {
	switch k {
	case insight.KeepUpOK:
		return "Dead rows are not growing, or stay under the autovacuum threshold."
	case insight.KeepUpNotStarted:
		return "Past its autovacuum threshold with dead rows growing, and no autovacuum completed in the window: the workers are busy elsewhere, or each run is cancelled by a conflicting lock."
	case insight.KeepUpFallingBehind:
		return "Autovacuum ran in the window, and dead rows are still higher than at its start."
	case insight.KeepUpDisabled:
		return "autovacuum_enabled is off on this table and it is past its threshold: only an anti-wraparound autovacuum will visit it."
	case insight.KeepUpNever:
		return "A temporary table: autovacuum never processes it, only the session that owns it can vacuum it."
	default: // insight.KeepUpUnknown
		return "Not enough samples yet: the console needs three over a quarter of an hour."
	}
}

func buildStatements(st *viewmodel.InsightStatements, r insight.InstanceReading, snap insight.Snapshot) {
	st.Instance = r.Instance
	for _, s := range r.Reading.Statements {
		row := viewmodel.InsightStatement{
			QueryID: s.QueryID, Database: s.Database, Role: s.Role, Calls: s.Calls, TotalMillis: s.TotalMillis,
			MeanMillis: s.MeanMillis, Rows: s.Rows, SharedBlksHit: s.SharedBlksHit, SharedBlksRead: s.SharedBlksRead,
		}
		if s.SharedBlksHit != nil && s.SharedBlksRead != nil && *s.SharedBlksHit+*s.SharedBlksRead > 0 {
			h := *s.SharedBlksHit / (*s.SharedBlksHit + *s.SharedBlksRead)
			row.HitRatio = &h
		}
		st.Top = append(st.Top, row)
	}
	sort.SliceStable(st.Top, func(i, j int) bool { return val(st.Top[i].TotalMillis) > val(st.Top[j].TotalMillis) })
	if info := r.Reading.StatementsInfo; info != nil {
		st.Deallocations, st.MaxStatements, st.StatsResetAt = info.Dealloc, info.Max, unix(info.StatsResetUnix)
	}
	cmp := snap.Statements
	st.Comparable, st.CoveredSeconds = cmp.Comparable, cmp.Covered.Seconds()
	st.NoBaseline, st.Restarted, st.Evicted = cmp.NoBaseline, cmp.Restarted, cmp.Evicted
	if cmp.Comparable {
		st.ComparisonText = fmt.Sprintf("%d statements compared: the last hour against the same hour a day earlier, on the same primary.", cmp.Compared)
		if cmp.NoBaseline > 0 {
			st.ComparisonText += fmt.Sprintf(" %d were not among the costliest a day earlier and have no baseline.", cmp.NoBaseline)
		}
		if cmp.Restarted > 0 {
			st.ComparisonText += fmt.Sprintf(" %d had their counters restart within a window and are not compared.", cmp.Restarted)
		}
		if cmp.Evicted != nil && *cmp.Evicted > 0 {
			st.ComparisonText += fmt.Sprintf(" pg_stat_statements evicted %.0f entries meanwhile: the regressions marked uncertain may have restarted.", *cmp.Evicted)
		}
	} else {
		st.ComparisonText = "No regression comparison yet: " + cmp.Reason + "."
	}
	for _, g := range cmp.Regressions {
		st.Regressions = append(st.Regressions, viewmodel.InsightRegression{
			QueryID: g.QueryID, Database: g.Database, Role: g.Role, Ratio: g.Ratio(),
			RecentMean: g.RecentMean, BaselineMean: g.BaselineMean, RecentCalls: g.RecentCalls, BaselineCalls: g.BaselineCalls,
			RecentFrom: g.RecentFrom.UTC(), RecentTo: g.RecentTo.UTC(), BaselineFrom: g.BaselineFrom.UTC(), BaselineTo: g.BaselineTo.UTC(),
			Uncertain: g.Uncertain,
		})
	}
}

func buildIndexes(ix *viewmodel.InsightIndexes, r insight.InstanceReading) {
	ix.Instance = r.Instance
	resets := map[string]*time.Time{}
	for _, db := range r.Reading.Databases {
		resets[db.Name] = unix(db.StatsResetUnix)
	}
	for _, i := range r.Reading.Indexes {
		row := viewmodel.InsightIndex{
			Database: i.Database, Schema: i.Schema, Table: i.Table, Name: i.Name, DuplicateOf: i.DuplicateOf,
			SizeBytes: i.SizeBytes, Scans: i.Scans, Unique: boolOf(i.Unique),
			BacksConstraint: boolOf(i.BacksConstraint), Droppable: boolOf(i.Droppable), FKSupport: boolOf(i.FKSupport),
			StatsResetAt: resets[i.Database],
		}
		switch i.Kind {
		case "unused":
			ix.Unused = append(ix.Unused, row)
		case "invalid":
			ix.Invalid = append(ix.Invalid, row)
		case "duplicate":
			ix.Duplicate = append(ix.Duplicate, row)
		}
	}
	for _, list := range [][]viewmodel.InsightIndex{ix.Unused, ix.Invalid, ix.Duplicate} {
		sort.SliceStable(list, func(i, j int) bool { return val(list[i].SizeBytes) > val(list[j].SizeBytes) })
	}
	for _, db := range r.Reading.Databases {
		if t := unix(db.StatsResetUnix); t != nil && (ix.StatsResetAt == nil || t.Before(*ix.StatsResetAt)) {
			ix.StatsResetAt = t
		}
	}
}

func buildConnections(r insight.InstanceReading, p viewmodel.Provenance) viewmodel.InsightInstanceConnections {
	c := viewmodel.InsightInstanceConnections{Instance: r.Instance, Primary: r.Primary, Groups: []viewmodel.InsightConnectionGroup{}, Health: viewmodel.HealthUnknown, Provenance: p}
	setting := func(name string) *float64 {
		if v, ok := r.Reading.Settings[name]; ok {
			return &v
		}
		return nil
	}
	c.MaxConnections = setting("max_connections")
	if a, b := setting("superuser_reserved_connections"), setting("reserved_connections"); a != nil || b != nil {
		v := val(a) + val(b)
		c.Reserved = &v
	}
	if t := setting("idle_in_transaction_session_timeout"); t != nil {
		v := *t / 1000 // the setting is in milliseconds
		c.IdleTimeoutSeconds = &v
	}
	if b := r.Reading.Backends; b != nil {
		c.Client, c.Active, c.IdleInTransaction = b.Client, b.Active, b.IdleInTransaction
		c.IdleInTransactionMaxSeconds, c.LockWaiting, c.MaxXactAgeSeconds = b.IdleInTransactionMaxSeconds, b.LockWaiting, b.MaxXactAgeSeconds
	}
	if c.MaxConnections != nil {
		u := *c.MaxConnections - val(c.Reserved)
		c.Usable = &u
	}
	if c.Client != nil && c.Usable != nil && *c.Usable > 0 {
		share := *c.Client / *c.Usable
		c.Share = &share
		switch {
		case share > 0.95:
			c.Health = viewmodel.HealthCritical
		case share > 0.80:
			c.Health = viewmodel.HealthWarning
		default:
			c.Health = viewmodel.HealthOK
		}
	}
	for _, g := range r.Reading.Connections {
		c.Groups = append(c.Groups, viewmodel.InsightConnectionGroup{
			Database: g.Database, Application: g.Application, State: g.State, Sessions: g.Sessions, MaxStateAgeSeconds: g.MaxStateAgeSeconds,
		})
	}
	sort.SliceStable(c.Groups, func(i, j int) bool { return val(c.Groups[i].Sessions) > val(c.Groups[j].Sessions) })
	return c
}

func blockNode(n insight.BlockNode) viewmodel.InsightBlockNode {
	s := n.Session
	out := viewmodel.InsightBlockNode{
		PID: s.PID, Prepared: s.Prepared() && !n.Missing, Missing: n.Missing, Cycle: n.Cycle, Database: s.Database, User: s.User, Application: s.Application,
		State: s.State, WaitEventType: s.WaitEventType, WaitEvent: s.WaitEvent, BackendType: s.BackendType, QueryID: s.QueryID,
		WaitSeconds: s.WaitSeconds, XactAgeSeconds: s.XactAgeSeconds, StateAgeSeconds: s.StateAgeSeconds,
		Blocked: n.Blocked, Waiters: []viewmodel.InsightBlockNode{},
	}
	for _, w := range n.Waiters {
		out.Waiters = append(out.Waiters, blockNode(w))
	}
	return out
}

// forecast converts a forecast; freeze is the anti-wraparound threshold
// in force, which decides when an age is overdue rather than due.
func forecast(f insight.Forecast, freeze float64) *viewmodel.InsightForecast {
	h := func(h insight.Horizon) viewmodel.InsightHorizon {
		out := viewmodel.InsightHorizon{Threshold: h.Threshold, Remaining: h.Remaining, Past: h.Past}
		if h.In != nil {
			s := h.In.Seconds()
			out.InSeconds = &s
		}
		return out
	}
	out := &viewmodel.InsightForecast{Age: f.Age, Autovacuum: h(f.Autovacuum), Failsafe: h(f.Failsafe), Stop: h(f.Stop), Health: viewmodel.HealthOK}
	within := func(d time.Duration) bool { return f.Stop.In != nil && *f.Stop.In < d }
	switch {
	case f.Age >= insight.WarnLimitAge || f.Stop.Past || within(24*time.Hour) || f.Failsafe.Past || within(7*24*time.Hour):
		out.Health = viewmodel.HealthCritical
	case f.Age > 1.5*freeze:
		out.Health = viewmodel.HealthWarning
	case f.Autovacuum.Past:
		// Due: anti-wraparound autovacuum is expected to be on it.
		out.Health = viewmodel.HealthProgressing
	}
	return out
}

func forecastAge(f *viewmodel.InsightForecast) float64 {
	if f == nil {
		return -1
	}
	return f.Age
}

func rate(r insight.Rate) *viewmodel.InsightRate {
	return &viewmodel.InsightRate{PerSecond: r.PerSecond, WindowSeconds: r.Window.Seconds(), From: r.From.UTC(), To: r.To.UTC()}
}

func boolOf(v *float64) *bool {
	if v == nil {
		return nil
	}
	b := *v != 0
	return &b
}

func unix(v *float64) *time.Time {
	if v == nil || *v <= 0 || math.IsInf(*v, 0) {
		return nil
	}
	t := time.Unix(int64(*v), 0).UTC()
	return &t
}

func val(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

// worseHealth is the more severe of two health values.
func worseHealth(a, b viewmodel.Health) viewmodel.Health {
	rank := map[viewmodel.Health]int{viewmodel.HealthOK: 0, viewmodel.HealthProgressing: 1, viewmodel.HealthWarning: 2, viewmodel.HealthCritical: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// FleetCluster is one Cluster's input to the fleet view.
type FleetCluster struct {
	Ref      viewmodel.ClusterRef
	Snapshot insight.Snapshot
	Has      bool
	Interval time.Duration
}

// Fleet is the team-wide query insight of the given Clusters: the
// caller passes only those the user may see, so the cut to
// FleetTopStatements is taken over the visible statements.
func Fleet(clusters []FleetCluster, now time.Time) viewmodel.FleetStatements {
	out := viewmodel.FleetStatements{
		GeneratedAt: now, Clusters: []viewmodel.FleetClusterStatement{},
		Regressions: []viewmodel.FleetRegression{}, Top: []viewmodel.FleetStatement{},
		BlindSpots: BlindSpots("fleet"),
	}
	for _, c := range clusters {
		v := Build(nil, c.Snapshot, c.Has, c.Interval, now)
		out.Clusters = append(out.Clusters, viewmodel.FleetClusterStatement{
			Ref: c.Ref, State: v.Pack.State, Statements: v.Statements.Available,
			Comparable: v.Statements.Comparable, ComparisonText: v.Statements.ComparisonText,
			CoveredSeconds: v.Statements.CoveredSeconds,
		})
		for _, r := range v.Statements.Regressions {
			out.Regressions = append(out.Regressions, viewmodel.FleetRegression{Ref: c.Ref, InsightRegression: r})
		}
		for _, st := range v.Statements.Top {
			out.Top = append(out.Top, viewmodel.FleetStatement{Ref: c.Ref, InsightStatement: st})
		}
	}
	sort.SliceStable(out.Regressions, func(i, j int) bool { return out.Regressions[i].Ratio > out.Regressions[j].Ratio })
	sort.SliceStable(out.Top, func(i, j int) bool { return val(out.Top[i].TotalMillis) > val(out.Top[j].TotalMillis) })
	out.TopTotal = len(out.Top)
	if len(out.Top) > FleetTopStatements {
		out.Top, out.TopTruncated = out.Top[:FleetTopStatements], true
	}
	return out
}
