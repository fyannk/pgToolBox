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

package mcpleader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fyannk/pgToolBox/internal/console/auth"
)

func TestPathsMatchByElement(t *testing.T) {
	for _, tc := range []struct {
		path  string
		gated bool
	}{
		{"/mcp", true},
		{"/mcp/confirm", true},
		{"/mcpx", false},
		{"/oauth/token", true},
		{"/oauth/authorize", true},
		{"/oauth/register", true},
		{"/.well-known/oauth-authorization-server", false},
		{"/api/v1/session/grants", true},
		{"/api/v1/session/grants/grant-1", true},
		{"/api/v1/session", false},
		{"/api/v1/assistant-setup", false},
		{"/auth/oidc/callback", false},
		{"/logout", false}, // routed to the leader, not gated
		{"/", false},
	} {
		if got := gated(tc.path); got != tc.gated {
			t.Errorf("gated(%q) = %v, want %v", tc.path, got, tc.gated)
		}
	}
	routed := 0
	for _, p := range Paths {
		if p.Match("/logout") {
			routed++
		}
	}
	if routed != 1 {
		t.Fatalf("/logout matched by %d paths, want 1", routed)
	}
}

func TestGate(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	for _, tc := range []struct {
		name, method, path string
		leading            bool
		status             int
		contentType        string
	}{
		{"leader serves mcp", http.MethodPost, "/mcp", true, http.StatusTeapot, ""},
		{"leader serves token", http.MethodPost, "/oauth/token", true, http.StatusTeapot, ""},
		{"follower refuses mcp", http.MethodPost, "/mcp", false, http.StatusServiceUnavailable, "text/plain; charset=utf-8"},
		{"follower refuses token as OAuth", http.MethodPost, "/oauth/token", false, http.StatusServiceUnavailable, "application/json"},
		{"follower refuses grants as a problem", http.MethodDelete, "/api/v1/session/grants/grant-1", false, http.StatusServiceUnavailable, "application/problem+json"},
		{"follower refuses consent", http.MethodGet, "/oauth/authorize", false, http.StatusServiceUnavailable, "text/plain; charset=utf-8"},
		{"follower passes logout", http.MethodPost, "/logout", false, http.StatusTeapot, ""},
		{"follower passes the rest", http.MethodGet, "/api/v1/clusters", false, http.StatusTeapot, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Leader{}
			l.serving.Store(tc.leading)
			rec := httptest.NewRecorder()
			l.Gate(ok).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.status != http.StatusServiceUnavailable {
				return
			}
			if rec.Header().Get("Retry-After") != RetryAfter || rec.Header().Get("Content-Type") != tc.contentType {
				t.Fatalf("headers = %v", rec.Header())
			}
			switch tc.contentType {
			case "application/json":
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] != "temporarily_unavailable" {
					t.Fatalf("body = %s", rec.Body)
				}
			case "application/problem+json":
				var p auth.Problem
				if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || p.Status != http.StatusServiceUnavailable {
					t.Fatalf("body = %s", rec.Body)
				}
			}
		})
	}
}

func TestHookRefusesOffTheLeader(t *testing.T) {
	called := 0
	hook := func(context.Context, auth.Client, string) error { called++; return nil }
	l := &Leader{}
	if err := l.Hook(hook)(t.Context(), auth.Client{}, "alice"); !errors.Is(err, ErrNotLeader) || called != 0 {
		t.Fatalf("off the leader: err = %v, called = %d", err, called)
	}
	l.serving.Store(true)
	if err := l.Hook(hook)(t.Context(), auth.Client{}, "alice"); err != nil || called != 1 {
		t.Fatalf("on the leader: err = %v, called = %d", err, called)
	}
}

// sliceAPI is EndpointSlices with the API server's optimistic concurrency.
type sliceAPI struct {
	mu        sync.Mutex
	slices    map[string]*discoveryv1.EndpointSlice
	rv        int
	updates   int
	conflicts int
}

var endpointSlices = schema.GroupResource{Group: "discovery.k8s.io", Resource: "endpointslices"}

func newSliceAPI(names ...string) *sliceAPI {
	a := &sliceAPI{slices: map[string]*discoveryv1.EndpointSlice{}}
	for _, name := range names {
		family := discoveryv1.AddressTypeIPv4
		if name == "console-mcp-ipv6" {
			family = discoveryv1.AddressTypeIPv6
		}
		a.rv++
		a.slices[name] = &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "team-a", ResourceVersion: strconv.Itoa(a.rv),
				Labels: map[string]string{discoveryv1.LabelServiceName: "console-mcp", discoveryv1.LabelManagedBy: ManagedBy},
			},
			AddressType: family,
		}
	}
	return a
}

func (a *sliceAPI) Get(_ context.Context, name string, _ metav1.GetOptions) (*discoveryv1.EndpointSlice, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.slices[name]
	if !ok {
		return nil, apierrors.NewNotFound(endpointSlices, name)
	}
	return s.DeepCopy(), nil
}

func (a *sliceAPI) Update(_ context.Context, s *discoveryv1.EndpointSlice, _ metav1.UpdateOptions) (*discoveryv1.EndpointSlice, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, ok := a.slices[s.Name]
	switch {
	case !ok:
		return nil, apierrors.NewNotFound(endpointSlices, s.Name)
	case a.conflicts > 0 || s.ResourceVersion != cur.ResourceVersion:
		if a.conflicts > 0 {
			a.conflicts--
			a.rv++
			cur.ResourceVersion = strconv.Itoa(a.rv)
		}
		return nil, apierrors.NewConflict(endpointSlices, s.Name, errors.New("stale"))
	}
	a.rv++
	a.updates++
	s = s.DeepCopy()
	s.ResourceVersion = strconv.Itoa(a.rv)
	a.slices[s.Name] = s
	return s.DeepCopy(), nil
}

func (a *sliceAPI) addresses(name string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.slices[name].Endpoints {
		out = append(out, e.Addresses...)
	}
	return out
}

func testPublisher(api *sliceAPI, ip string) *publisher {
	return newPublisher(Options{
		Slices: api, Lease: "console-mcp", SliceNames: []string{"console-mcp-ipv4", "console-mcp-ipv6"}, PodIP: ip, Port: 3000,
	}.withDefaults())
}

func TestPublisherWritesItsFamilyPinsLabelsAndIsIdempotent(t *testing.T) {
	api := newSliceAPI("console-mcp-ipv4", "console-mcp-ipv6")
	// Someone relabelled the slice and pointed it elsewhere.
	api.slices["console-mcp-ipv4"].Labels[discoveryv1.LabelServiceName] = "kube-dns"
	api.slices["console-mcp-ipv4"].Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.99"}}}
	api.conflicts = 1 // a hand-over racing the first write
	p := testPublisher(api, "10.0.0.1")
	p.publish(t.Context())
	s := api.slices["console-mcp-ipv4"]
	if got := api.addresses("console-mcp-ipv4"); len(got) != 1 || got[0] != "10.0.0.1" {
		t.Fatalf("ipv4 addresses = %v", got)
	}
	if s.Labels[discoveryv1.LabelServiceName] != "console-mcp" || s.Labels[discoveryv1.LabelManagedBy] != ManagedBy ||
		len(s.Ports) != 1 || *s.Ports[0].Port != 3000 || *s.Ports[0].Name != "http" {
		t.Fatalf("slice not pinned: %+v", s)
	}
	if got := api.addresses("console-mcp-ipv6"); len(got) != 0 {
		t.Fatalf("the other family's slice was written: %v", got)
	}
	updates := api.updates
	p.publish(t.Context())
	if api.updates != updates {
		t.Fatal("a slice already published was written again")
	}
	if p.failing {
		t.Fatal("a conflict retried was taken for a failure")
	}
}

func TestPublisherClearsOnlyItsOwnAddress(t *testing.T) {
	api := newSliceAPI("console-mcp-ipv4")
	a := testPublisher(api, "10.0.0.1")
	a.publish(t.Context())
	b := testPublisher(api, "10.0.0.2")
	b.publish(t.Context()) // the next leader took the slice over
	a.clear(t.Context())   // the old one leaves late
	if got := api.addresses("console-mcp-ipv4"); len(got) != 1 || got[0] != "10.0.0.2" {
		t.Fatalf("addresses = %v, want the new leader's", got)
	}
	b.clear(t.Context())
	if got := api.addresses("console-mcp-ipv4"); len(got) != 0 {
		t.Fatalf("addresses = %v after the leader cleared", got)
	}
}

func TestPublisherIPv6(t *testing.T) {
	api := newSliceAPI("console-mcp-ipv4", "console-mcp-ipv6")
	testPublisher(api, "fd00::1").publish(t.Context())
	if got := api.addresses("console-mcp-ipv6"); len(got) != 1 || got[0] != "fd00::1" {
		t.Fatalf("ipv6 addresses = %v", got)
	}
	if got := api.addresses("console-mcp-ipv4"); len(got) != 0 {
		t.Fatalf("ipv4 addresses = %v", got)
	}
}
