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

package diagnose

import (
	"testing"

	"github.com/robfig/cron"
)

// FuzzRunsPerDay: the cadence reader takes a ScheduledBackup's schedule,
// written by anyone who can create the object. Whatever the string, the
// count is a real day's (0 to 86 400 runs, one a second at most), never
// negative, and a schedule the operator's parser rejects is never read
// as a cadence.
func FuzzRunsPerDay(f *testing.F) {
	for _, seed := range []string{
		"0 0 2 * * *", "* * * * * *", "@every 30m", "@every 1s", "@every 500ms", "@daily", "0 0 0 30 2 *",
		"0 2 * * 1-5", "*/0 * * * * *", "", "@every -1s", "0 0 0 * * 0,7", "60 * * * * *", "@every 87600h",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		perDay, everyDay, ok := runsPerDay(expr)
		if _, err := cron.Parse(expr); err != nil && ok {
			t.Fatalf("runsPerDay(%q) ok on a schedule the operator rejects (%v): %d", expr, err, perDay)
		}
		if !ok {
			if perDay != 0 || everyDay {
				t.Fatalf("runsPerDay(%q) not ok but %d, everyDay %v", expr, perDay, everyDay)
			}
			return
		}
		if perDay < 0 || perDay > 86400 {
			t.Fatalf("runsPerDay(%q) = %d runs a day", expr, perDay)
		}
	})
}
