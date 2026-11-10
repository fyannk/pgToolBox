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

package timeline

import (
	"testing"
	"unicode/utf8"
)

// FuzzLatest drives the resolver with arbitrary history statuses and checks
// the probe's invariants: Latest is never below the start, every timeline it
// steps over has a valid history, the stop is the first that is not valid,
// a complete resolution reaches the newest timeline, and an incomplete one
// never claims to reach it.
func FuzzLatest(f *testing.F) {
	f.Add(uint32(1), []byte{3, 1, 2, 0}, false, "reason", "extra")
	f.Add(uint32(1), []byte{2, 2}, true, "", "")
	f.Fuzz(func(t *testing.T, start uint32, statuses []byte, truncated bool, reason, extra string) {
		if start == 0 || len(statuses) > 64 {
			return
		}
		archive := NewArchive(truncated)
		archive.Observe(start)
		for index, value := range statuses {
			archive.History(start+uint32(index)+1, Status(value%3))
		}
		got := archive.Latest(start)
		if got.Latest < start || got.Newest < got.Latest {
			t.Fatalf("Latest(%d) = %+v", start, got)
		}
		for probe := start + 1; probe <= got.Latest && probe != 0; probe++ {
			if archive.histories[probe] != Valid {
				t.Fatalf("stepped over timeline %d without a valid history: %+v", probe, got)
			}
		}
		if got.Stop != 0 && archive.histories[got.Stop] == Valid {
			t.Fatalf("stopped at a valid history: %+v", got)
		}
		if got.Complete() != (got.Settled && got.Latest >= got.Newest) || !got.Complete() && got.Reaches(got.Newest) && got.Newest > got.Latest {
			t.Fatalf("inconsistent resolution: %+v", got)
		}
		if truncated && got.Settled {
			t.Fatalf("a truncated listing settled the probe: %+v", got)
		}
		if !utf8.ValidString(reason) || !utf8.ValidString(extra) {
			return
		}
		if joined := Join(reason, extra); len(joined) > MaxReasonBytes || !utf8.ValidString(joined) {
			t.Fatalf("Join = %d bytes, valid UTF-8 %v", len(joined), utf8.ValidString(joined))
		}
	})
}
