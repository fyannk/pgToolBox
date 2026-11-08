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
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The committed openapi.yaml and fixtures are the generator's output:
// the API golden files and the drift check in one (make openapi).
func TestGeneratedArtifactsUpToDate(t *testing.T) {
	t.Parallel()
	files, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s is stale: run make openapi", name)
		}
	}
	entries, err := os.ReadDir("testdata/fixtures")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := files["testdata/fixtures/"+e.Name()]; !ok {
			t.Errorf("testdata/fixtures/%s is not generated any more: delete it", e.Name())
		}
	}
}

// spySource records whether the read model was consulted.
type spySource struct {
	FixtureSource
	mu    sync.Mutex
	calls []string
}

func (s *spySource) record(what string) {
	s.mu.Lock()
	s.calls = append(s.calls, what)
	s.mu.Unlock()
}

func (s *spySource) ClusterOverview(ctx context.Context, ns, name string) (viewmodel.ClusterOverview, error) {
	s.record("overview " + ns + "/" + name)
	return s.FixtureSource.ClusterOverview(ctx, ns, name)
}

func (s *spySource) Instances(ctx context.Context, ns, name string) (viewmodel.InstanceList, error) {
	s.record("instances " + ns + "/" + name)
	return s.FixtureSource.Instances(ctx, ns, name)
}

func call(t *testing.T, h http.Handler, p *auth.Principal, path string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	if p != nil {
		ctx = auth.WithPrincipal(ctx, p)
	}
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthorizationComesBeforeTheReadModel(t *testing.T) {
	t.Parallel()
	billing := authz.ObjectRef{Namespace: "team-a", Name: "billing"}
	ghost := authz.ObjectRef{Namespace: "team-a", Name: "ghost"}
	cases := []struct {
		name   string
		az     FixtureAuthorizer
		path   string
		status int
		reads  int
	}{
		{"allowed", FixtureAuthorizer{}, "/api/v1/namespaces/team-a/clusters/billing/instances", http.StatusOK, 1},
		{"denied", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{billing: true}}, "/api/v1/namespaces/team-a/clusters/billing/instances", http.StatusForbidden, 0},
		{"undecided", FixtureAuthorizer{Unchecked: map[authz.ObjectRef]bool{billing: true}}, "/api/v1/namespaces/team-a/clusters/billing", http.StatusServiceUnavailable, 0},
		// A name the user may not get is 403 whether or not it exists:
		// the API is no oracle for names.
		{"denied and absent", FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{ghost: true}}, "/api/v1/namespaces/team-a/clusters/ghost", http.StatusForbidden, 0},
		{"allowed and absent", FixtureAuthorizer{}, "/api/v1/namespaces/team-a/clusters/ghost", http.StatusNotFound, 1},
		{"invalid namespace", FixtureAuthorizer{}, "/api/v1/namespaces/Team_A/clusters/billing", http.StatusUnprocessableEntity, 0},
		{"overlong name", FixtureAuthorizer{}, "/api/v1/namespaces/team-a/clusters/" + strings.Repeat("a", 254), http.StatusUnprocessableEntity, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := &spySource{}
			h, _ := New(Deps{Source: src, Authz: tc.az})
			w := call(t, h, FixturePrincipal(), tc.path)
			if w.Code != tc.status {
				t.Fatalf("GET %s = %d %s, want %d", tc.path, w.Code, w.Body, tc.status)
			}
			if len(src.calls) != tc.reads {
				t.Fatalf("read model consulted %d times (%v), want %d", len(src.calls), src.calls, tc.reads)
			}
			if tc.status >= 400 && !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("error content type = %q", w.Header().Get("Content-Type"))
			}
		})
	}
}

func TestWithoutPrincipal(t *testing.T) {
	t.Parallel()
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	for _, p := range []string{"/api/v1/session", "/api/v1/clusters", "/api/v1/namespaces/team-a/clusters/orders"} {
		if w := call(t, h, nil, p); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a principal = %d", p, w.Code)
		}
	}
}

func TestClusterListDropsInvisibleRows(t *testing.T) {
	t.Parallel()
	for name, want := range map[string][]string{
		"testdata/fixtures/listClusters.json":         {"billing", "orders"},
		"testdata/fixtures/listClusters.denied.json":  {"orders"},
		"testdata/fixtures/listClusters.partial.json": {"orders"},
	} {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		got := regexp.MustCompile(`"ref": \{\s*"namespace": "team-a",\s*"name": "([a-z]+)"`).FindAllSubmatch(body, -1)
		var names []string
		for _, m := range got {
			names = append(names, string(m[1]))
		}
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("%s rows = %v, want %v", name, names, want)
		}
	}
	partial, _ := os.ReadFile("testdata/fixtures/listClusters.partial.json")
	if !bytes.Contains(partial, []byte(`"unchecked"`)) {
		t.Error("the partial list does not name the unchecked cluster")
	}
}

func TestOpenAPIHasNoThirdPartyDocsAndNoSchemaLinks(t *testing.T) {
	t.Parallel()
	h, _ := New(Deps{Source: FixtureSource{}, Authz: FixtureAuthorizer{}})
	if w := call(t, h, FixturePrincipal(), "/docs"); w.Code != http.StatusNotFound {
		t.Errorf("/docs = %d: the docs page loads scripts from a CDN", w.Code)
	}
	if w := call(t, h, FixturePrincipal(), "/api/v1/openapi.json"); w.Code != http.StatusOK {
		t.Errorf("/api/v1/openapi.json = %d", w.Code)
	}
	body := call(t, h, FixturePrincipal(), "/api/v1/clusters").Body.String()
	if strings.Contains(body, "$schema") {
		t.Error("responses carry $schema links")
	}
}

// The stream over a real connection: retry hint and hello first,
// heartbeat comments, invalidations filtered by the user's access,
// reauth before the session ends, closed at its end.
func TestStreamOverHTTP(t *testing.T) {
	t.Parallel()
	broker := sse.NewBroker()
	h, _ := New(Deps{
		Source: FixtureSource{}, Broker: broker, Heartbeat: 50 * time.Millisecond,
		Authz: FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "billing"}: true}},
	})
	p := FixturePrincipal()
	// Inside the reauth lead from the start: reauth at once, end in 1 s.
	p.Expiry = time.Now().Add(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	u, _ := url.JoinPath(srv.URL, "/api/v1/stream")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	go func() {
		for broker.Clients() == 0 {
			time.Sleep(time.Millisecond)
		}
		broker.Publish(sse.Invalidate{Topic: sse.TopicCluster, Namespace: "team-a", Name: "billing"})
		broker.Publish(sse.Invalidate{Topic: sse.TopicCluster, Namespace: "team-a", Name: "orders"})
	}()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	all := strings.Join(lines, "\n")
	if !strings.HasPrefix(all, "retry: 2000\nevent: hello\n") {
		t.Fatalf("stream does not start with the retry hint and hello:\n%s", all)
	}
	for _, want := range []string{": heartbeat", `"name":"orders"`, "event: reauth", `"refreshable":true`} {
		if !strings.Contains(all, want) {
			t.Errorf("stream lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, `"name":"billing"`) {
		t.Errorf("an invalidation for a cluster the user may not get was sent:\n%s", all)
	}
	deadline := time.Now().Add(2 * time.Second)
	for broker.Clients() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if broker.Clients() != 0 {
		t.Error("the subscription outlived the stream")
	}
}

// failingSource fails every overview read with an error quoting internals.
type failingSource struct {
	FixtureSource
	err error
}

func (s failingSource) ClusterOverview(context.Context, string, string) (viewmodel.ClusterOverview, error) {
	return viewmodel.ClusterOverview{}, s.err
}

// TestReadModelFailureNamesAReferenceNotTheCause proves the 503 of a
// failed read carries a fixed message and a reference, never the error's
// text, and that the console log has the text under the same reference.
func TestReadModelFailureNamesAReferenceNotTheCause(t *testing.T) {
	t.Parallel()
	const canary = "pods is forbidden: User \"system:serviceaccount:team-a:pgtoolbox-console\" canary-7f3a"
	var logs bytes.Buffer
	h, _ := New(Deps{
		Source: failingSource{err: errors.New(canary)}, Authz: FixtureAuthorizer{},
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	w := call(t, h, FixturePrincipal(), "/api/v1/namespaces/team-a/clusters/orders")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, "canary-7f3a") || strings.Contains(body, "forbidden") {
		t.Fatalf("the response quotes the cause: %s", body)
	}
	ref := regexp.MustCompile(`reference ([0-9a-f]{12})`).FindStringSubmatch(body)
	if ref == nil {
		t.Fatalf("no reference in %s", body)
	}
	if !strings.Contains(logs.String(), "reference="+ref[1]) || !strings.Contains(logs.String(), "canary-7f3a") {
		t.Fatalf("the log does not hold the cause under reference %s: %s", ref[1], logs.String())
	}
}
