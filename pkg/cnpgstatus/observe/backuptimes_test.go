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

// TestSaneBackupTimes: a plugin's unset time, which CloudNativePG stamps
// 1970-01-01, a time in the future and a stop before the start are
// unknown, never an instant; a tool time more than BackupClockSkew
// before the object's creation drops both tool times.
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
	}{
		{"both plausible", created, at(time.Minute), at(time.Hour), at(time.Minute), at(time.Hour)},
		{"neither reported", created, nil, nil, nil, nil},
		{"plugin left stoppedAt unset", created, at(time.Minute), &epoch, at(time.Minute), nil},
		{"plugin left both unset", created, &epoch, &epoch, nil, nil},
		{"epoch with no creation", time.Time{}, &epoch, at(time.Hour), nil, at(time.Hour)},
		{"stop before the start", created, at(time.Hour), at(time.Minute), at(time.Hour), nil},
		{"lone stop well before creation", created, nil, at(-time.Hour), nil, nil},
		{"start within the clock skew before creation", created, at(-time.Minute), at(time.Hour), at(-time.Minute), at(time.Hour)},
		{"start well before creation, stop after it", created, at(-time.Hour), at(time.Hour), nil, nil},
		{"stop equal to the start", created, at(time.Minute), at(time.Minute), at(time.Minute), at(time.Minute)},
		{"coherent pair older than the object (restored status)", created, at(-72 * time.Hour), at(-71 * time.Hour), nil, nil},
		{"stop in the far future (milliseconds as seconds)", created, at(time.Minute), at(1000 * 24 * time.Hour), at(time.Minute), nil},
		{"stop within the skew after now", created, at(time.Minute), at(24*time.Hour + 4*time.Minute), at(time.Minute), at(24*time.Hour + 4*time.Minute)},
		{"stop past the skew after now", created, at(time.Minute), at(24*time.Hour + 6*time.Minute), at(time.Minute), nil},
		{"start in the future", created, at(48 * time.Hour), nil, nil, nil},
	} {
		got := SaneBackupTimes(now, BackupStamps{Created: c.created, Started: c.started, Stopped: c.stopped})
		if !sameInstant(got.Start, c.wantStarted) || !sameInstant(got.Stop, c.wantStopped) || got.ClockBehind {
			t.Errorf("%s: got %v %v behind=%v, want %v %v", c.name, got.Start, got.Stop, got.ClockBehind, c.wantStarted, c.wantStopped)
		}
		if got.Start != nil && got.Start.Location() != time.UTC || got.Stop != nil && got.Stop.Location() != time.UTC {
			t.Errorf("%s: times not in UTC", c.name)
		}
	}
}

// TestSaneBackupTimesCreationSkew: the one creation-skew rule. A time the
// instance wrote (tool start and stop, the completion stamp) more than
// BackupClockSkew before the object's creation drops all three; an
// operator start stamp that early is dropped on its own. ClockBehind
// says the operator started this Backup live (its start stamp kept): a
// restored status (Velero), whose stamps are all old, has no plausible
// end either, never its old one.
func TestSaneBackupTimesCreationSkew(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	now := created.Add(time.Hour)
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	for _, c := range []struct {
		name       string
		queue      time.Duration // operator's start stamp after creation
		skew       time.Duration // instance clock behind
		restored   bool          // the whole status is older than the object
		wantBehind bool
		wantKept   bool
	}{
		{"honest clock", 2 * time.Second, 0, false, false, true},
		{"2 min behind: within the allowance", 2 * time.Second, 2 * time.Minute, false, false, true},
		{"6 min behind", 2 * time.Second, 6 * time.Minute, false, true, false},
		{"30 min behind", 2 * time.Second, 30 * time.Minute, false, true, false},
		{"queued 20 min, 6 min behind: still after the creation", 20 * time.Minute, 6 * time.Minute, false, false, true},
		{"Velero copy of a Backup two days old", 2 * time.Second, 0, true, false, false},
	} {
		shift := -c.skew
		if c.restored {
			shift = -48 * time.Hour
		}
		opStarted := at(c.queue)
		if c.restored {
			opStarted = at(c.queue - 48*time.Hour)
		}
		start, stop, ended := at(c.queue+5*time.Second+shift), at(c.queue+10*time.Minute+shift), at(c.queue+10*time.Minute+3*time.Second+shift)
		got := SaneBackupTimes(now, BackupStamps{Created: created, Started: start, Stopped: stop, ReconciliationStarted: opStarted, ReconciliationTerminated: ended})
		if got.ClockBehind != c.wantBehind {
			t.Errorf("%s: clock behind %v, want %v", c.name, got.ClockBehind, c.wantBehind)
		}
		if c.wantKept {
			if !sameInstant(got.Start, start) || !sameInstant(got.Stop, stop) || !sameInstant(got.OperatorTerminated, ended) {
				t.Errorf("%s: dropped a plausible time: %+v", c.name, got)
			}
		} else if got.Start != nil || got.Stop != nil || got.OperatorTerminated != nil {
			t.Errorf("%s: kept a time before the creation: %v %v %v", c.name, got.Start, got.Stop, got.OperatorTerminated)
		}
		wantStarted := opStarted
		if c.restored {
			wantStarted = nil
		}
		if !sameInstant(got.OperatorStarted, wantStarted) {
			t.Errorf("%s: the operator's start stamp %v, want %v", c.name, got.OperatorStarted, wantStarted)
		}
	}
	// The operator's stamps alone take the floor, the ceiling and the
	// creation rule.
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
		{"older than the object", at(-48 * time.Hour), at(-47 * time.Hour), nil, nil},
	} {
		got := SaneBackupTimes(now, BackupStamps{Created: created, ReconciliationStarted: c.started, ReconciliationTerminated: c.ended})
		if !sameInstant(got.OperatorStarted, c.wantStart) || !sameInstant(got.OperatorTerminated, c.wantEnd) || got.ClockBehind {
			t.Errorf("%s: got %v %v behind=%v, want %v %v", c.name, got.OperatorStarted, got.OperatorTerminated, got.ClockBehind, c.wantStart, c.wantEnd)
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
