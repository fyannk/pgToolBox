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

package readiness

import (
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
)

func TestProbeStateRequiresRecentReachability(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	state := New(true, time.Minute, func() time.Time { return now })
	if result := state.Result(); result.Ready || result.Category != fault.Unavailable {
		t.Fatalf("initial result = %#v", result)
	}
	state.MarkReachable(now.Add(-30 * time.Second))
	if result := state.Result(); !result.Ready {
		t.Fatalf("fresh result = %#v", result)
	}
	state.MarkReachable(now.Add(-2 * time.Minute))
	if result := state.Result(); result.Ready || result.Category != fault.Unavailable {
		t.Fatalf("stale result = %#v", result)
	}
	state.MarkFailure(now, fault.Authorization)
	if result := state.Result(); result.Ready || result.Category != fault.Authorization {
		t.Fatalf("failure result = %#v", result)
	}
}
