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

package observe

import (
	"testing"
	"time"
)

// TestSaneBackupTimes (round 18, CNPG P3; round 19, CNPG P3-1 and PG
// P3-B): a plugin's unset time, which CloudNativePG stamps 1970-01-01, a
// time in the future, a lone time well before the object's creation and
// a stop before the start are unknown, never an instant; a coherent pair
// older than the object (a restored Backup) is kept and marked imported.
func TestSaneBackupTimes(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	now := created.Add(24 * time.Hour)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	epoch := time.Unix(0, 0)
	for _, c := range []struct {
		name                     string
		created                  time.Time
		started, stopped         *time.Time
		wantStarted, wantStopped *time.Time
		wantImported             bool
	}{
		{"both plausible", created, at(time.Minute), at(time.Hour), at(time.Minute), at(time.Hour), false},
		{"neither reported", created, nil, nil, nil, nil, false},
		{"plugin left stoppedAt unset", created, at(time.Minute), &epoch, at(time.Minute), nil, false},
		{"plugin left both unset", created, &epoch, &epoch, nil, nil, false},
		{"epoch with no creation", time.Time{}, &epoch, at(time.Hour), nil, at(time.Hour), false},
		{"stop before the start", created, at(time.Hour), at(time.Minute), at(time.Hour), nil, false},
		{"lone stop well before creation", created, nil, at(-time.Hour), nil, nil, false},
		{"start within the clock skew before creation", created, at(-time.Minute), at(time.Hour), at(-time.Minute), at(time.Hour), false},
		{"start well before creation, stop after it", created, at(-time.Hour), at(time.Hour), nil, at(time.Hour), false},
		{"stop equal to the start", created, at(time.Minute), at(time.Minute), at(time.Minute), at(time.Minute), false},
		{"coherent pair older than the object: imported", created, at(-72 * time.Hour), at(-71 * time.Hour), at(-72 * time.Hour), at(-71 * time.Hour), true},
		{"incoherent pair older than the object", created, at(-71 * time.Hour), at(-72 * time.Hour), nil, nil, false},
		{"stop in the far future (milliseconds as seconds)", created, at(time.Minute), at(1000 * 24 * time.Hour), at(time.Minute), nil, false},
		{"stop within the skew after now", created, at(time.Minute), at(24*time.Hour + 4*time.Minute), at(time.Minute), at(24*time.Hour + 4*time.Minute), false},
		{"stop past the skew after now", created, at(time.Minute), at(24*time.Hour + 6*time.Minute), at(time.Minute), nil, false},
		{"start in the future", created, at(48 * time.Hour), nil, nil, nil, false},
	} {
		got := SaneBackupTimes(now, BackupStamps{Created: c.created, Started: c.started, Stopped: c.stopped})
		if !sameInstant(got.Start, c.wantStarted) || !sameInstant(got.Stop, c.wantStopped) || got.Imported != c.wantImported {
			t.Errorf("%s: got %v %v imported=%v, want %v %v imported=%v", c.name, got.Start, got.Stop, got.Imported, c.wantStarted, c.wantStopped, c.wantImported)
		}
		if got.Start != nil && got.Start.Location() != time.UTC || got.Stop != nil && got.Stop.Location() != time.UTC {
			t.Errorf("%s: times not in UTC", c.name)
		}
	}
}

// TestSaneBackupOperatorTimes: the operator's reconciliation times take
// the floor and the upper bound only; one older than the object (a
// restored status) is the operator's own clock and is kept.
func TestSaneBackupOperatorTimes(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	now := created.Add(time.Hour)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	epoch := time.Unix(0, 0)
	for _, c := range []struct {
		name               string
		started, ended     *time.Time
		wantStart, wantEnd *time.Time
	}{
		{"both plausible", at(time.Minute), at(10 * time.Minute), at(time.Minute), at(10 * time.Minute)},
		{"unset", nil, nil, nil, nil},
		{"epoch", &epoch, &epoch, nil, nil},
		{"future", at(2 * time.Hour), at(2 * time.Hour), nil, nil},
		{"older than the object", at(-48 * time.Hour), at(-47 * time.Hour), at(-48 * time.Hour), at(-47 * time.Hour)},
	} {
		got := SaneBackupTimes(now, BackupStamps{Created: created, ReconciliationStarted: c.started, ReconciliationTerminated: c.ended})
		if !sameInstant(got.OperatorStarted, c.wantStart) || !sameInstant(got.OperatorTerminated, c.wantEnd) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got.OperatorStarted, got.OperatorTerminated, c.wantStart, c.wantEnd)
		}
	}
}

// TestSaneBackupTimesImportedNeedsEarlyStamps (round 20, PG P3-B, CNPG
// P3-2): tool times older than the object read as Imported only when
// CloudNativePG's stamps, if any, are as old. A Velero copy carries its
// old stamps and is imported; a live Backup on an instance whose clock
// runs 15 min behind has stamps at or after its creation, and its skewed
// tool times are dropped, never kept as a false old stop.
func TestSaneBackupTimesImportedNeedsEarlyStamps(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	now := created.Add(10 * time.Minute)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	skewedStart, skewedStop := at(5*time.Second-15*time.Minute), at(8*time.Minute-15*time.Minute)
	for _, c := range []struct {
		name                   string
		opStarted, opEnded     *time.Time
		wantImported           bool
		wantStart, wantStopped *time.Time
	}{
		{"no stamps (a restored status without them)", nil, nil, true, skewedStart, skewedStop},
		{"stamps as old as the tool times (Velero)", at(-16 * time.Minute), at(8*time.Minute + 3*time.Second - 15*time.Minute), true, skewedStart, skewedStop},
		{"live: started at creation, completed after", at(2 * time.Second), at(8*time.Minute + 3*time.Second), false, nil, nil},
		{"live: only the start stamp", at(2 * time.Second), nil, false, nil, nil},
		{"live: only the completion stamp", nil, at(8*time.Minute + 3*time.Second), false, nil, nil},
		{"old start stamp, completion after creation", at(-16 * time.Minute), at(8*time.Minute + 3*time.Second), false, nil, nil},
	} {
		got := SaneBackupTimes(now, BackupStamps{Created: created, Started: skewedStart, Stopped: skewedStop, ReconciliationStarted: c.opStarted, ReconciliationTerminated: c.opEnded})
		if got.Imported != c.wantImported || !sameInstant(got.Start, c.wantStart) || !sameInstant(got.Stop, c.wantStopped) {
			t.Errorf("%s: got imported=%v %v %v, want imported=%v %v %v", c.name, got.Imported, got.Start, got.Stop, c.wantImported, c.wantStart, c.wantStopped)
		}
		if !sameInstant(got.OperatorTerminated, c.opEnded) {
			t.Errorf("%s: completion stamp %v, want %v", c.name, got.OperatorTerminated, c.opEnded)
		}
	}
}

// TestSaneBackupTimesZeroNow (round 20, Go P3-2): a zero now sets no
// ceiling; the floor and the creation checks still apply.
func TestSaneBackupTimesZeroNow(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	epoch := time.Unix(0, 0)
	got := SaneBackupTimes(time.Time{}, BackupStamps{Created: created, Started: at(time.Minute), Stopped: at(time.Hour), ReconciliationStarted: at(0), ReconciliationTerminated: at(time.Hour + time.Second)})
	if !sameInstant(got.Start, at(time.Minute)) || !sameInstant(got.Stop, at(time.Hour)) || !sameInstant(got.OperatorStarted, at(0)) || !sameInstant(got.OperatorTerminated, at(time.Hour+time.Second)) {
		t.Errorf("zero now dropped plausible times: %+v", got)
	}
	got = SaneBackupTimes(time.Time{}, BackupStamps{Created: created, Started: &epoch, Stopped: at(-time.Hour)})
	if got.Start != nil || got.Stop != nil {
		t.Errorf("zero now kept an epoch or a lone pre-creation time: %+v", got)
	}
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
