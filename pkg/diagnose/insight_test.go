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

package diagnose

import (
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
)

func pf(v float64) *float64 { return &v }

// packInput is an input whose primary orders-1 reports the given pack
// reading, read now.
func packInput(r insight.Reading) Input {
	r.Version = "1"
	return Input{Now: now, HasInsight: true, Insight: insight.Snapshot{
		Primary: "orders-1",
		Instances: map[string]insight.InstanceReading{
			"orders-1": {Instance: "orders-1", At: now, Primary: true, Reading: r},
		},
	}}
}

func TestInsightChecksSayWhyTheyCannotRun(t *testing.T) {
	t.Parallel()
	detectors := []Detector{wraparoundDetector{}, idleInTransactionDetector{}, connectionSaturationDetector{}}
	notOptedIn := Input{Now: now, HasInsight: true, Insight: insight.Snapshot{
		Instances: map[string]insight.InstanceReading{"orders-1": {Instance: "orders-1", At: now}},
	}}
	stale := packInput(insight.Reading{Backends: &insight.Backends{Client: pf(1)}})
	r := stale.Insight.Instances["orders-1"]
	r.At = now.Add(-time.Hour)
	stale.Insight.Instances["orders-1"] = r
	for _, d := range detectors {
		if _, why := d.Detect(Input{Now: now}); why != reasonMetricsOff || !sourceOff(why) {
			t.Errorf("%s without metrics: %q", d.Name(), why)
		}
		_, why := d.Detect(notOptedIn)
		if why != reasonInsightOff || !sourceOff(why) || !strings.Contains(why, insight.SpecChange) {
			t.Errorf("%s without the pack must name the spec change as a source switched off: %q", d.Name(), why)
		}
		if _, why := d.Detect(stale); why == "" || sourceOff(why) {
			t.Errorf("%s on an hour-old reading must not clear, and is a fault: %q", d.Name(), why)
		}
		if _, why := d.Detect(packInput(insight.Reading{})); why == "" {
			t.Errorf("%s with the pack loaded but its query silent must not clear", d.Name())
		}
		// A cut exposition can hide the pack: a fault, never "not opted in".
		cut := Input{Now: now, HasInsight: true, Insight: insight.Snapshot{Primary: "orders-1", Instances: map[string]insight.InstanceReading{
			"orders-1": {Instance: "orders-1", At: now, Primary: true, Reading: insight.Reading{Truncated: []string{insight.TruncatedExposition}}},
		}}}
		if _, why := d.Detect(cut); why != reasonInsightCut || sourceOff(why) {
			t.Errorf("%s on a cut exposition must report a fault: %q", d.Name(), why)
		}
		pg13 := packInput(insight.Reading{ServerVersionNum: pf(130014), Backends: &insight.Backends{Client: pf(1)}})
		if _, why := d.Detect(pg13); why != reasonInsightUnsupported || !sourceOff(why) {
			t.Errorf("%s on PostgreSQL 13 must say it is not covered: %q", d.Name(), why)
		}
	}
}

// TestWraparoundNamesWhatHoldsFreezing: the finding carries the
// prepared transactions of its database and the Cluster's horizons.
func TestWraparoundNamesWhatHoldsFreezing(t *testing.T) {
	t.Parallel()
	in := packInput(insight.Reading{
		Databases:     []insight.Database{{Name: "app", XIDAge: pf(3.5e8)}, {Name: "billing", XIDAge: pf(3.5e8)}},
		PreparedXacts: []insight.PreparedXacts{{Database: "app", Count: pf(1), OldestXIDAge: pf(3.4e8)}},
		Horizons:      &insight.Horizons{SlotCatalogXminAge: pf(1.2e8)},
	})
	f, why := wraparoundDetector{}.Detect(in)
	if why != "" || len(f) != 2 {
		t.Fatalf("findings = %+v (%s)", f, why)
	}
	app, billing := evidenceText(f[0]), evidenceText(f[1])
	if !strings.Contains(app, "1 prepared transaction(s), the oldest 340,000,000") || !strings.Contains(app, "catalog_xmin is 120,000,000") {
		t.Errorf("app evidence: %s", app)
	}
	if strings.Contains(billing, "prepared") || !strings.Contains(billing, "catalog_xmin") {
		t.Errorf("billing evidence: %s", billing)
	}
}

func TestWraparoundGrades(t *testing.T) {
	t.Parallel()
	d := wraparoundDetector{}
	grade := func(age float64, rate *insight.Rate, settings map[string]float64) []Finding {
		in := packInput(insight.Reading{Settings: settings, Databases: []insight.Database{{Name: "app", XIDAge: pf(age)}}})
		in.Insight.XIDRate = rate
		findings, why := d.Detect(in)
		if why != "" {
			t.Fatalf("unavailable: %s", why)
		}
		return findings
	}
	if f := grade(2.5e8, nil, nil); len(f) != 0 {
		t.Errorf("just past autovacuum_freeze_max_age is autovacuum's job, not a finding: %+v", f)
	}
	f := grade(3.5e8, nil, nil)
	if len(f) != 1 || f[0].Severity != SeverityWarning || f[0].Subject.Name != "orders-1" {
		t.Fatalf("1.75x the threshold: %+v", f)
	}
	if !strings.Contains(evidenceText(f[0]), "autovacuum_freeze_max_age") {
		t.Errorf("the assumed default is not named: %s", evidenceText(f[0]))
	}
	if !strings.Contains(evidenceText(f[0]), "no consumption rate measured yet") {
		t.Errorf("no rate must be said: %s", evidenceText(f[0]))
	}
	// The instance's own setting is used and the assumption disappears.
	if f := grade(3.5e8, nil, map[string]float64{"autovacuum_freeze_max_age": 3e8}); len(f) != 0 {
		t.Errorf("past 1.5x of the default but under 1.5x of the instance's own setting: %+v", f)
	}
	if f := grade(1.7e9, nil, nil); len(f) != 1 || f[0].Severity != SeverityHigh {
		t.Errorf("past the failsafe: %+v", f)
	}
	if f := grade(2.11e9, nil, nil); len(f) != 1 || f[0].Severity != SeverityCritical {
		t.Errorf("past the warn limit: %+v", f)
	}
	// 1.2e9 at 2,000 xids/s leaves ~0.94e9: about 5.4 days, so high.
	fast := &insight.Rate{PerSecond: 2000, Window: time.Hour}
	if f := grade(1.2e9, fast, nil); len(f) != 1 || f[0].Severity != SeverityHigh ||
		!strings.Contains(evidenceText(f[0]), "at the measured rate") {
		t.Errorf("a week to the stop limit: %+v", f)
	}
	faster := &insight.Rate{PerSecond: 20000, Window: time.Hour}
	if f := grade(1.2e9, faster, nil); len(f) != 1 || f[0].Severity != SeverityCritical {
		t.Errorf("half a day to the stop limit: %+v", f)
	}
}

func TestMultixactWraparound(t *testing.T) {
	t.Parallel()
	in := packInput(insight.Reading{Databases: []insight.Database{{Name: "app", XIDAge: pf(1e6), MXIDAge: pf(7e8)}}})
	f, _ := wraparoundDetector{}.Detect(in)
	if len(f) != 1 || !strings.Contains(f[0].Summary, "multixact") || strings.Contains(f[0].Summary, "transaction id") {
		t.Fatalf("multixact: %+v", f)
	}
}

func TestIdleInTransaction(t *testing.T) {
	t.Parallel()
	d := idleInTransactionDetector{}
	idle := func(seconds float64) []Finding {
		f, why := d.Detect(packInput(insight.Reading{Backends: &insight.Backends{
			IdleInTransaction: pf(2), IdleInTransactionMaxSeconds: pf(seconds),
		}}))
		if why != "" {
			t.Fatal(why)
		}
		return f
	}
	if f := idle(300); len(f) != 0 {
		t.Errorf("five minutes: %+v", f)
	}
	if f := idle(1200); len(f) != 1 || f[0].Severity != SeverityWarning || !strings.Contains(f[0].Summary, "20m00s") {
		t.Errorf("twenty minutes: %+v", f)
	}
	if f := idle(4000); len(f) != 1 || f[0].Severity != SeverityHigh {
		t.Errorf("over an hour: %+v", f)
	}
	// None idle: the max is NULL, which is clear, not unknown.
	if f, why := d.Detect(packInput(insight.Reading{Backends: &insight.Backends{IdleInTransaction: pf(0)}})); len(f) != 0 || why != "" {
		t.Errorf("no idle session: %+v %q", f, why)
	}
}

func TestConnectionSaturation(t *testing.T) {
	t.Parallel()
	d := connectionSaturationDetector{}
	use := func(clients float64) []Finding {
		f, why := d.Detect(packInput(insight.Reading{
			Settings: map[string]float64{"max_connections": 200, "superuser_reserved_connections": 3, "reserved_connections": 2},
			Backends: &insight.Backends{Client: pf(clients)},
		}))
		if why != "" {
			t.Fatal(why)
		}
		return f
	}
	// The share is of the 195 slots ordinary roles may take.
	if f := use(156); len(f) != 0 {
		t.Errorf("exactly 80%% of 195: %+v", f)
	}
	f := use(157) // 78.5% of max_connections, 80.5% of the usable slots
	if len(f) != 1 || f[0].Severity != SeverityWarning || !strings.Contains(f[0].Detail, "195") {
		t.Errorf("80.5%%: %+v", f)
	}
	if f := use(186); len(f) != 1 || f[0].Severity != SeverityHigh { // 93% of 200, 95.4% of 195
		t.Errorf("95.4%%: %+v", f)
	}
	// Without max_connections reported nothing is assumed.
	if _, why := d.Detect(packInput(insight.Reading{Backends: &insight.Backends{Client: pf(99)}})); why == "" {
		t.Error("a ceiling the instance did not report must not be assumed")
	}
}

func evidenceText(f Finding) string {
	var b strings.Builder
	for _, e := range f.Evidence {
		b.WriteString(e.Detail + "\n")
	}
	return b.String()
}

func TestCount(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]string{0: "0", 999: "999", 1000: "1,000", 2144483647: "2,144,483,647", -1500: "-1,500"} {
		if got := count(in); got != want {
			t.Errorf("count(%v) = %q, want %q", in, got, want)
		}
	}
}
