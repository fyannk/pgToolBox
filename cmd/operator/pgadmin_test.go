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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fyannk/pgToolBox/internal/operator/configuration"
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

// TestPrepareInfoMetricListsWatchedNamespaces: pgtoolbox_config_info is
// registered once the namespaces the operator cannot reach are left out, so
// that WATCH_NAMESPACES names only the namespaces actually watched.
func TestPrepareInfoMetricListsWatchedNamespaces(t *testing.T) {
	t.Setenv("WATCH_NAMESPACES", "team-a,gone")
	t.Setenv("OPERATOR_IMAGE", "registry.example.com/pgtoolbox/operator:test")
	registry := prometheus.NewRegistry()
	prepared, err := preparePgAdminWith(t.Context(), refusingReader("gone"), registry, time.Minute,
		configuration.Objects{Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prepared.Config.WatchNamespaces, []string{"team-a"}) {
		t.Fatalf("watched = %q, want [team-a]", prepared.Config.WatchNamespaces)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var value string
	for _, family := range families {
		if family.GetName() != "pgtoolbox_config_info" {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["name"] == "WATCH_NAMESPACES" {
				value = labels["value"]
			}
		}
	}
	if value != "team-a" {
		t.Fatalf("pgtoolbox_config_info{name=WATCH_NAMESPACES} value = %q, want team-a", value)
	}
}

// TestPrepareStartupDeadline: an API server that never answers fails the
// start within the step's deadline, naming the step.
func TestPrepareStartupDeadline(t *testing.T) {
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	tests := []struct {
		name, watch, step string
		funcs             interceptor.Funcs
	}{
		{
			name: "the configuration read hangs", step: `"load the configuration"`,
			funcs: interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return hang(ctx)
			}},
		},
		{
			name: "the namespace check hangs", watch: "team-a", step: `"check the watched namespaces"`,
			funcs: interceptor.Funcs{List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return hang(ctx)
			}},
		},
		{
			name: "a feature probe hangs", step: `"probe the optional APIs"`,
			funcs: interceptor.Funcs{List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return hang(ctx)
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WATCH_NAMESPACES", tt.watch)
			t.Setenv("OPERATOR_IMAGE", "registry.example.com/pgtoolbox/operator:test")
			reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(tt.funcs).Build()
			start := time.Now()
			_, err := preparePgAdminWith(t.Context(), reader, prometheus.NewRegistry(), 50*time.Millisecond,
				configuration.Objects{Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config"})
			if err == nil || !strings.Contains(err.Error(), tt.step) || !strings.Contains(err.Error(), "did not finish within") {
				t.Fatalf("error = %v, want the step %s named with its deadline", err, tt.step)
			}
			if elapsed := time.Since(start); elapsed > 10*time.Second {
				t.Fatalf("took %s", elapsed)
			}
		})
	}
}
