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
	"fmt"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/insight"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// Every snapshot a check reads carries its own staleness: the collector
// lost contact, and what it holds is the retained last-good observation.
// Stale data is fine to display with its age beside it. It is not fine
// to clear a check with, because "nothing matched in what was last seen"
// is not "nothing is wrong now" — and a match found in it is a claim
// about the past dressed as the present. So a stale source reads as
// unavailable, exactly as an unobserved one does, and the check says
// which of the two it was.
//
// Each source answers the question once, here, so a condition and a
// hand-written detector reading the same snapshot refuse it for the same
// reason. A new condition over one of these sources starts with the
// matching helper; a condition over a new source adds one.

// A check that could not run has two quite different reasons for it, and
// the screen has to tell them apart or neither gets read.
//
// A source can be switched off — log following, the object timeline, the
// scrapers, the repository-evidence consumer are all deliberate choices,
// and a deployment that has not made them is not faulty. Those reasons
// are permanent, identical on every refresh, and there is nothing to
// react to: they are a decision to make once.
//
// Or a source that is on can fail to answer: contact lost, nothing
// observed yet, a sweep that stopped. That is a fault, it is new, and it
// is the reason an operator needs to see now.
//
// Rendering both as one list of "could not run" buries the second in the
// first, on every screen of every healthy cluster. So the reasons for
// the first kind are named here as constants, returned by the helpers
// below and recognised by sourceOff — producer and classifier sharing
// one string, so they cannot drift apart. A reason nobody declared reads
// as a fault, which is the safe direction to be wrong in: an unread
// notice is better than a hidden failure.
const (
	reasonEvidenceOff      = "the repository-evidence consumer is not configured"
	reasonHistoryOff       = "the object timeline is not recorded, so nothing can be counted over time"
	reasonLogsOff          = "log following is off, so nothing in the logs has been read"
	reasonMetricsOff       = "instance metrics are not scraped"
	reasonPoolerMetricsOff = "pooler metrics are not scraped"
	reasonStatusOff        = "the instance managers' status reports are not read"
	reasonInsightOff       = "the Cluster has not opted into the insight pack (" + insight.SpecChange + ")"
	// reasonInsightUnsupported is PostgreSQL 13 or earlier: the pack
	// reports its version there and nothing else.
	reasonInsightUnsupported = "the instances run PostgreSQL 13 or earlier, which the insight pack does not cover"
)

// sourceOff reports whether a reason names a source that is switched off
// rather than one that is on and not answering.
func sourceOff(reason string) bool {
	switch reason {
	case reasonEvidenceOff, reasonHistoryOff, reasonLogsOff, reasonMetricsOff, reasonPoolerMetricsOff, reasonStatusOff,
		reasonInsightOff, reasonInsightUnsupported, reasonVolumesNotReported,
		reasonDrillsOff, reasonDrillsInMemory, reasonDrillsRecordShort:
		return true
	}
	return false
}

// eventsUnavailable is the reason the event list cannot be read, empty
// when it can.
func eventsUnavailable(in Input) string {
	switch {
	case !in.HasEvents:
		return "events have not been observed yet"
	case in.Events.Stale:
		return "the event list is stale, so current events are unknown"
	}
	return ""
}

// podsUnavailable is the reason the instance pods cannot be read, empty
// when they can.
func podsUnavailable(in Input) string {
	switch {
	case !in.HasPods:
		return "instance pods have not been observed yet"
	case in.Pods.Stale:
		return "the instance pods are stale, so current container states are unknown"
	}
	return ""
}

// poolerPodsUnavailable is the reason the pooler pods cannot be read,
// empty when they can. Absence is not a reason: poolers are optional,
// and a cluster without them has nothing to observe. Only a snapshot
// that exists and has gone stale withholds a result.
func poolerPodsUnavailable(in Input) string {
	if in.HasPoolerPods && in.PoolerPods.Stale {
		return "the pooler pods are stale, so current container states are unknown"
	}
	return ""
}

// clusterUnavailable is the reason the Cluster's status cannot be read,
// empty when it can. Staleness is checked before presence: a stale
// snapshot that says "no Cluster" is not evidence that there is none.
func clusterUnavailable(in Input) string {
	switch {
	case !in.HasCluster:
		return "the Cluster has not been observed yet"
	case in.Cluster.Stale:
		return "the Cluster snapshot is stale, so its current status is unknown"
	case !in.Cluster.Cluster.Present:
		return "the API server reports no Cluster object"
	}
	return ""
}

// poolersUnavailable is the reason the Pooler set cannot be read, empty
// when it can. An empty set is readable: a cluster with no poolers has
// nothing to be short of.
func poolersUnavailable(in Input) string {
	switch {
	case !in.HasPoolers:
		return "poolers have not been observed yet"
	case in.Poolers.Stale:
		return "the poolers are stale, so current instance counts are unknown"
	}
	return ""
}

// quorumUnavailable is the reason the failover quorum cannot be read,
// empty when it can. Absence of the resource is readable — it is the
// operator's way of saying the cluster runs no quorum.
func quorumUnavailable(in Input) string {
	switch {
	case !in.HasFailoverQuorum:
		return "the failover quorum has not been observed yet"
	case in.FailoverQuorum.Stale:
		return "the failover quorum is stale, so the current standby set is unknown"
	}
	return ""
}

// leaseUnavailable is the reason the primary lease cannot be read,
// empty when it can. Absence of the Lease is readable: an operator
// before 1.30 keeps none, and the version pins on the lease checks are
// what say whether one is expected.
func leaseUnavailable(in Input) string {
	switch {
	case !in.HasPrimaryLease:
		return "the primary lease has not been observed yet"
	case in.PrimaryLease.Stale:
		return "the primary lease snapshot is stale, so the current holder is unknown"
	}
	return ""
}

// imageCatalogsUnavailable is the reason the image catalogs cannot be
// read, empty when they can.
func imageCatalogsUnavailable(in Input) string {
	switch {
	case !in.HasImageCatalogs:
		return "image catalogs have not been observed yet"
	case in.ImageCatalogs.Stale:
		return "the image catalogs are stale, so their current content is unknown"
	}
	return ""
}

// evidenceUnavailable is the reason the repository-evidence report cannot
// be read, empty when it can. The channel has more ways to be silent
// than a watch does — not configured, never answered, contact lost, no
// scan completed yet, the evidence agent's own staleness against the
// repository, a details variant this consumer does not know — and each
// is named, because "could not run" is only honest with its reason.
func evidenceUnavailable(in Input) string {
	if !in.HasEvidence {
		return reasonEvidenceOff
	}
	status := in.Evidence
	if !status.HasReport {
		reason := "the evidence agent has not answered yet"
		if status.Failure != "" {
			reason += " (latest poll: " + string(status.Failure) + ")"
		}
		return reason
	}
	switch {
	case status.Snapshot.Stale:
		return "contact with the evidence agent is lost, so the retained report is not current"
	case status.Snapshot.Report.Completeness == "no-completed-scan":
		return "the evidence agent has not completed a scan"
	case status.Snapshot.Report.SourceStale:
		return "the evidence agent reports its own evidence as stale against the repository"
	case status.Snapshot.Report.Facts == nil:
		return "the repository report's details are of a variant this console does not recognise"
	}
	return ""
}

// historyUnavailable is the reason the object timeline cannot be read,
// empty when it can. The timeline has no staleness of its own: it is a
// record of what was observed, not a claim about now, and its bounds
// are stated on every finding counted from it instead.
func historyUnavailable(in Input) string {
	if !in.HasHistory {
		return reasonHistoryOff
	}
	return ""
}

// infrastructureUnavailable is the reason the cluster's volumes and
// children cannot be read, empty when they can.
func infrastructureUnavailable(in Input) string {
	switch {
	case !in.HasInfrastructure:
		return "the cluster's volumes have not been observed yet"
	case in.Infrastructure.Stale:
		return "the cluster's volumes are stale, so current phases are unknown"
	}
	return ""
}

// logsUnavailable is the reason nothing in the logs can be read at all,
// empty when following is on. It answers only the on-or-off question;
// whether a check that read them may clear is logsIncomplete.
func logsUnavailable(in Input) string {
	if in.Logs == nil {
		return reasonLogsOff
	}
	return ""
}

// logsIncomplete is the reason a log check cannot report that it looked
// and found nothing, empty when the follower is reading every container
// it means to.
//
// The log record is the one source where absence is the whole signal: a
// rule fires on a line appearing, so its clear result is the claim that
// the line was not written. That claim rests entirely on the console
// having been listening, and while a container's stream is not open it
// was not. A hole in the past record is a different matter and not this
// one -- following is best effort, every reconnect misses something, and
// a check that could never clear because a pod once restarted would
// teach a reader to ignore the screen. What withholds a clear is a
// window that is open now.
func logsIncomplete(in Input) string {
	if in.Logs == nil {
		return ""
	}
	// The follower's roster is the pod list. Without it the console does
	// not know which containers ought to be talking, so it cannot know
	// that it is listening to all of them -- and a container it has
	// never heard of is one whose silence proves nothing.
	if reason := podsUnavailable(in); reason != "" {
		return "which containers should be talking is unknown, because " + reason
	}
	unread := in.Logs.Unread()
	if len(unread) == 0 {
		return ""
	}
	first := unread[0]
	where := fmt.Sprintf("%s container %s", first.Pod, first.Container)
	if len(unread) > 1 {
		where = fmt.Sprintf("%s and %d other container", where, len(unread)-1)
		if len(unread) > 2 {
			where += "s"
		}
	}
	since := ""
	if !first.Since.IsZero() {
		since = fmt.Sprintf(" for %s", in.Now.Sub(first.Since).Round(time.Second))
	}
	return fmt.Sprintf("no log stream is open for %s%s (%s), so a line written there would not have been seen",
		where, since, first.Reason)
}

// metricsUnavailable is the reason the instance metrics window cannot be
// read, empty when it can.
func metricsUnavailable(in Input) string {
	if in.Metrics == nil {
		return reasonMetricsOff
	}
	return ""
}

// poolerMetricsUnavailable is the reason the pooler metrics window
// cannot be read, empty when it can.
func poolerMetricsUnavailable(in Input) string {
	if in.PoolerMetrics == nil {
		return reasonPoolerMetricsOff
	}
	return ""
}

// A scraped window is the one source whose staleness is per reading
// rather than per snapshot. The scraper sweeps every instance on a
// cadence, so losing one exporter leaves that instance's readings
// frozen while the rest stay current, and a flag on the window as a
// whole would be wrong in both directions. Each reading carries the
// sweep time that produced it, and freshness judges them one at a time.
//
// The judgement matters more here than anywhere else, because a
// reading has no way to look stale. An unobserved snapshot is absent
// and a stale one says so, but a frozen exporter answers every read
// with a plausible number — and a check that takes it withdraws the
// present tense from the reader without saying it has: a lag reading
// from an hour ago reported as a match is a claim about the past, and
// the same reading below its threshold is the false clear the package
// exists to refuse.

// missedSweeps is how many scrapes may pass unanswered before a reading
// stops being a claim about now: two consecutive misses tolerated, the
// third refused. The graphs break their line after one and a half
// (metrics.Store's own gap break), which is right for a drawing of the
// past and too eager for a statement about the present.
const missedSweeps = 3

// freshnessFloor is the shortest horizon regardless of cadence. How
// long a reading stays a fair statement of now is a property of what is
// being claimed, not of how often the console asks: at the default
// ten-second sweep, three of them is half a minute, and withdrawing a
// check over half a minute of jitter would cost the reader more than it
// tells them.
const freshnessFloor = time.Minute

// freshness bounds how old a scraped reading may be before a check
// refuses to read the present tense into it, and remembers what it
// refused so the check can say so instead of falling silent.
type freshness struct {
	horizon time.Duration
	now     time.Time
	source  string
	// refused counts readings withheld for age; newest is the smallest
	// of their ages. Both speak for the refused readings only — a run
	// that withheld one instance may have read another perfectly well —
	// so the reason quotes the best case among what it refused and says
	// nothing about what it accepted.
	refused int
	newest  time.Duration
}

// freshnessOf is the bound for one window, taken from its own scrape
// cadence so the console calibrates against how often it actually asks.
func freshnessOf(window MetricsWindow, now time.Time, source string) *freshness {
	interval := window.Interval()
	if interval <= 0 {
		// A window that does not state its cadence is judged at the
		// package's own default rather than at zero, which would refuse
		// every reading ever taken.
		interval = metrics.DefaultInterval
	}
	horizon := time.Duration(missedSweeps) * interval
	if horizon < freshnessFloor {
		horizon = freshnessFloor
	}
	return &freshness{horizon: horizon, now: now, source: source}
}

// current reports whether a reading swept at the given Unix second is
// recent enough to state in the present tense, recording it as refused
// when it is not.
func (f *freshness) current(at int64) bool {
	age := f.now.Sub(time.Unix(at, 0))
	if age <= f.horizon {
		return true
	}
	if f.refused == 0 || age < f.newest {
		f.newest = age
	}
	f.refused++
	return false
}

// unavailable is the reason a check withdrew for want of a current
// reading, empty when it refused none.
//
// It is worded to claim only what was refused. A check reaches here
// having matched nothing, which does not mean it read nothing current:
// one instance's exporter may have frozen while another answered and
// simply had nothing to report. Saying "the newest reading is an hour
// old" would then be false, and would send an operator after a scraper
// that is mostly working. What is true either way is that some
// instances went unjudged, which is why the check cannot clear.
func (f *freshness) unavailable() string {
	switch f.refused {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(
			"a reading %s is %s old, past the %s a sweep should leave, so the instance it came from went unjudged",
			f.source, f.newest.Round(time.Second), f.horizon)
	}
	return fmt.Sprintf(
		"%d readings %s are past the %s a sweep should leave, the newest of them %s old, "+
			"so the instances they came from went unjudged",
		f.refused, f.source, f.horizon, f.newest.Round(time.Second))
}
