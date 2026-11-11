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
	"math"
	"slices"
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
	// primaryUnread: the sweep did not read status.currentPrimary
	// (primaryArchiver is nil), whatever the archiving fact's reason: with
	// ContinuousArchiving False the reason is CNPG's own, not
	// PrimaryUnread (round 13, CNPG P3-1).
	primaryUnread bool
	// quiet are the sweeps, oldest first, that read the current primary's
	// archiver with no .ready file at all, since it became primary: at
	// each, nothing waited to be archived. A reading of a few files
	// proves nothing (a slow archiver can hold them for long), and a
	// failed read is no reading.
	quiet []quietSweep
	// bounds are, for the sweeps that read the current primary with
	// .ready files waiting, the time before which every commit was
	// archived by then (archivedBounds); empty when the WAL positions or
	// the segment size are not known.
	bounds []archivedBound
	// former is what the sweeps kept of the instances that were primary
	// before the current one was promoted: their archiver wrote the WAL
	// of the older timelines, so a restore point near the end of WAL on
	// such a timeline (a failover's parent tail, a hole before the
	// switch) is bounded by their backlog, not the current primary's.
	former formerArchiver
	// promoted is when the current primary became primary
	// (status.currentPrimaryTimestamp); zero when it does not parse.
	promoted time.Time
	// liveTimeline is the timeline the current primary writes
	// (liveTimelineOf); 0 when the console cannot tell.
	liveTimeline uint64
	// rejoined are the instances back in the Cluster since the promotion:
	// the current primary, and every standby that is Ready and read as a
	// standby since then (rejoinedSince). CloudNativePG archives the WAL
	// waiting on a former primary before it rejoins (and does not start it
	// otherwise), so such an instance no longer holds unarchived WAL.
	rejoined map[string]bool
}

// formerHolder is the former primary that may still hold the unarchived
// WAL of timeline (formerArchiver holder), and whether it has rejoined
// the Cluster since: its waiting WAL was archived then, or never was on
// it. Empty when the console names none.
func (f recoveryFacts) formerHolder(timeline uint64) (name string, rejoined bool) {
	name = f.former.holder(timeline)
	return name, name != "" && f.rejoined[name]
}

// archivedBound says that by the sweep at readAt every commit before
// before was in WAL the archiver had archived. timeline is the timeline
// the archiver was primary on at that sweep (former primaries only; 0
// for the current primary's, which need none).
type archivedBound struct {
	readAt, before time.Time
	timeline       uint64
}

// quietSweep is a sweep that read the archiver with no .ready file: at
// is the sweep's start (the count was taken at or after it), by is when
// the read returned (the count was taken at or before it). Every segment
// completed by at was archived by by. timeline is as archivedBound's.
type quietSweep struct {
	at, by   time.Time
	timeline uint64
}

// formerArchiver are the readings of the former primaries' archivers
// from before the promotion (status.currentPrimaryTimestamp), each
// stamped with the timeline the instance was primary on then (sweep
// SeriesTimeline): the rings keep a readyWalFiles value only while an
// instance reports itself primary (sweep pushRings), and only the
// archiver of a timeline says how late that timeline's WAL reached the
// repository. read holds the timelines some ring has a reading of; a
// timeline it lacks (the former primary's pod is gone, the ring's
// retention passed, the reading carried no timeline, or the promotion
// time is not known) has no archiver state at the switch.
type formerArchiver struct {
	read   map[uint64]bool
	quiet  []quietSweep
	bounds []archivedBound
	// holders are, per timeline, the instances read as its primary.
	holders map[uint64]map[string]bool
}

// holder is the one instance read as primary on timeline: the former
// primary that may still hold that timeline's unarchived WAL. Empty when
// none or more than one is (the console then names none).
func (f formerArchiver) holder(timeline uint64) string {
	if len(f.holders[timeline]) != 1 {
		return ""
	}
	for name := range f.holders[timeline] {
		return name
	}
	return ""
}

// on is the former archiver's readings of one timeline.
func (f formerArchiver) on(timeline uint64) (read bool, quiet []quietSweep, bounds []archivedBound) {
	if timeline == 0 || !f.read[timeline] {
		return false, nil, nil
	}
	for _, q := range f.quiet {
		if q.timeline == timeline {
			quiet = append(quiet, q)
		}
	}
	for _, b := range f.bounds {
		if b.timeline == timeline {
			bounds = append(bounds, b)
		}
	}
	return true, quiet, bounds
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
	f.primaryUnread = live == nil
	switch b := cnpgstatus.ArchiveBacklogOf(g.cluster, live); {
	case b.Behind():
		f.backlog = b.Segments
	case b.Unconfirmed > 0:
		f.backlog = b.Unconfirmed
	}
	primary := g.cluster.Status.CurrentPrimary
	promoted := promotedAt(g)
	if f.backlog > 0 || f.archiverUnread {
		var until time.Time
		if live != nil {
			until = live.ObservedAt
		}
		f.quiet = quietSweeps(g.readyRings[primary], promoted, until, f.sweepInterval)
		f.bounds = archivedBounds(g, primary, g.lsnRings[primary], promoted, time.Time{}, f.archiveTimeout, f.sweepInterval)
	}
	f.former = formerArchiverOf(g, promoted, f.archiveTimeout, f.sweepInterval)
	f.promoted, f.liveTimeline, f.rejoined = promoted, liveTimelineOf(g, promoted), rejoinedSince(g, promoted)
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

// promotedAt is when the current primary became primary
// (status.currentPrimaryTimestamp); zero when it does not parse.
func promotedAt(g *gathered) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, g.cluster.Status.CurrentPrimaryTimestamp); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// readByOf is when the read behind a ring point returned: the sweep
// keeps it (sweep ring readBy); for a point restored without it, the
// sweep's start plus one interval (a sweep's reads run within its
// interval), the start itself when the interval is not known.
func readByOf(p viewmodel.Point, interval time.Duration) time.Time {
	if !p.ReadBy.IsZero() && !p.ReadBy.Before(p.At) {
		return p.ReadBy.UTC()
	}
	return p.At.Add(max(interval, 0)).UTC()
}

// quietSweeps are the sweeps, oldest first, of an instance's
// readyWalFiles ring that read no .ready file at all, from from (the
// promotion, when it parses) and before until (the current reading, when
// there is one; zero for no limit): a reading taken while the instance
// was a replica holds no value, a failed read is a gap, so both only
// widen.
func quietSweeps(ring []viewmodel.Point, from, until time.Time, interval time.Duration) []quietSweep {
	var out []quietSweep
	for _, p := range ring {
		if p.Value == nil || *p.Value != 0 || p.At.Before(from) || (!until.IsZero() && !p.At.Before(until)) {
			continue
		}
		out = append(out, quietSweep{at: p.At.UTC(), by: readByOf(p, interval)})
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
// T' is the newest such sweep at or before T, both in [from, until) (the
// instance's term as primary; until zero for no end); a failed read is
// no T or T' (it only widens). T is when the read returned (readByOf), T'
// the sweep's start, before the position was read: each is the
// conservative end of its read. CloudNativePG reads the position before
// it counts the .ready files (catalogpins TestPinLSNBeforeReadyCount),
// so a segment completed between the two reads only raises k. Empty
// without the position ring, the segment size, or archive_timeout.
func archivedBounds(g *gathered, instance string, lsnRing []viewmodel.Point, from, until time.Time, archiveTimeout, interval time.Duration) []archivedBound {
	return archivedBoundsOn(g, instance, lsnRing, nil, from, until, archiveTimeout, interval)
}

// archivedBoundsOn is archivedBounds with each sweep's timeline: when
// timelines is not nil, a sweep without one is no T or T', T' is on T's
// timeline, and each bound carries it.
func archivedBoundsOn(g *gathered, instance string, lsnRing []viewmodel.Point, timelines map[int64]uint64, from, until time.Time, archiveTimeout, interval time.Duration) []archivedBound {
	size, ok := clusterSegmentSize(g, instance)
	if !ok || archiveTimeout <= 0 || len(lsnRing) == 0 {
		return nil
	}
	inTerm := func(at time.Time) bool { return !at.Before(from) && (until.IsZero() || at.Before(until)) }
	type sample struct {
		at       time.Time
		lsn      uint64
		timeline uint64
	}
	// stamped is the sweep's timeline, and whether it counts: every sweep
	// does when no timelines are given.
	stamped := func(at time.Time) (uint64, bool) {
		if timelines == nil {
			return 0, true
		}
		tl := timelines[at.UnixNano()]
		return tl, tl != 0
	}
	var positions []sample // oldest first
	// Keyed by the instant, not time.Time (whose == compares the
	// monotonic reading and the location too).
	byAt := map[int64]uint64{}
	for _, p := range lsnRing {
		if p.Value == nil || *p.Value < 0 || !inTerm(p.At) {
			continue
		}
		tl, ok := stamped(p.At)
		if !ok {
			continue
		}
		v := uint64(*p.Value)
		positions = append(positions, sample{at: p.At, lsn: v, timeline: tl})
		byAt[p.At.UnixNano()] = v
	}
	var out []archivedBound
	for _, p := range g.readyRings[instance] {
		if p.Value == nil || *p.Value < 1 || !inTerm(p.At) {
			continue
		}
		lsn, read := byAt[p.At.UnixNano()]
		if !read {
			continue
		}
		tl, _ := stamped(p.At)
		k := uint64(*p.Value)
		segment := lsn / size
		if segment+1 <= k {
			continue
		}
		// The start of segment S-k+1: S-k is finished once the position
		// reaches it.
		start := (segment - k + 1) * size
		for i := len(positions) - 1; i >= 0; i-- {
			if positions[i].at.After(p.At) || positions[i].timeline != tl {
				continue
			}
			if positions[i].lsn < start {
				out = append(out, archivedBound{readAt: readByOf(p, interval), before: positions[i].at.Add(-archiveTimeout).UTC(), timeline: tl})
				break
			}
		}
	}
	return out
}

// clusterSegmentSize is the WAL segment size, read from the instance's
// own reading, else from any other instance read (one Cluster has one
// segment size).
func clusterSegmentSize(g *gathered, instance string) (uint64, bool) {
	if size, ok := walSegmentSize(g.swept.Instances[instance].Reading); ok {
		return size, true
	}
	names := make([]string, 0, len(g.swept.Instances))
	for name := range g.swept.Instances {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if size, ok := walSegmentSize(g.swept.Instances[name].Reading); ok {
			return size, true
		}
	}
	return 0, false
}

// formerArchiverOf reads the former primaries' rings up to the
// promotion: every instance's sweeps from before the current primary's
// term (the current primary's own included, for a term it held earlier:
// A, then B, then A again), each reading stamped with the timeline the
// instance was primary on (timelineAt). A reading without a timeline is
// no evidence: after several failovers only the timeline tells which
// archiver wrote which older timeline's WAL, so a quiet sweep of the
// primary of timeline 2 never bounds the end of timeline 1 (round 9, P2).
func formerArchiverOf(g *gathered, promoted time.Time, archiveTimeout, interval time.Duration) formerArchiver {
	out := formerArchiver{read: map[uint64]bool{}, holders: map[uint64]map[string]bool{}}
	if promoted.IsZero() {
		return out
	}
	names := make([]string, 0, len(g.readyRings))
	for name := range g.readyRings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		timelines := timelineAt(g, name)
		if len(timelines) == 0 {
			continue
		}
		for _, p := range g.readyRings[name] {
			if tl := timelines[p.At.UnixNano()]; p.Value != nil && p.At.Before(promoted) && tl != 0 {
				out.read[tl] = true
				if out.holders[tl] == nil {
					out.holders[tl] = map[string]bool{}
				}
				out.holders[tl][name] = true
			}
		}
		for _, q := range quietSweeps(g.readyRings[name], time.Time{}, promoted, interval) {
			if q.timeline = timelines[q.at.UnixNano()]; q.timeline != 0 {
				out.quiet = append(out.quiet, q)
			}
		}
		out.bounds = append(out.bounds, archivedBoundsOn(g, name, g.lsnRings[name], timelines, time.Time{}, promoted, archiveTimeout, interval)...)
	}
	sort.Slice(out.quiet, func(i, j int) bool { return out.quiet[i].at.Before(out.quiet[j].at) })
	return out
}

// liveTimelineOf is the timeline the current primary writes: the one
// its current reading's WAL file is named on, when it reads itself
// primary, else the newest timeline its sweeps stamped since the
// promotion (sweep SeriesTimeline). 0 when neither is known: a
// designated primary (which reports no primary reading), a primary not
// read since it was promoted, or a promotion time that does not parse
// for the rings.
func liveTimelineOf(g *gathered, promoted time.Time) uint64 {
	if r := primaryArchiverReading(g); r != nil && r.IsPrimary {
		if tl, ok := walNameTimeline(r.CurrentWAL); ok {
			return tl
		}
	}
	if promoted.IsZero() {
		return 0
	}
	var newest time.Time
	var tl uint64
	for _, p := range g.timelineRings[g.cluster.Status.CurrentPrimary] {
		if p.Value == nil || *p.Value < 1 || *p.Value > math.MaxUint32 || p.At.Before(promoted) || p.At.Before(newest) {
			continue
		}
		newest, tl = p.At, uint64(*p.Value)
	}
	return tl
}

// walNameTimeline is the timeline a WAL file name is on (its first eight
// hex digits); false when the name is not one.
func walNameTimeline(name string) (uint64, bool) {
	if len(name) != 24 {
		return 0, false
	}
	tl, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil || tl == 0 {
		return 0, false
	}
	if _, err := strconv.ParseUint(name[8:], 16, 64); err != nil {
		return 0, false
	}
	return tl, true
}

// rejoinedSince are the instances back in the Cluster since the
// promotion: the current primary (A, then B, then A again), and every
// instance whose pod is Ready, not being deleted, and that the sweep read
// as a standby after the promotion. A former primary rejoins as a standby
// only after CloudNativePG archived the WAL waiting on it (it archives
// the .ready files before pg_rewind and blocks the start if that fails).
// Nil when the promotion time does not parse.
func rejoinedSince(g *gathered, promoted time.Time) map[string]bool {
	if promoted.IsZero() {
		return nil
	}
	out := map[string]bool{}
	if p := g.cluster.Status.CurrentPrimary; p != "" {
		out[p] = true
	}
	for _, pod := range g.pods {
		if pod == nil || pod.DeletionTimestamp != nil || !podReady(pod) {
			continue
		}
		inst, ok := g.swept.Instances[pod.Name]
		if !ok || inst.State != viewmodel.StatusReadOK || inst.Reading == nil || inst.Reading.IsPrimary || !inst.Reading.ObservedAt.After(promoted) {
			continue
		}
		if inst.Reading.PodUID != "" && inst.Reading.PodUID != string(pod.UID) {
			continue
		}
		out[pod.Name] = true
	}
	return out
}

// timelineAt is the instance's timeline at each sweep that read it as
// primary with one (sweep SeriesTimeline), keyed by the sweep's instant.
func timelineAt(g *gathered, instance string) map[int64]uint64 {
	out := map[int64]uint64{}
	for _, p := range g.timelineRings[instance] {
		if p.Value != nil && *p.Value >= 1 && *p.Value <= math.MaxUint32 {
			out[p.At.UnixNano()] = uint64(*p.Value)
		}
	}
	return out
}

// boundBy is the latest bound from a read that returned at or before by
// (the scan, else now): the most recent time before which every commit
// was in the repository when the agent read it. Nil when there is none.
func (f recoveryFacts) boundBy(by *time.Time) *archivedBound {
	return newestBound(f.bounds, f.now, by)
}

func newestBound(bounds []archivedBound, now time.Time, by *time.Time) *archivedBound {
	limit := now
	if by != nil {
		limit = *by
	}
	var best *archivedBound
	for i := range bounds {
		b := bounds[i]
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

// quietBy is the newest quiet sweep whose read returned at or before by
// (the scan, else now): the archiver had nothing waiting then, so every
// segment completed by its start was in the repository when the agent
// read it. Nil when none is.
func (f recoveryFacts) quietBy(by *time.Time) *quietSweep {
	return newestQuiet(f.quiet, f.now, by)
}

func newestQuiet(quiet []quietSweep, now time.Time, by *time.Time) *quietSweep {
	bound := now
	if by != nil {
		bound = *by
	}
	var best *quietSweep
	for i := range quiet {
		if !quiet[i].by.After(bound) && (best == nil || quiet[i].at.After(best.at)) {
			q := quiet[i]
			best = &q
		}
	}
	return best
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
	var from time.Time
	if instance == g.cluster.Status.CurrentPrimary {
		from = promotedAt(g)
	}
	samples := earlierReady(g, instance, from, r.ObservedAt)
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
		scanned := scannedAt(ev.result)
		if rec.Stale && rec.ScannedAt != nil {
			// The paths kept from an earlier publication describe the
			// repository as that scan read it: their edges (the purge band
			// above all) are timed from it, not from the fresh snapshot's.
			scanned = rec.ScannedAt
		}
		in := timelineInput{
			rec: rec, backups: ev.result.Backups, facts: facts, stale: stale,
			scannedAt: scanned, observedAt: ev.result.ObservedAt, refresh: ev.result.RefreshInterval,
			scanDuration: scanDuration(ev.result.Snapshot),
			pgBackRest:   ev.result.Snapshot.Identity.Repository.Format == evidencev1alpha1.FormatPGBackRest,
			backupIDs:    ev.result.BackupIDs,
			backupsDone:  ev.result.BackupsComplete,
		}
		evidenceTimeline(&out, in)
		out.OffTarget = offTargetSpans(rec.Paths, ev.result.Backups, in.notRestorable)
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
		if p, ok := keptDroppedReason(ev.result.Recovery.Dropped); ok {
			addReason(&out, p)
		}
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

// scanDuration is how long the snapshot's scan took; 0 when either time
// is missing or they are out of order.
func scanDuration(s *evidencev1alpha1.RepositoryEvidenceSnapshot) time.Duration {
	if s == nil || s.StartedAt == nil || s.CompletedAt == nil || !s.CompletedAt.After(*s.StartedAt) {
		return 0
	}
	return s.CompletedAt.Sub(*s.StartedAt)
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

// keptDroppedReason says why the last good recovery paths were not kept
// beside a failed read (evidence.Recovery Dropped); false when none were
// dropped.
func keptDroppedReason(dropped string) (viewmodel.Phrase, bool) {
	switch dropped {
	case evidence.KeptDroppedTooOld:
		return say("recovery.reason.keptTooOld",
			"the last good recovery paths are more than three refresh intervals (at least an hour) older than the latest scan and are no longer shown; check the evidence agent's logs for why reading its recovery paths fails, and that the object store answers", nil), true
	case evidence.KeptDroppedRepositoryChanged:
		return say("recovery.reason.keptRepositoryChanged",
			"the repository's identity changed since the last good recovery paths were read: they describe another repository and are no longer shown; check the evidence agent's logs and that its object store configuration still names this cluster's repository", nil), true
	}
	return viewmodel.Phrase{}, false
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
	// though it reaches its frontier (degradedPath), or when its backup
	// is not usable (unusable, unusableBackup: the path then carries the
	// backup's reason); nil otherwise.
	degraded *evidence.RecoveryPath
	unusable bool
	// timeline is the timeline of the path's frontier (0 when unknown);
	// for a merged run, that of the path that reaches furthest.
	timeline uint64
	// formerMayHold: the path's reason says a former primary may still
	// hold the missing segment (evidencev1alpha1.FormerPrimaryMayHold);
	// for a merged run, that of the path that reaches furthest.
	formerMayHold bool
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

// backupNotUsable says why an unusable backup's span is not covered:
// lead, the agent's reason in parentheses when it gave one, then tail.
// reason is the first unusable code (chain-broken, backup-error, ...).
func backupNotUsable(code string, p evidence.RecoveryPath, lead, tail string) viewmodel.Phrase {
	text := lead
	if p.Reason.Message != "" {
		text += " (" + p.Reason.Message + ")"
	}
	return say(code, text+tail, map[string]string{"backupId": p.BackupID, "reason": p.Reason.Code, "detail": p.Reason.Message})
}

// timelineInput is the evidence a timeline is drawn from.
type timelineInput struct {
	rec     evidence.Recovery
	backups []evidence.BackupItem
	facts   recoveryFacts
	// stale says why the evidence is stale; nil when fresh.
	stale     *viewmodel.Phrase
	scannedAt *time.Time
	// observedAt is when the console fetched the snapshot (its own clock):
	// the purge band is timed from the earlier of it and scannedAt.
	observedAt *time.Time
	refresh    time.Duration
	// scanDuration is how long the snapshot's scan took (started_at to
	// completed_at); 0 when the snapshot does not say. The purge band times
	// the read from the scan's start, and missedScan allows a slow scan
	// its own duration (round 14, PG P3-B). For paths kept from an earlier
	// scan it is the latest scan's: an estimate of that scan's.
	scanDuration time.Duration
	// pgBackRest: the repository is pgBackRest's (its identity's format),
	// whose usability the agent rates per backup item (a path's backup
	// missing from the items read is not known to be usable).
	pgBackRest bool
	// backupIDs are the repository's backups, read with the snapshot;
	// backupsDone that the collection was read complete, so a backup not in
	// it is not in the repository (gone).
	backupIDs   map[string]bool
	backupsDone bool
}

// gone reports a path's backup that the repository's backup collection,
// read complete, does not list: retention expired it since the paths were
// read (paths kept stale beside a fresh collection), or the agent
// published paths of a backup it no longer lists. A restore from it is
// not possible, whichever the format (round 10, PG P2).
func (in timelineInput) gone(id string) bool {
	return in.backupsDone && !in.backupIDs[id]
}

// unconfirmed reports a path's backup that a cut backup collection does
// not list beside recovery paths kept stale from an earlier scan: the
// collection cannot say it is gone, nor that it is still there, and the
// paths were read before a retention purge may have removed it, so a
// restore from it is not confirmed (round 11, PG P3). Fresh paths come
// from the same scan as the backups, which held them then.
func (in timelineInput) unconfirmed(id string) bool {
	return in.rec.Stale && !in.backupsDone && !in.backupIDs[id]
}

// notRestorable reports a path's backup that is gone or unconfirmed.
func (in timelineInput) notRestorable(id string) bool {
	return in.gone(id) || in.unconfirmed(id)
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
func offTargetSpans(paths []evidence.RecoveryPath, backups []evidence.BackupItem, gone func(id string) bool) []viewmodel.OffTargetSpan {
	ends := make(map[string]evidence.BackupItem, len(backups))
	for _, b := range backups {
		ends[b.ID] = b
	}
	var out []viewmodel.OffTargetSpan
	for _, p := range paths {
		if !offTarget(p) || p.FrontierTimeline == nil || p.LowerBoundAt == nil || p.FrontierReceiptAt == nil ||
			!p.FrontierReceiptAt.After(*p.LowerBoundAt) || !reachedConsistency(p, ends) || gone(p.BackupID) {
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
	holes, candidates := drawRuns(out, spans, runs, margin, in.facts.formerHolder)
	first, last := runs[0].from, runs[len(runs)-1].to
	out.Earliest, out.Latest = &first, &last
	tail := runs[len(runs)-1].stop
	if tail == evidencev1alpha1.CoverageFrontier {
		walEnd(out, in.facts, last, margin)
	}
	failedRecently := markEdges(out, in, spans, first, last, margin)
	// The repository's newest timeline can lag the live one: right after a
	// failover, until the new timeline's history file is archived and the
	// agent reads the repository again, every path targets the parent
	// timeline, whose WAL the former primary wrote (round 10, P2).
	live := in.facts.liveTimeline
	top := max(target, live)
	for _, r := range runs {
		switch {
		case r.timeline != 0 && r.timeline < top:
			// The WAL up to this end (a failover's parent tail, a hole
			// before the switch) was written by a former primary.
			formerArchiverEdge(out, in.facts, r.timeline, r.to, in.scannedAt)
		case r.timeline != 0 && live == 0 && !in.facts.promoted.IsZero() && r.to.Before(in.facts.promoted):
			// The live timeline is not known, but this WAL reached the
			// repository before the current primary was promoted: a former
			// primary archived it.
			formerArchiverEdge(out, in.facts, r.timeline, r.to, in.scannedAt)
		}
	}
	var ahead uint64
	if live != 0 && target > live {
		// The repository's newest timeline is ahead of the live one: a
		// default restore follows a timeline the current primary never
		// wrote (round 12, PG P3-2).
		ahead = target
		repositoryAheadEdge(out, in, target, live, first, last)
	}
	newest := runs[len(runs)-1]
	holder, rejoined := "", false
	if newest.formerMayHold {
		holder, rejoined = in.facts.formerHolder(newest.timeline)
	}
	var behind uint64
	if newest.timeline != 0 && newest.timeline < live {
		behind = live
	}
	tailSegment(out, in, last, tailFacts{stop: tail, formerMayHold: newest.formerMayHold, holder: holder, rejoined: rejoined, liveAhead: behind, targetLive: target >= live})
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
	degradedUnusable := false
	newestDegraded, newestHealthy := false, false
	for _, sp := range spans {
		if sp.degraded == nil {
			newestHealthy = newestHealthy || sp.to.Equal(last)
			continue
		}
		if degraded == nil || sp.to.After(*degraded.FrontierReceiptAt) {
			degraded, degradedUnusable = sp.degraded, sp.unusable
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
		tail: tail, formerMayHold: newest.formerMayHold, holder: holder, holderRejoined: rejoined, holes: holes, candidates: candidates, failedRecently: failedRecently, backlog: in.facts.backlog, liveAhead: behind, repositoryAhead: ahead, live: live,
		archiverUnread: in.facts.archiverUnread, hibernated: in.facts.hibernated, notReported: in.facts.notReported, degraded: degraded, degradedUnusable: degradedUnusable,
		primaryUnread: in.facts.primaryUnread, failing: in.facts.failing,
		orphan: orphan, undrawn: undrawn,
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
		if p.FrontierTimeline != nil {
			sp.timeline = *p.FrontierTimeline
		}
		sp.formerMayHold = p.Stop == evidencev1alpha1.CoverageGapLimited && strings.Contains(p.Reason.Message, evidencev1alpha1.FormerPrimaryMayHold)
		item, listed := ends[p.BackupID]
		if in.gone(p.BackupID) {
			// The repository no longer lists the backup: whatever its
			// path says, a restore from it is not possible now.
			path := p
			path.Reason = evidencev1alpha1.Reason{Code: backupGoneCode}
			sp.degraded, sp.unusable = &path, true
		} else if in.unconfirmed(p.BackupID) {
			// Kept stale beside a cut collection that does not list it: a
			// purge since the paths were read may have removed it.
			path := p
			path.Reason = evidencev1alpha1.Reason{Code: backupUnconfirmedCode}
			sp.degraded, sp.unusable = &path, true
		} else if in.pgBackRest && !listed {
			// The console keeps the newest MaximumBackupItems items but
			// more paths: a path whose backup was cut from the items read
			// has no usability rating, so it is drawn, never as covered
			// (round 9, PG P2; the roll-up already reads it so, cap.go
			// rollupUnexplained).
			path := p
			path.Reason = evidencev1alpha1.Reason{Code: backupNotListedCode}
			sp.degraded, sp.unusable = &path, true
		} else if code, detail, unusable := unusableBackup(p, item); unusable {
			path := p
			path.Reason = evidencev1alpha1.Reason{Code: code, Message: detail}
			sp.degraded, sp.unusable = &path, true
		} else if degradedPath(p) {
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

// backupNotListedCode is the console's reason for a pgBackRest path whose
// backup is not among the backup items read (cut at the console's bound):
// its usability is not known.
const backupNotListedCode = "backup-not-listed"

// backupItemsLimit is the number of the newest backup items the console
// reads (evidence.MaximumBackupItems), as the sentences say it.
var backupItemsLimit = strconv.Itoa(evidence.MaximumBackupItems)

// backupGoneCode is the console's reason for a path whose backup the
// repository's complete backup collection does not list (timelineInput
// gone).
const backupGoneCode = "backup-not-in-repository"

// backupUnconfirmedCode is the console's reason for a path kept stale
// whose backup a cut backup collection does not list (timelineInput
// unconfirmed).
const backupUnconfirmedCode = "backup-not-confirmed"

// pathShownUnusable are the pgBackRest unusable reasons that come from
// the backup's recovery path itself (pgbackrest assess.go): the path the
// console draws carries them already (its stop, its state, off target
// or not consistent), so they do not make a drawn span degraded again.
var pathShownUnusable = map[string]bool{
	evidencev1alpha1.UnusableWALToConsistency:    true,
	evidencev1alpha1.UnusableWALUnknown:          true,
	evidencev1alpha1.UnusableWALToTargetTimeline: true,
	evidencev1alpha1.UnusableOffTargetTimeline:   true,
}

// unusableBackup reports a pgBackRest backup the agent rates not usable
// for a reason its recovery path does not show: a broken chain, a
// manifest (its own or a dependency's) not verified, pgBackRest's own
// error flag (page checksums), a manifest not inspected. Its path is
// judged from the WAL alone, so its span is drawn, but never as covered
// (round 8, P2-1). code is that first reason, detail the agent's
// message. A backup with no reason given is unusable too; one whose only
// reasons come from the path is unusable only when the path itself reads
// healthy at its frontier, which then does not show them. Barman Cloud
// backups (no PgBackRest item) are never judged here.
func unusableBackup(p evidence.RecoveryPath, b evidence.BackupItem) (code, detail string, unusable bool) {
	item := b.PgBackRest
	if item == nil || item.Usable {
		return "", "", false
	}
	detail = b.Reason.Message
	for _, r := range item.UnusableReasons {
		if !pathShownUnusable[r] {
			return r, detail, true
		}
	}
	if len(item.UnusableReasons) == 0 {
		return "backup-not-usable", detail, true
	}
	if p.Stop == evidencev1alpha1.CoverageFrontier && p.State == evidencev1alpha1.Healthy {
		return item.UnusableReasons[0], detail, true
	}
	return "", "", false
}

// formerMayHoldPhrase is a missing-WAL sentence (missing, ending with a
// space) followed by the advice that the former primary may still hold
// the WAL: it names that instance when the console knows it (instance,
// the one read as primary on the timeline the WAL stops on), else says
// "that instance". When that instance has rejoined the Cluster since
// (rejoined: it is the current primary, or a Ready standby), it holds no
// unarchived WAL any more: CloudNativePG archives the WAL waiting on it
// before it starts again, so the sentence says so instead of advising to
// keep it (round 10, P3); the code then ends in "Rejoined".
func formerMayHoldPhrase(code, missing, instance string, rejoined bool) viewmodel.Phrase {
	switch {
	case instance == "":
		return say(code, missing+"The former primary may still hold it, and CloudNativePG archives it before that instance rejoins: do not delete that instance or its volume.", nil)
	case rejoined:
		return say(code+"Rejoined", missing+"The former primary, "+instance+", has rejoined the cluster since, and CloudNativePG archives the WAL waiting on an instance before it starts again: that WAL has been archived since the repository was read, or it was never on "+instance+".",
			map[string]string{"instance": instance})
	}
	return say(code, missing+"The former primary, "+instance+", may still hold it, and CloudNativePG archives it before that instance rejoins: do not delete "+instance+" or its volume.",
		map[string]string{"instance": instance})
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
				last.to, last.stop, last.timeline, last.formerMayHold = sp.to, sp.stop, sp.timeline, sp.formerMayHold
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
func drawRuns(out *viewmodel.RecoveryTimeline, spans, runs []interval, margin time.Duration, holder func(timeline uint64) (string, bool)) (holes, candidates int) {
	for i, r := range runs {
		for _, piece := range coveredPieces(spans, r.from, r.to) {
			if p := piece.degraded; p != nil && piece.unusable && p.Reason.Code == backupGoneCode {
				out.Segments = append(out.Segments, segment(piece.from, piece.to, segmentUnknown, say("recovery.segment.backupGone",
					"Base backup "+p.BackupID+" reaches here in the recovery paths read, but the repository's backup list, read complete, no longer has it (a retention purge may have removed it since): not counted as covered.",
					map[string]string{"backupId": p.BackupID, "reason": backupGoneCode})))
				continue
			}
			if p := piece.degraded; p != nil && piece.unusable && p.Reason.Code == backupUnconfirmedCode {
				out.Segments = append(out.Segments, segment(piece.from, piece.to, segmentUnknown, say("recovery.segment.backupUnconfirmed",
					"Base backup "+p.BackupID+" reaches here in the recovery paths kept from an earlier scan, but the repository's backup list was cut at a bound and does not list it, so it may have been purged since: not counted as covered.",
					map[string]string{"backupId": p.BackupID, "reason": backupUnconfirmedCode})))
				continue
			}
			if p := piece.degraded; p != nil && piece.unusable && p.Reason.Code == backupNotListedCode {
				out.Segments = append(out.Segments, segment(piece.from, piece.to, segmentUnknown, say("recovery.segment.backupNotListed",
					"Base backup "+p.BackupID+" reaches here, but it is not among the newest "+backupItemsLimit+" backups the console reads from the evidence agent, so its usability is not known: not counted as covered. Expire old backups or lower the retention so the repository holds fewer than "+backupItemsLimit+" backups, or restore from a newer backup.",
					map[string]string{"backupId": p.BackupID, "reason": backupNotListedCode, "limit": backupItemsLimit})))
				continue
			}
			if p := piece.degraded; p != nil && piece.unusable {
				out.Segments = append(out.Segments, segment(piece.from, piece.to, segmentUnknown, backupNotUsable("recovery.segment.backupNotUsable", *p,
					"Base backup "+p.BackupID+" reaches here, but the evidence agent rates the backup not usable", ": not counted as covered.")))
				continue
			}
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
		if kind == segmentMissingWAL && r.formerMayHold {
			name, rejoined := holder(r.timeline)
			text = formerMayHoldPhrase("recovery.segment.missingWALFormerPrimary", "WAL is missing from the repository: no restore target in this span. ", name, rejoined)
		}
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
func markEdges(out *viewmodel.RecoveryTimeline, in timelineInput, spans []interval, first, last time.Time, margin time.Duration) (failedRecently bool) {
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
	purge := purgeBand(in, spans, first, last)
	uncertain(out, first, first.Add(purge), say("recovery.segment.purgeEdge",
		"Within "+humanDuration(purge)+" of the oldest restore point: a retention purge since the repository was last read may have removed this base backup.",
		map[string]string{"marginSeconds": secondsParam(purge)}))
	return failedRecently
}

// purgeBand is how long after the oldest restore point a retention purge
// may have removed its base backup unseen: the agent's refresh interval
// plus the age of the scan the paths come from, timed from the earliest
// of the scan's start (the listing is read during the scan, not when it
// completes; round 14, PG P3-B), the agent's completion time and the
// console's fetch time (an agent clock ahead of the console's does not
// shrink it), and never past the window [first, last] (an absurd scan
// time, which saturates time.Sub, does not overflow it to nothing; round
// 11, PG P3).
//
// A purge drops a whole backup, with the WAL only it needs, not a span as
// long as the time elapsed. Under a recovery window the first restore
// point the repository keeps is the newest retention anchor (purgeAnchors)
// at or before the window's start, which moves with the clock: when the
// paths were read the oldest anchor was still needed, so the window
// started before the next anchor (first plus oldestOnly); since then it
// has moved by at most the age of the read plus one refresh interval.
// Every anchor whose successor it passed may be gone, several during a
// long age (round 13, PG P3-B). The window's start is now before reach =
// first + oldestOnly + age + refresh, so the newest anchor before reach,
// base, is still there with every newer backup. The band runs to the
// first drawn restore point at or after base: before it, the drawn spans
// come from backups that may be gone (an anchor with no recovery path,
// or a pgBackRest incremental of an expired full, keeps no span covered).
//
// Retention never removes the newest backup, so within a normal cycle
// the band never passes the first drawn restore point at or after the
// newest anchor the snapshot lists: a lone backup scanned on time has no
// band (round 14, PG P3-A). Once a scan was missed it may: a backup
// taken since, which the snapshot cannot list, lets retention remove the
// newest one listed, so the elapsed band is not capped there and reaches
// the whole window as the age grows.
//
// The band extends only once a scan was missed (missedScan). Within a
// normal cycle the scan's age passes one refresh interval for up to a
// console poll period before the next scan lands; extending there flipped
// the oldest backup's span between covered and unknown every cycle (round
// 13, PG P3-C). A purge within that cycle is the evidence's ordinary lag,
// which the elapsed band (one refresh plus the age) covers in time.
func purgeBand(in timelineInput, spans []interval, first, last time.Time) time.Duration {
	purge := in.refresh
	if purge <= 0 {
		purge = evidence.DefaultAgentRefreshInterval
	}
	var read *time.Time
	for _, at := range []*time.Time{in.scannedAt, in.observedAt} {
		if at != nil && (read == nil || at.Before(*read)) {
			read = at
		}
	}
	if read != nil && in.scanDuration > 0 {
		start := read.Add(-in.scanDuration)
		read = &start
	}
	window := max(last.Sub(first), 0)
	var age time.Duration
	if read != nil && in.facts.now.After(*read) {
		age = min(in.facts.now.Sub(*read), window)
	}
	band := window
	if purge <= window-age {
		band = purge + age
	}
	anchors := purgeAnchors(in, spans)
	if age <= missedScan(purge, in.scanDuration) {
		if newest, ok := newestAnchor(anchors, nil); ok {
			if kept, ok := drawnFrom(spans, newest); ok {
				band = min(band, max(kept.Sub(first), 0))
			}
		}
		return band
	}
	only := oldestOnly(anchors, first)
	if only <= 0 || band >= window {
		return band
	}
	// Two Adds: each duration is at most the window or a saturated Sub,
	// which a time adds without overflow.
	reach := first.Add(only).Add(band)
	base, ok := newestAnchor(anchors, &reach)
	if !ok || base.Before(first) {
		return band
	}
	kept, ok := drawnFrom(spans, base)
	if !ok {
		return window
	}
	return min(max(band, kept.Sub(first)), window)
}

// drawnFrom is the earliest drawn span's lower bound at or after at: the
// first restore point a backup that is kept with the anchor at at (or a
// newer one) draws. False when no span starts there.
func drawnFrom(spans []interval, at time.Time) (time.Time, bool) {
	var out time.Time
	found := false
	for _, sp := range spans {
		if !sp.from.Before(at) && (!found || sp.from.Before(out)) {
			out, found = sp.from, true
		}
	}
	return out, found
}

// purgeAnchors are the lower bounds of the backups a retention policy
// keeps as a recovery base, each one making every older restore point
// purgeable once the policy's window passes it. pgBackRest expires a full
// backup together with every differential and incremental backup taken
// against it, so only full backups are anchors: the oldest full's whole
// chain goes at once, up to the next full (round 14, PG P3-A). A Barman
// backup is removed on its own: every backup the repository lists that
// did not fail is an anchor, drawn or not (a backup with no recovery path
// is still a retention anchor), with the drawn spans' lower bounds. A
// pgBackRest repository whose items are not read falls back to the drawn
// spans, as if each backup were a full: the chains are not known.
func purgeAnchors(in timelineInput, spans []interval) []time.Time {
	// A drawn backup's anchor is its span's lower bound, the time the
	// window is drawn from, not its item's end time, which may differ.
	drawn := make(map[string]bool, len(spans))
	for _, sp := range spans {
		drawn[sp.backupID] = true
	}
	if !in.pgBackRest || len(in.backups) == 0 {
		var out []time.Time
		for _, b := range in.backups {
			if !in.pgBackRest && !b.Failed && b.EndAt != nil && !drawn[b.ID] {
				out = append(out, b.EndAt.UTC())
			}
		}
		for _, sp := range spans {
			out = append(out, sp.from)
		}
		return out
	}
	full := map[string]bool{}
	var out []time.Time
	for _, b := range in.backups {
		if b.Failed || b.PgBackRest == nil || b.PgBackRest.Type != "full" {
			continue
		}
		full[b.ID] = true
		if b.EndAt != nil && !drawn[b.ID] {
			out = append(out, b.EndAt.UTC())
		}
	}
	for _, sp := range spans {
		if full[sp.backupID] {
			out = append(out, sp.from)
		}
	}
	return out
}

// newestAnchor is the newest of the anchors, before before when it is
// set; false when there is none.
func newestAnchor(anchors []time.Time, before *time.Time) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, a := range anchors {
		if (before == nil || a.Before(*before)) && (!found || a.After(newest)) {
			newest, found = a, true
		}
	}
	return newest, found
}

// missedScan is the age, from the scan's start, past which the next scan
// of an agent refreshing every refresh is overdue. A scan that takes
// longer than the interval delays the next one (the agent's ticker drops
// the ticks it missed), so a normal cycle lasts the longer of the
// interval and the scan, then the next scan's own duration, then up to
// the console's poll period before the console reads it, plus scanSlack
// (round 14, PG P3-B: two intervals held the short band for a whole day
// at a 24 h refresh and flapped with a scan slower than a 30 s refresh).
// Without a scan duration (no start time in the snapshot) it is two
// intervals, and at least one interval plus the poll period and
// scanSlack.
func missedScan(refresh, scan time.Duration) time.Duration {
	if scan <= 0 {
		return max(2*refresh, refresh+evidence.DefaultInterval+scanSlack)
	}
	return max(refresh, scan) + scan + evidence.DefaultInterval + scanSlack
}

// scanSlack is the time a scan is allowed to take on top of its interval
// before missedScan counts it as missed.
const scanSlack = time.Minute

// oldestOnly is how long after first only the oldest anchor's backups
// reach: until the earliest anchor after first; 0 when there is none.
func oldestOnly(anchors []time.Time, first time.Time) time.Duration {
	var next *time.Time
	for i := range anchors {
		if anchors[i].After(first) && (next == nil || anchors[i].Before(*next)) {
			next = &anchors[i]
		}
	}
	if next == nil {
		return 0
	}
	return next.Sub(first)
}

// repositoryAheadEdge marks the restore points a default restore may
// reach on another cluster's line when the repository's newest timeline,
// target, is ahead of live, the one the current primary writes: from the
// newest lower bound of an on-target backup on the live line (onLiveLine:
// an on-target path from it ends its backup before the fork), else from
// the first restore point, to the newest. The fork itself is not placed:
// what lies between is unknown, never covered.
func repositoryAheadEdge(out *viewmodel.RecoveryTimeline, in timelineInput, target, live uint64, first, last time.Time) {
	from := first
	for _, p := range in.rec.Paths {
		if p.TargetTimeline != target || offTarget(p) || p.LowerBoundAt == nil || !onLiveLine(in.rec.Paths, p, live) {
			continue
		}
		if b := p.LowerBoundAt.UTC(); b.After(from) {
			from = b
		}
	}
	tl, lv := strconv.FormatUint(target, 10), strconv.FormatUint(live, 10)
	uncertain(out, from, last, say("recovery.segment.repositoryAheadOfLive",
		"The repository's newest timeline, "+tl+", is ahead of timeline "+lv+", the one the current primary writes: another cluster may archive into this repository, and a default restore follows timeline "+tl+", so a time past the fork may restore that cluster's data.",
		map[string]string{"timeline": tl, "liveTimeline": lv}))
}

// onLiveLine reports a path whose backup is on the line the current
// primary writes, live: the backup is on live itself, or the agent
// evaluated a path from the same backup toward live, which it does only
// along valid history from the backup's timeline that does not branch
// before the backup's consistency point (Barman Cloud's descendant
// edges). A start timeline at or below live is not enough: a sibling
// timeline (another cluster's line, numbered below live) would move the
// start of the unknown span past the fork (round 13, CNPG P3-2). When no
// such path proves it (a pgBackRest repository publishes one path per
// backup, toward the newest timeline), the backup is not counted, and the
// span starts earlier: conservative.
func onLiveLine(paths []evidence.RecoveryPath, p evidence.RecoveryPath, live uint64) bool {
	if p.StartTimeline == nil {
		return false
	}
	if *p.StartTimeline == live {
		return true
	}
	if *p.StartTimeline > live {
		return false
	}
	return slices.ContainsFunc(paths, func(q evidence.RecoveryPath) bool {
		return q.BackupID == p.BackupID && q.TargetTimeline == live && q.StartTimeline != nil && *q.StartTimeline == *p.StartTimeline && q.FrontierTimeline != nil
	})
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
	if b := f.boundBy(scanned); b != nil && f.archiveTimeout > 0 && f.sweepInterval > 0 && (quiet == nil || b.before.After(quiet.at.Add(-f.archiveTimeout))) {
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
	uncertain(out, quiet.at.Add(-f.archiveTimeout), last, say("recovery.segment.archiveBacklogSince",
		segments+" WAL segments are waiting to be archived on the primary, which last had none waiting at "+rfc3339(quiet.at)+": WAL archived since then may have reached the repository late, so its arrival time overstates how recent a restore can be.",
		map[string]string{"segments": segments, "quietAt": rfc3339(quiet.at)}))
}

// formerArchiverEdge marks the time before an end of the archived WAL on
// a timeline older than the target (a failover's parent tail, a hole
// before the switch): that WAL was written and archived by a former
// primary, whose backlog at the switch the current primary's readings
// say nothing of. A segment it never archived (its .ready tail lost in a
// crash, the standby having marked the streamed segments .done) holds
// commits older than the end's receipt by however long it waited. Only
// the readings of the primary of that same timeline count (formerArchiver
// on): from the newest sweep, at or before the scan, that read it with no
// .ready file (less one archive_timeout), or the newest bound its WAL
// positions give (archivedBounds), whichever is later; from
// failureLookback before the end when the console kept no reading of
// that timeline's archiver from before the promotion, none bounds it,
// the band they give does not end before the end (no evidence for it),
// the sweep period is not known, or archive_timeout is disabled (round 8,
// P2-2; round 9, P2).
func formerArchiverEdge(out *viewmodel.RecoveryTimeline, f recoveryFacts, timeline uint64, end time.Time, scanned *time.Time) {
	tl := strconv.FormatUint(timeline, 10)
	unknown := func() {
		uncertain(out, end.Add(-failureLookback), end, say("recovery.segment.formerArchiverUnknown",
			"The WAL up to here on timeline "+tl+" was archived by a former primary, and the console kept no reading of that timeline's archiver that bounds this end: WAL that waited on it, or never left it, may hold commits older than this end's arrival time, so a restore this recent may stop short.",
			map[string]string{"timeline": tl}))
	}
	read, quietOn, boundsOn := f.former.on(timeline)
	if !read || f.archiveTimeout <= 0 || f.sweepInterval <= 0 {
		unknown()
		return
	}
	quiet := newestQuiet(quietOn, f.now, scanned)
	b := newestBound(boundsOn, f.now, scanned)
	switch {
	case b != nil && (quiet == nil || b.before.After(quiet.at.Add(-f.archiveTimeout))):
		if !end.After(b.before) {
			unknown()
			return
		}
		uncertain(out, b.before, end, say("recovery.segment.formerArchiverBound",
			"The WAL up to here on timeline "+tl+" was archived by a former primary; by its reading at "+rfc3339(b.readAt)+" every commit before "+rfc3339(b.before)+" was archived: WAL archived after that may have waited on it, or never left it, so a restore this recent may stop short.",
			map[string]string{"timeline": tl, "readAt": rfc3339(b.readAt), "boundAt": rfc3339(b.before)}))
	case quiet != nil:
		if !end.After(quiet.at.Add(-f.archiveTimeout)) {
			unknown()
			return
		}
		uncertain(out, quiet.at.Add(-f.archiveTimeout), end, say("recovery.segment.formerArchiverSince",
			"The WAL up to here on timeline "+tl+" was archived by a former primary, which last had nothing waiting at "+rfc3339(quiet.at)+": WAL archived after that may have waited on it, or never left it, so a restore this recent may stop short.",
			map[string]string{"timeline": tl, "quietAt": rfc3339(quiet.at)}))
	default:
		uncertain(out, end.Add(-failureLookback), end, say("recovery.segment.formerArchiverBacklog",
			"The WAL up to here on timeline "+tl+" was archived by a former primary, which had WAL waiting in every reading the console kept before the switch: WAL that waited on it, or never left it, may hold commits older than this end's arrival time, so a restore this recent may stop short.",
			map[string]string{"timeline": tl}))
	}
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
			"The archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report how many WAL segments wait to be archived: WAL that reached the repository may have waited on it first, so its arrival time may overstate how recent a restore can be.", nil))
		return
	}
	quiet := f.quietBy(scanned)
	if b := f.boundBy(scanned); b != nil && f.archiveTimeout > 0 && f.sweepInterval > 0 && (quiet == nil || b.before.After(quiet.at.Add(-f.archiveTimeout))) {
		uncertain(out, b.before, last, say("recovery.segment.archiverUnreadBound",
			"The primary's archiver is not read now; by its reading at "+rfc3339(b.readAt)+" every commit before "+rfc3339(b.before)+" was archived: WAL archived after that may have waited on the primary first, so its arrival time may overstate how recent a restore can be.",
			map[string]string{"readAt": rfc3339(b.readAt), "boundAt": rfc3339(b.before)}))
		return
	}
	if (quiet == nil || f.archiveTimeout <= 0 || f.sweepInterval <= 0) && f.hibernated {
		uncertain(out, last.Add(-failureLookback), last, say("recovery.segment.archiverHibernated",
			"The Cluster is hibernated, and no sweep the console keeps saw its primary with nothing waiting before it slept: WAL that reached the repository may have waited on it first, so its arrival time may overstate how recent a restore can be.", nil))
		return
	}
	if quiet == nil || f.archiveTimeout <= 0 || f.sweepInterval <= 0 {
		uncertain(out, last.Add(-failureLookback), last, say("recovery.segment.archiverUnread",
			"The primary's archiver is not read, and no sweep the console keeps saw it with nothing waiting: WAL that reached the repository may have waited on the primary first, so its arrival time may overstate how recent a restore can be.", nil))
		return
	}
	uncertain(out, quiet.at.Add(-f.archiveTimeout), last, say("recovery.segment.archiverUnreadSince",
		"The primary's archiver is not read now; it last had nothing waiting at "+rfc3339(quiet.at)+": WAL archived after that may have waited on the primary first, so its arrival time may overstate how recent a restore can be.",
		map[string]string{"quietAt": rfc3339(quiet.at)}))
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

// tailFacts are what the drawing found about the newest run's end.
type tailFacts struct {
	stop          evidencev1alpha1.CoverageStop
	formerMayHold bool
	// holder is the former primary that may hold the missing WAL (empty
	// when not known); rejoined that it has rejoined the Cluster since.
	holder   string
	rejoined bool
	// liveAhead is the live timeline when the newest run ends on an older
	// one (the repository lags the failover); 0 otherwise.
	liveAhead uint64
	// targetLive: the paths already target the live timeline (its history
	// file is in the repository) though their frontier is still on the
	// parent: no segment of the live timeline was archived yet.
	targetLive bool
}

// tailSegment draws the time after the newest restore point: only a
// confirmed gap says the time is not restorable; anything else leaves it
// unknown. A fresh, complete frontier is a fact about the scan, not about
// now: the WAL of a time just after it may have been in a segment not
// archived yet when the agent read the repository (archive_timeout), and
// reach it after the scan, so the time after the frontier, before the
// scan included, is unknown.
func tailSegment(out *viewmodel.RecoveryTimeline, in timelineInput, last time.Time, t tailFacts) {
	if !in.facts.now.After(last) {
		return
	}
	var p viewmodel.Phrase
	kind := segmentUnknown
	switch tail := t.stop; {
	case tail == evidencev1alpha1.CoverageGapLimited && t.formerMayHold:
		// The agent says the segment is not proven gone (round 8, PG
		// plan 5): the hint reaches the bar, not only the path's reason.
		kind = segmentMissingWAL
		p = formerMayHoldPhrase("recovery.segment.missingAfterLatestFormerPrimary", "WAL is missing from the repository after the latest restore point. ", t.holder, t.rejoined)
	case tail == evidencev1alpha1.CoverageGapLimited:
		kind = segmentMissingWAL
		p = say("recovery.segment.missingAfterLatest", "WAL is missing from the repository after the latest restore point.", nil)
	case tail != evidencev1alpha1.CoverageFrontier:
		_, p = holeOf(tail)
	case !in.rec.Complete:
		p = say("recovery.segment.collectionCut", "The recovery path collection was cut at a bound: newer coverage may exist.", nil)
	case t.liveAhead != 0 && t.targetLive:
		// The history file of the live timeline is archived (the paths
		// target it), but no segment of it yet (round 11, CNPG P3).
		tl := strconv.FormatUint(t.liveAhead, 10)
		p = say("recovery.segment.liveTimelineNoSegment",
			"The repository held the history file of timeline "+tl+", the one the current primary writes, but no WAL segment of it yet when it was last read: this time's WAL may not be in the repository yet.",
			map[string]string{"timeline": tl})
	case t.liveAhead != 0:
		// The newest run ends on an older timeline than the one the
		// current primary writes (round 10, P2).
		tl := strconv.FormatUint(t.liveAhead, 10)
		p = say("recovery.segment.liveTimelineNotArchived",
			"The repository held no WAL of timeline "+tl+", the one the current primary writes, when it was last read (the failover's history file may not be archived yet, or the evidence agent has not read the repository since): this time's WAL may not be in the repository yet.",
			map[string]string{"timeline": tl})
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
	// formerMayHold: the newest run stops at WAL a former primary may
	// still hold (interval formerMayHold).
	formerMayHold bool
	// holder is the former primary that may hold it, when the console
	// knows it (formerArchiver holder); empty otherwise.
	holder string
	// holderRejoined: holder has rejoined the Cluster since the switch
	// (recoveryFacts rejoined).
	holderRejoined    bool
	tail              evidencev1alpha1.CoverageStop
	holes, candidates int
	failedRecently    bool
	// backlog is the archiver's waiting segments, 0 when keeping up.
	backlog int
	// liveAhead is the live timeline when the newest run ends on an older
	// one (tailFacts liveAhead); 0 otherwise.
	liveAhead uint64
	// repositoryAhead is the target timeline when it is newer than live,
	// the timeline the current primary writes (known); 0 otherwise.
	repositoryAhead, live uint64
	// archiverUnread, hibernated, notReported: the primary's archiver is
	// not read, or reports no readyWalFiles.
	archiverUnread, hibernated, notReported bool
	// primaryUnread: the current primary was not read (recoveryFacts).
	primaryUnread bool
	// failing: the archiving fact reads as failing (archivingFailing).
	failing bool
	// degraded is the newest drawn path the agent does not rate healthy
	// though it reaches its frontier; nil when none is.
	degraded *evidence.RecoveryPath
	// degradedUnusable: degraded is there because its backup is not
	// usable (unusableBackup), not because of the path's own state.
	degradedUnusable bool
	orphan           uint64
	undrawn          int
}

// timelineVerdict states the bar's state and reason, the gravest finding
// first: a cut collection, missing WAL at the end, an unconfirmed end,
// holes, candidate gaps, a recent archiving failure, an archive backlog,
// a live timeline the repository holds no WAL of, an orphan timeline,
// stale evidence.
func timelineVerdict(out *viewmodel.RecoveryTimeline, in timelineInput, f timelineFindings) {
	warning := string(evidencev1alpha1.Warning)
	switch {
	case !in.rec.Complete:
		out.State = string(evidencev1alpha1.Unknown)
		setReason(out, say("recovery.reason.pathsCut", "the recovery path collection was cut at a bound: the restore target timeline cannot be told", nil))
	case f.tail == evidencev1alpha1.CoverageGapLimited && f.formerMayHold:
		out.State = string(evidencev1alpha1.Unhealthy)
		switch {
		case f.holder != "" && f.holderRejoined:
			setReason(out, say("recovery.reason.missingAfterLatestFormerPrimaryRejoined",
				"WAL is missing after the latest restore point: recent times are not restorable from the repository now; the former primary, "+f.holder+", has rejoined the cluster since, so the WAL that waited on it has been archived since the repository was read, or it was never on "+f.holder,
				map[string]string{"instance": f.holder}))
		case f.holder != "":
			setReason(out, say("recovery.reason.missingAfterLatestFormerPrimary",
				"WAL is missing after the latest restore point: recent times are not restorable from the repository now; the former primary, "+f.holder+", may still hold it: do not delete "+f.holder+" or its volume",
				map[string]string{"instance": f.holder}))
		default:
			setReason(out, say("recovery.reason.missingAfterLatestFormerPrimary",
				"WAL is missing after the latest restore point: recent times are not restorable from the repository now; the former primary may still hold it: do not delete that instance or its volume", nil))
		}
	case f.tail == evidencev1alpha1.CoverageGapLimited:
		out.State = string(evidencev1alpha1.Unhealthy)
		setReason(out, say("recovery.reason.missingAfterLatest", "WAL is missing after the latest restore point: recent times are not restorable from the repository", nil))
	case f.tail != evidencev1alpha1.CoverageFrontier:
		out.State = warning
		_, p := holeOf(f.tail)
		setReason(out, p)
	case f.repositoryAhead != 0:
		out.State = warning
		tl, lv := strconv.FormatUint(f.repositoryAhead, 10), strconv.FormatUint(f.live, 10)
		setReason(out, say("recovery.reason.repositoryAheadOfLive",
			"the repository's newest timeline, "+tl+", is ahead of timeline "+lv+", the one the current primary writes: another cluster may archive into this repository, and a default restore follows timeline "+tl+", so the restore points past the fork are not counted as covered; check that no other cluster's backup configuration names this repository",
			map[string]string{"timeline": tl, "liveTimeline": lv}))
	case f.holes > 0:
		out.State = warning
		setReason(out, say("recovery.reason.holes", "the window has holes where WAL is missing", nil))
	case f.candidates > 0:
		out.State = warning
		setReason(out, say("recovery.reason.candidateGaps", "the window has candidate gaps a second scan has not confirmed yet", nil))
	case f.degraded != nil && f.degradedUnusable && f.degraded.Reason.Code == backupGoneCode:
		out.State = warning
		setReason(out, say("recovery.reason.backupGone",
			"base backup "+f.degraded.BackupID+" is no longer in the repository's backup list: the span only it reaches is not counted as covered",
			map[string]string{"backupId": f.degraded.BackupID, "reason": backupGoneCode}))
	case f.degraded != nil && f.degradedUnusable && f.degraded.Reason.Code == backupUnconfirmedCode:
		out.State = warning
		setReason(out, say("recovery.reason.backupUnconfirmed",
			"base backup "+f.degraded.BackupID+" is not in the repository's backup list, which was cut at a bound, beside recovery paths kept from an earlier scan: the span only it reaches is not counted as covered until the paths are read again",
			map[string]string{"backupId": f.degraded.BackupID, "reason": backupUnconfirmedCode}))
	case f.degraded != nil && f.degradedUnusable && f.degraded.Reason.Code == backupNotListedCode:
		out.State = warning
		setReason(out, say("recovery.reason.backupNotListed",
			"base backup "+f.degraded.BackupID+" is not among the newest "+backupItemsLimit+" backups read, so its usability is not known: the span only it reaches is not counted as covered; expire old backups or lower the retention so the repository holds fewer than "+backupItemsLimit+" backups",
			map[string]string{"backupId": f.degraded.BackupID, "reason": backupNotListedCode, "limit": backupItemsLimit}))
	case f.degraded != nil && f.degradedUnusable:
		out.State = warning
		setReason(out, backupNotUsable("recovery.reason.backupNotUsable", *f.degraded,
			"base backup "+f.degraded.BackupID+" is not usable", ": the span only it reaches is not counted as covered"))
	case f.degraded != nil:
		out.State = warning
		setReason(out, pathNotHealthy("recovery.reason.pathNotHealthy", *f.degraded,
			"the recovery path from base backup "+f.degraded.BackupID+" reaches the archive frontier, but the evidence agent rates it "+string(f.degraded.State), ""))
	case f.failedRecently:
		out.State = warning
		setReason(out, say("recovery.reason.failedRecently", "WAL archiving failed recently: the newest restore points are uncertain", nil))
	case f.failing:
		// Archiving is failing (ContinuousArchiving False, or the
		// primary's last attempt failed with WAL waiting) though no
		// readable instance reports a recent failure: the primary may not
		// be read (fenced after a failover to investigate). Ranked before
		// the post-failover lag, whose advice to run pg_switch_wal()
		// would not help: the repository lacks the live timeline because
		// archiving fails (round 13, CNPG P3-1).
		out.State = warning
		setReason(out, say("recovery.reason.archivingFailing", "WAL archiving is failing: WAL written since the newest segment the repository holds is likely not in it, so the newest restore points are uncertain; the Cluster's ContinuousArchiving condition says why", nil))
	case f.backlog > 0:
		out.State = warning
		n := strconv.Itoa(f.backlog)
		setReason(out, say("recovery.reason.archiveBacklog", n+" WAL segments are waiting to be archived on the primary: the newest restore points are uncertain",
			map[string]string{"segments": n}))
	case f.hibernated:
		// A hibernated Cluster writes no WAL and runs no archiver: nothing
		// is going wrong that a warning would ask to act on, and nothing
		// confirms that the WAL written before it reached the repository.
		// The newest restore points are unknown until it wakes up. Ranked
		// before a post-failover lag, whose advice to act on the primary
		// cannot be followed with no instance running (round 12, CNPG
		// P3-1).
		out.State = string(evidencev1alpha1.Unknown)
		setReason(out, say("recovery.reason.archiverHibernated", "the Cluster is hibernated: whether the WAL written before it reached the repository is not confirmed, so the newest restore points are uncertain", nil))
	case f.liveAhead != 0 && (f.primaryUnread || f.archiverUnread):
		// The primary was not read (fenced, unreachable): advice to run
		// anything on it may not be followable (round 12, CNPG P3-1).
		// Not read is the sweep's own fact (primaryUnread), not only the
		// archiving reason's (round 13, CNPG P3-1).
		out.State = warning
		tl := strconv.FormatUint(f.liveAhead, 10)
		setReason(out, say("recovery.reason.liveTimelineNotArchivedPrimaryUnread",
			"the repository held no WAL of timeline "+tl+", the one the current primary writes, when it was last read, and the primary was not read: the newest restore points are uncertain until the primary is read again and the agent reads WAL of timeline "+tl,
			map[string]string{"timeline": tl}))
	case f.liveAhead != 0:
		// Right after a failover the repository holds no WAL of the
		// timeline the new primary writes: the newest restore points are
		// not established, which a healthy chip would hide (round 11, DBA
		// P2). It clears once the agent reads the new timeline's WAL.
		out.State = warning
		tl := strconv.FormatUint(f.liveAhead, 10)
		setReason(out, say("recovery.reason.liveTimelineNotArchived",
			"the repository held no WAL of timeline "+tl+", the one the current primary writes, when it was last read: the newest restore points are uncertain; wait for the next evidence refresh, or run pg_switch_wal() on the new primary to archive its first segment",
			map[string]string{"timeline": tl}))
	case f.notReported:
		out.State = warning
		setReason(out, say("recovery.reason.archiverNotReported", "the archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report its archive backlog: how late recent WAL reached the repository is not known, so the newest restore points are uncertain", nil))
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
		out = append(out, interval{from: a, to: b, backupID: spans[best].backupID, degraded: spans[best].degraded, unusable: spans[best].unusable})
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
	switch a := b.Summary.Archiving; {
	case archivingFailing(a):
		if a.LastArchivedAt != nil {
			out = append(out, say("recovery.caveat.archivingFailingSince",
				"WAL archiving is failing: the primary last archived a segment at "+a.LastArchivedAt.UTC().Format(time.RFC3339)+"; newer WAL is on the instance, not in the repository.",
				map[string]string{"archivedAt": rfc3339(*a.LastArchivedAt)}))
		} else {
			out = append(out, say("recovery.caveat.archivingFailing", "WAL archiving is failing: the newest WAL is on the instance, not in the repository.", nil))
		}
	case a.Reason == cnpgstatus.ArchivingReasonBacklogNotReported:
		out = append(out, say("recovery.caveat.archiverNotReported",
			"The archiving instance does not report itself primary (a replica cluster's designated primary, or a primary mid-promotion), so it does not report its archive backlog: how long the newest archived WAL waited on it is not known, so restore points near the newest edge may not be in the repository.", nil))
	case a.Reason == cnpgstatus.ArchivingReasonHibernated:
		out = append(out, say("recovery.caveat.archiverHibernated",
			"The Cluster is hibernated: no instance runs, so nothing archives WAL, and whether the WAL written before it reached the repository is not confirmed: restore points near the newest edge may not be in the repository until it wakes up.", nil))
	case archiverUnread(a):
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
