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

package insightview

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	machineryapi "github.com/cloudnative-pg/machinery/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

const ordersUID = "0b6a2c1e-8f0d-4a55-9a3e-7c1f2b3d4e5f"

func golden(t *testing.T, name string, value any) {
	t.Helper()
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from its golden file; run go test ./internal/console/insightview -update and review the diff", name)
	}
}

// realReading is the pack's real output on PostgreSQL 18 (the parser's
// fixture): a blocked update, duplicate and invalid indexes, a table
// with autovacuum disabled, pg_stat_statements loaded.
func realReading(t *testing.T) insight.Reading {
	t.Helper()
	f, err := os.Open("../../../pkg/cnpgstatus/insight/testdata/pg18-primary.prom")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r, err := insight.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func optedIn(referenced bool) *cnpgv1.Cluster {
	c := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: ordersUID}}
	c.Spec.PostgresConfiguration.Parameters = map[string]string{"autovacuum_freeze_max_age": "150000000"}
	if referenced {
		c.Spec.Monitoring = &cnpgv1.MonitoringConfiguration{CustomQueriesConfigMap: []machineryapi.ConfigMapKeySelector{
			{LocalObjectReference: machineryapi.LocalObjectReference{Name: insight.ConfigMapName}, Key: insight.ConfigMapKey},
		}}
	}
	return c
}

// TestInsightGolden pins the whole /insight view of a primary reading
// the real pack and a replica that does not report it yet.
func TestInsightGolden(t *testing.T) {
	tr := insight.NewTracker()
	tr.Observe("orders-1", now, realReading(t), true)
	tr.Observe("orders-2", now, insight.Reading{}, false)
	v := Build(optedIn(true), "", tr.Snapshot(now), true, 30*time.Second, now)
	golden(t, "insight-orders", v)

	if v.Pack.State != "enabled" || v.Pack.Referenced == nil || !*v.Pack.Referenced {
		t.Errorf("pack = %+v", v.Pack)
	}
	if !v.Wraparound.Available || len(v.Wraparound.Databases) == 0 || v.Wraparound.XIDRate != nil {
		t.Errorf("wraparound = %+v", v.Wraparound)
	}
	// The instance reports its own setting, which wins over the spec.
	for _, l := range v.Wraparound.Limits {
		if l.Name == "autovacuum_freeze_max_age" && (l.Source != "instance" || l.Value != 2e8) {
			t.Errorf("limit = %+v", l)
		}
	}
	if !v.Statements.Available || v.Statements.Comparable || !strings.Contains(v.Statements.ComparisonText, "No regression comparison yet") {
		t.Errorf("statements: available=%v comparable=%v %q", v.Statements.Available, v.Statements.Comparable, v.Statements.ComparisonText)
	}
	if len(v.Indexes.Duplicate) != 9 || len(v.Indexes.Invalid) != 1 || v.Indexes.Partial {
		t.Errorf("indexes = %+v", v.Indexes)
	}
	for _, d := range v.Indexes.Duplicate {
		// Both back a unique constraint: reported, not droppable.
		if d.Name == "customers_email_key2" && (d.Droppable == nil || *d.Droppable || d.BacksConstraint == nil || !*d.BacksConstraint) {
			t.Errorf("customers_email_key2 = %+v", d)
		}
	}
	if len(v.Blocking.Instances) != 1 || len(v.Blocking.Instances[0].Roots) != 2 {
		t.Fatalf("blocking = %+v", v.Blocking)
	}
	prepared := v.Blocking.Instances[0].Roots[1]
	if !prepared.Prepared || prepared.PID != 0 || len(prepared.Waiters) != 1 || v.Blocking.Instances[0].Roots[0].Prepared {
		t.Errorf("a prepared transaction must be a blocker of its own: %+v", prepared)
	}
	if len(v.Pack.Instances) != 2 || v.Pack.Instances[1].Version != "" {
		t.Errorf("a replica without the pack must be listed without a version: %+v", v.Pack.Instances)
	}
	if len(v.Pack.QueryErrors) != 2 || v.Pack.Health != viewmodel.HealthWarning ||
		strings.Join(v.Pack.QueryErrors[0].Databases, ",") != "billing" {
		t.Errorf("query errors = %+v, health %s", v.Pack.QueryErrors, v.Pack.Health)
	}
	// pgtoolbox_tables failed on billing: the wraparound panel shows the
	// other databases and says it is partial.
	if !v.Wraparound.Available || !v.Wraparound.Partial || !strings.Contains(v.Wraparound.PartialText, "pgtoolbox_tables failed on database billing") {
		t.Errorf("wraparound partial = %v %q", v.Wraparound.Partial, v.Wraparound.PartialText)
	}
	if v.Statements.Partial || v.Connections.Partial || v.Blocking.Partial {
		t.Errorf("panels without a failed query must be complete: %+v %+v %+v", v.Statements.InsightPartial, v.Connections.InsightPartial, v.Blocking.InsightPartial)
	}
	if h := v.Wraparound.Horizons; h == nil || h.Holder == "" || h.PreparedXIDAge == nil {
		t.Errorf("horizons = %+v", h)
	}
	var toast bool
	for _, tb := range v.Wraparound.Tables {
		toast = toast || (tb.Kind == "toast" && tb.ParentTable == "documents")
	}
	if !toast {
		t.Error("a TOAST row must name its parent table")
	}
}

// TestInsightDataAge: the exporter answers from its cache, so the data
// is as old as its last run, not as the console's read.
func TestInsightDataAge(t *testing.T) {
	r := realReading(t)
	ran := now.Add(-40 * time.Second)
	r.ExporterUpdatedAt = &ran
	tr := insight.NewTracker()
	tr.Observe("orders-1", now, r, true)
	v := Build(optedIn(true), "", tr.Snapshot(now), true, 30*time.Second, now)
	b := v.Blocking.Instances[0]
	if b.DataAgeSeconds != 40 || !b.Provenance.ObservedAt.Equal(ran) || !v.Wraparound.Provenance.ObservedAt.Equal(ran) {
		t.Errorf("data age = %v, observed at %v", b.DataAgeSeconds, b.Provenance.ObservedAt)
	}
}

// TestConnectionShareUsesUsableSlots: reserved slots are not the
// application's to take.
func TestConnectionShareUsesUsableSlots(t *testing.T) {
	client := 92.0
	r := insight.Reading{Version: "1", Backends: &insight.Backends{Client: &client}, Settings: map[string]float64{
		"max_connections": 100, "superuser_reserved_connections": 3, "reserved_connections": 2,
	}}
	tr := insight.NewTracker()
	tr.Observe("orders-1", now, r, true)
	c := Build(optedIn(true), "", tr.Snapshot(now), true, 30*time.Second, now).Connections.Instances[0]
	if c.Usable == nil || *c.Usable != 95 || c.Share == nil || *c.Share != client/95 || c.Health != viewmodel.HealthCritical {
		t.Errorf("connections = usable %v share %v health %s", c.Usable, c.Share, c.Health)
	}
}

// TestFleetCutAfterFiltering: the cut is taken over the Clusters passed
// (the visible ones), and says it was taken.
func TestFleetCutAfterFiltering(t *testing.T) {
	cluster := func(name string, n int) FleetCluster {
		r := insight.Reading{Version: "1"}
		for i := 0; i < n; i++ {
			ms := float64(i + 1)
			r.Statements = append(r.Statements, insight.Statement{QueryID: name + strconv.Itoa(i), Database: "app", Role: "app", TotalMillis: &ms})
		}
		tr := insight.NewTracker()
		tr.Observe(name+"-1", now, r, true)
		return FleetCluster{Ref: viewmodel.ClusterRef{Namespace: "team-a", Name: name}, Snapshot: tr.Snapshot(now), Has: true, Interval: time.Minute}
	}
	f := Fleet([]FleetCluster{cluster("a", 50)}, "", now)
	if len(f.Top) != 50 || f.TopTotal != 50 || f.TopTruncated {
		t.Errorf("under the bound: %d of %d, truncated %v", len(f.Top), f.TopTotal, f.TopTruncated)
	}
	f = Fleet([]FleetCluster{cluster("a", 50), cluster("b", 50), cluster("c", 50)}, "", now)
	if len(f.Top) != FleetTopStatements || f.TopTotal != 150 || !f.TopTruncated {
		t.Errorf("over the bound: %d of %d, truncated %v", len(f.Top), f.TopTotal, f.TopTruncated)
	}
}

func TestInsightPackStates(t *testing.T) {
	one := func(r insight.Reading) insight.Snapshot {
		tr := insight.NewTracker()
		tr.Observe("orders-1", now, r, true)
		return tr.Snapshot(now)
	}
	cases := []struct {
		name    string
		cluster *cnpgv1.Cluster
		snap    insight.Snapshot
		has     bool
		state   string
		text    string
	}{
		{"metrics off", optedIn(false), insight.Snapshot{}, false, "metricsOff", "METRICS_ENABLED"},
		{"not swept", optedIn(false), insight.Snapshot{}, true, "noData", "read yet"},
		{"not opted in", optedIn(false), one(insight.Reading{}), true, "notEnabled", insight.SpecChange},
		{"referenced, silent", optedIn(true), one(insight.Reading{}), true, "notEnabled", "missing from this namespace"},
		{"outdated", optedIn(true), one(insight.Reading{Version: "0"}), true, "outdated", "older"},
		{"newer", optedIn(true), one(insight.Reading{Version: "9"}), true, "newer", "newer"},
		// A cut exposition never reads as "not enabled".
		{"cut before the pack", optedIn(false), one(insight.Reading{Truncated: []string{insight.TruncatedExposition}}), true, "truncated", "not known"},
		{"cut before pack_info", optedIn(true), one(insight.Reading{Seen: true, Truncated: []string{insight.TruncatedExposition}}), true, "truncated", "before its version"},
		{"series without pack_info", optedIn(true), one(insight.Reading{Seen: true}), true, "unknown", "without its version"},
		{"PostgreSQL 13", optedIn(true), one(insight.Reading{Version: "1", ServerVersionNum: new(130014.0)}), true, "unsupported", "PostgreSQL 14 and later"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Build(c.cluster, "", c.snap, c.has, 30*time.Second, now)
			if v.Pack.State != c.state || !strings.Contains(v.Pack.Text, c.text) {
				t.Fatalf("pack = %s %q", v.Pack.State, v.Pack.Text)
			}
			if !slices.Contains([]string{"enabled", "outdated", "newer", "unknown", "truncated"}, c.state) &&
				(v.Wraparound.Available || v.Statements.Available || v.Indexes.Available || v.Connections.Available) {
				t.Errorf("a Cluster without the pack has panels available: %+v", v)
			}
			if !v.Wraparound.Available && v.Wraparound.Text == "" {
				t.Error("an unavailable panel must say why")
			}
		})
	}
	// The not-opted-in view carries the spec change as a blind spot.
	v := Build(optedIn(false), "", one(insight.Reading{}), true, 30*time.Second, now)
	found := false
	for _, b := range v.BlindSpots {
		found = found || (b.ID == "insight.notEnabled" && strings.Contains(b.Text, insight.SpecChange))
	}
	if !found {
		t.Errorf("blind spots = %+v", v.BlindSpots)
	}
}

// TestInsightPackConfigMapName: a console told the pack's ConfigMap has
// another name (INSIGHT_PACK_CONFIGMAP, the pgtools chart's per-release
// name) recognises a Cluster referencing that name, not the default, and
// names it in every opt-in instruction.
func TestInsightPackConfigMapName(t *testing.T) {
	silent := func() insight.Snapshot {
		tr := insight.NewTracker()
		tr.Observe("orders-1", now, insight.Reading{}, true)
		return tr.Snapshot(now)
	}
	referencing := func(name string) *cnpgv1.Cluster {
		c := optedIn(false)
		c.Spec.Monitoring = &cnpgv1.MonitoringConfiguration{CustomQueriesConfigMap: []machineryapi.ConfigMapKeySelector{
			{LocalObjectReference: machineryapi.LocalObjectReference{Name: name}, Key: insight.ConfigMapKey},
		}}
		return c
	}
	cases := []struct {
		name, configMap string
		cluster         *cnpgv1.Cluster
		wantName        string
		wantReferenced  bool
	}{
		{"default name, default reference", "", referencing(insight.ConfigMapName), insight.ConfigMapName, true},
		{"release name, its reference", "pgc-insight-pack", referencing("pgc-insight-pack"), "pgc-insight-pack", true},
		// Another release's pack (or the default one) is not this console's.
		{"release name, the default reference", "pgc-insight-pack", referencing(insight.ConfigMapName), "pgc-insight-pack", false},
		{"default name, a release's reference", "", referencing("pgc-insight-pack"), insight.ConfigMapName, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Build(c.cluster, c.configMap, silent(), true, 30*time.Second, now)
			if v.Pack.ConfigMap != c.wantName || v.Pack.SpecChange != insight.SpecChangeFor(c.wantName) ||
				!strings.Contains(v.Pack.SpecChange, "name: "+c.wantName+",") {
				t.Fatalf("pack ConfigMap %q, spec change %q; want %q", v.Pack.ConfigMap, v.Pack.SpecChange, c.wantName)
			}
			if v.Pack.Referenced == nil || *v.Pack.Referenced != c.wantReferenced {
				t.Fatalf("referenced = %v, want %v", v.Pack.Referenced, c.wantReferenced)
			}
			if !strings.Contains(v.Pack.Text, c.wantName) {
				t.Errorf("pack text %q does not name %s", v.Pack.Text, c.wantName)
			}
			if !c.wantReferenced {
				found := false
				for _, b := range v.BlindSpots {
					found = found || (b.ID == "insight.notEnabled" && strings.Contains(b.Text, v.Pack.SpecChange))
				}
				if !found {
					t.Errorf("blind spots %+v do not carry %q", v.BlindSpots, v.Pack.SpecChange)
				}
			}
		})
	}
}

// TestInsightCutExposition: what was read past the cut is shown, and
// every panel says it may be missing the rest.
func TestInsightCutExposition(t *testing.T) {
	r := realReading(t)
	r.Truncated = append(r.Truncated, insight.TruncatedExposition)
	tr := insight.NewTracker()
	tr.Observe("orders-1", now, r, true)
	v := Build(optedIn(true), "", tr.Snapshot(now), true, 30*time.Second, now)
	if v.Pack.State != "enabled" || v.Pack.Health != viewmodel.HealthWarning || !strings.Contains(v.Pack.Text, "cut") {
		t.Errorf("pack = %s %s %q", v.Pack.State, v.Pack.Health, v.Pack.Text)
	}
	for name, p := range map[string]viewmodel.InsightPartial{
		"wraparound": v.Wraparound.InsightPartial, "statements": v.Statements.InsightPartial, "indexes": v.Indexes.InsightPartial,
		"connections": v.Connections.InsightPartial, "blocking": v.Blocking.InsightPartial,
	} {
		if !p.Partial || !strings.Contains(p.PartialText, "size bound") {
			t.Errorf("%s = %+v", name, p)
		}
	}
}

func TestInsightStaleReading(t *testing.T) {
	tr := insight.NewTracker()
	tr.Observe("orders-1", now.Add(-10*time.Minute), realReading(t), true)
	v := Build(optedIn(true), "", tr.Snapshot(now), true, 30*time.Second, now)
	if !v.Wraparound.Provenance.Stale || !strings.Contains(v.Wraparound.Provenance.StaleReason, "10m0s") {
		t.Errorf("a ten-minute-old reading must read as stale: %+v", v.Wraparound.Provenance)
	}
}

func TestForecastHealth(t *testing.T) {
	l := insight.LimitsFrom(nil, nil)
	day := 1e9 / 86400.0 // about a day to the stop limit from 1.14e9
	cases := map[string]struct {
		age  float64
		rate *float64
		want viewmodel.Health
	}{
		"young":          {1e8, nil, viewmodel.HealthOK},
		"due":            {2.5e8, nil, viewmodel.HealthProgressing},
		"overdue":        {3.5e8, nil, viewmodel.HealthWarning},
		"failsafe":       {1.7e9, nil, viewmodel.HealthCritical},
		"days from stop": {1.0e9, &day, viewmodel.HealthCritical},
	}
	for name, c := range cases {
		if got := forecast(insight.ForecastXID(c.age, l, 0, c.rate), l.FreezeMaxAge).Health; got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}
