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

package drills

import (
	"fmt"
	"math"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The check, without SQL (the console runs none):
//
//  1. healthy: the drill Cluster reaches CloudNativePG's healthy phase
//     with its one instance ready (Cluster.status);
//  2. promoted: the instance reports through /pg/status that it is a
//     primary on a timeline, i.e. recovery ended and it was promoted;
//  3. size: the instance's database size from the exporter (the sum
//     over databases of cnpg_pg_database_size_bytes) is within
//     SizeTolerancePercent of the source primary's when the drill
//     started. Recovery replays the archived WAL to its end, so the
//     restored size should track the source's current one; churn since
//     the last archived segment and bloat differences are what the
//     tolerance absorbs.
//
// What a passed drill does not prove is said in NotChecked.

// SizeTolerancePercent is the size check's tolerance.
const SizeTolerancePercent = 10

// sizeGrace is how long the size check waits for the drill's exporter
// after promotion before it says "not checked".
const sizeGrace = 5 * time.Minute

// NotChecked is what the check does not verify, shown with every result.
var NotChecked = []string{
	"the content of the data: no query is run (the console runs no SQL)",
	"each database separately: the size check compares the sum over all databases",
	"a recovery to a point in time other than the end of the archive",
	"WAL written after the last archived segment",
	"the replicas, poolers and applications of the source",
}

// Reading is the drill instance's /pg/status report.
type Reading struct {
	IsPrimary  bool
	TimelineID int
	ObservedAt time.Time
}

// Observation is what the console reads about a drill Cluster.
type Observation struct {
	Phase          string
	ReadyInstances int
	Reading        *Reading
	// ReadProblem is why the sweep has no reading of the instance (a TLS
	// error, a CA the bundle does not hold), secret-free; empty while it
	// has one or has not tried yet.
	ReadProblem   string
	DatabaseBytes *int64
}

// Judge advances r with o observed at now and decides the result when it
// can. It is pure; the supervisor persists and acts on its output.
func Judge(r Record, o Observation, now time.Time) Record {
	if r.Finished() {
		return r
	}
	r.Result = viewmodel.DrillRunning
	if o.DatabaseBytes != nil {
		b := *o.DatabaseBytes
		r.RestoredBytes = &b
	}
	healthy := viewmodel.DrillCheck{Name: viewmodel.DrillCheckHealthy, Source: "Cluster.status.phase, Cluster.status.readyInstances"}
	promoted := viewmodel.DrillCheck{Name: viewmodel.DrillCheckPromoted, Source: "/pg/status (isPrimary, timeLineID)"}
	size := viewmodel.DrillCheck{Name: viewmodel.DrillCheckSize, Source: "exporter cnpg_pg_database_size_bytes, sum over databases"}

	if r.HealthyAt.IsZero() && o.Phase == cnpgv1.PhaseHealthy && o.ReadyInstances >= 1 {
		r.HealthyAt = now
	}
	switch {
	case !r.HealthyAt.IsZero():
		healthy.Outcome, healthy.Text = viewmodel.DrillCheckPassed, "healthy after "+HumanDuration(r.HealthyAt.Sub(r.StartedAt))
	case o.Phase == "":
		healthy.Outcome, healthy.Text = viewmodel.DrillCheckPending, "the operator has not reported a phase yet"
	default:
		healthy.Outcome, healthy.Text = viewmodel.DrillCheckPending, "phase: "+o.Phase
	}

	promoted.Outcome, promoted.Text = viewmodel.DrillCheckPending, "waits for the Cluster to be healthy"
	if !r.HealthyAt.IsZero() {
		switch {
		case !r.PromotedAt.IsZero():
		case o.Reading == nil && o.ReadProblem != "":
			promoted.Text = "no /pg/status reading of the instance yet: " + o.ReadProblem
		case o.Reading == nil:
			promoted.Text = "no /pg/status reading of the instance yet"
		case o.Reading.IsPrimary && o.Reading.TimelineID > 0:
			r.PromotedAt = now
			tl := o.Reading.TimelineID
			r.Timeline = &tl
		default:
			promoted.Text = "the instance does not report itself as a primary yet (still in recovery)"
		}
		if !r.PromotedAt.IsZero() {
			promoted.Outcome = viewmodel.DrillCheckPassed
			promoted.Text = "recovery ended; primary"
			if r.Timeline != nil {
				promoted.Text += fmt.Sprintf(" on timeline %d", *r.Timeline)
			}
		}
	}

	size.Outcome, size.Text = viewmodel.DrillCheckPending, "waits for the promotion"
	if !r.PromotedAt.IsZero() {
		switch {
		case r.SourceBytes == nil:
			size.Outcome, size.Text = viewmodel.DrillCheckNotChecked, "the source's exporter reported no database size when the drill started"
		case o.DatabaseBytes == nil && now.Sub(r.PromotedAt) < sizeGrace:
			size.Text = "waits for the drill's exporter"
		case o.DatabaseBytes == nil:
			size.Outcome, size.Text = viewmodel.DrillCheckNotChecked, "the drill's exporter reported no database size"
		default:
			src, got := float64(*r.SourceBytes), float64(*o.DatabaseBytes)
			within := src == 0 && got == 0 || src > 0 && math.Abs(got-src)/src*100 <= SizeTolerancePercent
			size.Text = fmt.Sprintf("restored %s, source %s at the start (tolerance ±%d%%)", humanBytes(*o.DatabaseBytes), humanBytes(*r.SourceBytes), SizeTolerancePercent)
			size.Outcome = viewmodel.DrillCheckPassed
			if !within {
				size.Outcome = viewmodel.DrillCheckFailed
			}
		}
	}
	r.Checks = []viewmodel.DrillCheck{healthy, promoted, size}

	pending, failed := false, ""
	for _, c := range r.Checks {
		switch c.Outcome {
		case viewmodel.DrillCheckPending:
			pending = true
		case viewmodel.DrillCheckFailed:
			if failed == "" {
				failed = string(c.Name) + ": " + c.Text
			}
		case viewmodel.DrillCheckPassed, viewmodel.DrillCheckNotChecked:
			// Neither holds the verdict back nor fails it.
		}
	}
	switch {
	case failed != "":
		r.Result, r.Reason, r.FinishedAt = viewmodel.DrillFailed, failed, now
	case !pending:
		r.Result, r.FinishedAt = viewmodel.DrillPassed, now
	case !now.Before(r.Deadline):
		r.Result, r.FinishedAt = viewmodel.DrillTimedOut, now
		for i := range r.Checks {
			if r.Checks[i].Outcome == viewmodel.DrillCheckPending {
				if r.Reason == "" {
					r.Reason = string(r.Checks[i].Name) + " not reached within the time limit (" + HumanDuration(r.Deadline.Sub(r.StartedAt)) + "): " + r.Checks[i].Text
				}
				r.Checks[i].Outcome = viewmodel.DrillCheckFailed
				r.Checks[i].Text = "not reached within the time limit; last seen: " + r.Checks[i].Text
			}
		}
	}
	return r
}

// LeftInPlace is the record of a drill whose Cluster stopped verifying
// as this console's: unfinished, it is cancelled; either way it was not
// deleted.
func LeftInPlace(r Record, now time.Time) Record {
	if !r.Finished() {
		r.Result, r.FinishedAt = viewmodel.DrillCancelled, now
		r.Reason = "the drill Cluster stopped verifying as this console's drill before its check finished"
	}
	r.LeftInPlace = true
	r.TeardownError = leftInPlaceText
	return r
}

// Vanished is the record of a drill whose Cluster was read by name and
// found gone (NotFound, or a later incarnation under its name): a running
// drill was deleted by someone before its result; a decided one is torn
// down, at the latest now.
func Vanished(r Record, now time.Time) Record {
	if r.Finished() {
		if r.TornDownAt.IsZero() {
			r.TornDownAt = now
		}
		r.TeardownError = ""
		return r
	}
	r.Result, r.FinishedAt, r.TornDownAt = viewmodel.DrillCancelled, now, now
	r.Reason = "the drill Cluster was deleted before its check finished"
	return r
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

// HumanDuration is d to the second under a minute, else to the minute,
// without zero units: 45s, 14m, 2h, 1h30m.
func HumanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	text := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}
	return text
}
