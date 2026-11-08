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

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

type timelineSpy struct {
	FixtureSource
	reads    *int
	query    *TimelineQuery
	disabled bool
}

func (s timelineSpy) Timeline(ctx context.Context, ns, name string, q TimelineQuery) (viewmodel.Timeline, error) {
	*s.reads++
	*s.query = q
	return s.FixtureSource.Timeline(ctx, ns, name, q)
}

func (s timelineSpy) Digests(ctx context.Context) (DigestArchive, error) {
	if s.disabled {
		return DigestArchive{}, ErrDigestDisabled
	}
	return s.FixtureSource.Digests(ctx)
}

// fixtureNow is the request clock of the window tests.
var fixtureNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func TestTimelineAuthorizationAndWindow(t *testing.T) {
	t.Parallel()
	billing := authz.ObjectRef{Namespace: "team-a", Name: "billing"}
	base := "/api/v1/namespaces/team-a/clusters/billing/timeline"
	cases := []struct {
		name, path    string
		az            FixtureAuthorizer
		status, reads int
	}{
		{"allowed", base, FixtureAuthorizer{}, 200, 1},
		{"denied before any read", base, FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{billing: true}}, 403, 0},
		{"unknown kind", base + "?kind=secret", FixtureAuthorizer{}, 422, 0},
		{"window over 31 days", base + "?from=2026-09-01T00:00:00Z&to=2026-10-29T00:00:00Z", FixtureAuthorizer{}, 422, 0},
		{"empty window", base + "?from=2026-10-29T00:00:00Z&to=2026-10-29T00:00:00Z", FixtureAuthorizer{}, 422, 0},
		{"from alone, over 31 days before now", base + "?from=2026-09-01T00:00:00Z", FixtureAuthorizer{}, 422, 0},
		{"from alone, after now", base + "?from=2026-11-01T00:00:00Z", FixtureAuthorizer{}, 422, 0},
		{"to alone takes the default day", base + "?to=2026-10-29T00:00:00Z", FixtureAuthorizer{}, 200, 1},
		{"limit too high", base + "?limit=5000", FixtureAuthorizer{}, 422, 0},
		{"unknown cluster", "/api/v1/namespaces/team-a/clusters/gone/timeline", FixtureAuthorizer{}, 404, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			var q TimelineQuery
			h, _ := New(Deps{Source: timelineSpy{reads: &reads, query: &q}, Authz: tc.az, Features: FixtureFeatures, Now: func() time.Time { return fixtureNow }})
			w := do(t, h, http.MethodGet, tc.path, "")
			if w.Code != tc.status {
				t.Fatalf("GET %s = %d %s, want %d", tc.path, w.Code, w.Body, tc.status)
			}
			if reads != tc.reads {
				t.Fatalf("consulted %d times, want %d", reads, tc.reads)
			}
		})
	}
	reads := 0
	var q TimelineQuery
	h, _ := New(Deps{Source: timelineSpy{reads: &reads, query: &q}, Authz: FixtureAuthorizer{}, Features: FixtureFeatures, Now: func() time.Time { return fixtureNow }})
	if w := do(t, h, http.MethodGet, base+"?kind=finding,backup&limit=20&from=2026-10-29T00:00:00Z", ""); w.Code != 200 {
		t.Fatalf("query = %d %s", w.Code, w.Body)
	}
	if strings.Join(q.Kinds, ",") != "finding,backup" || q.Limit != 20 || q.From.IsZero() || !q.To.IsZero() {
		t.Fatalf("query passed as %+v", q)
	}
}

func TestDigestsFilterPerCaller(t *testing.T) {
	t.Parallel()
	denied := FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "billing"}: true}}
	h, _ := New(Deps{Source: FixtureSource{}, Authz: denied, Features: FixtureFeatures})
	w := do(t, h, http.MethodGet, "/api/v1/digests/20261029T0700Z", "")
	if w.Code != 200 {
		t.Fatalf("get = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "billing") {
		t.Fatalf("a hidden Cluster's name leaked: %s", w.Body)
	}
	var d viewmodel.Digest
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Hidden != 1 || d.Counts.Clusters != 1 || d.Counts.Attention != 0 || d.Counts.Failovers != 0 || len(d.Attention) != 0 {
		t.Fatalf("filtered digest: hidden=%d counts=%+v", d.Hidden, d.Counts)
	}
	if len(d.ConsoleGaps) != 1 {
		t.Fatalf("console-wide gaps stay: %+v", d.ConsoleGaps)
	}
	for _, path := range []string{"/api/v1/digests/../admin", "/api/v1/digests/not-an-id"} {
		if w := do(t, h, http.MethodGet, path, ""); w.Code == 200 {
			t.Fatalf("%s = 200", path)
		}
	}
}

func TestDigestsDisabled(t *testing.T) {
	t.Parallel()
	reads := 0
	var q TimelineQuery
	h, _ := New(Deps{Source: timelineSpy{reads: &reads, query: &q, disabled: true}, Authz: FixtureAuthorizer{}, Features: FixtureFeatures})
	w := do(t, h, http.MethodGet, "/api/v1/digests", "")
	var list viewmodel.DigestList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || w.Code != 200 {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	if list.Source.State != viewmodel.SourceDisabled || len(list.Items) != 0 {
		t.Fatalf("disabled list: %+v", list)
	}
	if w := do(t, h, http.MethodGet, "/api/v1/digests/20261029T0700Z", ""); w.Code != http.StatusNotFound {
		t.Fatalf("get on a disabled digest = %d", w.Code)
	}
}
