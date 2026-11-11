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

package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// refusingReader answers every List as the API server answers an operator
// with no RoleBinding in a namespace: Forbidden, whatever the kind.
func refusingReader(refused ...string) client.Reader {
	return fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, opts ...client.ListOption) error {
			o := &client.ListOptions{}
			o.ApplyOptions(opts)
			if slices.Contains(refused, o.Namespace) {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "any"}, "", errors.New("no RoleBinding"))
			}
			return nil
		},
	}).Build()
}

// TestReachableNamespacesAtStart: a watched namespace deleted after the
// install (its RoleBinding gone with it) neither fails the start nor turns
// a feature off in the namespaces that remain; only a watch with no
// namespace left fails.
func TestReachableNamespacesAtStart(t *testing.T) {
	ctx := context.Background()

	got, err := reachableNamespaces(ctx, refusingReader("gone"), []string{"team-a", "gone", "team-b"})
	if err != nil {
		t.Fatalf("one deleted namespace fails the start: %v", err)
	}
	if !slices.Equal(got, []string{"team-a", "team-b"}) {
		t.Fatalf("watched = %q, want [team-a team-b]", got)
	}
	features, err := probeFeatures(ctx, refusingReader("gone"), got)
	if err != nil {
		t.Fatalf("probeFeatures on what is left: %v", err)
	}
	for name, r := range map[string]discovery.Result{
		"routes": features.Routes, "gateways": features.Gateways, "databaseRoles": features.DatabaseRoles,
		"poolers": features.Poolers, "databases": features.Databases,
	} {
		if !r.Usable() {
			t.Errorf("%s turned off by a namespace no longer watched: %+v", name, r)
		}
	}

	if _, err := reachableNamespaces(ctx, refusingReader("a", "b"), []string{"a", "b"}); err == nil {
		t.Fatal("no namespace answering must fail the start")
	}
	if got, err := reachableNamespaces(ctx, refusingReader(), nil); err != nil || got != nil {
		t.Fatalf("every namespace (no list) = %q, %v", got, err)
	}
}
