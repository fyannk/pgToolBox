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

package status

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	// defaultArchiveTimeout is CloudNativePG's own archive_timeout: an
	// idle primary switches WAL segments at least that often, so the
	// newest archived segment holds commits at most about that old.
	defaultArchiveTimeout = 5 * time.Minute
	// minimumFrontierMargin is the narrowest uncertain edge at the end of
	// the archived WAL or before a hole: three periods of CloudNativePG's
	// default archive_timeout. A longer archive_timeout widens it.
	minimumFrontierMargin = 15 * time.Minute
	// unboundedFrontierMargin is the edge when archive_timeout is
	// disabled (0): no forced switch bounds the newest segment's age.
	unboundedFrontierMargin = time.Hour
	// failureLookback is how long an archiving outage before the last
	// recorded failure the console accounts for: WAL that reaches the
	// repository after an outage carries older commits than its arrival
	// time says, and the instance does not report when the outage began.
	failureLookback = 24 * time.Hour
	// maximumMarkers bounds the backups drawn on the bar.
	maximumMarkers = 200
)

// Segment kinds.
const (
	segmentCovered      = "covered"
	segmentMissingWAL   = "missingWAL"
	segmentCandidateGap = "candidateGap"
	segmentUnknown      = "unknown"
	segmentReported     = "reported"
)

// The caveats every evidence answer carries: the agent's own limits.
var evidenceCaveats = []string{
	"Judged from WAL segment names in the repository: the WAL bytes were not read and no restore was run.",
	"The latest restore point is bounded by when the last contiguous segment reached the repository; the newest transactions may still be only on the instance.",
}

// recoveryFacts are what the timeline needs besides the evidence.
type recoveryFacts struct {
	now time.Time
	// archiveTimeout is the Cluster's archive_timeout; 0 when disabled.
	archiveTimeout time.Duration
	// lastFailedWAL is when the primary last failed to archive a segment
	// (/pg/status); nil when unknown or never.
	lastFailedWAL *time.Time
}

// recoveryFactsOf reads the facts from a gathered Cluster.
func recoveryFactsOf(g *gathered, now time.Time) recoveryFacts {
	f := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout}
	if g.cluster != nil {
		if v, ok := g.cluster.Spec.PostgresConfiguration.Parameters["archive_timeout"]; ok {
			if d, parsed := postgresDuration(v); parsed {
				f.archiveTimeout = d
			}
		}
	}
	for _, inst := range g.swept.Instances {
		if inst.State == viewmodel.StatusReadOK && inst.Reading != nil && inst.Reading.IsPrimary && inst.Reading.LastFailedWALTime != nil {
			at := inst.Reading.LastFailedWALTime.UTC()
			f.lastFailedWAL = &at
		}
	}
	return f
}

// postgresDuration parses a PostgreSQL time setting in seconds (the unit
// of archive_timeout): a bare number, or a number with ms, s, min, h or d.
func postgresDuration(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 || i > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(v[:i])
	if err != nil {
		return 0, false
	}
	units := map[string]time.Duration{"": time.Second, "ms": time.Millisecond, "s": time.Second, "min": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	unit, ok := units[strings.TrimSpace(v[i:])]
	if !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// frontierMargin is how close to the end of the archived WAL (or to a
// hole) a target stops being answerable from segment names: the
// receipt time bounds the commits of the last segment from above, but a
// target just before it may sit in WAL not archived yet.
func (f recoveryFacts) frontierMargin() time.Duration {
	if f.archiveTimeout <= 0 {
		return unboundedFrontierMargin
	}
	return max(minimumFrontierMargin, 3*f.archiveTimeout)
}

// recoveryTimeline draws the recovery-window bar from the agent's
// recovery paths, else from the operator's own window.
func (s *Service) recoveryTimeline(ev clusterEvidence, summary viewmodel.BackupSummary, rows []viewmodel.BackupRow, facts recoveryFacts) viewmodel.RecoveryTimeline {
	out := viewmodel.RecoveryTimeline{
		Basis: "none", State: string(evidencev1alpha1.Unknown), Segments: []viewmodel.RecoverySegment{}, Markers: []viewmodel.RecoveryMarker{},
		Assumptions: []string{}, Provenance: ev.view.Provenance,
	}
	rec := ev.result.Recovery
	if ev.result.Snapshot != nil && rec.State == evidence.RecoveryAvailable {
		out.Basis = "evidence"
		out.Provenance.Source = "EvidenceAgent " + ev.result.Agent + " " + evidencev1alpha1.RecoveryPathsPath
		stale := ""
		switch {
		case rec.Stale:
			stale = "the recovery paths could not be read again (" + rec.StaleReason + "): these are the last good ones"
		case ev.view.Provenance.Stale:
			stale = "the repository evidence is stale: " + ev.view.Reason
		}
		out.Provenance.Stale = out.Provenance.Stale || stale != ""
		in := timelineInput{
			rec: rec, backups: ev.result.Backups, facts: facts, stale: stale,
			scannedAt: scannedAt(ev.result), refresh: ev.result.RefreshInterval,
		}
		evidenceTimeline(&out, in)
		out.Complete = rec.Complete && ev.result.BackupsComplete
		if !ev.result.BackupsComplete {
			out.Reason = joinReason(out.Reason, "the backup collection was cut at a bound: older backups may exist")
		}
	} else {
		reason := recoveryUnavailableReason(ev)
		if w := summary.RecoveryWindow; w != nil && w.First != nil && w.Last != nil && !w.Last.Before(*w.First) {
			out.Basis, out.Provenance = "reported", w.Provenance
			first, last := *w.First, *w.Last
			out.Earliest, out.Latest = &first, &last
			out.Segments = append(out.Segments, viewmodel.RecoverySegment{
				From: first, To: last, Kind: segmentReported,
				Text: "Reported by " + w.Provenance.Source + "; not verified against the repository.",
			})
			reason = joinReason("the window is the operator's report, not repository evidence", reason)
		}
		out.Reason = reason
	}
	out.Markers = recoveryMarkers(ev.result.Backups, rows)
	return out
}

// scannedAt is when the agent last completed a scan, else when the
// console fetched the snapshot.
func scannedAt(r evidence.Result) *time.Time {
	if r.Snapshot != nil && r.Snapshot.CompletedAt != nil {
		return r.Snapshot.CompletedAt
	}
	return r.ObservedAt
}

// recoveryUnavailableReason says why there is no evidence timeline.
func recoveryUnavailableReason(ev clusterEvidence) string {
	switch {
	case ev.result.Snapshot == nil:
		if ev.view.Reason != "" {
			return "no repository evidence: " + ev.view.Reason
		}
		return "no repository evidence"
	case ev.result.Recovery.Reason != "":
		return ev.result.Recovery.Reason
	}
	return "the evidence agent published no recovery paths"
}

// interval is one path's covered span and what stops it.
type interval struct {
	from, to time.Time
	backupID string
	stop     evidencev1alpha1.CoverageStop
}

// timelineInput is the evidence a timeline is drawn from.
type timelineInput struct {
	rec     evidence.Recovery
	backups []evidence.BackupItem
	facts   recoveryFacts
	// stale says why the evidence is stale; empty when fresh.
	stale     string
	scannedAt *time.Time
	refresh   time.Duration
}

// targetTimeline is the newest timeline a path was evaluated toward: a
// timeline the agent saw in the archive but could not connect by valid
// history (a path with no frontier) is not a restore target PostgreSQL
// can follow, so it does not move the bar; it is reported instead.
func targetTimeline(paths []evidence.RecoveryPath) (target, orphan uint64) {
	for _, p := range paths {
		if p.FrontierTimeline != nil {
			target = max(target, p.TargetTimeline)
		}
	}
	for _, p := range paths {
		if p.FrontierTimeline == nil && p.TargetTimeline > target {
			orphan = max(orphan, p.TargetTimeline)
		}
	}
	return target, orphan
}

// reachedConsistency reports whether a path's frontier is at or past
// its backup's end WAL: before it, the backup is not consistent and no
// time is restorable from it, whatever the receipt times say. A path
// stopped at the frontier, or one that crossed onto a later timeline,
// passed the end by construction; any other is checked by position, and
// a backup whose end the console cannot locate is not drawn.
func reachedConsistency(p evidence.RecoveryPath, ends map[string]evidence.BackupItem) bool {
	if p.Stop == evidencev1alpha1.CoverageFrontier && p.State == evidencev1alpha1.Healthy {
		return true
	}
	if p.Consistent != nil {
		// The agent answered itself (pgBackRest paths).
		return *p.Consistent
	}
	if p.StartTimeline == nil || p.FrontierTimeline == nil || p.FrontierPosition == nil {
		return false
	}
	if *p.FrontierTimeline != *p.StartTimeline {
		return true
	}
	b, ok := ends[p.BackupID]
	if !ok || b.EndTimeline == nil || b.EndPosition == nil || *b.EndTimeline != *p.StartTimeline {
		return false
	}
	return *p.FrontierPosition >= *b.EndPosition
}

// evidenceTimeline fills the bar from the recovery paths toward the
// newest target timeline (recovery_target_timeline 'latest').
func evidenceTimeline(out *viewmodel.RecoveryTimeline, in timelineInput) {
	target, orphan := targetTimeline(in.rec.Paths)
	out.TargetTimeline = target
	spans, undrawn := recoverySpans(out, in, target)
	if len(spans) == 0 {
		out.State, out.Reason = string(evidencev1alpha1.Unknown), "no recovery path reaches its base backup's consistency point: the evidence cannot place a restore point"
		out.Reason = joinReason(out.Reason, in.stale)
		return
	}
	runs := mergeSpans(spans)
	margin := in.facts.frontierMargin()
	holes, candidates := drawRuns(out, spans, runs, margin)
	first, last := runs[0].from, runs[len(runs)-1].to
	out.Earliest, out.Latest = &first, &last
	tail := runs[len(runs)-1].stop
	failedRecently := markEdges(out, in, first, last, margin)
	tailSegment(out, in, last, tail)
	if !in.rec.Complete {
		// The paths read may all be toward an older timeline than a cut
		// one: what they cover is not drawn as current.
		for i := range out.Segments {
			if out.Segments[i].Kind == segmentCovered {
				out.Segments[i].Kind, out.Segments[i].BackupID = segmentUnknown, ""
				out.Segments[i].Text = "Covered by the paths read, but the collection was cut: a newer timeline may change where a restore goes."
			}
		}
	}
	sort.SliceStable(out.Segments, func(i, j int) bool { return out.Segments[i].From.Before(out.Segments[j].From) })
	timelineVerdict(out, in, timelineFindings{
		tail: tail, holes: holes, candidates: candidates, failedRecently: failedRecently, orphan: orphan, undrawn: undrawn,
	})
}

// recoverySpans is, for each path toward the target timeline, the span
// of restore points it proves: from its lower bound to its frontier's
// receipt, once it reached its base backup's consistency point. undrawn
// counts the paths that prove none. The paths' assumptions are listed on
// out, sorted.
func recoverySpans(out *viewmodel.RecoveryTimeline, in timelineInput, target uint64) (spans []interval, undrawn int) {
	ends := make(map[string]evidence.BackupItem, len(in.backups))
	for _, b := range in.backups {
		ends[b.ID] = b
	}
	assumptions := map[string]bool{}
	for _, p := range in.rec.Paths {
		if p.TargetTimeline != target || p.FrontierTimeline == nil {
			continue
		}
		for _, a := range p.Assumptions {
			assumptions[a] = true
		}
		if p.LowerBoundAt == nil || p.FrontierReceiptAt == nil || !p.FrontierReceiptAt.After(*p.LowerBoundAt) || !reachedConsistency(p, ends) {
			undrawn++
			continue
		}
		spans = append(spans, interval{from: p.LowerBoundAt.UTC(), to: p.FrontierReceiptAt.UTC(), backupID: p.BackupID, stop: p.Stop})
	}
	for a := range assumptions {
		out.Assumptions = append(out.Assumptions, a)
	}
	sort.Strings(out.Assumptions)
	return spans, undrawn
}

// mergeSpans sorts the spans (earliest first, longest first on a tie) and
// merges them into covered runs; each run ends with the stop of the path
// that reaches furthest. The spans stay per path: a run is drawn as
// pieces, each naming the newest backup whose own span covers it.
func mergeSpans(spans []interval) []interval {
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].from.Equal(spans[j].from) {
			return spans[i].from.Before(spans[j].from)
		}
		return spans[i].to.After(spans[j].to)
	})
	var runs []interval
	for _, sp := range spans {
		if n := len(runs); n > 0 && !sp.from.After(runs[n-1].to) {
			last := &runs[n-1]
			if sp.to.After(last.to) {
				last.to, last.stop = sp.to, sp.stop
			}
			continue
		}
		runs = append(runs, sp)
	}
	return runs
}

// drawRuns draws each run as covered pieces and each gap between two runs
// as a hole, the margin before it uncertain; it counts the confirmed holes
// and the candidate gaps.
func drawRuns(out *viewmodel.RecoveryTimeline, spans, runs []interval, margin time.Duration) (holes, candidates int) {
	for i, r := range runs {
		for _, piece := range coveredPieces(spans, r.from, r.to) {
			out.Segments = append(out.Segments, viewmodel.RecoverySegment{
				From: piece.from, To: piece.to, Kind: segmentCovered, BackupID: piece.backupID,
				Text: "Structurally covered: base backup " + piece.backupID + " and contiguous archived WAL.",
			})
		}
		if i+1 == len(runs) {
			break
		}
		uncertain(out, r.to.Add(-margin), r.to, "Within "+humanDuration(margin)+" of a hole in the archived WAL: segment names cannot place this target on either side.")
		kind, text := holeOf(r.stop)
		switch kind {
		case segmentMissingWAL:
			holes++
		case segmentCandidateGap:
			candidates++
		}
		out.Segments = append(out.Segments, viewmodel.RecoverySegment{From: r.to, To: runs[i+1].from, Kind: kind, Text: text})
	}
	return holes, candidates
}

// markEdges makes both ends of the window uncertain. The newest edge: the
// last segment's commits are older than its receipt, by up to the
// archive_timeout an idle primary allows, and further after a recent
// archiving failure (failedRecently). The oldest edge: a retention purge
// can remove the oldest backup at any moment; the evidence shows it at
// most one scan later.
func markEdges(out *viewmodel.RecoveryTimeline, in timelineInput, first, last time.Time, margin time.Duration) (failedRecently bool) {
	marginText := "Within " + humanDuration(margin) + " of the end of the archived WAL: segment names cannot tell whether this target's WAL is archived yet."
	if in.facts.archiveTimeout <= 0 {
		marginText = "Near the end of the archived WAL with archive_timeout disabled: nothing bounds how old the newest archived commit is."
	}
	uncertain(out, last.Add(-margin), last, marginText)
	if f := in.facts.lastFailedWAL; f != nil && f.After(last.Add(-failureLookback)) {
		failedRecently = true
		from := minTime(*f, last).Add(-failureLookback)
		uncertain(out, from, last, "WAL archiving failed at "+f.Format(time.RFC3339)+": WAL that reaches the repository after a failure holds older commits than its arrival time says, so the arrival time overstates how recent a restore can be.")
	}
	purge := in.refresh
	if purge <= 0 {
		purge = evidence.DefaultAgentRefreshInterval
	}
	if in.scannedAt != nil && in.facts.now.After(*in.scannedAt) {
		purge += in.facts.now.Sub(*in.scannedAt)
	}
	uncertain(out, first, first.Add(purge), "Within "+humanDuration(purge)+" of the oldest restore point: a retention purge since the repository was last read may have removed this base backup.")
	return failedRecently
}

// tailSegment draws the time after the newest restore point: only a
// confirmed gap or a fresh, complete frontier says the time is not
// restorable; anything else leaves it unknown.
func tailSegment(out *viewmodel.RecoveryTimeline, in timelineInput, last time.Time, tail evidencev1alpha1.CoverageStop) {
	switch {
	case tail == evidencev1alpha1.CoverageGapLimited:
		out.Segments = append(out.Segments, viewmodel.RecoverySegment{From: last, To: in.facts.now, Kind: segmentMissingWAL, Text: "WAL is missing from the repository after the latest restore point."})
	case tail != evidencev1alpha1.CoverageFrontier:
		_, text := holeOf(tail)
		out.Segments = append(out.Segments, viewmodel.RecoverySegment{From: last, To: in.facts.now, Kind: segmentUnknown, Text: text})
	case !in.rec.Complete:
		out.Segments = append(out.Segments, viewmodel.RecoverySegment{From: last, To: in.facts.now, Kind: segmentUnknown, Text: "The recovery path collection was cut at a bound: newer coverage may exist."})
	case in.stale != "":
		out.Segments = append(out.Segments, viewmodel.RecoverySegment{From: last, To: in.facts.now, Kind: segmentUnknown, Text: "The repository evidence is stale: newer WAL may have reached the repository since."})
	}
}

// timelineFindings are what the drawing found that decides the verdict.
type timelineFindings struct {
	tail              evidencev1alpha1.CoverageStop
	holes, candidates int
	failedRecently    bool
	orphan            uint64
	undrawn           int
}

// timelineVerdict states the bar's state and reason, the gravest finding
// first: a cut collection, missing WAL at the end, an unconfirmed end,
// holes, candidate gaps, a recent archiving failure, an orphan timeline,
// stale evidence.
func timelineVerdict(out *viewmodel.RecoveryTimeline, in timelineInput, f timelineFindings) {
	switch {
	case !in.rec.Complete:
		out.State, out.Reason = string(evidencev1alpha1.Unknown), "the recovery path collection was cut at a bound: the restore target timeline cannot be told"
	case f.tail == evidencev1alpha1.CoverageGapLimited:
		out.State, out.Reason = string(evidencev1alpha1.Unhealthy), "WAL is missing after the latest restore point: recent times are not restorable from the repository"
	case f.tail != evidencev1alpha1.CoverageFrontier:
		out.State = string(evidencev1alpha1.Warning)
		_, out.Reason = holeOf(f.tail)
	case f.holes > 0:
		out.State, out.Reason = string(evidencev1alpha1.Warning), "the window has holes where WAL is missing"
	case f.candidates > 0:
		out.State, out.Reason = string(evidencev1alpha1.Warning), "the window has candidate gaps a second scan has not confirmed yet"
	case f.failedRecently:
		out.State, out.Reason = string(evidencev1alpha1.Warning), "WAL archiving failed recently: the newest restore points are uncertain"
	case f.orphan > 0:
		out.State, out.Reason = string(evidencev1alpha1.Warning), "timeline "+strconv.FormatUint(f.orphan, 10)+" is in the archive but not connected by valid history: a restore cannot follow it"
	case in.stale != "":
		out.State = string(evidencev1alpha1.Warning)
	default:
		out.State = string(evidencev1alpha1.Healthy)
	}
	out.Reason = joinReason(out.Reason, in.stale)
	if f.undrawn > 0 && out.Reason == "" {
		out.Reason = "some backups have no observed recovery path past their consistency point"
	}
}

// uncertain turns the covered part of [from, to] into an unknown
// segment with text, splitting the covered segments it overlaps.
func uncertain(out *viewmodel.RecoveryTimeline, from, to time.Time, text string) {
	if !to.After(from) {
		return
	}
	var segs []viewmodel.RecoverySegment
	for _, seg := range out.Segments {
		if seg.Kind != segmentCovered || !seg.To.After(from) || !to.After(seg.From) {
			segs = append(segs, seg)
			continue
		}
		if seg.From.Before(from) {
			before := seg
			before.To = from
			segs = append(segs, before)
		}
		mid := viewmodel.RecoverySegment{From: maxTime(seg.From, from), To: minTime(seg.To, to), Kind: segmentUnknown, Text: text}
		segs = append(segs, mid)
		if seg.To.After(to) {
			after := seg
			after.From = to
			segs = append(segs, after)
		}
	}
	out.Segments = segs
}

// humanDuration renders a margin for a sentence: "15 min", "1 h 5 min".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h == 0:
		return strconv.Itoa(max(m, 1)) + " min"
	case m == 0:
		return strconv.Itoa(h) + " h"
	}
	return strconv.Itoa(h) + " h " + strconv.Itoa(m) + " min"
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// coveredPieces splits the run [from, to] at every span edge inside it
// and names, for each piece, the newest backup (latest lower bound) whose
// own span covers the whole piece: the backup CloudNativePG restores
// from when no backupID is given, and one whose WAL is known to reach
// the piece. Adjacent pieces of the same backup are joined.
func coveredPieces(spans []interval, from, to time.Time) []interval {
	edges := []time.Time{from, to}
	for _, sp := range spans {
		for _, e := range []time.Time{sp.from, sp.to} {
			if e.After(from) && e.Before(to) {
				edges = append(edges, e)
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Before(edges[j]) })
	var out []interval
	for i := 0; i+1 < len(edges); i++ {
		a, b := edges[i], edges[i+1]
		if !b.After(a) {
			continue
		}
		best := -1
		for j, sp := range spans {
			if sp.from.After(a) || sp.to.Before(b) {
				continue
			}
			if best < 0 || sp.from.After(spans[best].from) || (sp.from.Equal(spans[best].from) && sp.backupID > spans[best].backupID) {
				best = j
			}
		}
		if best < 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].backupID == spans[best].backupID && out[n-1].to.Equal(a) {
			out[n-1].to = b
			continue
		}
		out = append(out, interval{from: a, to: b, backupID: spans[best].backupID})
	}
	return out
}

// holeOf names the span after a path's frontier.
func holeOf(stop evidencev1alpha1.CoverageStop) (kind, text string) {
	switch stop {
	case evidencev1alpha1.CoverageGapLimited:
		return segmentMissingWAL, "WAL is missing from the repository: no restore target in this span."
	case evidencev1alpha1.CoverageCandidateLimited:
		return segmentCandidateGap, "A WAL gap seen by one scan, not yet confirmed by a second."
	default: // CoverageUnknownLimited, and a frontier that is not a stop
		return segmentUnknown, "The evidence cannot tell whether WAL continues here."
	}
}

// recoveryMarkers are the base backups on the bar: the repository's
// (completed, failed, incomplete) and the failed Backup objects.
func recoveryMarkers(items []evidence.BackupItem, rows []viewmodel.BackupRow) []viewmodel.RecoveryMarker {
	out := []viewmodel.RecoveryMarker{}
	for _, item := range items {
		at := item.EndAt
		if at == nil {
			at = item.BeginAt
		}
		if at == nil {
			continue
		}
		m := viewmodel.RecoveryMarker{At: at.UTC(), BackupID: item.ID, Source: "repository", Kind: "completed", Text: item.Reason.Message}
		switch {
		case item.Failed, item.Status != nil && *item.Status == "FAILED":
			m.Kind = "failed"
		case item.State != evidencev1alpha1.Healthy:
			m.Kind = "incomplete"
		}
		out = append(out, m)
	}
	for _, row := range rows {
		if !row.Terminal || row.Health != viewmodel.HealthCritical || row.PredatesCluster {
			continue
		}
		at := row.StoppedAt
		if at == nil {
			at = row.StartedAt
		}
		if at == nil {
			at = row.CreatedAt
		}
		if at == nil {
			continue
		}
		out = append(out, viewmodel.RecoveryMarker{At: at.UTC(), BackupID: row.BackupID, Name: row.Name, Kind: "failed", Source: "backupObject", Text: row.Error})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if len(out) > maximumMarkers {
		out = out[len(out)-maximumMarkers:]
	}
	return out
}

// RestoreCheck answers whether the Cluster can be restored to at.
func (s *Service) RestoreCheck(ctx context.Context, namespace, name string, at time.Time) (viewmodel.RestoreCheck, error) {
	b, err := s.Backups(ctx, namespace, name)
	if err != nil {
		return viewmodel.RestoreCheck{}, err
	}
	return restoreCheck(b, at.UTC()), nil
}

// restoreCheck reads the answer off the timeline: the timeline and the
// check can never disagree. Every uncertain edge is a segment of its
// own, so the check is a lookup.
func restoreCheck(b viewmodel.Backups, at time.Time) viewmodel.RestoreCheck {
	t := b.RecoveryTimeline
	out := viewmodel.RestoreCheck{
		Ref: b.Ref, GeneratedAt: b.GeneratedAt, At: at, Basis: t.Basis, Caveats: []string{}, Provenance: t.Provenance,
	}
	if t.Basis == "evidence" {
		out.Caveats = append(out.Caveats, evidenceCaveats...)
		if !t.Complete {
			out.Caveats = append(out.Caveats, "The evidence collections were cut at a bound: a span shown as not covered may be covered.")
		}
		if t.Provenance.Stale {
			out.Caveats = append(out.Caveats, "The repository evidence is stale.")
		}
		if b.Summary.Archiving.Health == viewmodel.HealthCritical {
			out.Caveats = append(out.Caveats, "WAL archiving is failing: the newest WAL is on the instance, not in the repository.")
		}
	}
	switch {
	case t.Basis == "none" || t.Earliest == nil || t.Latest == nil:
		out.Answer, out.Text = "unknown", "No restore window is known for this cluster: "+orDefault(t.Reason, "no evidence")+"."
		return out
	case at.Before(*t.Earliest) && t.Basis == "evidence" && !t.Complete:
		out.Answer, out.Text = "unknown", "Before the earliest restore point the evidence shows ("+t.Earliest.Format(time.RFC3339)+"), but the collections were cut: an older backup may exist."
		return out
	case at.Before(*t.Earliest):
		out.Answer, out.Text = "notCovered", "Before the earliest restore point ("+t.Earliest.Format(time.RFC3339)+"): no base backup that old is "+basisWord(t.Basis)+"."
		return out
	case t.Basis == "reported" && !at.After(*t.Latest):
		out.Answer, out.Text = "reported", "Inside the window the operator reports; no repository evidence confirms it."
		return out
	}
	for _, seg := range t.Segments {
		if at.Before(seg.From) || at.After(seg.To) {
			continue
		}
		switch seg.Kind {
		case segmentCovered:
			out.Answer, out.BackupID = "covered", seg.BackupID
			out.Text = "Structurally covered: restore from base backup " + seg.BackupID + " and replay the archived WAL to this time."
		case segmentMissingWAL, segmentCandidateGap:
			out.Answer, out.Text = "gap", seg.Text
		case segmentReported:
			continue
		default:
			out.Answer, out.Text = "unknown", seg.Text
		}
		return out
	}
	if at.After(*t.Latest) {
		out.Answer, out.Text = "notCovered", "After the latest restore point ("+t.Latest.Format(time.RFC3339)+"): that WAL has not reached the repository."
		return out
	}
	out.Answer, out.Text = "unknown", "The evidence does not place this time."
	return out
}

func basisWord(basis string) string {
	if basis == "evidence" {
		return "in the repository"
	}
	return "reported"
}

func joinReason(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
