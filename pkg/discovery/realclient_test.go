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

package discovery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// countingAPIServer is the smallest API server a real controller-runtime
// client probes against: discovery for the core group and
// postgresql.cnpg.io/v1 (clusters), 404 for every other group, an empty
// metadata list for every List of clusters, each answer after latency. It
// counts the requests by path and the most it saw in flight.
type countingAPIServer struct {
	latency  time.Duration
	mu       sync.Mutex
	requests map[string]int
	inFlight int
	peak     int
}

func (s *countingAPIServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

func (s *countingAPIServer) countPrefix(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for path, c := range s.requests {
		if strings.HasPrefix(path, prefix) {
			n += c
		}
	}
	return n
}

func (s *countingAPIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.requests == nil {
		s.requests = map[string]int{}
	}
	s.requests[r.URL.Path]++
	s.inFlight++
	s.peak = max(s.peak, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	time.Sleep(s.latency)
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch path := r.URL.Path; {
	case path == "/api":
		write(map[string]any{"kind": "APIVersions", "versions": []string{"v1"}})
	case path == "/apis":
		write(map[string]any{"kind": "APIGroupList", "apiVersion": "v1", "groups": []any{map[string]any{
			"name":             "postgresql.cnpg.io",
			"versions":         []any{map[string]any{"groupVersion": "postgresql.cnpg.io/v1", "version": "v1"}},
			"preferredVersion": map[string]any{"groupVersion": "postgresql.cnpg.io/v1", "version": "v1"},
		}}})
	case path == "/apis/postgresql.cnpg.io/v1":
		write(map[string]any{"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": "postgresql.cnpg.io/v1", "resources": []any{map[string]any{
			"name": "clusters", "singularName": "cluster", "namespaced": true, "kind": "Cluster", "verbs": []string{"get", "list", "watch"},
		}}})
	case strings.HasPrefix(path, "/apis/postgresql.cnpg.io/v1/namespaces/") && strings.HasSuffix(path, "/clusters"):
		write(map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "metadata": map[string]any{}, "items": []any{}})
	default:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": "NotFound", "code": 404})
	}
}

// TestProbeKindsRealClientMissingKindOnce: through a real client, whose lazy
// RESTMapper sends a discovery GET of a missing group on every lookup, one
// after the other under its lock, a kind the cluster does not serve costs
// one lookup, not one per watched namespace (64 namespaces at 20 ms were
// 1.3 s per missing kind, and the EvidenceAgent's step always has one or two:
// the 20 s step deadline fell at a few hundred namespaces). An installed kind
// is still listed in every namespace, concurrently.
func TestProbeKindsRealClientMissingKindOnce(t *testing.T) {
	api := &countingAPIServer{latency: 20 * time.Millisecond}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	// QPS -1, as ctrl.GetConfig sets it: no client-side rate limit.
	c, err := client.New(&rest.Config{Host: server.URL, QPS: -1}, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	namespaces := make([]string, 64)
	for i := range namespaces {
		namespaces[i] = fmt.Sprintf("team-%02d", i)
	}
	clusterGVK := ObjectStoreGVK
	clusterGVK.Group, clusterGVK.Kind = "postgresql.cnpg.io", "Cluster"
	start := time.Now()
	got, err := ProbeKinds(t.Context(), c, []schema.GroupVersionKind{StanzaGVK, ArchiveGVK, clusterGVK}, namespaces)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if got.Results[0].State != NotInstalled || got.Results[1].State != NotInstalled || got.Results[2].State != Available {
		t.Fatalf("results %+v", got.Results)
	}
	for _, group := range []string{"/apis/pgbackrest.dalibo.com/", "/apis/pgbackrest.cnpg.opera.com/"} {
		if n := api.countPrefix(group); n != 1 {
			t.Errorf("%d requests under %s for a kind not served, want 1", n, group)
		}
	}
	if n := api.countPrefix("/apis/postgresql.cnpg.io/v1/namespaces/"); n != len(namespaces) {
		t.Errorf("%d Cluster Lists, want one per namespace (%d)", n, len(namespaces))
	}
	if n := api.count("/apis"); n != 1 {
		t.Errorf("%d GET /apis, want 1", n)
	}
	if api.peak < 2 {
		t.Errorf("peak in flight %d: the installed kind was not listed concurrently", api.peak)
	}
	// 3 discovery requests + 2 missing kinds + about 64/16 rounds of Lists.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("took %s", elapsed)
	}
}
