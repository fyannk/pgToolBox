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
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

// TestPushRingsStampsThePrimaryTimelineAndAFirstArchive: a primary's
// sweep keeps the timeline it writes, from its current WAL file's name
// (the recovery facts match a former primary's readings to the timeline
// it wrote, round 9 P2), not pg_control's timelineId, which lags a
// promotion until the next checkpoint; it records "read,
// nothing archived yet" as ArchivedNone, never as a gap, so a first
// archive after it counts as progress (CNPG P3-b). A replica keeps
// neither.
func TestPushRingsStampsThePrimaryTimelineAndAFirstArchive(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	archived := at.Add(-time.Minute)
	s := &Sweeper{opts: Options{Retention: time.Hour}}
	cs := &clusterState{rings: map[string]map[string]*ring{}}
	read := func(r *instancestatus.Reading) Instance {
		return Instance{State: viewmodel.StatusReadOK, Reading: r, readBy: at.Add(time.Second)}
	}
	s.pushRings(cs, Snapshot{Instances: map[string]Instance{
		// Just promoted to timeline 3: pg_control still says 2.
		"db-1": read(&instancestatus.Reading{IsPrimary: true, TimelineID: 2, CurrentWAL: "000000030000000000000003", CurrentLSN: "0/3000000"}),
		"db-2": read(&instancestatus.Reading{TimelineID: 3, CurrentWAL: "000000030000000000000003", ReplayLSN: "0/3000000"}),
	}}, at)
	s.pushRings(cs, Snapshot{Instances: map[string]Instance{
		"db-1": read(&instancestatus.Reading{IsPrimary: true, TimelineID: 3, CurrentWAL: "not a wal file name", CurrentLSN: "0/3000000", LastArchivedWALTime: &archived}),
		"db-2": read(&instancestatus.Reading{TimelineID: 3, CurrentWAL: "000000030000000000000003", ReplayLSN: "0/3000000"}),
	}}, at.Add(30*time.Second))
	// Round 10, CNPG P3: a time the instance sent that does not parse is
	// no reading, never "nothing archived yet".
	s.pushRings(cs, Snapshot{Instances: map[string]Instance{
		"db-1": read(&instancestatus.Reading{IsPrimary: true, CurrentWAL: "000000030000000000000003", CurrentLSN: "0/3000000", LastArchivedWALTimeUnparsed: true}),
		"db-2": read(&instancestatus.Reading{TimelineID: 3, CurrentWAL: "000000030000000000000003", ReplayLSN: "0/3000000"}),
	}}, at.Add(time.Minute))
	value := func(instance, key string, i int) *float64 {
		points := cs.rings[instance][key].points()
		if i >= len(points) {
			t.Fatalf("%s %s: %d points", instance, key, len(points))
		}
		return points[i].Value
	}
	if v := value("db-1", SeriesTimeline, 0); v == nil || *v != 3 {
		t.Errorf("primary timeline = %v, want 3 (its current WAL file's)", v)
	}
	if v := value("db-1", SeriesTimeline, 1); v != nil {
		t.Errorf("primary timeline from an unparsable WAL name = %v, want none", *v)
	}
	if v := value("db-1", SeriesLastArchivedAt, 0); v == nil || *v != ArchivedNone {
		t.Errorf("primary that archived nothing yet = %v, want ArchivedNone", v)
	}
	if v := value("db-1", SeriesLastArchivedAt, 1); v == nil || *v != float64(archived.UnixMilli()) {
		t.Errorf("primary's first archive = %v", v)
	}
	if v := value("db-1", SeriesLastArchivedAt, 2); v != nil {
		t.Errorf("primary whose archive time does not parse = %v, want no reading", *v)
	}
	if v := value("db-2", SeriesTimeline, 0); v != nil {
		t.Errorf("replica timeline = %v, want none", *v)
	}
	if v := value("db-2", SeriesLastArchivedAt, 0); v != nil {
		t.Errorf("replica last archived = %v, want none", *v)
	}
}
