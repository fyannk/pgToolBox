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
// archive does not return. A timeline above a missing history file is
// therefore not reached by a default restore, however much WAL and history
// it has: it needs an explicit recovery_target_timeline. A history file
// that is listed but cannot be read or is invalid leaves the answer
// unknown: PostgreSQL might read it, or might not.
package timeline

import "strconv"

// Status is what the archive shows of one timeline's history file.
type Status uint8

const (
	// Absent means no history file of the timeline is listed.
	Absent Status = iota
	// Valid means the history file was read and its ancestry is valid.
	Valid
	// Invalid means a history file is listed but could not be read, is
	// malformed, contradicts another one, or is duplicated.
	Invalid
)

// Archive is what one archive (a Barman server, a pgBackRest archive id)
// shows of its timelines: the status of each history file and the
// timelines seen in its WAL or its backups.
type Archive struct {
	histories map[uint32]Status
	truncated bool
	newest    uint32
}

// NewArchive returns an empty archive. truncated says the history files were
// not all listed or read (a safety bound): the absence of one then proves
// nothing.
func NewArchive(truncated bool) *Archive {
	return &Archive{histories: make(map[uint32]Status), truncated: truncated}
}

// History records the status of a timeline's history file. A timeline
// recorded twice keeps the weaker status (Invalid over Valid over Absent).
func (a *Archive) History(timeline uint32, status Status) {
	if timeline == 0 {
		return
	}
	if prior, ok := a.histories[timeline]; ok && prior == Invalid {
		return
	}
	if status != Absent {
		a.histories[timeline] = status
	}
	a.newest = max(a.newest, timeline)
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
	// did not find valid, and StopStatus why (Absent or Invalid).
	Stop       uint32
	StopStatus Status
	// Newest is the archive's newest timeline.
	Newest uint32
}

// Latest probes the history files from start+1 on, as findNewestTimeLine
// does. The probe stops at the first history file that is absent or
// invalid, so it ends after at most one step per recorded history file.
func (a *Archive) Latest(start uint32) Resolution {
	result := Resolution{Start: start, Latest: start, Settled: true, Newest: max(a.newest, start)}
	for probe := start + 1; probe != 0; probe++ {
		status := a.histories[probe]
		if status == Valid {
			result.Latest = probe
			continue
		}
		result.Stop, result.StopStatus = probe, status
		result.Settled = status == Absent && !a.truncated
		break
	}
	return result
}

// Complete reports whether 'latest' is known and reaches the newest
// timeline the archive shows: a default restore follows the newest
// timeline, nothing in the archive is beyond its reach.
func (r Resolution) Complete() bool { return r.Settled && r.Latest >= r.Newest }

// Reaches reports whether a default restore is known to reach target: the
// resolution is settled and target is not above Latest.
func (r Resolution) Reaches(target uint32) bool { return r.Settled && target <= r.Latest }

// Gap says, in a few words, why 'latest' stops where it does: the missing
// or unreadable history file, or the incomplete listing. Empty when the
// resolution is Complete.
func (r Resolution) Gap() string {
	switch {
	case r.Complete():
		return ""
	case r.StopStatus == Invalid:
		return "history " + strconv.FormatUint(uint64(r.Stop), 10) + " is unreadable or invalid"
	case !r.Settled:
		return "timeline history files were not all read"
	default:
		return "history " + strconv.FormatUint(uint64(r.Stop), 10) + " is missing"
	}
}

// BeyondReason is the reason of a path toward target that a default
// restore cannot be shown to reach: it needs an explicit targetTLI when
// the probe stopped at a missing history file, and is undecided when a
// history file is unreadable or the listing incomplete.
func (r Resolution) BeyondReason(target uint32) string {
	switch {
	case r.Settled && target == r.Stop && r.StopStatus == Absent:
		// Recovery reads the target's own history file to learn its
		// ancestry: without it no restore can follow this timeline.
		return "history " + format(target) + " is missing: no restore can follow timeline " + format(target)
	case r.Settled:
		return "timeline " + format(target) + " needs an explicit targetTLI; " + r.Gap()
	}
	return "the timeline a default restore follows is unknown: " + r.Gap()
}

// OwnReason is the reason of a backup's own-timeline path when 'latest'
// cannot be shown to reach the archive's newest timeline.
func (r Resolution) OwnReason() string {
	if r.Settled {
		return "a default restore (recovery_target_timeline 'latest') stops on timeline " + format(r.Latest) + ": " + r.Gap() +
			"; timeline " + format(r.Newest) + " needs an explicit targetTLI"
	}
	return "the timeline a default restore follows is unknown: " + r.Gap()
}

func format(value uint32) string { return strconv.FormatUint(uint64(value), 10) }

// MaxReasonBytes is the longest reason the wire contract carries.
const MaxReasonBytes = 256

// Join appends extra to reason with "; ", keeping the result within
// MaxReasonBytes: extra is kept whole (it is the actionable part, such as
// the timeline a restore must name) and reason is shortened at a rune
// boundary when both do not fit.
func Join(reason, extra string) string {
	switch {
	case reason == "":
		return truncate(extra, MaxReasonBytes)
	case extra == "":
		return truncate(reason, MaxReasonBytes)
	}
	if len(reason)+2+len(extra) <= MaxReasonBytes {
		return reason + "; " + extra
	}
	room := MaxReasonBytes - 2 - len(extra) - len("…")
	if room <= 0 {
		return truncate(extra, MaxReasonBytes)
	}
	return truncate(reason, room) + "…; " + extra
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
