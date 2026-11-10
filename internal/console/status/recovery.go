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
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
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
	// walEndFloor is the narrowest archive latency allowance of the band
	// before the newest restore point marked as the end of the WAL
	// (archiveLatencyAllowance): the frontier is when the last segment
	// reached the repository, not the last commit in it, so a target in
	// the last archive_timeout plus that latency may be after the last
	// commit the archive holds.
	walEndFloor = time.Minute
	// cappedPathCode is the reason code the evidence reader gives a path
	// it capped to unknown (internal/console/evidence capCode).
	cappedPathCode = "coverage-path-unknown"
)

// Segment kinds.
const (
	segmentCovered      = "covered"
	segmentMissingWAL   = "missingWAL"
	segmentCandidateGap = "candidateGap"
	segmentUnknown      = "unknown"
	segmentReported     = "reported"
	segmentWALEnd       = "walEnd"
)

// The caveats every evidence answer carries: the agent's own limits.
var evidenceCaveats = []viewmodel.Phrase{
	say("recovery.caveat.segmentNames", "Judged from WAL segment names in the repository: the WAL bytes were not read and no restore was run.", nil),
	say("recovery.caveat.latestBounded", "The latest restore point is bounded by when the last contiguous segment reached the repository; the newest transactions may still be only on the instance.", nil),
}

// say is a sentence with its stable code and the values it names (see
// viewmodel.Phrase): the SPA translates the code, Text is the English.
func say(code, text string, params map[string]string) viewmodel.Phrase {
	return viewmodel.Phrase{Code: code, Params: params, Text: text}
}

// segment is a span of the bar saying p.
func segment(from, to time.Time, kind string, p viewmodel.Phrase) viewmodel.RecoverySegment {
	return viewmodel.RecoverySegment{From: from, To: to, Kind: kind, Text: p.Text, Code: p.Code, Params: p.Params}
}

// rfc3339 is an instant as a Phrase parameter.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// secondsParam is a duration as a Phrase parameter, in whole seconds.
func secondsParam(d time.Duration) string {
	return strconv.FormatInt(int64(d.Round(time.Second)/time.Second), 10)
}

// recoveryFacts are what the timeline needs besides the evidence.
type recoveryFacts struct {
	now time.Time
	// archiveTimeout is the Cluster's archive_timeout; 0 when disabled.
	archiveTimeout time.Duration
	// sweepInterval is the instance sweep's period; 0 when unknown.
	sweepInterval time.Duration
	// lastFailedWAL is the newest failure to archive a segment any
	// readable instance reports (/pg/status); nil when unknown or never.
	lastFailedWAL *time.Time
	// caughtUpBy is a sweep by which the archiver that reported
	// lastFailedWAL had caught up since: its last_archived_time is after
	// its last_failed_time, and from that sweep on every reading had
	// fewer .ready files than the backlog threshold. Nil when it has not
	// caught up, or the console cannot tell.
	caughtUpBy *time.Time
	// failing is the archiving fact read as failing (archivingFailing).
	failing bool
	// backlog is the archiver's readyWalFiles when the backlog persists
	// across sweeps, or is at the threshold with too few sweeps kept to
	// judge it (cnpgstatus.ArchiveBacklogOf, Segments or Unconfirmed); 0
	// otherwise or when unread.
	backlog int
	// archiverUnread: the archiving fact rests on no reading of the
	// primary's archiver (archiverUnread); hibernated or notReported (a
	// designated primary, which reports no readyWalFiles) says why.
	archiverUnread, hibernated, notReported bool
	// quiet are the sweeps, oldest first, that read the current primary's
	// archiver with no .ready file at all, since it became primary: at
	// each, nothing waited to be archived. A reading of a few files
	// proves nothing (a slow archiver can hold them for long), and a
	// failed read is no reading.
	quiet []time.Time
	// bounds are, for the sweeps that read the current primary with
	// .ready files waiting, the time before which every commit was
	// archived by then (archivedBounds); empty when the WAL positions or
	// the segment size are not known.
	bounds []archivedBound
}

// archivedBound says that by the sweep at readAt every commit before
// before was in WAL the archiver had archived.
type archivedBound struct {
	readAt, before time.Time
}

// recoveryFactsOf reads the facts from a gathered Cluster and its
// archiving fact (the backups summary's, so both agree).
func recoveryFactsOf(g *gathered, arch viewmodel.Archiving, now time.Time) recoveryFacts {
	f := recoveryFacts{now: now, archiveTimeout: defaultArchiveTimeout, failing: archivingFailing(arch), sweepInterval: g.swept.Interval}
	if g.cluster == nil {
		return f
	}
	if v, ok := g.cluster.Spec.PostgresConfiguration.Parameters["archive_timeout"]; ok {
		if d, parsed := postgresDuration(v); parsed {
			f.archiveTimeout = d
		}
	}
	f.archiverUnread, f.hibernated = archiverUnread(arch), arch.Reason == cnpgstatus.ArchivingReasonHibernated
	f.notReported = arch.Reason == cnpgstatus.ArchivingReasonBacklogNotReported
	live := primaryArchiver(g)
	switch b := cnpgstatus.ArchiveBacklogOf(g.cluster, live); {
	case b.Behind():
		f.backlog = b.Segments
	case b.Unconfirmed > 0:
		f.backlog = b.Unconfirmed
	}
	if f.backlog > 0 || f.archiverUnread {
		f.quiet = quietSweeps(g, live)
		f.bounds = archivedBounds(g, f.archiveTimeout)
	}
	var failedOn string
	f.lastFailedWAL, failedOn = newestArchiveFailure(g)
	if f.lastFailedWAL != nil {
		f.caughtUpBy = caughtUpBy(g, failedOn)
	}
	return f
}

// archiverUnread reads the archiving fact as resting on no reading of
// the primary's archiver: unknown because the primary was not read
// (PrimaryUnread), the Cluster is hibernated, the archiver is a
// designated primary that reports no readyWalFiles
// (ArchiveBacklogNotReported), or no condition is written and the
// primary was not read either.
func archiverUnread(a viewmodel.Archiving) bool {
	switch a.Reason {
	case cnpgstatus.ArchivingReasonPrimaryUnread, cnpgstatus.ArchivingReasonHibernated, cnpgstatus.ArchivingReasonBacklogNotReported:
		return true
	case cnpgstatus.ArchivingReasonNoCondition:
		return a.LiveArchiving == nil
	}
	return false
}

// quietSweeps are the sweeps, oldest first, before the current reading
// (live; when the primary is unread now, every sweep its ring keeps,
// service.go readyRings) that read the current primary with no .ready
// file, from when it became primary (status.currentPrimaryTimestamp,
// when it parses): a reading taken while it was a replica says nothing of
// the archiver then, and a failed read is a gap, so it only widens.
func quietSweeps(g *gathered, live *cnpgstatus.ArchiverLive) []time.Time {
	var promoted time.Time
	if t, err := time.Parse(time.RFC3339Nano, g.cluster.Status.CurrentPrimaryTimestamp); err == nil {
		promoted = t.UTC()
	}
	var out []time.Time
	for _, p := range g.readyRings[g.cluster.Status.CurrentPrimary] {
		if p.Value == nil || *p.Value != 0 || p.At.Before(promoted) || (live != nil && !p.At.Before(live.ObservedAt)) {
			continue
		}
		out = append(out, p.At.UTC())
	}
	return out
}

// archivedBounds bound a backlog's onset on a busy primary, which may
// never read zero .ready files. At a sweep T that read k .ready files
// with the write position in segment S, the archiver had archived every
// segment before S-k by T: the .ready files are the newest completed
// segments it has not been asked to finish with (PostgreSQL archives in
// order, and segments a parallel archiver uploaded ahead stay .ready, so
// k only overcounts). A commit in a later segment is at most one
// archive_timeout older than that segment's switch (archive_timeout
// forces the switch once anything was written), and segment S-k switched
// after any sweep T' at which the position had not reached the start of
// S-k+1. So by T every commit before T' - archive_timeout was archived.
// T' is the newest such sweep at or before T, both since the instance
// became primary; a failed read is no T or T' (it only widens). Empty
// without the position ring, the segment size, or archive_timeout.
func archivedBounds(g *gathered, archiveTimeout time.Duration) []archivedBound {
	primary := g.cluster.Status.CurrentPrimary
	size, ok := walSegmentSize(g.swept.Instances[primary].Reading)
	if !ok || archiveTimeout <= 0 || len(g.lsnRing) == 0 {
		return nil
	}
	var promoted time.Time
	if t, err := time.Parse(time.RFC3339Nano, g.cluster.Status.CurrentPrimaryTimestamp); err == nil {
		promoted = t.UTC()
	}
	type sample struct {
		at  time.Time
		lsn uint64
	}
	var positions []sample // oldest first
	byAt := map[time.Time]uint64{}
	for _, p := range g.lsnRing {
		if p.Value == nil || *p.Value < 0 || p.At.Before(promoted) {
			continue
		}
		v := uint64(*p.Value)
		positions = append(positions, sample{at: p.At, lsn: v})
		byAt[p.At] = v
	}
	var out []archivedBound
	for _, p := range g.readyRings[primary] {
		if p.Value == nil || *p.Value < 1 || p.At.Before(promoted) {
			continue
		}
		lsn, read := byAt[p.At]
		if !read {
			continue
		}
		k := uint64(*p.Value)
		segment := lsn / size
		if segment+1 <= k {
			continue
		}
		// The start of segment S-k+1: S-k is finished once the position
		// reaches it.
		start := (segment - k + 1) * size
		for i := len(positions) - 1; i >= 0; i-- {
			if positions[i].at.After(p.At) {
				continue
			}
			if positions[i].lsn < start {
				out = append(out, archivedBound{readAt: p.At.UTC(), before: positions[i].at.Add(-archiveTimeout).UTC()})
				break
			}
		}
	}
	return out
}

// boundBy is the latest bound from a sweep at or before by (the scan,
// else now): the most recent time before which every commit was in the
// repository when the agent read it. Nil when there is none.
func (f recoveryFacts) boundBy(by *time.Time) *archivedBound {
	limit := f.now
	if by != nil {
		limit = *by
	}
	var best *archivedBound
	for i := range f.bounds {
		b := f.bounds[i]
		if b.readAt.After(limit) {
			continue
		}
		if best == nil || b.before.After(best.before) {
			best = &b
		}
	}
	return best
}

// walSegmentSize is the WAL segment size a reading's current WAL file
// name and position agree on: the one power of two from 1 MiB to 1 GiB
// (PostgreSQL's range) for which pg_walfile_name(pg_current_wal_lsn())
// names that file (at a segment boundary it names the one before). False
// when the reading is missing, the name does not parse, or more than one
// size fits (a position early in its log id), so a guess never narrows a
// band.
func walSegmentSize(r *instancestatus.Reading) (uint64, bool) {
	if r == nil || len(r.CurrentWAL) != 24 {
		return 0, false
	}
	lsn, ok := cnpgstatus.ParseLSN(r.CurrentLSN)
	if !ok {
		return 0, false
	}
	logID, err1 := strconv.ParseUint(r.CurrentWAL[8:16], 16, 32)
	seg, err2 := strconv.ParseUint(r.CurrentWAL[16:24], 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	var found uint64
	for size := uint64(1) << 20; size <= 1<<30; size <<= 1 {
		perLog := (uint64(1) << 32) / size
		named := logID*perLog + seg
		if seg >= perLog || (named != lsn/size && (lsn == 0 || named != (lsn-1)/size)) {
			continue
		}
		if found != 0 {
			return 0, false
		}
		found = size
	}
	return found, found != 0
}

// quietBy is the newest quiet sweep at or before by (the scan, else now):
// the archiver had nothing waiting then, so every segment completed by
// then was in the repository when the agent read it. Nil when none is.
func (f recoveryFacts) quietBy(by *time.Time) *time.Time {
	bound := f.now
	if by != nil {
		bound = *by
	}
	for i := len(f.quiet) - 1; i >= 0; i-- {
		if !f.quiet[i].After(bound) {
			q := f.quiet[i]
			return &q
		}
	}
	return nil
}

// newestArchiveFailure is the newest pg_stat_archiver last_failed_time
// any instance read in good standing reports, each clamped to when it
// was read, and the instance that reports it. Not only the current
// primary's: after a failover the failure is on the former primary,
// whose statistics survive a clean demotion, and the WAL it failed to
// archive is still what reached the repository late. A replica that
// never archived reports none.
func newestArchiveFailure(g *gathered) (*time.Time, string) {
	var newest *time.Time
	var instance string
	names := make([]string, 0, len(g.swept.Instances))
	for name := range g.swept.Instances {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		inst := g.swept.Instances[name]
		if inst.State != viewmodel.StatusReadOK || inst.Reading == nil || inst.Reading.LastFailedWALTime == nil {
			continue
		}
		at := inst.Reading.LastFailedWALTime.UTC()
		if observed := inst.Reading.ObservedAt; !observed.IsZero() && at.After(observed) {
			at = observed.UTC()
		}
		if newest == nil || at.After(*newest) {
			newest, instance = &at, name
		}
	}
	return newest, instance
}

// caughtUpBy is the earliest sweep after the instance's archive failure
// from which every reading of it, the current one included, had fewer
// .ready files than the backlog threshold, provided its current reading
// shows a success after the failure (last_archived_time after
// last_failed_time: PostgreSQL archives segments in order, so the failed
// one is in the repository). The archiver caught up at or before that
// sweep. Nil when it has not caught up, or nothing shows it: an instance
// that is not a primary now (a demoted former primary, a designated
// primary) reports no readyWalFiles, so its 0 says nothing.
func caughtUpBy(g *gathered, instance string) *time.Time {
	inst, ok := g.swept.Instances[instance]
	if !ok || inst.Reading == nil || !inst.Reading.IsPrimary {
		return nil
	}
	r := inst.Reading
	if r.LastFailedWALTime == nil || r.LastArchivedWALTime == nil || !r.LastArchivedWALTime.After(*r.LastFailedWALTime) || r.ObservedAt.IsZero() {
		return nil
	}
	failed := r.LastFailedWALTime.UTC()
	threshold := cnpgstatus.ArchiveBacklogThreshold(g.cluster, pluginMaxParallel(g))
	if r.ReadyWALFiles >= threshold || !r.ObservedAt.After(failed) {
		return nil
	}
	by := r.ObservedAt.UTC()
	samples := earlierReady(g, instance, r.ObservedAt)
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if !s.At.After(failed) {
			break
		}
		if s.Files == nil {
			continue
		}
		if *s.Files >= threshold {
			break
		}
		by = s.At.UTC()
	}
	return &by
}

// postgresDuration parses a PostgreSQL time setting in seconds (the unit
// of archive_timeout): a bare number, or a number with us, ms, s, min, h
// or d (PostgreSQL's time units).
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
	units := map[string]time.Duration{"": time.Second, "us": time.Microsecond, "ms": time.Millisecond, "s": time.Second, "min": time.Minute, "h": time.Hour, "d": 24 * time.Hour}
	unit, ok := units[strings.TrimSpace(v[i:])]
	if !ok {
		return 0, false
	}
	return time.Duration(n) * unit, true
}

// archiveLatencyAllowance is how long the console allows between a
// segment's switch and its receipt in the repository when the archiver
// keeps up: one sweep period (the archiver had no backlog the sweeps
// saw, and a backlog widens the newest edge on its own), walEndFloor
// when the period is not known, never less. A slower archiver held
// below the backlog threshold can exceed it; the near-end margin still
// covers most of that.
func (f recoveryFacts) archiveLatencyAllowance() time.Duration {
	return max(f.sweepInterval, walEndFloor)
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
		Assumptions: []string{}, Reasons: []viewmodel.Phrase{}, Provenance: ev.view.Provenance,
	}
	rec := ev.result.Recovery
	if ev.result.Snapshot != nil && rec.State == evidence.RecoveryAvailable {
		out.Basis = "evidence"
		out.Provenance.Source = "EvidenceAgent " + ev.result.Agent + " " + evidencev1alpha1.RecoveryPathsPath
		var stale *viewmodel.Phrase
		switch {
		case rec.Stale:
			p := say("recovery.reason.pathsStale", "the recovery paths could not be read again ("+rec.StaleReason+"): these are the last good ones",
				map[string]string{"detail": rec.StaleReason})
			stale = &p
		case ev.view.Provenance.Stale:
			p := say("recovery.reason.evidenceStale", "the repository evidence is stale: "+ev.view.Reason, map[string]string{"detail": ev.view.Reason})
			stale = &p
		}
		out.Provenance.Stale = out.Provenance.Stale || stale != nil
		in := timelineInput{
			rec: rec, backups: ev.result.Backups, facts: facts, stale: stale,
			scannedAt: scannedAt(ev.result), refresh: ev.result.RefreshInterval,
		}
		evidenceTimeline(&out, in)
		out.OffTarget = offTargetSpans(rec.Paths, ev.result.Backups)
		if w := rec.TimelineWarning; w != "" {
			p := evidenceReason("recovery.reason.timelineWarning", w)
			if f := historyFileIn(w); f != "" && p.Params["historyFile"] == "" {
				p.Params = cloneParams(p.Params)
				p.Params["historyFile"] = f
			}
			out.TimelineWarning = &p
		}
		out.Complete = rec.Complete && ev.result.BackupsComplete
		if !ev.result.BackupsComplete {
			addReason(&out, say("recovery.reason.backupsCut", "the backup collection was cut at a bound: older backups may exist", nil))
		}
	} else {
		reason := recoveryUnavailableReason(ev)
		if w := summary.RecoveryWindow; w != nil && w.First != nil && w.Last != nil && !w.Last.Before(*w.First) {
			out.Basis, out.Provenance = "reported", w.Provenance
			first, last := *w.First, *w.Last
			out.Earliest, out.Latest = &first, &last
			out.Segments = append(out.Segments, segment(first, last, segmentReported, say("recovery.segment.reported",
				"Reported by "+w.Provenance.Source+"; not verified against the repository.", map[string]string{"source": w.Provenance.Source})))
			if facts.now.After(last) {
				out.Segments = append(out.Segments, reportedTail(w.Provenance, summary.Archiving, facts, last)...)
			}
			addReason(&out, say("recovery.reason.reportedWindow", "the window is the operator's report, not repository evidence", nil))
		}
		addReason(&out, reason)
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
func recoveryUnavailableReason(ev clusterEvidence) viewmodel.Phrase {
	switch {
	case ev.result.Snapshot == nil:
		if ev.view.Reason != "" {
			return say("recovery.reason.noEvidenceBecause", "no repository evidence: "+ev.view.Reason, map[string]string{"detail": ev.view.Reason})
		}
		return say("recovery.reason.noEvidence", "no repository evidence", nil)
	case ev.result.Recovery.Reason != "":
		return evidenceReason("recovery.reason.pathsNotCoverage", ev.result.Recovery.Reason)
	}
	return say("recovery.reason.noPaths", "the evidence agent published no recovery paths", nil)
}

// addReason appends p to the timeline's reasons; Reason is their English
// texts joined, as before the codes.
func addReason(out *viewmodel.RecoveryTimeline, p viewmodel.Phrase) {
	if p.Text == "" {
		return
	}
	out.Reasons = append(out.Reasons, p)
	out.Reason = joinReason(out.Reason, p.Text)
}

// setReason replaces the timeline's reasons with p.
func setReason(out *viewmodel.RecoveryTimeline, p viewmodel.Phrase) {
	out.Reasons, out.Reason = out.Reasons[:0], ""
	addReason(out, p)
}

// interval is one path's covered span and what stops it.
type interval struct {
	from, to time.Time
	backupID string
	stop     evidencev1alpha1.CoverageStop
	// degraded is the path when the agent does not rate it healthy
	// though it reaches its frontier (degradedPath); nil otherwise.
	degraded *evidence.RecoveryPath
}

// degradedPath reports a path that reaches its frontier but that the
// agent rates below healthy: an unproven pgBackRest segment size across
// a log id or timeline switch, or the console's cap at a roll-up whose
// reason no path explains (internal/console/evidence capAtRollup,
// RollupCapCode: every backup off the newest timeline, a pgBackRest
// stanza split). Its span is drawn, but not as covered. Another backup's
// gap or unknown stop caps nothing: it is drawn where it is. A path stopped short has the state of its
// stop (a gap is unhealthy, a candidate gap a warning), and the span up
// to the stop is covered.
func degradedPath(p evidence.RecoveryPath) bool {
	return p.Stop == evidencev1alpha1.CoverageFrontier && p.State != evidencev1alpha1.Healthy
}

// pathNotHealthy says why a degraded path's span is not covered: lead,
// the agent's reason in parentheses when it gave one, then tail.
func pathNotHealthy(code string, p evidence.RecoveryPath, lead, tail string) viewmodel.Phrase {
	text := lead
	if p.Reason.Message != "" {
		text += " (" + p.Reason.Message + ")"
	}
	return say(code, text+tail, map[string]string{"backupId": p.BackupID, "state": string(p.State), "detail": p.Reason.Message})
}

// timelineInput is the evidence a timeline is drawn from.
type timelineInput struct {
	rec     evidence.Recovery
	backups []evidence.BackupItem
	facts   recoveryFacts
	// stale says why the evidence is stale; nil when fresh.
	stale     *viewmodel.Phrase
	scannedAt *time.Time
	refresh   time.Duration
}

// targetTimeline is the newest timeline a path was evaluated toward: a
// timeline the agent saw in the archive but could not connect by valid
// history (a path with no frontier) is not a restore target PostgreSQL
// can follow, so it does not move the bar; it is reported instead. An
// off-target path is one a default restore (recovery_target_timeline
// 'latest') does not follow: on the backup's own timeline when 'latest'
// left the backup behind, or on another timeline, newer or older, that
// is a sibling of the line 'latest' follows. Either way it never names the
// target, whatever its number; offTargetSpans lists what it reaches with
// the targetTLI a restore along it must give.
func targetTimeline(paths []evidence.RecoveryPath) (target, orphan uint64) {
	for _, p := range paths {
		if p.FrontierTimeline != nil && !offTarget(p) {
			target = max(target, p.TargetTimeline)
		}
	}
	for _, p := range paths {
		if p.FrontierTimeline == nil && !offTarget(p) && p.TargetTimeline > target {
			orphan = max(orphan, p.TargetTimeline)
		}
	}
	return target, orphan
}

// offTargetSpans are the spans the off-target paths reach, each with the
// timeline a restore along it must name (recovery_target_timeline, the
// path's target): a default restore does not follow them, so they are
// never drawn as covered, but a DBA can still restore along one by
// naming the timeline. Only a path that reached its backup's consistency
// point and kept its frontier receipt (a capped path loses it) is listed.
func offTargetSpans(paths []evidence.RecoveryPath, backups []evidence.BackupItem) []viewmodel.OffTargetSpan {
	ends := make(map[string]evidence.BackupItem, len(backups))
	for _, b := range backups {
		ends[b.ID] = b
	}
	var out []viewmodel.OffTargetSpan
	for _, p := range paths {
		if !offTarget(p) || p.FrontierTimeline == nil || p.LowerBoundAt == nil || p.FrontierReceiptAt == nil ||
			!p.FrontierReceiptAt.After(*p.LowerBoundAt) || !reachedConsistency(p, ends) {
			continue
		}
		tl := strconv.FormatUint(p.TargetTimeline, 10)
		phrase := say("recovery.offTarget.needsTarget",
			"Base backup "+p.BackupID+" reaches this span on timeline "+tl+", which a default restore (recovery_target_timeline 'latest') does not follow: a restore along it needs targetTLI "+tl+" (recovery_target_timeline: '"+tl+"').",
			map[string]string{"backupId": p.BackupID, "timeline": tl})
		out = append(out, viewmodel.OffTargetSpan{
			From: p.LowerBoundAt.UTC(), To: p.FrontierReceiptAt.UTC(), BackupID: p.BackupID, TargetTimeline: p.TargetTimeline,
			Code: phrase.Code, Params: phrase.Params, Text: phrase.Text,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].From.Equal(out[j].From) {
			return out[i].From.Before(out[j].From)
		}
		return out[i].TargetTimeline < out[j].TargetTimeline
	})
	return out
}

// offTarget reports a path a default restore does not follow: flagged
// off_target, or carrying the reason code that says so.
func offTarget(p evidence.RecoveryPath) bool {
	return p.OffTarget || p.Reason.Code == evidencev1alpha1.ReasonPathNeedsTargetTimeline
}

func cloneParams(p map[string]string) map[string]string {
	out := make(map[string]string, len(p)+1)
	for k, v := range p {
		out[k] = v
	}
	return out
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
		out.State = string(evidencev1alpha1.Unknown)
		if in.rec.CoverageReason != "" {
			setReason(out, evidenceReason("recovery.reason.agentCoverage", in.rec.CoverageReason))
		} else {
			setReason(out, say("recovery.reason.noConsistentPath",
				"no recovery path reaches its base backup's consistency point: the evidence cannot place a restore point", nil))
		}
		if in.stale != nil {
			addReason(out, *in.stale)
		}
		return
	}
	runs := mergeSpans(spans)
	margin := in.facts.frontierMargin()
	holes, candidates := drawRuns(out, spans, runs, margin)
	first, last := runs[0].from, runs[len(runs)-1].to
	out.Earliest, out.Latest = &first, &last
	tail := runs[len(runs)-1].stop
	if tail == evidencev1alpha1.CoverageFrontier {
		walEnd(out, in.facts, last, margin)
	}
	failedRecently := markEdges(out, in, first, last, margin)
	tailSegment(out, in, last, tail)
	if !in.rec.Complete {
		// The paths read may all be toward an older timeline than a cut
		// one: what they cover is not drawn as current.
		for i := range out.Segments {
			if out.Segments[i].Kind == segmentCovered {
				p := say("recovery.segment.coveredCut", "Covered by the paths read, but the collection was cut: a newer timeline may change where a restore goes.", nil)
				out.Segments[i].Kind, out.Segments[i].BackupID = segmentUnknown, ""
				out.Segments[i].Text, out.Segments[i].Code, out.Segments[i].Params = p.Text, p.Code, p.Params
			}
		}
	}
	sort.SliceStable(out.Segments, func(i, j int) bool { return out.Segments[i].From.Before(out.Segments[j].From) })
	var degraded *evidence.RecoveryPath
	newestDegraded, newestHealthy := false, false
	for _, sp := range spans {
		if sp.degraded == nil {
			newestHealthy = newestHealthy || sp.to.Equal(last)
			continue
		}
		if degraded == nil || sp.to.After(*degraded.FrontierReceiptAt) {
			degraded = sp.degraded
		}
		newestDegraded = newestDegraded || sp.to.Equal(last)
	}
	if !newestDegraded || newestHealthy {
		// When only a path not rated healthy reaches the newest end, no
		// target is named; a healthy path that ends there too covers it
		// (coveredPieces prefers it).
		out.NewestCoveredTarget = lastCovered(out.Segments)
	}
	timelineVerdict(out, in, timelineFindings{
		tail: tail, holes: holes, candidates: candidates, failedRecently: failedRecently, backlog: in.facts.backlog,
		archiverUnread: in.facts.archiverUnread, hibernated: in.facts.hibernated, notReported: in.facts.notReported, degraded: degraded, orphan: orphan, undrawn: undrawn,
	})
	if in.rec.CoverageReason != "" {
		// The agent's roll-up (the worst over every backup) is said beside
		// the spans, never drawn as un-coverage: each path is drawn by its
		// own state.
		addReason(out, evidenceReason("recovery.reason.agentCoverage", in.rec.CoverageReason))
	}
}

// recoverySpans is, for each path toward the target timeline that a
// default restore follows and the console did not cap, the span
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
		if p.TargetTimeline != target || p.FrontierTimeline == nil || offTarget(p) {
			// An off-target path toward the target's number is a sibling a
			// default restore does not follow: listed by offTargetSpans,
			// never drawn as covered.
			continue
		}
		for _, a := range p.Assumptions {
			assumptions[a] = true
		}
		if p.Reason.Code == cappedPathCode || p.LowerBoundAt == nil || p.FrontierReceiptAt == nil || !p.FrontierReceiptAt.After(*p.LowerBoundAt) || !reachedConsistency(p, ends) {
			// A path the console capped (its timeline evidence is not
			// established) loses its receipt there; it is skipped by its
			// code too, should a receipt ever survive the cap.
			undrawn++
			continue
		}
		sp := interval{from: p.LowerBoundAt.UTC(), to: p.FrontierReceiptAt.UTC(), backupID: p.BackupID, stop: p.Stop}
		if degradedPath(p) {
			path := p
			sp.degraded = &path
		}
		spans = append(spans, sp)
	}
	for a := range assumptions {
		out.Assumptions = append(out.Assumptions, a)
	}
	sort.Strings(out.Assumptions)
	return spans, undrawn
}

// lastCovered is the end of the newest covered segment: the newest
// instant a restore check answers "covered" (the newest covered target);
// nil when none is.
func lastCovered(segs []viewmodel.RecoverySegment) *time.Time {
	var last *time.Time
	for i := range segs {
		if segs[i].Kind == segmentCovered && (last == nil || segs[i].To.After(*last)) {
			to := segs[i].To.UTC()
			last = &to
		}
	}
	return last
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
			if p := piece.degraded; p != nil {
				out.Segments = append(out.Segments, segment(piece.from, piece.to, segmentUnknown, pathNotHealthy("recovery.segment.pathNotHealthy", *p,
					"Base backup "+p.BackupID+"'s recovery path reaches here, but the evidence agent rates it "+string(p.State), ": not counted as covered.")))
				continue
			}
			seg := segment(piece.from, piece.to, segmentCovered, say("recovery.segment.covered",
				"Structurally covered: base backup "+piece.backupID+" and contiguous archived WAL.", map[string]string{"backupId": piece.backupID}))
			seg.BackupID = piece.backupID
			out.Segments = append(out.Segments, seg)
		}
		if i+1 == len(runs) {
			break
		}
		uncertain(out, r.to.Add(-margin), r.to, say("recovery.segment.nearHole",
			"Within "+humanDuration(margin)+" of a hole in the archived WAL: segment names cannot place this target on either side.",
			map[string]string{"marginSeconds": secondsParam(margin)}))
		kind, text := holeOf(r.stop)
		switch kind {
		case segmentMissingWAL:
			holes++
		case segmentCandidateGap:
			candidates++
		}
		out.Segments = append(out.Segments, segment(r.to, runs[i+1].from, kind, text))
	}
	return holes, candidates
}

// markEdges makes both ends of the window uncertain. The newest edge: the
// last segment's commits are older than its receipt, by up to the
// archive_timeout an idle primary allows, and further after an archiving
// failure the archiver has not caught up from before the newest segment
// reached the repository (failedRecently), or while a backlog persists.
// The oldest edge: a retention purge can remove the oldest backup at any
// moment; the evidence shows it at most one scan later.
func markEdges(out *viewmodel.RecoveryTimeline, in timelineInput, first, last time.Time, margin time.Duration) (failedRecently bool) {
	marginText := say("recovery.segment.nearEnd",
		"Within "+humanDuration(margin)+" of the end of the archived WAL: segment names cannot tell whether this target's WAL is archived yet.",
		map[string]string{"marginSeconds": secondsParam(margin)})
	if in.facts.archiveTimeout <= 0 {
		marginText = say("recovery.segment.nearEndNoTimeout",
			"Near the end of the archived WAL with archive_timeout disabled: nothing bounds how old the newest archived commit is.", nil)
	}
	uncertain(out, last.Add(-margin), last, marginText)
	if f := in.facts.lastFailedWAL; f != nil && f.After(last.Add(-failureLookback)) && !caughtUpBefore(in.facts.caughtUpBy, last) {
		failedRecently = true
		from := minTime(*f, last).Add(-failureLookback)
		uncertain(out, from, last, say("recovery.segment.archivingFailed",
			"WAL archiving failed at "+f.Format(time.RFC3339)+": WAL that reaches the repository after a failure holds older commits than its arrival time says, so the arrival time overstates how recent a restore can be.",
			map[string]string{"failedAt": rfc3339(*f)}))
	}
	if n := in.facts.backlog; n > 0 {
		backlogEdge(out, in.facts, n, last, in.scannedAt)
	}
	if in.facts.archiverUnread {
		archiverUnreadEdge(out, in.facts, last, in.scannedAt)
	}
	purge := in.refresh
	if purge <= 0 {
		purge = evidence.DefaultAgentRefreshInterval
	}
	if in.scannedAt != nil && in.facts.now.After(*in.scannedAt) {
		purge += in.facts.now.Sub(*in.scannedAt)
	}
	uncertain(out, first, first.Add(purge), say("recovery.segment.purgeEdge",
		"Within "+humanDuration(purge)+" of the oldest restore point: a retention purge since the repository was last read may have removed this base backup.",
		map[string]string{"marginSeconds": secondsParam(purge)}))
	return failedRecently
}

// caughtUpBefore reports an archiver that had caught up from its failure
// by a sweep no later than last, the newest segment's receipt: that
// segment, and every one before it, reached the repository after the
// catch-up, so its receipt bounds its commits as without a failure.
func caughtUpBefore(by *time.Time, last time.Time) bool {
	return by != nil && !by.After(last)
}

// backlogEdge marks the newest edge while n segments wait on the primary
// across sweeps: a segment archived during the backlog reached the
// repository late, its commits older than its receipt by however long
// it waited. From the newest sweep, at or before the scan, that read no
// .ready file on the primary, less one archive_timeout: nothing waited
// then, so every segment completed by then (all commits up to one
// archive_timeout before it) was in the repository when it was read.
// A busy primary may never read zero: a sweep with files waiting bounds
// the onset too, by the WAL positions (archivedBounds), and the later of
// the two bounds is drawn. From failureLookback before the end when
// neither exists, the sweep period is not known, or archive_timeout is
// disabled.
func backlogEdge(out *viewmodel.RecoveryTimeline, f recoveryFacts, n int, last time.Time, scanned *time.Time) {
	segments := strconv.Itoa(n)
	quiet := f.quietBy(scanned)
	if b := f.boundBy(scanned); b != nil && f.archiveTimeout > 0 && f.sweepInterval > 0 && (quiet == nil || b.before.After(quiet.Add(-f.archiveTimeout))) {
		uncertain(out, b.before, last, say("recovery.segment.archiveBacklogBound",
			segments+" WAL segments are waiting to be archived on the primary; by its reading at "+rfc3339(b.readAt)+" every commit before "+rfc3339(b.before)+" was archived: WAL archived since then may have reached the repository late, so its arrival time overstates how recent a restore can be.",
			map[string]string{"segments": segments, "readAt": rfc3339(b.readAt), "boundAt": rfc3339(b.before)}))
		return
	}
	if quiet == nil || f.archiveTimeout <= 0 || f.sweepInterval <= 0 {
		uncertain(out, last.Add(-failureLookback), last, say("recovery.segment.archiveBacklog",
			segments+" WAL segments are waiting to be archived on the primary, and no sweep the console keeps saw it with none waiting: segments reach the repository late, so the arrival time of the newest overstates how recent a restore can be.",
			map[string]string{"segments": segments}))
		return
	}
	uncertain(out, quiet.Add(-f.archiveTimeout), last, say("recovery.segment.archiveBacklogSince",
		segments+" WAL segments are waiting to be archived on the primary, which last had none waiting at "+rfc3339(*quiet)+": WAL archived since then may have reached the repository late, so its arrival time overstates how recent a restore can be.",
		map[string]string{"segments": segments, "quietAt": rfc3339(*quiet)}))
}

// archiverUnreadEdge marks the newest edge while the primary's archiver
// is not read (or the Cluster is hibernated): nothing says how long the
// WAL that reached the repository waited on the primary first. From the
// newest sweep, at or before the scan, that read no .ready file on it,
// less one archive_timeout (as backlogEdge); else from failureLookback
// before the end.
func archiverUnreadEdge(out *viewmodel.RecoveryTimeline, f recoveryFacts, last time.Time, scanned *time.Time) {
	if f.notReported {
		// A designated primary never reports readyWalFiles: no sweep can
		// say nothing waited on it.
		uncertain(out, last.Add(-failureLookback), last, say("recovery.segment.archiverNotReported",
			"The archiving instance is a designated primary, which does not report how many WAL segments wait to be archived: WAL that reached the repository may have waited on it first, so its arrival time may overstate how recent a restore can be.", nil))
		return
	}
	quiet := f.quietBy(scanned)
	if b := f.boundBy(scanned); b != nil && f.archiveTimeout > 0 && f.sweepInterval > 0 && (quiet == nil || b.before.After(quiet.Add(-f.archiveTimeout))) {
		uncertain(out, b.before, last, say("recovery.segment.archiverUnreadBound",
			"The primary's archiver is not read now; by its reading at "+rfc3339(b.readAt)+" every commit before "+rfc3339(b.before)+" was archived: WAL archived after that may have waited on the primary first, so its arrival time may overstate how recent a restore can be.",
			map[string]string{"readAt": rfc3339(b.readAt), "boundAt": rfc3339(b.before)}))
		return
	}
	if quiet == nil || f.archiveTimeout <= 0 || f.sweepInterval <= 0 {
		uncertain(out, last.Add(-failureLookback), last, say("recovery.segment.archiverUnread",
			"The primary's archiver is not read, and no sweep the console keeps saw it with nothing waiting: WAL that reached the repository may have waited on the primary first, so its arrival time may overstate how recent a restore can be.", nil))
		return
	}
	uncertain(out, quiet.Add(-f.archiveTimeout), last, say("recovery.segment.archiverUnreadSince",
		"The primary's archiver is not read now; it last had nothing waiting at "+rfc3339(*quiet)+": WAL archived after that may have waited on the primary first, so its arrival time may overstate how recent a restore can be.",
		map[string]string{"quietAt": rfc3339(*quiet)}))
}

// walEnd marks the band just before the newest restore point where the
// archived WAL ends: the frontier is when the last segment reached the
// repository, not the last commit it holds. That commit can be one
// archive_timeout before the segment switched, and the receipt is the
// switch plus the archiver's latency, so the band is archive_timeout plus
// a latency allowance (archiveLatencyAllowance); the whole margin when
// archive_timeout is disabled. A recovery_target_time after that last
// commit stops recovery with "recovery ended before configured recovery
// target was reached", so the band says to restore to the end of WAL
// instead. Drawn before the other edges, which then leave it as it is.
func walEnd(out *viewmodel.RecoveryTimeline, f recoveryFacts, last time.Time, margin time.Duration) {
	band := margin
	if f.archiveTimeout > 0 {
		band = f.archiveTimeout + f.archiveLatencyAllowance()
	}
	uncertainAs(out, segmentWALEnd, last.Add(-band), last, say("recovery.segment.walEnd",
		"WAL ends near here: the newest archived segment reached the repository at "+rfc3339(last)+", and its last commit may be up to "+humanDuration(band)+
			" older. A recovery_target_time after the last commit fails with \"recovery ended before configured recovery target was reached\"; to restore everything archived, restore to the end of WAL (no recovery target).",
		map[string]string{"endAt": rfc3339(last), "marginSeconds": secondsParam(band)}))
}

// tailSegment draws the time after the newest restore point: only a
// confirmed gap says the time is not restorable; anything else leaves it
// unknown. A fresh, complete frontier is a fact about the scan, not about
// now: the WAL of a time just after it may have been in a segment not
// archived yet when the agent read the repository (archive_timeout), and
// reach it after the scan, so the time after the frontier, before the
// scan included, is unknown.
func tailSegment(out *viewmodel.RecoveryTimeline, in timelineInput, last time.Time, tail evidencev1alpha1.CoverageStop) {
	if !in.facts.now.After(last) {
		return
	}
	var p viewmodel.Phrase
	kind := segmentUnknown
	switch {
	case tail == evidencev1alpha1.CoverageGapLimited:
		kind = segmentMissingWAL
		p = say("recovery.segment.missingAfterLatest", "WAL is missing from the repository after the latest restore point.", nil)
	case tail != evidencev1alpha1.CoverageFrontier:
		_, p = holeOf(tail)
	case !in.rec.Complete:
		p = say("recovery.segment.collectionCut", "The recovery path collection was cut at a bound: newer coverage may exist.", nil)
	case in.stale != nil:
		p = say("recovery.segment.evidenceStale", "The repository evidence is stale: newer WAL may have reached the repository since.", nil)
	case in.facts.failing && in.scannedAt != nil:
		p = say("recovery.segment.afterLastReadFailing",
			"Not in the repository when it was last read ("+in.scannedAt.UTC().Format(time.RFC3339)+"), and WAL archiving is failing: this time's WAL is likely not in the repository.",
			map[string]string{"scannedAt": rfc3339(*in.scannedAt)})
	case in.facts.failing:
		p = say("recovery.segment.afterFrontierFailing", "After the newest archived WAL the evidence shows, and WAL archiving is failing: this time's WAL is likely not in the repository.", nil)
	case in.facts.backlog > 0 && in.scannedAt != nil:
		n := strconv.Itoa(in.facts.backlog)
		p = say("recovery.segment.afterLastReadBacklog",
			"Not in the repository when it was last read ("+in.scannedAt.UTC().Format(time.RFC3339)+"), and "+n+" WAL segments are waiting to be archived on the primary: this time's WAL is likely still only on the primary.",
			map[string]string{"scannedAt": rfc3339(*in.scannedAt), "segments": n})
	case in.facts.backlog > 0:
		n := strconv.Itoa(in.facts.backlog)
		p = say("recovery.segment.afterFrontierBacklog",
			"After the newest archived WAL the evidence shows, and "+n+" WAL segments are waiting to be archived on the primary: this time's WAL is likely still only on the primary.",
			map[string]string{"segments": n})
	case in.scannedAt != nil:
		p = say("recovery.segment.afterLastRead",
			"Not in the repository when it was last read ("+in.scannedAt.UTC().Format(time.RFC3339)+"): WAL archived since may cover this time.",
			map[string]string{"scannedAt": rfc3339(*in.scannedAt)})
	default:
		p = say("recovery.segment.afterFrontier", "After the newest archived WAL the evidence shows: WAL archived since may cover this time.", nil)
	}
	out.Segments = append(out.Segments, segment(last, in.facts.now, kind, p))
}

// archivingFailing reads the archiving fact as forecasts do: the
// verdict is critical, or the primary's last attempt failed with WAL
// waiting (cnpgstatus.Archiving's FailureWithBacklog).
func archivingFailing(a viewmodel.Archiving) bool {
	return a.Health == viewmodel.HealthCritical || a.Reason == cnpgstatus.ArchivingReasonFailureWithBacklog
}

// reportedTail draws the time after a reported window's end. The end is
// not a WAL frontier (the operator's lastSuccessfulBackupTime is the last
// base backup), so the time after it is never "not covered"; what the
// console can add is the archiving instance's own pg_stat_archiver
// report: last_archived_time says up to when WAL was reaching the
// repository, and a failing or backlogged archiver that WAL after it
// likely is not. A restore "may well be possible" only before the last
// archived WAL time and with archiving healthy, fresh and keeping up.
// last_archived_time is the instance's clock: a time after the console
// read it is clamped to the reading.
func reportedTail(window viewmodel.Provenance, arch viewmodel.Archiving, facts recoveryFacts, last time.Time) []viewmodel.RecoverySegment {
	now := facts.now
	origin, prefix := "operator", "After the last base backup the operator reports ("+rfc3339(last)+"): the operator does not report the archived WAL beyond it. "
	if window.Origin == viewmodel.OriginEvidence {
		origin, prefix = "evidence", "After the end of the window the evidence agent reported ("+rfc3339(last)+"): without the agent's recovery paths the console cannot place this time. "
	}
	failing := archivingFailing(arch)
	backlog := facts.backlog > 0 && !failing
	healthy := !failing && !backlog && arch.Health == viewmodel.HealthOK && !arch.Provenance.Stale
	archived := arch.LastArchivedAt
	if archived != nil && arch.Provenance.ObservedAt != nil && archived.After(*arch.Provenance.ObservedAt) {
		clamped := arch.Provenance.ObservedAt.UTC()
		archived = &clamped
	}
	segments := strconv.Itoa(facts.backlog)
	params := func() map[string]string {
		p := map[string]string{"windowOrigin": origin, "endAt": rfc3339(last)}
		if archived != nil {
			p["archivedAt"] = rfc3339(*archived)
		}
		if arch.Provenance.ObservedAt != nil {
			p["ageSeconds"] = secondsParam(max(now.Sub(*arch.Provenance.ObservedAt), 0))
		}
		if backlog {
			p["segments"] = segments
		}
		return p
	}
	unknownSeg := func(from, to time.Time, code, text string) viewmodel.RecoverySegment {
		return segment(from, to, segmentUnknown, say("recovery.reportedTail."+code, prefix+text, params()))
	}
	if archived == nil {
		switch {
		case failing:
			return []viewmodel.RecoverySegment{unknownSeg(last, now, "archivedUnknownFailing",
				"WAL archiving is failing and the primary's last archived WAL time is not known: this time's WAL is likely not in the repository.")}
		case backlog:
			return []viewmodel.RecoverySegment{unknownSeg(last, now, "archivedUnknownBacklog",
				segments+" WAL segments are waiting to be archived on the primary and its last archived WAL time is not known: this time's WAL may still be only on the primary.")}
		}
		return []viewmodel.RecoverySegment{unknownSeg(last, now, "archivedUnknown",
			"The primary's last archived WAL time is not known (its /pg/status was not read, or it reports none): the console cannot say whether this time's WAL reached the repository; deploy an evidence agent to place it.")}
	}
	report := "its own report on /pg/status"
	if arch.Provenance.ObservedAt != nil {
		report += ", read " + humanDuration(max(now.Sub(*arch.Provenance.ObservedAt), 0)) + " ago"
	}
	at := archived.UTC().Format(time.RFC3339)
	var segs []viewmodel.RecoverySegment
	if archived.After(last) {
		to := minTime(*archived, now)
		switch {
		case healthy:
			segs = append(segs, unknownSeg(last, to, "beforeArchived",
				"WAL archiving is healthy and the primary last archived a segment at "+at+", after this time ("+report+"): a restore to this time may well be possible; deploy an evidence agent to place it."))
		case failing:
			segs = append(segs, unknownSeg(last, to, "beforeArchivedFailing",
				"The primary last archived a segment at "+at+", after this time ("+report+"), but WAL archiving is failing: this time's WAL may be in the repository; deploy an evidence agent to place it."))
		case backlog:
			segs = append(segs, unknownSeg(last, to, "beforeArchivedBacklog",
				"The primary last archived a segment at "+at+", after this time ("+report+"), with "+segments+" segments waiting to be archived: the archiver is behind, so this time's WAL may not be in the repository yet; deploy an evidence agent to place it."))
		default:
			segs = append(segs, unknownSeg(last, to, "beforeArchivedUnconfirmed",
				"The primary last archived a segment at "+at+", after this time ("+report+"), but WAL archiving is not confirmed healthy: this time's WAL may be in the repository; deploy an evidence agent to place it."))
		}
	}
	if from := maxTime(last, *archived); now.After(from) {
		switch {
		case failing:
			segs = append(segs, unknownSeg(from, now, "afterArchivedFailing",
				"WAL archiving is failing: WAL after the primary's last archived segment ("+at+", "+report+") is likely not in the repository."))
		case backlog:
			segs = append(segs, unknownSeg(from, now, "afterArchivedBacklog",
				segments+" WAL segments are waiting to be archived on the primary: WAL after its last archived segment ("+at+", "+report+") is likely still only on the primary."))
		default:
			segs = append(segs, unknownSeg(from, now, "afterArchived",
				"This time is after the primary's last archived segment ("+at+", "+report+"): its WAL may still be only on the primary."))
		}
	}
	return segs
}

// timelineFindings are what the drawing found that decides the verdict.
type timelineFindings struct {
	tail              evidencev1alpha1.CoverageStop
	holes, candidates int
	failedRecently    bool
	// backlog is the archiver's waiting segments, 0 when keeping up.
	backlog int
	// archiverUnread, hibernated, notReported: the primary's archiver is
	// not read, or reports no readyWalFiles.
	archiverUnread, hibernated, notReported bool
	// degraded is the newest drawn path the agent does not rate healthy
	// though it reaches its frontier; nil when none is.
	degraded *evidence.RecoveryPath
	orphan   uint64
	undrawn  int
}

// timelineVerdict states the bar's state and reason, the gravest finding
// first: a cut collection, missing WAL at the end, an unconfirmed end,
// holes, candidate gaps, a recent archiving failure, an archive backlog,
// an orphan timeline, stale evidence.
func timelineVerdict(out *viewmodel.RecoveryTimeline, in timelineInput, f timelineFindings) {
	warning := string(evidencev1alpha1.Warning)
	switch {
	case !in.rec.Complete:
		out.State = string(evidencev1alpha1.Unknown)
		setReason(out, say("recovery.reason.pathsCut", "the recovery path collection was cut at a bound: the restore target timeline cannot be told", nil))
	case f.tail == evidencev1alpha1.CoverageGapLimited:
		out.State = string(evidencev1alpha1.Unhealthy)
		setReason(out, say("recovery.reason.missingAfterLatest", "WAL is missing after the latest restore point: recent times are not restorable from the repository", nil))
	case f.tail != evidencev1alpha1.CoverageFrontier:
		out.State = warning
		_, p := holeOf(f.tail)
		setReason(out, p)
	case f.holes > 0:
		out.State = warning
		setReason(out, say("recovery.reason.holes", "the window has holes where WAL is missing", nil))
	case f.candidates > 0:
		out.State = warning
		setReason(out, say("recovery.reason.candidateGaps", "the window has candidate gaps a second scan has not confirmed yet", nil))
	case f.degraded != nil:
		out.State = warning
		setReason(out, pathNotHealthy("recovery.reason.pathNotHealthy", *f.degraded,
			"the recovery path from base backup "+f.degraded.BackupID+" reaches the archive frontier, but the evidence agent rates it "+string(f.degraded.State), ""))
	case f.failedRecently:
		out.State = warning
		setReason(out, say("recovery.reason.failedRecently", "WAL archiving failed recently: the newest restore points are uncertain", nil))
	case f.backlog > 0:
		out.State = warning
		n := strconv.Itoa(f.backlog)
		setReason(out, say("recovery.reason.archiveBacklog", n+" WAL segments are waiting to be archived on the primary: the newest restore points are uncertain",
			map[string]string{"segments": n}))
	case f.notReported:
		out.State = warning
		setReason(out, say("recovery.reason.archiverNotReported", "the archiving instance is a designated primary, which does not report its archive backlog: how late recent WAL reached the repository is not known, so the newest restore points are uncertain", nil))
	case f.hibernated:
		// A hibernated Cluster writes no WAL and runs no archiver: nothing
		// is going wrong that a warning would ask to act on, and nothing
		// confirms that the WAL written before it reached the repository.
		// The newest restore points are unknown until it wakes up.
		out.State = string(evidencev1alpha1.Unknown)
		setReason(out, say("recovery.reason.archiverHibernated", "the Cluster is hibernated: whether the WAL written before it reached the repository is not confirmed, so the newest restore points are uncertain", nil))
	case f.archiverUnread:
		out.State = warning
		setReason(out, say("recovery.reason.archiverUnread", "the primary's archiver was not read: how late recent WAL reached the repository is not known, so the newest restore points are uncertain", nil))
	case f.orphan > 0:
		out.State = warning
		tl := strconv.FormatUint(f.orphan, 10)
		setReason(out, say("recovery.reason.orphanTimeline", "timeline "+tl+" is in the archive but not connected by valid history: a restore cannot follow it",
			map[string]string{"timeline": tl}))
	case in.stale != nil:
		out.State = warning
		setReason(out, viewmodel.Phrase{})
	default:
		out.State = string(evidencev1alpha1.Healthy)
		setReason(out, viewmodel.Phrase{})
	}
	if in.stale != nil {
		addReason(out, *in.stale)
	}
	if f.undrawn > 0 && out.Reason == "" {
		addReason(out, say("recovery.reason.undrawn", "some backups have no observed recovery path past their consistency point", nil))
	}
}

// uncertain turns the covered part of [from, to] into an unknown
// segment saying p, splitting the covered segments it overlaps.
func uncertain(out *viewmodel.RecoveryTimeline, from, to time.Time, p viewmodel.Phrase) {
	uncertainAs(out, segmentUnknown, from, to, p)
}

// uncertainAs is uncertain with the segment kind given.
func uncertainAs(out *viewmodel.RecoveryTimeline, kind string, from, to time.Time, p viewmodel.Phrase) {
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
		mid := segment(maxTime(seg.From, from), minTime(seg.To, to), kind, p)
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
// the piece. A path the agent rates healthy is preferred; a piece only a
// degraded path covers carries that path (degraded). Adjacent pieces of
// the same backup are joined.
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
			if best < 0 {
				best = j
				continue
			}
			if healthy, bestHealthy := sp.degraded == nil, spans[best].degraded == nil; healthy != bestHealthy {
				if healthy {
					best = j
				}
				continue
			}
			if sp.from.After(spans[best].from) || (sp.from.Equal(spans[best].from) && sp.backupID > spans[best].backupID) {
				best = j
			}
		}
		if best < 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].backupID == spans[best].backupID && out[n-1].to.Equal(a) && (out[n-1].degraded == nil) == (spans[best].degraded == nil) {
			out[n-1].to = b
			continue
		}
		out = append(out, interval{from: a, to: b, backupID: spans[best].backupID, degraded: spans[best].degraded})
	}
	return out
}

// holeOf names the span after a path's frontier.
func holeOf(stop evidencev1alpha1.CoverageStop) (kind string, text viewmodel.Phrase) {
	switch stop {
	case evidencev1alpha1.CoverageGapLimited:
		return segmentMissingWAL, say("recovery.segment.missingWAL", "WAL is missing from the repository: no restore target in this span.", nil)
	case evidencev1alpha1.CoverageCandidateLimited:
		return segmentCandidateGap, say("recovery.segment.candidateGap", "A WAL gap seen by one scan, not yet confirmed by a second.", nil)
	default: // CoverageUnknownLimited, and a frontier that is not a stop
		return segmentUnknown, say("recovery.segment.continuationUnknown", "The evidence cannot tell whether WAL continues here.", nil)
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
		Ref: b.Ref, GeneratedAt: b.GeneratedAt, At: at, Basis: t.Basis, Caveats: restoreCaveats(b), Provenance: t.Provenance,
		HistoryFile: timelineHistoryFile(b),
	}
	answer := func(a string, p viewmodel.Phrase) viewmodel.RestoreCheck {
		out.Answer, out.Text, out.Code, out.Params = a, p.Text, p.Code, p.Params
		if a != "covered" {
			// A sibling or own-timeline path a default restore does not
			// follow may still reach the time: say which timeline to name.
			for _, o := range t.OffTarget {
				if !at.Before(o.From) && !at.After(o.To) {
					out.Caveats = append(out.Caveats, viewmodel.Phrase{Code: o.Code, Params: o.Params, Text: o.Text})
				}
			}
		}
		return out
	}
	switch {
	case t.Basis == "none" || t.Earliest == nil || t.Latest == nil:
		reason := orDefault(t.Reason, "no evidence")
		out.Reasons = t.Reasons
		return answer("unknown", say("recovery.check.noWindow", "No restore window is known for this cluster: "+reason+".", map[string]string{"reason": reason}))
	case at.Before(*t.Earliest) && t.Basis == "evidence" && !t.Complete:
		return answer("unknown", say("recovery.check.beforeEarliestCut",
			"Before the earliest restore point the evidence shows ("+t.Earliest.Format(time.RFC3339)+"), but the collections were cut: an older backup may exist.",
			map[string]string{"earliestAt": rfc3339(*t.Earliest)}))
	case at.Before(*t.Earliest) && t.Basis == "evidence":
		return answer("notCovered", say("recovery.check.beforeEarliest",
			"Before the earliest restore point ("+t.Earliest.Format(time.RFC3339)+"): no base backup that old is in the repository.",
			map[string]string{"earliestAt": rfc3339(*t.Earliest)}))
	case at.Before(*t.Earliest):
		return answer("notCovered", say("recovery.check.beforeEarliestReported",
			"Before the earliest restore point ("+t.Earliest.Format(time.RFC3339)+"): no base backup that old is reported.",
			map[string]string{"earliestAt": rfc3339(*t.Earliest)}))
	case t.Basis == "reported" && !at.After(*t.Latest) && t.Provenance.Origin == viewmodel.OriginEvidence:
		return answer("reported", say("recovery.check.reportedByAgent",
			"Inside the window the evidence agent reports ("+t.Provenance.Source+"); its recovery paths do not confirm it.",
			map[string]string{"source": t.Provenance.Source}))
	case t.Basis == "reported" && !at.After(*t.Latest):
		return answer("reported", say("recovery.check.reported",
			"Inside the window the operator reports ("+t.Provenance.Source+"); no repository evidence confirms it.",
			map[string]string{"source": t.Provenance.Source}))
	}
	segs := t.Segments
	if n := len(segs); n > 0 && at.After(segs[n-1].To) && segs[n-1].Kind != segmentCovered && segs[n-1].Kind != segmentReported {
		// Later than the tail, which is drawn up to now: the same answer
		// as now (a later time's WAL does not exist yet, and neither the
		// clocks nor the tail are exact enough to say "not covered").
		segs = segs[n-1:]
		at = segs[0].To
	}
	for _, seg := range segs {
		if at.Before(seg.From) || at.After(seg.To) {
			continue
		}
		p := viewmodel.Phrase{Code: seg.Code, Params: seg.Params, Text: seg.Text}
		switch seg.Kind {
		case segmentCovered:
			out.BackupID = seg.BackupID
			return answer("covered", say("recovery.check.covered",
				"Structurally covered: restore from base backup "+seg.BackupID+" and replay the archived WAL to this time.",
				map[string]string{"backupId": seg.BackupID}))
		case segmentMissingWAL, segmentCandidateGap:
			return answer("gap", p)
		case segmentReported:
			continue
		default:
			return answer("unknown", p)
		}
	}
	return answer("unknown", say("recovery.check.notPlaced", "The evidence does not place this time.", nil))
}

// timelineHistoryFile is the history file the timeline's reasons or the
// repository's timeline evidence name as missing or unreadable: the
// check shows it, since one such file can withhold every restore point.
func timelineHistoryFile(b viewmodel.Backups) string {
	texts := make([]string, 0, len(b.RecoveryTimeline.Reasons)+2)
	for _, r := range b.RecoveryTimeline.Reasons {
		if f := r.Params["historyFile"]; f != "" {
			return f
		}
		texts = append(texts, r.Text)
	}
	if b.Evidence.Repository != nil && b.Evidence.Repository.Timeline.State != string(evidencev1alpha1.Warning) {
		// A timeline warning names a history no default restore reads:
		// coverage does not rest on it (it is a caveat instead).
		if p := b.Evidence.Repository.Timeline.Phrase; p != nil && p.Params["historyFile"] != "" {
			return p.Params["historyFile"]
		}
		texts = append(texts, b.Evidence.Repository.Timeline.Message, b.Evidence.Repository.Coverage.Message)
	}
	return historyFileIn(texts...)
}

// restoreCaveats are what an answer does not prove: the evidence's own
// limits, a stale or cut basis, failing or unread archiving and the
// absence of a passed restore drill.
func restoreCaveats(b viewmodel.Backups) []viewmodel.Phrase {
	t := b.RecoveryTimeline
	out := []viewmodel.Phrase{}
	switch t.Basis {
	case "evidence":
		out = append(out, evidenceCaveats...)
		if !t.Complete {
			out = append(out, say("recovery.caveat.collectionsCut", "The evidence collections were cut at a bound: a span shown as not covered may be covered.", nil))
		}
		if t.Provenance.Stale {
			out = append(out, say("recovery.caveat.evidenceStale", "The repository evidence is stale.", nil))
		}
		if w := t.TimelineWarning; w != nil {
			params := map[string]string{"detail": w.Text}
			if f := w.Params["historyFile"]; f != "" {
				params["historyFile"] = f
			}
			out = append(out, say("recovery.caveat.timelineWarning",
				"The evidence reports a timeline history file no default restore reads as invalid ("+w.Text+"): the coverage shown does not rest on it, but a restore that names its timeline would fail.",
				params))
		}
	case "reported":
		if t.Provenance.Stale {
			out = append(out, say("recovery.caveat.windowStale", "The reported window is stale.", nil))
		}
	default:
		return out
	}
	if a := b.Summary.Archiving; archivingFailing(a) {
		if a.LastArchivedAt != nil {
			out = append(out, say("recovery.caveat.archivingFailingSince",
				"WAL archiving is failing: the primary last archived a segment at "+a.LastArchivedAt.UTC().Format(time.RFC3339)+"; newer WAL is on the instance, not in the repository.",
				map[string]string{"archivedAt": rfc3339(*a.LastArchivedAt)}))
		} else {
			out = append(out, say("recovery.caveat.archivingFailing", "WAL archiving is failing: the newest WAL is on the instance, not in the repository.", nil))
		}
	} else if a.Reason == cnpgstatus.ArchivingReasonBacklogNotReported {
		out = append(out, say("recovery.caveat.archiverNotReported",
			"The archiving instance is a designated primary, which does not report its archive backlog: how long the newest archived WAL waited on it is not known, so restore points near the newest edge may not be in the repository.", nil))
	} else if archiverUnread(a) {
		out = append(out, say("recovery.caveat.archiverUnread",
			"The primary's archiver was not read: how long the newest archived WAL waited on the primary is not known, so it may hold commits older than its arrival in the repository suggests, and restore points near the newest edge may not be in the repository.", nil))
	}
	if !b.Drills.RestoreTested {
		days := strconv.Itoa(b.Drills.WindowDays)
		out = append(out, say("recovery.caveat.notRestoreTested", "Not restore-tested: no restore drill of this Cluster passed in the last "+days+" days.", map[string]string{"days": days}))
	}
	return out
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
