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
	"encoding/json"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/sweep"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/instancestatus"
)

const maskedError = "FATAL: the database system is shutting down (SQLSTATE 57P03)"

// maskedSnaps are the replication fixture's sweep with the primary
// orders-1 and the replica orders-2 masked: their managers answered
// without reaching PostgreSQL, the primary still carrying walsenders and
// slots and the replica pending_restart, replay paused and a ready-file
// count, as a report filled before the error would.
func maskedSnaps() map[sweep.Key]sweep.Snapshot {
	snaps := replicationSnaps()
	key := sweep.Key{Namespace: "team-a", Name: "orders"}
	snap := snaps[key]
	instances := map[string]sweep.Instance{}
	for name, inst := range snap.Instances {
		if name == "orders-1" || name == "orders-2" {
			r := *inst.Reading
			r.MightBeUnavailable, r.MightBeUnavailableMaskedError = true, maskedError
			r.PendingRestart, r.ReplayPaused, r.ReadyWALFiles, r.LastFailedWAL = true, !r.IsPrimary, 7, "000000010000000000000002"
			inst.Reading = &r
		}
		instances[name] = inst
	}
	snap.Instances = instances
	snaps[key] = snap
	return snaps
}

func maskedService(t *testing.T) *Service {
	t.Helper()
	svc := newScreenService(t)
	svc.mu.Lock()
	svc.sweep = fakeSweep{snaps: maskedSnaps()}
	svc.mu.Unlock()
	return svc
}

// TestMaskedInstanceIsUnread (round 17, PG/CNPG P3): an instance whose
// manager answered without reaching PostgreSQL serves none of
// PostgreSQL's fields on the Instances screen, says so with the masked
// error, and keeps the manager's own flags.
func TestMaskedInstanceIsUnread(t *testing.T) {
	list, err := maskedService(t).Instances(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range list.Items {
		if row.Name != "orders-1" && row.Name != "orders-2" {
			if row.Live != nil && row.Live.PostgresUnread != nil {
				t.Errorf("%s: read instance marked unread", row.Name)
			}
			continue
		}
		seen++
		l := row.Live
		if l == nil || l.PostgresUnread == nil || l.PostgresUnread.Error != maskedError || l.PostgresUnread.Instance != row.Name {
			t.Fatalf("%s: live %+v, want postgresUnread with the masked error", row.Name, l)
		}
		if l.PendingRestart || l.ReplayPaused || l.WALReceiverActive || l.ArchivingWAL || l.ReadyWALFiles != 0 || l.LastFailedWAL != "" ||
			l.CurrentLSN != "" || l.ReplayLSN != "" || l.TimelineID != nil || l.LagBytes != nil || len(l.Standbys) != 0 {
			t.Errorf("%s: a masked report serves PostgreSQL's fields: %+v", row.Name, l)
		}
		if !l.MightBeUnavailable || l.ManagerVersion == "" {
			t.Errorf("%s: the manager's own flags are lost: %+v", row.Name, l)
		}
		if row.Health == viewmodel.HealthOK || row.Health == viewmodel.HealthProgressing {
			t.Errorf("%s: health %s on a masked report", row.Name, row.Health)
		}
	}
	if seen != 2 {
		t.Fatalf("%d masked rows, want 2", seen)
	}
}

// TestMaskedReplicationIsUnread (round 17, CNPG P3): a masked primary's
// walsenders and slots are not read, never "none"; a masked replica's
// receiver is not read, never inactive; the topology places no replica
// against the masked primary's walsender list.
func TestMaskedReplicationIsUnread(t *testing.T) {
	svc := maskedService(t)
	rep, err := svc.Replication(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if rep.PrimaryUnread == nil || rep.PrimaryUnread.Instance != "orders-1" || rep.PrimaryUnread.Error != maskedError {
		t.Fatalf("primaryUnread %+v, want orders-1 with the masked error", rep.PrimaryUnread)
	}
	if len(rep.Standbys) != 0 || len(rep.Slots) != 0 {
		t.Errorf("a masked primary's walsenders (%d) or slots (%d) are served", len(rep.Standbys), len(rep.Slots))
	}
	for _, row := range rep.Receivers {
		switch row.Name {
		case "orders-2":
			if row.PostgresUnread == nil || row.WALReceiverActive != nil || row.ReplayPaused || row.Health != viewmodel.HealthUnknown || row.TimelineID != nil {
				t.Errorf("masked replica row %+v, want unread receiver, unknown health", row)
			}
		default:
			if row.PostgresUnread != nil {
				t.Errorf("%s: read replica marked unread", row.Name)
			}
		}
	}
	list, err := svc.Instances(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]viewmodel.InstanceRow, 0, len(list.Items))
	for _, r := range list.Items {
		if r.Name == "orders-1" {
			r.Role.Operator = viewmodel.RolePrimary
		}
		rows = append(rows, r)
	}
	if got := attachedStandbys(rows); got != nil {
		t.Errorf("attachedStandbys from a masked primary = %v, want nil (unknown)", got)
	}
	topo, err := svc.Topology(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	text, err := json.Marshal(topo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "no walsender on the primary") {
		t.Errorf("topology reads a masked primary's walsenders as none: %s", text)
	}
}

// TestReadingOK: the one test of a reading of PostgreSQL itself.
func TestReadingOK(t *testing.T) {
	read := &instancestatus.Reading{IsPrimary: true}
	masked := &instancestatus.Reading{IsPrimary: true, MightBeUnavailable: true, MightBeUnavailableMaskedError: maskedError}
	for _, c := range []struct {
		name string
		inst sweep.Instance
		want bool
	}{
		{"ok", sweep.Instance{State: viewmodel.StatusReadOK, Reading: read}, true},
		{"masked", sweep.Instance{State: viewmodel.StatusReadOK, Reading: masked}, false},
		{"no reading", sweep.Instance{State: viewmodel.StatusReadOK}, false},
		{"failed read", sweep.Instance{State: viewmodel.StatusReadUnreachable, Reading: read}, false},
	} {
		if got := c.inst.ReadingOK(); got != c.want {
			t.Errorf("%s: ReadingOK %v, want %v", c.name, got, c.want)
		}
	}
}
