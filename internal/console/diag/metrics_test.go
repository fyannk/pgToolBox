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

package diag

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

func TestFindingMetrics(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	matched := diagnose.Result{
		Checks: []diagnose.Check{
			{Name: "image-pull", Outcome: diagnose.CheckMatched},
			{Name: "wal-archiving", Outcome: diagnose.CheckMatched},
			{Name: "quota", Outcome: diagnose.CheckClear},
			{Name: "log-oom", Outcome: diagnose.CheckUnavailable, Because: "events stale"},
			{Name: "drills", Outcome: diagnose.CheckUnavailable, SourceOff: true},
			{Name: "cnpg-eol", Outcome: diagnose.CheckNotApplicable},
		},
		Findings: []diagnose.Finding{
			// Two pods, one series: the gauge is per check and severity.
			{ID: "image-pull/orders-1", Check: "image-pull", Severity: diagnose.SeverityCritical},
			{ID: "image-pull/orders-2", Check: "image-pull", Severity: diagnose.SeverityCritical},
			{ID: "wal-archiving", Check: "wal-archiving", Severity: diagnose.SeverityHigh},
		},
	}
	tests := []struct {
		name   string
		engine func(diagnose.Input) diagnose.Result
		opts   Options
		want   string
	}{
		{
			name:   "active findings, and every check outcome counted",
			engine: func(diagnose.Input) diagnose.Result { return matched },
			want: `
# HELP pgtoolbox_finding 1 for each check that reports a finding on a Cluster in the latest diagnostics run, by the finding's severity (note, warning, high, critical). Absent when nothing is found, when the check could not run, and when the run failed: read it with pgtoolbox_finding_checks.
# TYPE pgtoolbox_finding gauge
pgtoolbox_finding{check="image-pull",cluster="orders",cluster_namespace="a",severity="critical"} 1
pgtoolbox_finding{check="wal-archiving",cluster="orders",cluster_namespace="a",severity="high"} 1
# HELP pgtoolbox_finding_checks Checks of the latest diagnostics run of a Cluster by outcome: matched, clear, unavailable (its input is absent, forbidden, stale or not observed yet), off (its source is switched off), not_applicable (the observed versions are outside its pins).
# TYPE pgtoolbox_finding_checks gauge
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="clear"} 1
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="matched"} 2
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="not_applicable"} 1
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="off"} 1
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="unavailable"} 1
# HELP pgtoolbox_finding_run_failed 1 when the latest diagnostics run of a Cluster failed (it then reports no finding and no check), else 0.
# TYPE pgtoolbox_finding_run_failed gauge
pgtoolbox_finding_run_failed{cluster="orders",cluster_namespace="a"} 0
# HELP pgtoolbox_finding_run_timestamp_seconds When (Unix seconds) the latest diagnostics run of a Cluster was made.
# TYPE pgtoolbox_finding_run_timestamp_seconds gauge
pgtoolbox_finding_run_timestamp_seconds{cluster="orders",cluster_namespace="a"} 1.7952624e+09
`,
		},
		{
			name:   "a failed run reports no finding and says it failed",
			engine: func(diagnose.Input) diagnose.Result { panic("rule bug") },
			want: `
# HELP pgtoolbox_finding_checks Checks of the latest diagnostics run of a Cluster by outcome: matched, clear, unavailable (its input is absent, forbidden, stale or not observed yet), off (its source is switched off), not_applicable (the observed versions are outside its pins).
# TYPE pgtoolbox_finding_checks gauge
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="clear"} 0
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="matched"} 0
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="not_applicable"} 0
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="off"} 0
pgtoolbox_finding_checks{cluster="orders",cluster_namespace="a",outcome="unavailable"} 0
# HELP pgtoolbox_finding_run_failed 1 when the latest diagnostics run of a Cluster failed (it then reports no finding and no check), else 0.
# TYPE pgtoolbox_finding_run_failed gauge
pgtoolbox_finding_run_failed{cluster="orders",cluster_namespace="a"} 1
# HELP pgtoolbox_finding_run_timestamp_seconds When (Unix seconds) the latest diagnostics run of a Cluster was made.
# TYPE pgtoolbox_finding_run_timestamp_seconds gauge
pgtoolbox_finding_run_timestamp_seconds{cluster="orders",cluster_namespace="a"} 1.7952624e+09
`,
		},
		{
			name:   "a disabled runner exports nothing",
			engine: func(diagnose.Input) diagnose.Result { return matched },
			opts:   Options{Disabled: "DIAGNOSTICS_ENABLED=false"},
			want:   ``,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{keys: []cache.ClusterKey{orders}, inputs: map[cache.ClusterKey]diagnose.Input{orders: {}}, calls: map[cache.ClusterKey]int{}}
			opts := tt.opts
			opts.Interval, opts.Now, opts.engine = time.Minute, func() time.Time { return at }, tt.engine
			r := New(src, noChanges{}, opts)
			r.RunAll(context.Background())
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(findingCollector{runner: func() *Runner { return r }})
			if err := testutil.GatherAndCompare(reg, strings.NewReader(tt.want)); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestFindingMetricsFollowTheRuns(t *testing.T) {
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{keys: []cache.ClusterKey{orders, billing}, inputs: map[cache.ClusterKey]diagnose.Input{orders: crashing(at), billing: {}}, calls: map[cache.ClusterKey]int{}}
	r := New(src, noChanges{}, Options{Interval: time.Minute, Now: func() time.Time { return at }, Rules: []diagnose.Rule{}})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(findingCollector{runner: func() *Runner { return r }})
	count := func() int {
		n, err := testutil.GatherAndCount(reg, "pgtoolbox_finding")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}

	if n := count(); n != 0 {
		t.Fatalf("before any run: %d finding series", n)
	}
	r.RunAll(context.Background())
	if n := count(); n != 1 {
		t.Fatalf("the image pull finding: %d series, want 1", n)
	}
	// The finding clears: its series goes at once, nothing holds it.
	src.inputs[orders] = diagnose.Input{Now: at, HasPods: true}
	r.RunAll(context.Background())
	if n := count(); n != 0 {
		t.Errorf("a cleared finding is still exported (%d series)", n)
	}
	// A deleted Cluster takes every one of its series with it.
	src.keys = []cache.ClusterKey{orders}
	r.RunAll(context.Background())
	if n, _ := testutil.GatherAndCount(reg, "pgtoolbox_finding_run_failed"); n != 1 {
		t.Errorf("run series after billing went: %d, want 1", n)
	}
}
