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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
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

// TestStartupStepBeforeLivenessKill: a hung start-up step fails the start
// with its named error before the liveness probe of any shipped install
// (Helm values, kustomize, OLM) kills the container, with room for the
// steps before it.
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
				if kill := killAfter(t, probe); 2*startupStepTimeout >= kill {
					t.Errorf("the liveness probe kills a hung start after %s, before two start-up steps of %s can fail with their named error",
						kill, startupStepTimeout)
				}
			}
		})
	}
}

// TestEvidenceAgentProbesStartupDeadline: an API server that never answers
// the EvidenceAgent's optional-API probes fails the start within the
// step's deadline, naming the step.
func TestEvidenceAgentProbesStartupDeadline(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}).Build()
	start := time.Now()
	_, _, _, err := probeEvidenceAgentAPIs(t.Context(), reader, []string{"team-a"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), `"probe the EvidenceAgent's optional APIs" did not finish within`) {
		t.Fatalf("error = %v, want the step named with its deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s", elapsed)
	}
}
