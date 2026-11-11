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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/fyannk/pgToolBox/internal/operator/configuration"
)

// livenessProbes returns every livenessProbe (and startupProbe) object in
// a YAML document, wherever it is nested.
func livenessProbes(node any, out *[]map[string]any) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if probe, ok := child.(map[string]any); ok && (key == "livenessProbe" || key == "startupProbe") {
				*out = append(*out, probe)
			}
			livenessProbes(child, out)
		}
	case []any:
		for _, child := range value {
			livenessProbes(child, out)
		}
	}
}

// killAfter is how long after the container starts the kubelet kills it
// when the probe never succeeds (the health endpoint is not served during
// the start-up): the first probe after initialDelaySeconds, then one per
// periodSeconds until failureThreshold failures, each up to
// timeoutSeconds (the Kubernetes defaults when unset).
func killAfter(t *testing.T, probe map[string]any) time.Duration {
	t.Helper()
	field := func(name string, fallback float64) float64 {
		if v, ok := probe[name]; ok {
			f, ok := v.(float64)
			if !ok {
				t.Fatalf("probe field %s = %v, not a number", name, v)
			}
			return f
		}
		return fallback
	}
	delay, period, threshold, timeout := field("initialDelaySeconds", 0), field("periodSeconds", 10), field("failureThreshold", 3), field("timeoutSeconds", 1)
	return time.Duration((delay + period*(threshold-1) + timeout) * float64(time.Second))
}

// TestStartupStepBeforeLivenessKill: the whole start-up, from the first
// read to the manager's start (the reads, the manager, every controller's
// setup: one shared budget, buildManager), fails with a named error before
// the liveness probe of any shipped install (Helm values, kustomize, OLM)
// kills the container, with room for the process start before the budget
// and the manager's start after it. A discovery request still running when
// a step's deadline passes does not count: the step returns at the deadline.
func TestStartupStepBeforeLivenessKill(t *testing.T) {
	sources := []string{
		"../../charts/pgtoolbox/values.yaml",
		"../../config/manager/manager.yaml",
		"../../deploy/olm/bundle/manifests/pgtoolbox.clusterserviceversion.yaml",
	}
	for _, source := range sources {
		t.Run(filepath.Base(source), func(t *testing.T) {
			raw, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			var probes []map[string]any
			for _, document := range strings.Split(string(raw), "\n---") {
				var node any
				if err := yaml.Unmarshal([]byte(document), &node); err != nil {
					t.Fatalf("parse %s: %v", source, err)
				}
				livenessProbes(node, &probes)
			}
			if len(probes) == 0 {
				t.Fatalf("no livenessProbe in %s", source)
			}
			for _, probe := range probes {
				if kill := killAfter(t, probe); startupBudgetTotal+startupMargin >= kill {
					t.Errorf("the liveness probe kills a slow start after %s, before the start-up budget of %s (plus %s outside it) can fail with its named error",
						kill, startupBudgetTotal, startupMargin)
				}
				if startupStepTimeout > startupBudgetTotal {
					t.Errorf("a step's deadline %s exceeds the whole budget %s", startupStepTimeout, startupBudgetTotal)
				}
			}
		})
	}
}

// TestStartupBudgetIsShared: an API server slow on every request, but
// answering, keeps every step under its own deadline; the steps together
// still fail the start, naming the step the shared budget ran out in,
// rather than adding up past the liveness kill with no error named.
func TestStartupBudgetIsShared(t *testing.T) {
	t.Setenv("WATCH_NAMESPACES", "team-a")
	t.Setenv("OPERATOR_IMAGE", "registry.example.com/pgtoolbox/operator:test")
	slow := func(ctx context.Context) error {
		select {
		case <-time.After(60 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			if err := slow(ctx); err != nil {
				return err
			}
			return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "pgtoolbox-operator-config")
		},
		List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			return slow(ctx)
		},
	}).Build()
	objects := configuration.Objects{Namespace: "pgtoolbox", ConfigMap: "pgtoolbox-operator-config", Secret: "pgtoolbox-operator-config"}

	// Each step alone fits in its 2 s; together they take about 0.8 s.
	if _, err := preparePgAdminWith(t.Context(), reader, prometheus.NewRegistry(), newStartupBudget(2*time.Second, 10*time.Second), objects); err != nil {
		t.Fatalf("with budget to spare: %v", err)
	}
	start := time.Now()
	_, err := preparePgAdminWith(t.Context(), reader, prometheus.NewRegistry(), newStartupBudget(2*time.Second, 400*time.Millisecond), objects)
	if err == nil || !strings.Contains(err.Error(), `"probe the optional APIs"`) || !strings.Contains(err.Error(), "start-up budget of 400ms is spent") {
		t.Fatalf("error = %v, want the optional-API step named with the spent budget", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("took %s, want about the budget", elapsed)
	}
}

// TestStartupBudgetSpentBeforeAStep: a step that starts with the budget
// spent fails at once, naming itself, without reading.
func TestStartupBudgetSpentBeforeAStep(t *testing.T) {
	budget := newStartupBudget(time.Second, time.Second)
	budget.now = func() time.Time { return budget.deadline.Add(time.Millisecond) }
	read := false
	_, err := budgetedRead(t.Context(), budget, "check the watched namespaces", func(context.Context) (struct{}, error) {
		read = true
		return struct{}{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), `"check the watched namespaces" not started: the start-up budget of 1s is spent`) {
		t.Fatalf("error = %v", err)
	}
	if read {
		t.Fatal("the step read with the budget spent")
	}
}

// TestTimedMapperProviderHungAPIServer: the manager's RESTMapper, which the
// controllers' setups (IndexField) use before the manager starts and whose
// discovery requests take no context, fails a lookup against an API server
// that never answers after its request timeout, instead of hanging the
// start until the liveness probe kills it.
func TestTimedMapperProviderHungAPIServer(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	cfg := &rest.Config{Host: server.URL, QPS: -1}
	shared, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mapper, err := timedMapperProvider(300*time.Millisecond)(cfg, shared)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 0 || shared.Timeout != 0 {
		t.Fatalf("the manager's config or HTTP client got a timeout: %s, %s", cfg.Timeout, shared.Timeout)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := mapper.RESTMapping(schema.GroupKind{Group: "postgresql.cnpg.io", Kind: "Cluster"}, "v1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lookup against a hung API server succeeded")
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("failed after %s, want about the request timeout", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lookup is still running 5 s after its 300 ms request timeout")
	}
	if mapperRequestTimeout <= 0 || mapperRequestTimeout >= startupStepTimeout {
		t.Fatalf("mapperRequestTimeout %s, want positive and under a start-up step", mapperRequestTimeout)
	}
}

// TestStartupBudgetCoversTheSetups: the manager and the controllers' setups
// run under what is left of the start-up budget. An API server that answers
// every discovery request, each well under the mapper's timeout, but slowly
// enough that the setups add up past the budget, fails the start naming the
// step the budget ran out in, at about the budget, instead of running into
// the liveness kill with no error (round 13, operator P3-1). A setup that
// never returns (its requests ignore their context) is cut at the deadline.
func TestStartupBudgetCoversTheSetups(t *testing.T) {
	slow := func(d time.Duration) func(context.Context, ctrl.Manager) ([]client.Object, error) {
		return func(context.Context, ctrl.Manager) ([]client.Object, error) {
			time.Sleep(d) // a discovery request answered late, ignoring its context like the RESTMapper's
			return []client.Object{&corev1.ConfigMap{}}, nil
		}
	}
	newManager := func() (ctrl.Manager, error) { time.Sleep(100 * time.Millisecond); return nil, nil }
	setups := func(d time.Duration) []managerSetup {
		return []managerSetup{{"EvidenceAgent", slow(d)}, {"PgAdmin", slow(d)}, {"PgAccessRequest", slow(d)}, {"PgToolBoxConsole", slow(d)}}
	}

	// With budget to spare: every setup runs, in order.
	_, kinds, err := buildManager(t.Context(), newStartupBudget(2*time.Second, 5*time.Second), newManager, setups(100*time.Millisecond))
	if err != nil || len(kinds) != 4 {
		t.Fatalf("with budget to spare: %d kinds, %v", len(kinds), err)
	}

	// Each setup fits in its own 2 s step; together they need 1.3 s, over
	// the 700 ms budget.
	start := time.Now()
	_, _, err = buildManager(t.Context(), newStartupBudget(2*time.Second, 700*time.Millisecond), newManager, setups(300*time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "start-up budget of 700ms is spent") || !strings.Contains(err.Error(), "controller") {
		t.Fatalf("error = %v, want a setup step named with the spent budget", err)
	}
	if elapsed := time.Since(start); elapsed > 1200*time.Millisecond {
		t.Fatalf("took %s, want about the 700ms budget", elapsed)
	}

	// A setup that never returns is cut at its deadline.
	hung := []managerSetup{{"EvidenceAgent", func(context.Context, ctrl.Manager) ([]client.Object, error) { select {} }}}
	start = time.Now()
	_, _, err = buildManager(t.Context(), newStartupBudget(300*time.Millisecond, time.Minute), newManager, hung)
	if err == nil || !strings.Contains(err.Error(), `"set up the EvidenceAgent controller" did not finish within 300ms`) {
		t.Fatalf("error = %v, want the hung setup named", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want about the 300ms step", elapsed)
	}

	// The budget spent before the manager: nothing is created.
	spent := newStartupBudget(time.Second, time.Second)
	spent.now = func() time.Time { return spent.deadline.Add(time.Millisecond) }
	created := false
	_, _, err = buildManager(t.Context(), spent, func() (ctrl.Manager, error) { created = true; return nil, nil }, nil)
	if err == nil || !strings.Contains(err.Error(), `"create the manager" not started`) || created {
		t.Fatalf("error = %v, created %t; want the manager step refused unrun", err, created)
	}
}
