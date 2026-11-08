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

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// bothWindow is a MetricsWindow serving instant readings and series
// samples together, which is what a corroborating rule reads.
type bothWindow struct {
	instants map[string]map[string]metrics.Instant
	series   map[string]map[string][]float64
	times    []int64
}

func (w bothWindow) Interval() time.Duration { return metrics.DefaultInterval }

func (w bothWindow) Instances() []string { return nil }

func (w bothWindow) Range(key string, tier metrics.Tier) ([]int64, map[string][]*float64) {
	if tier != metrics.TierRaw {
		return nil, nil
	}
	out := map[string][]*float64{}
	for instance, byKey := range w.series {
		samples, ok := byKey[key]
		if !ok {
			continue
		}
		column := make([]*float64, len(samples))
		for i := range samples {
			value := samples[i]
			column[i] = &value
		}
		out[instance] = column
	}
	return w.times, out
}

func (w bothWindow) InstantReadings() map[string]map[string]metrics.Instant { return w.instants }

// held builds a window whose one instance carries the given samples,
// spaced five minutes apart and ending now.
func held(key string, samples ...float64) bothWindow {
	times := make([]int64, len(samples))
	for i := range samples {
		times[i] = now.Add(-time.Duration(len(samples)-1-i) * 5 * time.Minute).Unix()
	}
	return bothWindow{
		series: map[string]map[string][]float64{"orders-2": {key: samples}},
		times:  times,
	}
}

// TestSeriesAboveHeldRejectsASpike is the hysteresis property: a
// threshold with a holding window matches only a value that stayed past
// it across every retained sample of that window, says so in the
// evidence, and reports that it could not judge an instance whose
// window is too short rather than matching on one sample.
func TestSeriesAboveHeldRejectsASpike(t *testing.T) {
	t.Parallel()
	sustained := Rule{ID: "lag", Summary: "Lag.", When: SeriesAbove{
		Key: "replication-lag", Threshold: 300, For: 15 * time.Minute,
	}}
	latest := Rule{ID: "lag", Summary: "Lag.", When: SeriesAbove{Key: "replication-lag", Threshold: 300}}

	// Four samples five minutes apart: the window holds the last four,
	// all past the threshold.
	in := Input{Now: now, Metrics: held("replication-lag", 900, 800, 700, 600)}
	check, findings := evaluateRule(sustained, in)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v (%s) on a held breach, want matched", check.Outcome, check.Because)
	}
	if detail := findings[0].Evidence[0].Detail; !strings.Contains(detail, "every one of the") {
		t.Errorf("evidence does not state that the value held: %q", detail)
	}
	if !strings.Contains(check.Describes, "held for 15m0s") {
		t.Errorf("describes = %q, want the holding window stated", check.Describes)
	}

	// A dip inside the window is not a held breach, even though the
	// latest sample is past the threshold.
	spike := Input{Now: now, Metrics: held("replication-lag", 900, 10, 700, 600)}
	if check, _ := evaluateRule(sustained, spike); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with a dip inside the window, want clear", check.Outcome)
	}
	// The same readings without a holding window match on the latest
	// sample alone, which is what the window exists to prevent.
	if check, _ := evaluateRule(latest, spike); check.Outcome != CheckMatched {
		t.Errorf("outcome = %v without a holding window, want matched on the latest sample", check.Outcome)
	}

	// One sample inside the window cannot show that anything was held.
	lone := Input{Now: now, Metrics: bothWindow{
		series: map[string]map[string][]float64{"orders-2": {"replication-lag": {900}}},
		times:  []int64{now.Unix()},
	}}
	check, _ = evaluateRule(sustained, lone)
	if check.Outcome != CheckUnavailable || !strings.Contains(check.Because, "fewer than two samples") {
		t.Errorf("outcome = %v (%q), want could-not-run naming the short window", check.Outcome, check.Because)
	}
	// A series the console holds nothing for stays clear: that is the
	// reading being absent, not a window too short to judge.
	empty := Input{Now: now, Metrics: bothWindow{}}
	if check, _ := evaluateRule(sustained, empty); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with no samples at all, want clear", check.Outcome)
	}
}

// TestInstantZeroTreatsUnreportedAsUnreported proves the inverse flag
// condition does not read a missing metric as a zero.
func TestInstantZeroTreatsUnreportedAsUnreported(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "receiver", Summary: "Receiver.", When: InstantZero{Key: "wal-receiver-up"}}
	if check, _ := evaluateRule(rule, Input{Now: now}); check.Outcome != CheckUnavailable {
		t.Errorf("outcome = %v without metrics, want could-not-run", check.Outcome)
	}
	readings := func(value float64) Input {
		return Input{Now: now, Metrics: staticWindow{
			"orders-2": {"wal-receiver-up": {At: now.Unix(), Value: value}},
		}}
	}
	if check, _ := evaluateRule(rule, readings(1)); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with the receiver up, want clear", check.Outcome)
	}
	check, findings := evaluateRule(rule, readings(0))
	if check.Outcome != CheckMatched || len(findings) != 1 || findings[0].Subject.Name != "orders-2" {
		t.Fatalf("outcome = %v, findings %+v; want one on orders-2", check.Outcome, findings)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, "cnpg_pg_replication_is_wal_receiver_up = 0") {
		t.Errorf("evidence does not quote the exporter's name and reading: %q", findings[0].Evidence[0].Detail)
	}
	// The instance reports other metrics but not this one: unreported
	// is not off.
	unreported := Input{Now: now, Metrics: staticWindow{
		"orders-2": {"in-recovery": {At: now.Unix(), Value: 1}},
	}}
	if check, _ := evaluateRule(rule, unreported); check.Outcome != CheckClear {
		t.Errorf("outcome = %v with the flag unreported, want clear rather than a match", check.Outcome)
	}
}

// TestInstantShortfallNeedsBothReadings proves the comparison judges an
// instance only when it reports both numbers, and treats an instance
// reporting neither as one where the feature is not configured.
func TestInstantShortfallNeedsBothReadings(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "sync", Summary: "Sync.", When: InstantShortfall{
		Expected: "sync-replicas-expected", Observed: "sync-replicas-observed",
		Noun: "synchronous replicas",
	}}
	window := func(readings map[string]metrics.Instant) Input {
		return Input{Now: now, Metrics: staticWindow{"orders-1": readings}}
	}
	at := metrics.Instant{At: now.Unix()}
	expected, observed := "sync-replicas-expected", "sync-replicas-observed"

	for name, readings := range map[string]map[string]metrics.Instant{
		"neither reported": {"in-recovery": at},
		"expected only":    {expected: {At: at.At, Value: 2}},
		"observed only":    {observed: {At: at.At, Value: 1}},
		"met":              {expected: {At: at.At, Value: 2}, observed: {At: at.At, Value: 2}},
		"exceeded":         {expected: {At: at.At, Value: 1}, observed: {At: at.At, Value: 2}},
		"none expected":    {expected: {At: at.At, Value: 0}, observed: {At: at.At, Value: 0}},
	} {
		if check, _ := evaluateRule(rule, window(readings)); check.Outcome != CheckClear {
			t.Errorf("%s: outcome = %v, want clear", name, check.Outcome)
		}
	}

	short := window(map[string]metrics.Instant{
		expected: {At: at.At, Value: 2}, observed: {At: at.At, Value: 1},
	})
	check, findings := evaluateRule(rule, short)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v, want matched on the shortfall", check.Outcome)
	}
	if !strings.Contains(findings[0].Summary, "1 of the 2 synchronous replicas") {
		t.Errorf("summary does not state both numbers: %q", findings[0].Summary)
	}
	if !strings.Contains(findings[0].Evidence[0].Detail, "expected 2, observed 1") {
		t.Errorf("evidence does not quote both readings: %q", findings[0].Evidence[0].Detail)
	}
}

// TestAllOfRequiresOneSubject is the corroboration property: branches
// matching on different instances are two facts, not one finding. Only
// a branch about the same instance — or one about no single object at
// all, such as a cluster-wide condition — corroborates.
func TestAllOfRequiresOneSubject(t *testing.T) {
	t.Parallel()
	rule := Rule{ID: "stalled", Summary: "Stalled.", When: AllOf{Of: []Condition{
		InstantNonZero{Key: "in-recovery"},
		InstantZero{Key: "wal-receiver-up"},
	}}}

	// The two readings land on different instances: neither instance
	// shows both, so there is nothing to report.
	split := Input{Now: now, Metrics: staticWindow{
		"orders-2": {"in-recovery": {At: now.Unix(), Value: 1}, "wal-receiver-up": {At: now.Unix(), Value: 1}},
		"orders-3": {"in-recovery": {At: now.Unix(), Value: 0}, "wal-receiver-up": {At: now.Unix(), Value: 0}},
	}}
	if check, findings := evaluateRule(rule, split); check.Outcome != CheckClear {
		t.Errorf("outcome = %v across two instances, want clear: %+v", check.Outcome, findings)
	}

	// Both on one instance, with a third instance carrying one of them.
	together := Input{Now: now, Metrics: staticWindow{
		"orders-2": {"in-recovery": {At: now.Unix(), Value: 1}, "wal-receiver-up": {At: now.Unix(), Value: 0}},
		"orders-3": {"in-recovery": {At: now.Unix(), Value: 0}, "wal-receiver-up": {At: now.Unix(), Value: 1}},
	}}
	check, findings := evaluateRule(rule, together)
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with %d findings, want one on the instance carrying both", check.Outcome, len(findings))
	}
	if findings[0].Subject.Name != "orders-2" || len(findings[0].Evidence) != 2 {
		t.Errorf("finding = %+v, want orders-2 with both readings quoted", findings[0])
	}

	// A branch about no single object corroborates any instance.
	cluster := clusterInput(17)
	cluster.Cluster.Cluster.Phase = "Waiting for user action"
	cluster.Metrics = together.Metrics
	wide := Rule{ID: "wide", Summary: "Wide.", When: AllOf{Of: []Condition{
		InstantZero{Key: "wal-receiver-up"},
		ClusterPhase{AnyOf: []string{"Waiting for user action"}},
	}}}
	if check, findings := evaluateRule(wide, cluster); check.Outcome != CheckMatched || len(findings) != 1 {
		t.Errorf("outcome = %v with %d findings, want the cluster fact to corroborate the instance",
			check.Outcome, len(findings))
	}
}

// TestLogFieldsDescribesAMalformedTestAsMalformed proves the check row
// names a mis-specified field rather than rendering one of its halves,
// which would describe a test the rule never made.
func TestLogFieldsDescribesAMalformedTestAsMalformed(t *testing.T) {
	t.Parallel()
	sound := LogFields{Fields: []LogField{{Path: "msg", Equals: "starting"}}}
	if got := sound.describe(); !strings.Contains(got, `msg is "starting"`) {
		t.Errorf("describe = %q, want the field and its value", got)
	}
	partial := LogFields{Fields: []LogField{{Path: "msg", Contains: "start"}}}
	if got := partial.describe(); !strings.Contains(got, `msg contains "start"`) {
		t.Errorf("describe = %q, want the substring form", got)
	}
	for name, field := range map[string]LogField{
		"neither":  {Path: "msg"},
		"both":     {Path: "msg", Equals: "starting", Contains: "start"},
		"no path":  {Equals: "starting"},
		"nothing":  {},
		"contains": {Contains: "start"},
	} {
		got := LogFields{Fields: []LogField{field}}.describe()
		if !strings.Contains(got, "malformed") || strings.Contains(got, `contains ""`) {
			t.Errorf("%s: describe = %q, want it named as malformed", name, got)
		}
	}
}
