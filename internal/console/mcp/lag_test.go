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

package mcp

import (
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The person confirms on the console's cached view of the Cluster; when
// the live one differs (the cache behind, or a real change), nothing is
// done and the person is asked again on the live state, once: confirmed
// again, the write runs on what they saw the second time; refused again,
// it is the 409.
func TestConfirmationOnALaggingCacheIsAskedAgain(t *testing.T) {
	t.Parallel()
	recreated := func(uid string) viewmodel.ClusterRevision {
		r := testRevision
		r.UID = uid
		return r
	}
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	for _, tc := range []struct {
		name   string
		live   []viewmodel.ClusterRevision
		ran    bool
		expect string // the second request's expectation (UID; reload keeps only it)
	}{
		{"the cache was behind", []viewmodel.ClusterRevision{recreated("uid-2")}, true, "uid-2"},
		{"it changed again", []viewmodel.ClusterRevision{recreated("uid-2"), recreated("uid-3")}, false, "uid-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, op := writeHarness(t)
			op.mu.Lock()
			op.live = tc.live
			op.mu.Unlock()
			cs, rec := h.elicitClient(t, "tok-dba", answer{action: "accept", typed: "orders"})
			res, _ := callTool(t, cs, "reload", orders)
			if res.IsError == tc.ran {
				t.Fatalf("error %v: %s", res.IsError, text(res))
			}
			if !tc.ran && !strings.Contains(text(res), "409") {
				t.Errorf("refused again: %s", text(res))
			}
			asked, got := rec.questions(), op.requests()
			if len(asked) != 2 || !strings.Contains(asked[1].Message, "Here is the question again") {
				t.Fatalf("questions = %+v", asked)
			}
			if len(got) != 2 || got[0].origin.Confirmed.UID != testRevision.UID || got[1].origin.Confirmed.UID != tc.expect {
				t.Errorf("requests = %+v", got)
			}
		})
	}
}
