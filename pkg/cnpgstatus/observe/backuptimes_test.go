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

// TestSaneBackupTimes (round 18, CNPG P3): a plugin's unset time, which
// CloudNativePG stamps 1970-01-01, a time well before the object's
// creation and a stop before the start are unknown, never an instant.
func TestSaneBackupTimes(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
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
		{"stop well before creation", created, nil, at(-time.Hour), nil, nil},
		{"start within the clock skew before creation", created, at(-time.Minute), at(time.Hour), at(-time.Minute), at(time.Hour)},
		{"start well before creation", created, at(-time.Hour), at(time.Hour), nil, at(time.Hour)},
		{"stop equal to the start", created, at(time.Minute), at(time.Minute), at(time.Minute), at(time.Minute)},
	} {
		start, stop := SaneBackupTimes(c.created, c.started, c.stopped)
		if !sameInstant(start, c.wantStarted) || !sameInstant(stop, c.wantStopped) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, start, stop, c.wantStarted, c.wantStopped)
		}
		if start != nil && start.Location() != time.UTC || stop != nil && stop.Location() != time.UTC {
			t.Errorf("%s: times not in UTC", c.name)
		}
	}
}

func sameInstant(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}
