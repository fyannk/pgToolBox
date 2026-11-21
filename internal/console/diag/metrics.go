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
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
)

// The findings are read at scrape time from the latest run of each
// Cluster, so a series exists exactly while the finding does and goes
// with its Cluster. The Cluster's namespace is `cluster_namespace`, not
// `namespace`: the scrape adds the console's own namespace as `namespace`,
// and a console can watch several.
var (
	findingDesc = prometheus.NewDesc("pgtoolbox_finding",
		"1 for each check that reports a finding on a Cluster in the latest diagnostics run, by the finding's severity (note, warning, high, critical). Absent when nothing is found, when the check could not run, and when the run failed: read it with pgtoolbox_finding_checks.",
		[]string{"cluster_namespace", "cluster", "check", "severity"}, nil)
	checksDesc = prometheus.NewDesc("pgtoolbox_finding_checks",
		"Checks of the latest diagnostics run of a Cluster by outcome: matched, clear, unavailable (its input is absent, forbidden, stale or not observed yet), off (its source is switched off), not_applicable (the observed versions are outside its pins).",
		[]string{"cluster_namespace", "cluster", "outcome"}, nil)
	runDesc = prometheus.NewDesc("pgtoolbox_finding_run_timestamp_seconds",
		"When (Unix seconds) the latest diagnostics run of a Cluster was made.",
		[]string{"cluster_namespace", "cluster"}, nil)
	failedDesc = prometheus.NewDesc("pgtoolbox_finding_run_failed",
		"1 when the latest diagnostics run of a Cluster failed (it then reports no finding and no check), else 0.",
		[]string{"cluster_namespace", "cluster"}, nil)
)

// Check outcomes as pgtoolbox_finding_checks labels them, every one
// exported (zero included) so that a count can be compared without
// absent().
const (
	outcomeMatched       = "matched"
	outcomeClear         = "clear"
	outcomeUnavailable   = "unavailable"
	outcomeOff           = "off"
	outcomeNotApplicable = "not_applicable"
)

var checkOutcomes = []string{outcomeMatched, outcomeClear, outcomeUnavailable, outcomeOff, outcomeNotApplicable}

func outcomeLabel(c diagnose.Check) string {
	switch c.Outcome {
	case diagnose.CheckMatched:
		return outcomeMatched
	case diagnose.CheckUnavailable:
		if c.SourceOff {
			return outcomeOff
		}
		return outcomeUnavailable
	case diagnose.CheckNotApplicable:
		return outcomeNotApplicable
	default:
		return outcomeClear
	}
}

// metricRunner is the runner the metrics read: the console builds one
// read model, a test may build several (the last one wins).
var metricRunner atomic.Pointer[Runner]

// ExportMetrics makes r the runner /metrics reports.
func ExportMetrics(r *Runner) { metricRunner.Store(r) }

func init() {
	crmetrics.Registry.MustRegister(findingCollector{runner: metricRunner.Load})
}

// findingCollector exports the latest runs of a runner.
type findingCollector struct {
	runner func() *Runner
}

func (findingCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- findingDesc
	ch <- checksDesc
	ch <- runDesc
	ch <- failedDesc
}

func (c findingCollector) Collect(ch chan<- prometheus.Metric) {
	r := c.runner()
	if r == nil || r.opts.Disabled != "" {
		return
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key, run := range r.runs {
		ns, name := key.Namespace, key.Name
		ch <- prometheus.MustNewConstMetric(runDesc, prometheus.GaugeValue, float64(run.At.UnixNano())/1e9, ns, name)
		failed := 0.0
		if run.Failed != "" {
			failed = 1
		}
		ch <- prometheus.MustNewConstMetric(failedDesc, prometheus.GaugeValue, failed, ns, name)
		counts := make(map[string]int, len(checkOutcomes))
		for _, check := range run.Result.Checks {
			counts[outcomeLabel(check)]++
		}
		for _, outcome := range checkOutcomes {
			ch <- prometheus.MustNewConstMetric(checksDesc, prometheus.GaugeValue, float64(counts[outcome]), ns, name, outcome)
		}
		// One series per check and severity, however many findings
		// (pods, backups) the check reported at that severity.
		type series struct{ check, severity string }
		seen := map[series]bool{}
		for _, finding := range run.Result.Findings {
			s := series{finding.Check, finding.Severity.String()}
			if s.check == "" || seen[s] {
				continue
			}
			seen[s] = true
			ch <- prometheus.MustNewConstMetric(findingDesc, prometheus.GaugeValue, 1, ns, name, s.check, s.severity)
		}
	}
}
