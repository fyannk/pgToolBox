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
)

// TestFailoverRehearsalStaleSweep: the rehearsal rests on one sweep; under
// the console's sweep staleness rule (three intervals, the one the
// instance self-reports contributor uses) it is marked stale, and the
// rehearsal input carries the reason so "none" cannot stand.
func TestFailoverRehearsalStaleSweep(t *testing.T) {
	svc := newScreenService(t)
	fresh, err := svc.Replication(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Failover.Provenance.Stale || fresh.Failover.Provenance.ObservedAt == nil {
		t.Fatalf("a fresh sweep: %+v", fresh.Failover.Provenance)
	}
	svc.now = func() time.Time { return now.Add(10 * time.Minute) }
	stale, err := svc.Replication(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	p := stale.Failover.Provenance
	if !p.Stale || p.StaleReason != sweepStaleReason || p.ObservedAt == nil {
		t.Errorf("a sweep older than three intervals is not stale: %+v", p)
	}
	if stale.Failover.DataLoss == "none" {
		t.Errorf("a stale sweep still says no loss: %+v", stale.Failover)
	}
}
