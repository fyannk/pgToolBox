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

package cache

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// scriptedLister answers a probe List per namespace from errs.
type scriptedLister struct{ errs map[string]error }

func (l scriptedLister) List(_ context.Context, _ client.ObjectList, opts ...client.ListOption) error {
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	return l.errs[lo.Namespace]
}

// A re-probe round that an error aborts (a 401 on a later namespace)
// changes no source state: the cache that keeps serving is not described
// as Forbidden in an earlier namespace for an interval (round-11 review).
func TestAnAbortedReprobeLeavesEverySourceState(t *testing.T) {
	m := &Manager{
		cfg:     config.Console{WatchNamespaces: []string{"team-a", "team-b"}, APIRequestTimeout: time.Second},
		kinds:   []Kind{{Source: SourcePods, GVK: schema.GroupVersionKind{Version: "v1", Kind: "Pod"}}},
		tracker: newTracker(time.Now),
	}
	m.prober = scriptedLister{}
	if _, err := m.probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.tracker.set("team-a", SourcePods, viewmodel.SourceLive, "")
	m.tracker.set("team-b", SourcePods, viewmodel.SourceLive, "")
	m.prober = scriptedLister{errs: map[string]error{
		"team-a": apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil),
		"team-b": apierrors.NewUnauthorized("token expired"),
	}}
	if _, err := m.probe(context.Background()); err == nil {
		t.Fatal("a 401 did not abort the round")
	}
	for _, ns := range []string{"team-a", "team-b"} {
		if got, _ := m.tracker.get(ns, SourcePods); got.State != viewmodel.SourceLive {
			t.Errorf("%s: %s after an aborted round, want live", ns, got.State)
		}
	}
	// A round that completes applies its outcomes.
	m.prober = scriptedLister{errs: map[string]error{"team-a": apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", nil)}}
	if _, err := m.probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.tracker.get("team-a", SourcePods); got.State != viewmodel.SourceForbidden {
		t.Errorf("team-a: %s after a complete round, want forbidden", got.State)
	}
	if got, _ := m.tracker.get("team-b", SourcePods); got.State != viewmodel.SourceLive {
		t.Errorf("team-b: %s, want live kept", got.State)
	}
}
