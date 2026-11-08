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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testdata/pg18-primary.prom and pg14-primary.prom are real exporter
// output: CloudNativePG 1.30.0's own metrics package (its QueryRunner,
// mappings and client_golang encoding, so label order, NaN and the error
// counters are the exporter's) running the pack and CNPG's default
// queries as a pg_monitor role on PostgreSQL 18.6 and 14.24, against the
// scenario in testdata/README.md: a lock tree behind an idle transaction
// and a prepared transaction, tables with known autovacuum thresholds,
// every duplicate-index case of the review, and (18 only) a catalog lock
// in billing that made two pack queries fail there on lock_timeout.
func readFixture(t *testing.T, name string) Reading {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func tableNamed(r Reading, db, name string) *Table {
	for i, tb := range r.Tables {
		if tb.Database == db && tb.Name == name {
			return &r.Tables[i]
		}
	}
	return nil
}

func indexRow(r Reading, name, kind string) *Index {
	for i, ix := range r.Indexes {
		if ix.Name == name && ix.Kind == kind {
			return &r.Indexes[i]
		}
	}
	return nil
}

func TestParseRealExposition(t *testing.T) {
	r := readFixture(t, "pg18-primary.prom")
	if !r.Present() || r.Version != "1" || r.ServerVersionNum == nil || *r.ServerVersionNum < 180000 {
		t.Fatalf("version = %q, server %v", r.Version, r.ServerVersionNum)
	}
	for name, want := range map[string]float64{
		"autovacuum_freeze_max_age": 2e8, "max_connections": 100, "superuser_reserved_connections": 3,
		"reserved_connections": 0, "autovacuum_vacuum_max_threshold": 1e8,
	} {
		if got, ok := r.Settings[name]; !ok || got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if r.NextXID == nil || *r.NextXID <= 0 {
		t.Errorf("next xid = %v", r.NextXID)
	}
	var template0 bool
	for _, d := range r.Databases {
		if d.Name == "template0" {
			template0 = d.AllowConnections != nil && *d.AllowConnections == 0
		}
	}
	if !template0 {
		t.Error("template0 must be reported with allow_connections 0: it ages like any database")
	}
	if r.ExporterUpdatedAt == nil {
		t.Error("cnpg_last_update_timestamp not read")
	}
	if len(r.Truncated) != 0 {
		t.Errorf("truncated = %v", r.Truncated)
	}
}

// TestFixtureAutovacuumThreshold is the repro of the OPERATOR() precedence
// bug: 10,000 analyzed rows, scale 0.2, base 50. The first pack computed
// (50 + 0.2) x 10,000 = 502,000.
// TestFixturesCarryNoGID: a prepared transaction's gid is application
// data; the pack reports counts and ages only.
func TestFixturesCarryNoGID(t *testing.T) {
	for _, name := range []string{"pg18-primary.prom", "pg14-primary.prom"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "fixture-secret-gid") {
			t.Errorf("%s exports the prepared transaction's gid", name)
		}
	}
}

func TestFixtureAutovacuumThreshold(t *testing.T) {
	for file, capped := range map[string]float64{"pg18-primary.prom": 500, "pg14-primary.prom": 2050} {
		t.Run(file, func(t *testing.T) {
			r := readFixture(t, file)
			accounts := tableNamed(r, "app", "accounts")
			if accounts == nil || accounts.AutovacuumThreshold == nil || *accounts.AutovacuumThreshold != 2050 ||
				accounts.Reltuples == nil || *accounts.Reltuples != 10000 {
				t.Fatalf("accounts = %+v, want threshold 50 + 0.2 x 10000", accounts)
			}
			// PostgreSQL 18's vacuum_max_threshold (here the table's own
			// 500) caps it; 14 has no cap.
			if c := tableNamed(r, "app", "capped"); c == nil || c.AutovacuumThreshold == nil || *c.AutovacuumThreshold != capped {
				t.Fatalf("capped = %+v, want %v", c, capped)
			}
			// Never analyzed: reltuples is -1, the threshold unknown.
			if fresh := tableNamed(r, "app", "fresh"); fresh == nil || fresh.Reltuples != nil || fresh.AutovacuumThreshold != nil ||
				fresh.DeadTuples == nil {
				t.Fatalf("fresh = %+v, want reltuples and threshold unknown", fresh)
			}
			if accounts.SinceAutovacuum != nil {
				t.Error("a never-autovacuumed table must read as not reported (NaN), not zero")
			}
			var toast *Table
			for i, tb := range r.Tables {
				if tb.Kind == "toast" && tb.Database == "app" {
					toast = &r.Tables[i]
				}
			}
			if toast == nil || toast.ParentSchema != "public" || toast.ParentTable != "documents" {
				t.Fatalf("toast = %+v, want its parent public.documents", toast)
			}
		})
	}
}

// TestFixtureBlocking: an idle transaction blocks an insert, a LOCK TABLE
// queues behind both, and an update waits on a prepared transaction,
// which pg_blocking_pids names 0.
func TestFixtureBlocking(t *testing.T) {
	for _, file := range []string{"pg18-primary.prom", "pg14-primary.prom"} {
		t.Run(file, func(t *testing.T) {
			r := readFixture(t, file)
			tree := BlockingTree(r.Sessions)
			if len(tree) != 2 {
				t.Fatalf("roots = %+v", tree)
			}
			idle, prepared := tree[0], tree[1]
			if idle.Session.Application != "billing-batch" || idle.Session.State != "idle in transaction" ||
				idle.Session.WaitSeconds != nil || idle.Blocked != 3 {
				t.Fatalf("idle root = %+v", idle)
			}
			if !prepared.Session.Prepared() || prepared.Missing || prepared.Session.BackendType != "prepared transaction" ||
				prepared.Blocked != 1 || prepared.Waiters[0].Session.Application != "psql" {
				t.Fatalf("prepared root = %+v", prepared)
			}
			if w := prepared.Waiters[0].Session; w.WaitEventType != "Lock" || w.WaitSeconds == nil {
				t.Fatalf("waiter on the prepared transaction = %+v", w)
			}
			if len(r.PreparedXacts) != 1 || r.PreparedXacts[0].Database != "app" || *r.PreparedXacts[0].Count != 1 ||
				r.PreparedXacts[0].OldestSeconds == nil || r.PreparedXacts[0].OldestXIDAge == nil {
				t.Fatalf("prepared xacts = %+v", r.PreparedXacts)
			}
			// The exporter's own backend is in client (it holds a slot) and
			// nowhere else.
			b := r.Backends
			if b == nil || *b.LockWaiting != 3 || *b.Active != 3 || *b.IdleInTransaction != 1 && *b.IdleInTransaction != 2 {
				t.Fatalf("backends = %+v", b)
			}
			for _, g := range r.Connections {
				if g.Application == "cnpg_metrics_exporter" && g.State == "active" {
					t.Errorf("the exporter's own backend counted: %+v", g)
				}
			}
			// The logical slot holds a catalog_xmin; no slot holds an xmin.
			if h := r.Horizons; h == nil || h.SlotCatalogXminAge == nil || h.SlotXminAge != nil || h.BackendXminAge == nil {
				t.Fatalf("horizons = %+v", h)
			}
		})
	}
}

// TestFixtureIndexes is the repro of the duplicate-index review: never
// propose dropping what backs a constraint, compare everything that makes
// two indexes different, keep the constraint's index.
func TestFixtureIndexes(t *testing.T) {
	for _, file := range []string{"pg18-primary.prom", "pg14-primary.prom"} {
		t.Run(file, func(t *testing.T) {
			r := readFixture(t, file)
			dups := map[string]Index{}
			for _, ix := range r.Indexes {
				if ix.Kind == "duplicate" {
					dups[ix.Name] = ix
				}
			}
			want := map[string]struct {
				keep      string
				droppable float64
			}{
				"customers_email_key2": {"customers_email_key", 0}, // both back a UNIQUE constraint
				"customers_id_copy":    {"customers_pkey", 1},
				"orders_customer_idx2": {"orders_customer_idx", 1},
				"orders_lower_note2":   {"orders_lower_note", 1}, // same expression, other :location
				"orders_open2":         {"orders_open", 1},       // same predicate
				"products_sku_b":       {"products_sku_a", 1},    // a is the FK's target
				"replid_id":            {"replid_id2", 1},        // id2 is the replica identity
				"rooms_r":              {"rooms_r_excl", 1},      // the exclusion constraint's index
				"vouchers_code_first":  {"vouchers_code_key", 1}, // older, but the constraint's is kept
			}
			for name, w := range want {
				d, ok := dups[name]
				if !ok || d.DuplicateOf != w.keep || d.Droppable == nil || *d.Droppable != w.droppable {
					t.Errorf("%s = %+v, want a duplicate of %s, droppable %v", name, d, w.keep, w.droppable)
				}
			}
			// Not duplicates: immediate vs deferrable, NULLS NOT DISTINCT vs
			// not (15+), other expression, predicate, indoption, opclass.
			for name := range dups {
				if _, ok := want[name]; !ok {
					t.Errorf("%s reported as a duplicate of %s", name, dups[name].DuplicateOf)
				}
			}
			// One index both unused and a duplicate: two rows.
			if indexRow(r, "orders_customer_idx2", "unused") == nil || indexRow(r, "orders_customer_idx2", "duplicate") == nil {
				t.Error("an index both unused and duplicate must keep both rows")
			}
			if u := indexRow(r, "orders_customer_idx", "unused"); u == nil || u.FKSupport == nil || *u.FKSupport != 1 {
				t.Errorf("orders_customer_idx = %+v, want marked as serving the FK", u)
			}
			if indexRow(r, "events_at", "unused") != nil || indexRow(r, "events_2026_at_idx", "unused") == nil {
				t.Error("a partitioned index has no scans of its own: its partitions' are reported")
			}
			if inv := indexRow(r, "broken_a", "invalid"); inv == nil || inv.SizeBytes == nil {
				t.Errorf("invalid = %+v", inv)
			}
		})
	}
}

// TestFixturePartialFailure: in the PostgreSQL 18 run a catalog lock in
// billing made two pack queries fail there; the reading names the query
// and the database, never the error text.
func TestFixturePartialFailure(t *testing.T) {
	r := readFixture(t, "pg18-primary.prom")
	want := map[string]float64{"pgtoolbox_tables": 1, "pgtoolbox_vacuum_progress": 1}
	if !reflect.DeepEqual(r.QueryErrors, want) {
		t.Fatalf("query errors = %v, want %v", r.QueryErrors, want)
	}
	if got := r.FailedDatabases["pgtoolbox_tables"]; !reflect.DeepEqual(got, []string{"billing"}) {
		t.Fatalf("failed databases = %v", r.FailedDatabases)
	}
	if tableNamed(r, "billing", "pg_class") != nil || tableNamed(r, "app", "accounts") == nil {
		t.Error("the other databases' rows stay")
	}
	if r14 := readFixture(t, "pg14-primary.prom"); r14.QueryErrors != nil {
		t.Errorf("pg14 errors = %v", r14.QueryErrors)
	}
}

func TestParseWithoutPack(t *testing.T) {
	r, err := Parse(strings.NewReader("cnpg_backends_total{datname=\"app\"} 3\ncnpg_pg_database_xid_age{datname=\"app\"} 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Present() || len(r.Tables) != 0 || r.Settings != nil {
		t.Fatalf("a body without the pack must read as absent: %+v", r)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"nan dropped":             `cnpg_pgtoolbox_tables_dead_tuples{datname="a",schema="s",table="t",kind="table"} NaN`,
		"unterminated label":      `cnpg_pgtoolbox_tables_dead_tuples{datname="a} 1`,
		"label without quotes":    `cnpg_pgtoolbox_tables_dead_tuples{datname=a} 1`,
		"bad pid":                 `cnpg_pgtoolbox_blocking_wait_seconds{pid="x",blocked_by=""} 1`,
		"negative pid":            `cnpg_pgtoolbox_blocking_wait_seconds{pid="-1",blocked_by=""} 1`,
		"statement without id":    `cnpg_pgtoolbox_statements_calls{queryid="",datname="a",rolname="r"} 1`,
		"info not one":            `cnpg_pgtoolbox_pack_info{version="9"} 0`,
		"inf":                     `cnpg_pgtoolbox_xid_next_xid +Inf`,
		"comment":                 `# cnpg_pgtoolbox_pack_info{version="1"} 1`,
		"unknown error query":     `cnpg_errors_total{errorUserQueries="pgtoolbox_nope: x"} 1`,
		"error on db, unknown":    `cnpg_errors_total{errorUserQueries="pgtoolbox_nope on db a: x"} 1`,
		"settings without a name": `cnpg_pgtoolbox_settings_value{name=""} 1`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := Parse(strings.NewReader(body + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if r.Present() || len(r.Tables)+len(r.Sessions)+len(r.Statements) != 0 || r.NextXID != nil ||
				r.QueryErrors != nil || r.Settings != nil {
				t.Fatalf("kept %+v", r)
			}
		})
	}
}

func TestParseBounds(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxRows+50; i++ {
		fmt.Fprintf(&b, "cnpg_pgtoolbox_tables_dead_tuples{datname=\"d\",schema=\"s\",table=\"t%d\",kind=\"table\"} %d\n", i, i)
	}
	long := strings.Repeat("x", 4096)
	fmt.Fprintf(&b, "cnpg_pgtoolbox_connections_sessions{datname=%q,application_name=\"a\",state=\"idle\"} 1\n", long)
	fmt.Fprintf(&b, "cnpg_pgtoolbox_blocking_wait_seconds{pid=\"7\",blocked_by=\"1,2,3,4,5,6,7,8,9,10,2,x\"} 1\n")
	r, err := Parse(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tables) != MaxRows || len(r.Truncated) != 1 || r.Truncated[0] != "pgtoolbox_tables" {
		t.Fatalf("tables = %d, truncated = %v", len(r.Tables), r.Truncated)
	}
	if len(r.Connections) != 1 || len(r.Connections[0].Database) != maxLabelValue {
		t.Fatalf("label value not cut: %d", len(r.Connections[0].Database))
	}
	if got := r.Sessions[0].BlockedBy; len(got) != maxBlockers {
		t.Fatalf("blocked_by = %v, want %d pids", got, maxBlockers)
	}
}

func TestParseStatementsInfoIsNotAStatement(t *testing.T) {
	r, err := Parse(strings.NewReader("cnpg_pgtoolbox_statements_info_dealloc 4\ncnpg_pgtoolbox_statements_calls{queryid=\"42\",datname=\"a\",rolname=\"r\"} 9\n"))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatementsInfo == nil || *r.StatementsInfo.Dealloc != 4 || len(r.Statements) != 1 || *r.Statements[0].Calls != 9 {
		t.Fatalf("%+v %+v", r.StatementsInfo, r.Statements)
	}
}

// FuzzParse holds the parser's invariants on arbitrary exporter text:
// bounded rows and label values, no NaN or infinity kept, a version only
// from an info sample of 1, pids never negative (0 is a prepared
// transaction).
func FuzzParse(f *testing.F) {
	for _, name := range []string{"pg18-primary.prom", "pg14-primary.prom"} {
		body, err := os.ReadFile("testdata/" + name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(body))
	}
	f.Add(`cnpg_pgtoolbox_blocking_wait_seconds{pid="7",blocked_by="1,2"} 1`)
	f.Add("cnpg_pgtoolbox_pack_info{version=\"1\\\"\"} 1\ncnpg_errors_total{errorUserQueries=\"pgtoolbox_tables: x\"} 2")
	f.Fuzz(func(t *testing.T, in string) {
		r, err := Parse(strings.NewReader(in))
		if err != nil {
			return
		}
		if len(r.Tables) > MaxRows || len(r.Sessions) > MaxRows || len(r.Statements) > MaxRows ||
			len(r.Indexes) > MaxRows || len(r.Connections) > MaxRows || len(r.Databases) > MaxRows || len(r.Vacuums) > MaxRows ||
			len(r.PreparedXacts) > MaxRows {
			t.Fatal("row bound exceeded")
		}
		if len(r.Version) > maxLabelValue || len(r.QueryErrors) > maxQueryErrors {
			t.Fatal("unbounded value")
		}
		for _, s := range r.Sessions {
			if s.PID < 0 || len(s.BlockedBy) > maxBlockers {
				t.Fatalf("session %+v", s)
			}
			for _, b := range s.BlockedBy {
				if b < 0 {
					t.Fatalf("pid %d", b)
				}
			}
		}
		check := func(p *float64) {
			if p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0)) {
				t.Fatal("non-finite value kept")
			}
		}
		for _, tb := range r.Tables {
			check(tb.DeadTuples)
			check(tb.XIDAge)
			if len(tb.Name) > maxLabelValue {
				t.Fatal("label not cut")
			}
		}
		for _, s := range r.Statements {
			check(s.Calls)
			check(s.TotalMillis)
			if s.QueryID == "" {
				t.Fatal("statement without queryid")
			}
		}
		check(r.NextXID)
		if r.Version != "" && !r.Present() {
			t.Fatal("a version without presence")
		}
		_ = BlockingTree(r.Sessions)
	})
}

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

// boundsBody overflows every bounded family: more than MaxRows rows of
// each, more settings than are kept, more failing queries than are
// counted, a label value past its cut and a blocked_by past its pids.
func boundsBody() string {
	var b strings.Builder
	for i := range MaxRows + 2 {
		fmt.Fprintf(&b, "cnpg_pgtoolbox_tables_dead_tuples{datname=\"d\",schema=\"s\",table=\"t%d\",kind=\"table\"} %d\n", i, i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_database_xid_age{datname=\"d%d\"} %d\n", i, i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_vacuum_progress_running_seconds{datname=\"d\",schema=\"s\",table=\"t%d\",phase=\"scanning heap\"} %d\n", i, i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_blocking_wait_seconds{pid=\"%d\",blocked_by=\"\"} 1\n", i+1)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_connections_sessions{datname=\"d\",application_name=\"a%d\",state=\"idle\"} 1\n", i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_statements_calls{queryid=\"%d\",datname=\"d\",rolname=\"r\"} 1\n", i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_indexes_scans{datname=\"d\",schema=\"s\",table=\"t\",index=\"i%d\",kind=\"unused\"} 0\n", i)
		fmt.Fprintf(&b, "cnpg_pgtoolbox_prepared_xacts_count{datname=\"d%d\"} 1\n", i)
	}
	for i := range maxLabels*2 + 2 {
		fmt.Fprintf(&b, "cnpg_pgtoolbox_settings_value{name=\"s%d\"} %d\n", i, i)
	}
	for _, q := range Queries {
		fmt.Fprintf(&b, "cnpg_errors_total{errorUserQueries=\"%s on db z: boom\"} 1\n", q)
		fmt.Fprintf(&b, "cnpg_errors_total{errorUserQueries=\"%s on db a: boom\"} 1\n", q)
	}
	fmt.Fprintf(&b, "cnpg_pgtoolbox_connections_sessions{datname=%q,application_name=\"a\",state=\"idle\"} 1\n", strings.Repeat("x", 4096))
	b.WriteString("cnpg_pgtoolbox_blocking_wait_seconds{pid=\"9999\",blocked_by=\"1,2,3,4,5,6,7,8,9,10,2,x\"} 1\n")
	return b.String()
}

// TestParseGolden pins Parse's whole Reading for the two real expositions,
// byte for byte, so a change to the parser shows every row it moves; the
// over-bound body (thousands of rows) is pinned as a digest of the same
// JSON. Rewrite with -update.
func TestParseGolden(t *testing.T) {
	read := func(name string) func() (string, error) {
		return func() (string, error) {
			b, err := os.ReadFile("testdata/" + name)
			return string(b), err
		}
	}
	cases := map[string]struct {
		body   func() (string, error)
		digest bool
	}{
		"pg18-primary": {body: read("pg18-primary.prom")},
		"pg14-primary": {body: read("pg14-primary.prom")},
		"bounds":       {body: func() (string, error) { return boundsBody(), nil }, digest: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			text, err := tc.body()
			if err != nil {
				t.Fatal(err)
			}
			r, err := Parse(strings.NewReader(text))
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(r, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".golden.json")
			if tc.digest {
				sum := sha256.Sum256(got)
				got = []byte(hex.EncodeToString(sum[:]) + "  truncated: " + strings.Join(r.Truncated, ",") + "\n")
				path = filepath.Join("testdata", name+".golden.sha256")
			}
			if *updateGolden {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if string(got) != string(want) {
				t.Errorf("%s differs from the golden file (run with -update to accept)", path)
			}
		})
	}
}
