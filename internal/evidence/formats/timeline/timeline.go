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

// Package timeline resolves which timeline PostgreSQL's
// recovery_target_timeline 'latest' follows from an archive, the rule
// both repository formats (Barman Cloud, pgBackRest) judge recovery paths
// against.
//
// PostgreSQL resolves 'latest' with findNewestTimeLine (timeline.c): from
// the timeline recovery starts on it probes the history file of the next
// timeline, then the one after, and stops at the first history file the
// archive does not return. The probe tests existence only
// (existsTimeLineHistory): a listed history file whose content is
// malformed still counts. A timeline above a missing history file is
// therefore not reached by a default restore, however much WAL and history
// it has: it needs an explicit recovery_target_timeline. Recovery then
// reads one history file, the one of the timeline 'latest' resolved to
// (readTimeLineHistory), and follows the ancestry it lists: a timeline
// that is not on it (a sibling branch) is not followed either, even when
// its number is lower. A history file that is listed but cannot be read
// leaves the answer unknown: PostgreSQL might fetch it, or might not.
package timeline

import (
	"slices"
	"strconv"
	"strings"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Status is what the archive shows of one timeline's history file.
type Status uint8

const (
	// Absent means no history file of the timeline is listed.
	Absent Status = iota
	// Valid means the history file was read and its ancestry is valid.
	Valid
	// Invalid means a history file is listed and was read, but is
	// malformed, contradicts another one, or is duplicated. The probe of
	// 'latest' counts it as present (it tests existence only), but its
	// ancestry is not established: a restore whose 'latest' resolves to it
	// fails reading it.
	Invalid
	// Unreadable means a history file is listed but could not be read:
	// whether recovery can fetch it is unknown, so the probe stops there
	// undecided.
	Unreadable
)

// weight orders the statuses from strongest to weakest evidence.
func (s Status) weight() int {
	switch s {
	case Valid:
		return 1
	case Invalid:
		return 2
	case Unreadable:
		return 3
	default:
		return 0
	}
}

// Archive is what one archive (a Barman server, a pgBackRest archive id)
// shows of its timelines: the status of each history file, the ancestry a
// valid one lists, and the timelines seen in its WAL or its backups.
type Archive struct {
	histories map[uint32]Status
	parents   map[uint32][]uint32
	truncated bool
	newest    uint32
}

// NewArchive returns an empty archive. truncated says the history files were
// not all listed or read (a safety bound): the absence of one then proves
// nothing.
func NewArchive(truncated bool) *Archive {
	return &Archive{histories: make(map[uint32]Status), parents: make(map[uint32][]uint32), truncated: truncated}
}

// History records the status of a timeline's history file and, for a valid
// one, the ancestors it lists (the parent column of each line, oldest
// first). A valid history recorded without ancestors leaves its ancestry
// unknown. A timeline recorded twice keeps the weaker status (Unreadable
// over Invalid over Valid over Absent).
func (a *Archive) History(timeline uint32, status Status, ancestors ...uint32) {
	if timeline == 0 {
		return
	}
	a.newest = max(a.newest, timeline)
	if prior, ok := a.histories[timeline]; ok && prior.weight() >= status.weight() {
		if prior == Valid && status == Valid && !slices.Equal(a.parents[timeline], ancestors) {
			a.histories[timeline] = Invalid
			delete(a.parents, timeline)
		}
		return
	}
	if status == Absent {
		return
	}
	a.histories[timeline] = status
	delete(a.parents, timeline)
	if status == Valid && validAncestry(ancestors, timeline) {
		a.parents[timeline] = slices.Clone(ancestors)
	}
}

// validAncestry reports whether ancestors is strictly increasing and below
// timeline, as a history file lists them.
func validAncestry(ancestors []uint32, timeline uint32) bool {
	if len(ancestors) == 0 {
		return false
	}
	for index, value := range ancestors {
		if value == 0 || value >= timeline || index > 0 && value <= ancestors[index-1] {
			return false
		}
	}
	return true
}

// Observe records a timeline seen in the archive's WAL or in a backup.
func (a *Archive) Observe(timeline uint32) {
	a.newest = max(a.newest, timeline)
}

// Newest is the newest timeline the archive shows, in its WAL, its backups
// or its history files.
func (a *Archive) Newest() uint32 { return a.newest }

// Truncated reports whether the history files were not all read.
func (a *Archive) Truncated() bool { return a.truncated }

// Resolution is where recovery_target_timeline 'latest' leads from one
// start timeline.
type Resolution struct {
	// Start is the timeline recovery starts on (the backup's).
	Start uint32
	// Latest is the timeline 'latest' resolves to as far as the archive
	// shows; a lower bound when Settled is false.
	Latest uint32
	// Settled is true when the probe stopped at a history file that is
	// proven absent from a complete listing, so Latest is what PostgreSQL
	// resolves.
	Settled bool
	// Stop is the first timeline after Latest whose history file the probe
	// did not find, and StopStatus why (Absent or Unreadable).
	Stop       uint32
	StopStatus Status
	// LatestStatus is the status of Latest's own history file, the one
	// recovery reads (Absent when Latest is timeline 1).
	LatestStatus Status
	// Newest is the archive's newest timeline.
	Newest uint32
	// ancestors are the timelines Latest's history lists, oldest first;
	// known says the ancestry of Latest is established (its history is
	// valid, or Latest is Start and recovery needs none).
	ancestors []uint32
	known     bool
}

// Latest probes the history files from start+1 on, as findNewestTimeLine
// does, then takes the ancestry of the timeline it resolved to from that
// timeline's history, as readTimeLineHistory does. The probe stops at the
// first history file that is absent or unreadable, so it ends after at
// most one step per recorded history file.
func (a *Archive) Latest(start uint32) Resolution {
	result := Resolution{Start: start, Latest: start, Settled: true, Newest: max(a.newest, start)}
	for probe := start + 1; probe != 0; probe++ {
		status := a.histories[probe]
		if status == Valid || status == Invalid {
			result.Latest = probe
			continue
		}
		result.Stop, result.StopStatus = probe, status
		result.Settled = status == Absent && !a.truncated
		break
	}
	result.LatestStatus = a.histories[result.Latest]
	switch {
	case result.Latest == start:
		// Recovery reads the start timeline's own history; an absent one
		// means no parents (readTimeLineHistory), an invalid one fails.
		result.known = result.LatestStatus == Absent || result.LatestStatus == Valid
		if result.LatestStatus == Valid {
			result.ancestors = a.parents[start]
			result.known = result.ancestors != nil
		}
	case result.LatestStatus == Valid && a.parents[result.Latest] != nil:
		result.ancestors = a.parents[result.Latest]
		result.known = true
	}
	return result
}

// Known reports whether the ancestry of Latest is established.
func (r Resolution) Known() bool { return r.known }

// Complete reports whether 'latest' is known, its ancestry established,
// and it reaches the newest timeline the archive shows: a default restore
// follows the newest timeline, nothing in the archive is beyond its reach.
func (r Resolution) Complete() bool { return r.Settled && r.known && r.Latest >= r.Newest }

// onLine reports whether timeline is Latest or one of its ancestors.
func (r Resolution) onLine(timeline uint32) bool {
	return timeline == r.Latest || slices.Contains(r.ancestors, timeline)
}

// Reaches reports whether a default restore from Start is known to follow
// target: the resolution is settled, the ancestry of Latest is established,
// and both Start and target are on it (target is Latest or one of its
// ancestors). Whether Latest's line leaves Start after the backup is
// consistent is the caller's check (it needs the switchpoints).
func (r Resolution) Reaches(target uint32) bool {
	return r.Settled && r.known && target >= r.Start && r.onLine(target) && r.onLine(r.Start)
}

// Elsewhere reports whether a default restore is known to follow another
// timeline than target, which is not beyond Latest: the resolution is
// settled, its ancestry established, and target is not on it (a sibling
// branch, or a line that leaves Start). A restore toward target must name
// it (recovery_target_timeline): NeedsReason says so.
func (r Resolution) Elsewhere(target uint32) bool {
	return r.Settled && r.known && target <= r.Latest && !r.Reaches(target)
}

// Needs reports whether the content of timeline's history file decides
// this resolution: it is the file recovery reads (Latest's), or the
// unreadable one the probe stopped at. Any other listed history file is
// only probed for existence, which its listing proves.
func (r Resolution) Needs(timeline uint32) bool {
	return timeline > 1 && timeline == r.Latest || timeline == r.Stop && r.StopStatus == Unreadable
}

// Gap says, in a few words, why 'latest' stops where it does: the missing
// or unreadable history file, the invalid history of the timeline it
// resolves to, or the incomplete listing. Empty when the resolution is
// Complete.
func (r Resolution) Gap() string {
	switch {
	case r.Complete():
		return ""
	case r.StopStatus == Unreadable:
		return "history " + format(r.Stop) + " is unreadable"
	case !r.Settled:
		return "timeline history files were not all read"
	case !r.known:
		return "history " + format(r.Latest) + " is unreadable or invalid"
	default:
		return "history " + format(r.Stop) + " is missing"
	}
}

// BeyondReason is the reason of a path toward target that a default
// restore cannot be shown to reach: it needs an explicit targetTLI when
// the probe stopped at a missing history file, and is undecided when a
// history file is unreadable or the listing incomplete.
func (r Resolution) BeyondReason(target uint32) string {
	switch {
	case r.Settled && r.known && target == r.Stop && r.StopStatus == Absent:
		// Recovery reads the target's own history file to learn its
		// ancestry: without it no restore can follow this timeline.
		return "history " + format(target) + " is missing: no restore can follow timeline " + format(target)
	case r.Settled && r.known:
		return "timeline " + format(target) + " needs an explicit targetTLI; " + r.Gap()
	}
	return "the timeline a default restore follows is unknown: " + r.Gap()
}

// NeedsReason is the reason of a path toward target when a default restore
// follows another timeline (Elsewhere): the restore must name target.
func (r Resolution) NeedsReason(target uint32) string {
	return "a default restore follows timeline " + format(r.Latest) + ", whose history does not lead through timeline " +
		format(target) + ": needs targetTLI " + format(target)
}

// OwnReason is the reason of a backup's own-timeline path when 'latest'
// cannot be shown to reach the archive's newest timeline.
func (r Resolution) OwnReason() string {
	if r.Settled && r.known {
		return "a default restore (recovery_target_timeline 'latest') stops on timeline " + format(r.Latest) + ": " + r.Gap() +
			"; timeline " + format(r.Newest) + " needs an explicit targetTLI"
	}
	return "the timeline a default restore follows is unknown: " + r.Gap()
}

func format(value uint32) string { return strconv.FormatUint(uint64(value), 10) }

// MaxReasonBytes is the longest reason the wire contract carries.
const MaxReasonBytes = 256

// FormerPrimaryMayHold ends the reason of a path that stops at a parent
// timeline's segment missing before a switch whose child already has WAL.
// The repository does not hold that segment and a restore past it fails
// now (the path stays unhealthy), but the segment is not proven gone: a
// primary that crashed with it still .ready keeps it on its volume, and
// CloudNativePG archives every .ready segment (ArchiveAllReadyWALs) when
// that instance restarts, before pg_rewind and before it rejoins. Deleting
// the instance or its volume is what would make the loss permanent.
// It is the contract's clause (evidencev1alpha1.FormerPrimaryMayHold), which
// the console looks for in a path's reason.
const FormerPrimaryMayHold = evidencev1alpha1.FormerPrimaryMayHold

// actionable are the clauses a reason must never lose to a cut: what an
// operator must or must not do. A reason that carries one keeps it whole
// and is shortened before it instead.
var actionable = []string{FormerPrimaryMayHold}

// Join appends extra to reason with "; ", keeping the result within
// MaxReasonBytes: extra is kept whole (it is the actionable part, such as
// the timeline a restore must name), and so is an actionable clause of
// reason (FormerPrimaryMayHold: "do not delete that instance or its
// volume"); the rest of reason is shortened at a rune boundary, before
// that clause, when both do not fit (round 8, PG P3).
func Join(reason, extra string) string {
	switch {
	case reason == "":
		return shorten(extra, MaxReasonBytes)
	case extra == "":
		return shorten(reason, MaxReasonBytes)
	}
	if len(reason)+2+len(extra) <= MaxReasonBytes {
		return reason + "; " + extra
	}
	room := MaxReasonBytes - 2 - len(extra)
	if room <= 0 {
		return truncate(extra, MaxReasonBytes)
	}
	return shorten(reason, room) + "; " + extra
}

// shorten cuts value to limit bytes: an actionable clause it carries is
// kept whole and what comes before it is cut (marked "…"); otherwise the
// end is cut. When even the clause does not fit, the end is cut.
func shorten(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for _, clause := range actionable {
		at := strings.LastIndex(value, clause)
		if at < 0 {
			continue
		}
		head, tail := strings.TrimSuffix(value[:at], "; "), value[at:]
		if len(tail)+len("…; ") > limit {
			// What follows the clause is cut first.
			tail = clause
		}
		room := limit - len(tail) - len("…; ")
		if room < 0 {
			break
		}
		return truncate(head, room) + "…; " + tail
	}
	room := limit - len("…")
	if room <= 0 {
		return truncate(value, limit)
	}
	return truncate(value, room) + "…"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && value[cut]&0xC0 == 0x80 {
		cut--
	}
	return value[:cut]
}
