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

// pacedWindow is a MetricsWindow with a settable cadence, so a test can
// watch the freshness bound calibrate against the rate the console
// claims to sweep at.
type pacedWindow struct {
	interval time.Duration
	instants map[string]map[string]metrics.Instant
	times    []int64
	series   map[string][]*float64
}

func (w pacedWindow) Interval() time.Duration { return w.interval }

func (w pacedWindow) Instances() []string { return nil }

func (w pacedWindow) Range(_ string, tier metrics.Tier) ([]int64, map[string][]*float64) {
	if tier != metrics.TierRaw {
		return nil, nil
	}
	return w.times, w.series
}

func (w pacedWindow) InstantReadings() map[string]map[string]metrics.Instant { return w.instants }

// swept is a window holding one instance's readings, all taken at the
// given age, at the default sweep cadence.
func swept(age time.Duration, values map[string]float64, series float64) pacedWindow {
	at := now.Add(-age).Unix()
	instants := map[string]metrics.Instant{}
	for key, value := range values {
		instants[key] = metrics.Instant{At: at, Value: value}
	}
	value := series
	return pacedWindow{
		interval: metrics.DefaultInterval,
		instants: map[string]map[string]metrics.Instant{"orders-1": instants},
		times:    []int64{at},
		series:   map[string][]*float64{"orders-1": {&value}},
	}
}

// primaryInput is a cluster input naming orders-1 the settled primary,
// which is what PrimaryDisagreement compares a scraped reading against.
func primaryInput(window MetricsWindow) Input {
	in := clusterInput(17)
	in.Cluster.Cluster.CurrentPrimary = "orders-1"
	in.Cluster.Cluster.TargetPrimary = "orders-1"
	in.Metrics = window
	return in
}

// TestStaleScrapedReadingsWithdrawTheCheckRatherThanAnswerIt is the
// honesty invariant applied to the one source that cannot look stale.
// A frozen exporter answers every read with a plausible number, so each
// condition is put twice past the horizon: once holding a value that
// would have matched, and once holding the value that would have
// cleared. Both must withdraw. The second is the dangerous one — a
// clear here would tell the reader the console had looked and found
// nothing, when what it found was an hour-old number.
func TestStaleScrapedReadingsWithdrawTheCheckRatherThanAnswerIt(t *testing.T) {
	t.Parallel()
	const stale = time.Hour
	for name, tc := range map[string]struct {
		when            Condition
		matching, clear Input
	}{
		"instant-non-zero": {
			when:     InstantNonZero{Key: "fencing-on"},
			matching: Input{Now: now, Metrics: swept(stale, map[string]float64{"fencing-on": 1}, 0)},
			clear:    Input{Now: now, Metrics: swept(stale, map[string]float64{"fencing-on": 0}, 0)},
		},
		"instant-zero": {
			when:     InstantZero{Key: "wal-receiver-up"},
			matching: Input{Now: now, Metrics: swept(stale, map[string]float64{"wal-receiver-up": 0}, 0)},
			clear:    Input{Now: now, Metrics: swept(stale, map[string]float64{"wal-receiver-up": 1}, 0)},
		},
		"instant-shortfall": {
			when: InstantShortfall{
				Expected: "sync-replicas-expected", Observed: "sync-replicas-observed",
				Noun: "synchronous replicas",
			},
			matching: Input{Now: now, Metrics: swept(stale, map[string]float64{
				"sync-replicas-expected": 2, "sync-replicas-observed": 1,
			}, 0)},
			clear: Input{Now: now, Metrics: swept(stale, map[string]float64{
				"sync-replicas-expected": 2, "sync-replicas-observed": 2,
			}, 0)},
		},
		"primary-disagreement": {
			when:     PrimaryDisagreement{},
			matching: primaryInput(swept(stale, map[string]float64{"in-recovery": 1}, 0)),
			clear:    primaryInput(swept(stale, map[string]float64{"in-recovery": 0}, 0)),
		},
		"series-above": {
			when:     SeriesAbove{Key: "replication-lag", Threshold: 300},
			matching: Input{Now: now, Metrics: swept(stale, nil, 900)},
			clear:    Input{Now: now, Metrics: swept(stale, nil, 1)},
		},
	} {
		rule := Rule{ID: "stale", Summary: "Stale.", When: tc.when}
		for _, side := range []struct {
			what string
			in   Input
		}{{"a reading that would have matched", tc.matching}, {"a reading that would have cleared", tc.clear}} {
			check, findings := evaluateRule(rule, side.in)
			if check.Outcome != CheckUnavailable {
				t.Errorf("%s: outcome on %s an hour past the sweep = %v with %d findings, want could-not-run",
					name, side.what, check.Outcome, len(findings))
				continue
			}
			// The reason has to say what went missing, or "could not
			// run" is just a shrug: the age is the operator's cue that
			// the exporter, not the check, is what needs attention.
			if !strings.Contains(check.Because, "1h0m0s old") {
				t.Errorf("%s: reason on %s = %q, want the reading's age in it", name, side.what, check.Because)
			}
		}
	}
}

// TestAReadingInsideTheSweepIsStillRead guards the other direction: the
// bound must not be so eager that a current reading stops being usable,
// which would trade a false clear for a screen that never says anything.
func TestAReadingInsideTheSweepIsStillRead(t *testing.T) {
	t.Parallel()
	in := Input{Now: now, Metrics: swept(30*time.Second, map[string]float64{"fencing-on": 1}, 900)}
	for name, when := range map[string]Condition{
		"instant": InstantNonZero{Key: "fencing-on"},
		"series":  SeriesAbove{Key: "replication-lag", Threshold: 300},
	} {
		check, findings := evaluateRule(Rule{ID: "fresh", Summary: "Fresh.", When: when}, in)
		if check.Outcome != CheckMatched || len(findings) != 1 {
			t.Errorf("%s: outcome on a reading half a sweep old = %v with %d findings (%s), want one match",
				name, check.Outcome, len(findings), check.Because)
		}
	}
}

// TestTheFreshnessBoundFollowsTheScrapeCadence proves the horizon is
// taken from the window rather than written into the catalog: the same
// four-minute-old reading is past the bound on a ten-second sweep and
// inside it on a two-minute one, because how long a reading stays
// current depends on how often the console asks.
func TestTheFreshnessBoundFollowsTheScrapeCadence(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		interval time.Duration
		want     CheckOutcome
	}{
		"a fast sweep leaves it behind": {metrics.DefaultInterval, CheckUnavailable},
		"a slow sweep still owns it":    {2 * time.Minute, CheckMatched},
	} {
		window := swept(4*time.Minute, map[string]float64{"fencing-on": 1}, 0)
		window.interval = tc.interval
		check, _ := evaluateRule(
			Rule{ID: "paced", Summary: "Paced.", When: InstantNonZero{Key: "fencing-on"}},
			Input{Now: now, Metrics: window})
		if check.Outcome != tc.want {
			t.Errorf("%s: outcome at a %s cadence = %v (%s), want %v",
				name, tc.interval, check.Outcome, check.Because, tc.want)
		}
	}
}

// TestTheFreshnessBoundNeverFallsBelowAMinute holds the floor. Three
// sweeps of a one-second cadence is three seconds, and withdrawing a
// check over three seconds of jitter would cost the reader far more
// than it told them: how long a reading stays a fair statement of now
// is a property of the claim, not of how often the console asks.
func TestTheFreshnessBoundNeverFallsBelowAMinute(t *testing.T) {
	t.Parallel()
	window := swept(45*time.Second, map[string]float64{"fencing-on": 1}, 0)
	window.interval = time.Second
	check, findings := evaluateRule(
		Rule{ID: "floor", Summary: "Floor.", When: InstantNonZero{Key: "fencing-on"}},
		Input{Now: now, Metrics: window})
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Errorf("outcome on a 45s reading at a 1s cadence = %v with %d findings (%s), want one match",
			check.Outcome, len(findings), check.Because)
	}
}

// TestAFrozenInstanceDoesNotSilenceALiveOne proves the judgement is per
// reading and not per window: the scraper sweeps every instance, so one
// lost exporter freezes that instance alone, and a finding on an
// instance still answering must survive it.
func TestAFrozenInstanceDoesNotSilenceALiveOne(t *testing.T) {
	t.Parallel()
	window := pacedWindow{
		interval: metrics.DefaultInterval,
		instants: map[string]map[string]metrics.Instant{
			"orders-1": {"fencing-on": {At: now.Add(-time.Hour).Unix(), Value: 1}},
			"orders-2": {"fencing-on": {At: now.Add(-20 * time.Second).Unix(), Value: 1}},
		},
	}
	check, findings := evaluateRule(
		Rule{ID: "mixed", Summary: "Mixed.", When: InstantNonZero{Key: "fencing-on"}},
		Input{Now: now, Metrics: window})
	if check.Outcome != CheckMatched || len(findings) != 1 {
		t.Fatalf("outcome = %v with %d findings (%s), want the live instance's one match",
			check.Outcome, len(findings), check.Because)
	}
	if findings[0].Subject.Name != "orders-2" {
		t.Errorf("subject = %s, want the instance still being swept", findings[0].Subject)
	}
}

// TestTheReasonClaimsOnlyWhatWasRefused is the mixed run: one instance
// frozen, another answering and simply reporting nothing. The check
// still withdraws — an instance that went unjudged is why it cannot
// clear — but the reason must not say the feed is dead, because an
// operator reading that would go looking for a scraper that is mostly
// working.
func TestTheReasonClaimsOnlyWhatWasRefused(t *testing.T) {
	t.Parallel()
	window := pacedWindow{
		interval: metrics.DefaultInterval,
		instants: map[string]map[string]metrics.Instant{
			"orders-1": {"fencing-on": {At: now.Add(-time.Hour).Unix(), Value: 1}},
			// Current, and nothing to report: the reading the reason
			// must not talk over.
			"orders-2": {"fencing-on": {At: now.Add(-20 * time.Second).Unix(), Value: 0}},
		},
	}
	check, findings := evaluateRule(
		Rule{ID: "mixed", Summary: "Mixed.", When: InstantNonZero{Key: "fencing-on"}},
		Input{Now: now, Metrics: window})
	if check.Outcome != CheckUnavailable || len(findings) != 0 {
		t.Fatalf("outcome = %v with %d findings, want could-not-run: an unjudged instance cannot be cleared over",
			check.Outcome, len(findings))
	}
	if !strings.Contains(check.Because, "1h0m0s old") {
		t.Errorf("reason = %q, want the refused reading's age", check.Because)
	}
	// The wording may change; what may not is claiming the whole window
	// is behind when one of its instances is current.
	for _, overstated := range []string{"the newest reading", "nothing here is a claim about now"} {
		if strings.Contains(check.Because, overstated) {
			t.Errorf("reason = %q, which claims %q of a window still holding a current reading",
				check.Because, overstated)
		}
	}
}

// TestTheReasonCountsEveryRefusedReading pins the plural: a run that
// withheld several says how many, so the reader can tell one frozen
// exporter from a scraper that has stopped altogether.
func TestTheReasonCountsEveryRefusedReading(t *testing.T) {
	t.Parallel()
	stale := metrics.Instant{At: now.Add(-time.Hour).Unix(), Value: 1}
	window := pacedWindow{
		interval: metrics.DefaultInterval,
		instants: map[string]map[string]metrics.Instant{
			"orders-1": {"fencing-on": stale},
			"orders-2": {"fencing-on": stale},
			"orders-3": {"fencing-on": stale},
		},
	}
	check, _ := evaluateRule(
		Rule{ID: "counted", Summary: "Counted.", When: InstantNonZero{Key: "fencing-on"}},
		Input{Now: now, Metrics: window})
	if check.Outcome != CheckUnavailable {
		t.Fatalf("outcome = %v, want could-not-run", check.Outcome)
	}
	if !strings.Contains(check.Because, "3 readings") {
		t.Errorf("reason = %q, want the count of what was refused", check.Because)
	}
}

// TestAnUnpacedWindowIsJudgedAtTheDefaultCadence covers the window that
// states no cadence: the bound falls back to the package default rather
// than to zero, which would refuse every reading ever taken and turn
// the whole screen into "could not run".
func TestAnUnpacedWindowIsJudgedAtTheDefaultCadence(t *testing.T) {
	t.Parallel()
	fresh := freshnessOf(pacedWindow{}, now, "of a metric")
	if want := freshnessFloor; fresh.horizon != want {
		t.Errorf("horizon of an unpaced window = %s, want %s", fresh.horizon, want)
	}
	if fresh.unavailable() != "" {
		t.Errorf("a freshness that refused nothing gave a reason: %q", fresh.unavailable())
	}
}

// TestTheReasonQuotesTheNewestRefusedReading pins which age the reason
// carries. Several instances may be frozen at different times, and the
// strongest true thing to say is that even the most recent of them is
// too old — quoting the oldest would overstate how far behind the
// console actually is.
func TestTheReasonQuotesTheNewestRefusedReading(t *testing.T) {
	t.Parallel()
	window := pacedWindow{
		interval: metrics.DefaultInterval,
		instants: map[string]map[string]metrics.Instant{
			"orders-1": {"fencing-on": {At: now.Add(-3 * time.Hour).Unix(), Value: 1}},
			"orders-2": {"fencing-on": {At: now.Add(-10 * time.Minute).Unix(), Value: 1}},
		},
	}
	check, _ := evaluateRule(
		Rule{ID: "newest", Summary: "Newest.", When: InstantNonZero{Key: "fencing-on"}},
		Input{Now: now, Metrics: window})
	if check.Outcome != CheckUnavailable {
		t.Fatalf("outcome = %v, want could-not-run", check.Outcome)
	}
	if !strings.Contains(check.Because, "10m0s old") {
		t.Errorf("reason = %q, want the newest refused reading's age", check.Because)
	}
}
