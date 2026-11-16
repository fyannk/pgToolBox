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
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// TestSweepStaleMarksInstanceRows: under the sweep staleness rule the
// Replication rows (standbys, slots, receivers) and the Instances live
// reports are marked stale with the sweep's reason, as the failover panel
// of the same screen is (issue #78); their health does not move.
func TestSweepStaleMarksInstanceRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    time.Time
		stale bool
	}{
		{name: "fresh sweep", at: now, stale: false},
		{name: "sweep older than three intervals", at: now.Add(10 * time.Minute), stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newScreenService(t)
			fresh, err := svc.Replication(context.Background(), "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			svc.now = func() time.Time { return tc.at }
			rep, err := svc.Replication(context.Background(), "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			list, err := svc.Instances(context.Background(), "team-a", "orders")
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Standbys) == 0 || len(rep.Slots) == 0 || len(rep.Receivers) == 0 || len(list.Items) == 0 {
				t.Fatalf("the fixture has no rows to check: %d standbys, %d slots, %d receivers, %d instances",
					len(rep.Standbys), len(rep.Slots), len(rep.Receivers), len(list.Items))
			}
			check := func(what string, p viewmodel.Provenance) {
				t.Helper()
				if p.Stale != tc.stale {
					t.Errorf("%s: stale = %v, want %v (%+v)", what, p.Stale, tc.stale, p)
				}
				if tc.stale && p.StaleReason != sweepStaleReason {
					t.Errorf("%s: reason %q, want the sweep's", what, p.StaleReason)
				}
			}
			for i, row := range rep.Standbys {
				check("standby "+row.Name, row.Provenance)
				if row.Health != fresh.Standbys[i].Health {
					t.Errorf("standby %s: health moved from %s to %s", row.Name, fresh.Standbys[i].Health, row.Health)
				}
			}
			for i, row := range rep.Slots {
				check("slot "+row.Name, row.Provenance)
				if row.Health != fresh.Slots[i].Health {
					t.Errorf("slot %s: health moved from %s to %s", row.Name, fresh.Slots[i].Health, row.Health)
				}
			}
			for i, row := range rep.Receivers {
				check("receiver "+row.Name, row.Provenance)
				if row.Health != fresh.Receivers[i].Health {
					t.Errorf("receiver %s: health moved from %s to %s", row.Name, fresh.Receivers[i].Health, row.Health)
				}
			}
			for _, row := range list.Items {
				if row.Live == nil {
					continue
				}
				check("instance "+row.Name, row.Live.Provenance)
			}
		})
	}
}
