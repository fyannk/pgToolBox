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

package sweep

import (
	"strconv"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// sample is one ring entry; a nil value is a gap (the read failed). at
// is the sweep's start, before the request was sent; readBy, when the
// read returned (zero when unknown: a gap, or a ring restored from a
// snapshot that predates it).
type sample struct {
	at     time.Time
	readBy time.Time
	value  *float64
}

// ring keeps the samples of one series younger than the retention. It is
// bounded by retention / interval: one sample per sweep.
type ring struct {
	samples []sample
}

func (r *ring) push(at, readBy time.Time, value *float64, retention time.Duration) {
	r.samples = append(r.samples, sample{at: at, readBy: readBy, value: value})
	cut := 0
	for cut < len(r.samples) && at.Sub(r.samples[cut].at) > retention {
		cut++
	}
	if cut > 0 {
		r.samples = append(r.samples[:0], r.samples[cut:]...)
	}
}

func (r *ring) points() []viewmodel.Point {
	out := make([]viewmodel.Point, 0, len(r.samples))
	for _, s := range r.samples {
		p := viewmodel.Point{At: s.at, ReadBy: s.readBy}
		if s.value != nil {
			v := *s.value
			p.Value = &v
		}
		out = append(out, p)
	}
	return out
}

// Series keys of the status-derived rings.
const (
	SeriesLagBytes      = "lagBytes"
	SeriesReadyWALFiles = "readyWalFiles"
	// SeriesLastArchivedAt is a primary's pg_stat_archiver
	// last_archived_time at each sweep, in Unix milliseconds on the
	// instance's own clock (nil for a standby, a failed read, or none
	// reported). The backlog rule compares it across sweeps of the same
	// instance (cnpgstatus ArchiveBacklogOf); it is not a sparkline.
	SeriesLastArchivedAt = "lastArchivedAt"
	// SeriesTimeline is the timeline a primary writes at each sweep: the
	// one its current WAL file is named on (/pg/status currentWal,
	// pg_walfile_name(pg_current_wal_lsn())), not timelineId, which
	// CloudNativePG reads from pg_control_checkpoint() and which stays on
	// the old timeline after a promotion until the first checkpoint
	// completes. Nil for a standby, a failed read, or a name that does
	// not parse. The recovery facts match a former primary's readings to
	// the timeline whose WAL its archiver wrote; it is not a sparkline.
	SeriesTimeline = "timeline"
)

// ArchivedNone is the SeriesLastArchivedAt value of a sweep that read the
// primary's archiver and found it had archived nothing yet
// (last_archived_time null): a first archive after it is progress, which
// a gap (nil, no reading) could not show (CNPG P3-b).
const ArchivedNone = -1

var seriesUnits = map[string]string{
	SeriesLagBytes: "bytes", SeriesReadyWALFiles: "files", SeriesLastArchivedAt: "ms", SeriesTimeline: "",
}

// walFileTimeline is the timeline a WAL file name (24 hex digits) is on;
// false when it does not parse or names timeline 0.
func walFileTimeline(name string) (uint32, bool) {
	if len(name) != 24 {
		return 0, false
	}
	tl, err := strconv.ParseUint(name[:8], 16, 32)
	if err != nil || tl == 0 {
		return 0, false
	}
	return uint32(tl), true
}

// parseLSN reads PostgreSQL's "X/Y" LSN text.
func parseLSN(s string) (uint64, bool) { return cnpgstatus.ParseLSN(s) }

// LagBytes is a replica's replay distance behind the primary's current
// position, both as last read; false when either is unknown or the
// replica reads ahead (a sweep that read them at different instants).
func LagBytes(primaryCurrent, replicaReplay string) (int64, bool) {
	p, ok := parseLSN(primaryCurrent)
	if !ok {
		return 0, false
	}
	r, ok := parseLSN(replicaReplay)
	if !ok {
		return 0, false
	}
	if r > p {
		return 0, true
	}
	diff := p - r
	if diff > 1<<62 {
		return 0, false
	}
	return int64(diff), true
}
