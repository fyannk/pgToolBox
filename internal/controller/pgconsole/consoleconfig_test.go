/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package pgconsole

import (
	"reflect"
	"testing"
	"time"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// envNames lists the rendered variables in order, so a test can assert the
// order the deterministic-rendering contract depends on.
func envNames(console *pgtoolboxv1alpha1.PgConsole) []string {
	env := consoleEnv(console)
	names := make([]string, 0, len(env))
	for _, entry := range env {
		names = append(names, entry.Name)
	}
	return names
}

func envMap(console *pgtoolboxv1alpha1.PgConsole) map[string]string {
	values := map[string]string{}
	for _, entry := range consoleEnv(console) {
		values[entry.Name] = entry.Value
	}
	return values
}

// TestConsoleEnvStatesEveryCapability holds the rule that capabilities are
// rendered rather than inherited: a reader of the Pod sees what is switched
// on without knowing the application's defaults.
func TestConsoleEnvStatesEveryCapability(t *testing.T) {
	got := envMap(testConsole())

	want := map[string]string{
		"CLUSTER_NAME":           "cluster-1",
		"NAMESPACE":              "test",
		"TRUSTED_USER_HEADER":    "X-Forwarded-User",
		"TRUSTED_LEVEL_HEADER":   "X-PgToolBox-Level",
		"ALLOW_OPERATIONS":       "true",
		"ALLOW_LOGS":             "true",
		"ALLOW_ACCESS_REVIEW":    "true",
		"ALLOW_CLUSTER_CATALOGS": "false",
		"ALLOW_INSECURE_LINKS":   "false",
		"HISTORY_ENABLED":        "true",
		"METRICS_ENABLED":        "true",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("env %s = %q, want %q", name, got[name], value)
		}
	}
}

// TestConsoleEnvLevelHeaderMatchesTheProxy is the alignment this whole
// contract rests on: the console reads the header the proxy sets. If either
// side renames it, every screen above the denial page becomes unreachable.
func TestConsoleEnvLevelHeaderMatchesTheProxy(t *testing.T) {
	if consoleTrustedLevelHeader != "X-PgToolBox-Level" {
		t.Fatalf("level header = %q; the proxy sets X-PgToolBox-Level", consoleTrustedLevelHeader)
	}
	if consoleTrustedUserHeader != "X-Forwarded-User" {
		t.Fatalf("user header = %q; the proxy sets X-Forwarded-User", consoleTrustedUserHeader)
	}
}

// TestConsoleEnvOmitsUnsetTunables holds the second rule: the operator does
// not restate the application's numeric defaults, so the two cannot drift.
func TestConsoleEnvOmitsUnsetTunables(t *testing.T) {
	got := envMap(testConsole())

	for _, name := range []string{
		"LOG_TAIL_LINES", "LOG_TAIL_MAX_BYTES", "EVENTS_MAX_AGE", "API_REQUEST_TIMEOUT",
		"HISTORY_MAX_REVISIONS", "HISTORY_MAX_BYTES", "HISTORY_PER_OBJECT_REVISIONS",
		"HISTORY_COALESCE_WINDOW", "METRICS_INTERVAL", "METRICS_RETENTION", "MONITORING_URL",
	} {
		if _, present := got[name]; present {
			t.Fatalf("env %s rendered while unset in the spec", name)
		}
	}
}

func TestConsoleEnvRendersSetTunables(t *testing.T) {
	console := testConsole()
	console.Spec.Console = pgtoolboxv1alpha1.ConsoleSpec{
		MonitoringURL:     "https://grafana.example.com/d/pg",
		APIRequestTimeout: &metav1.Duration{Duration: 15 * time.Second},
		EventsMaxAge:      &metav1.Duration{Duration: 2 * time.Hour},
		LogTail: pgtoolboxv1alpha1.ConsoleLogTailSpec{
			Lines:    ptrTo(int32(500)),
			MaxBytes: ptrTo(int32(65536)),
		},
		Metrics: pgtoolboxv1alpha1.ConsoleMetricsSpec{
			Interval:  &metav1.Duration{Duration: 30 * time.Second},
			Retention: &metav1.Duration{Duration: 48 * time.Hour},
		},
		History: pgtoolboxv1alpha1.ConsoleHistorySpec{
			MaxRevisions:       ptrTo(int32(500)),
			MaxBytes:           ptrTo(int32(2097152)),
			PerObjectRevisions: ptrTo(int32(10)),
			CoalesceWindow:     &metav1.Duration{Duration: 30 * time.Second},
		},
	}

	got := envMap(console)
	want := map[string]string{
		"LOG_TAIL_LINES":               "500",
		"LOG_TAIL_MAX_BYTES":           "65536",
		"EVENTS_MAX_AGE":               "2h0m0s",
		"API_REQUEST_TIMEOUT":          "15s",
		"HISTORY_MAX_REVISIONS":        "500",
		"HISTORY_MAX_BYTES":            "2097152",
		"HISTORY_PER_OBJECT_REVISIONS": "10",
		"HISTORY_COALESCE_WINDOW":      "30s",
		"METRICS_INTERVAL":             "30s",
		"METRICS_RETENTION":            "48h0m0s",
		"MONITORING_URL":               "https://grafana.example.com/d/pg",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("env %s = %q, want %q", name, got[name], value)
		}
	}
}

// TestConsoleEnvSwitchesCapabilitiesOff proves the flags reach the
// application; rbac_test proves the matching authority leaves with them.
func TestConsoleEnvSwitchesCapabilitiesOff(t *testing.T) {
	console := testConsole()
	console.Spec.Console = pgtoolboxv1alpha1.ConsoleSpec{
		AllowOperations:   ptrTo(false),
		AllowLogs:         ptrTo(false),
		AllowAccessReview: ptrTo(false),
		Metrics:           pgtoolboxv1alpha1.ConsoleMetricsSpec{Enabled: ptrTo(false)},
		History:           pgtoolboxv1alpha1.ConsoleHistorySpec{Enabled: ptrTo(false)},
	}

	got := envMap(console)
	for name, want := range map[string]string{
		"ALLOW_OPERATIONS":    "false",
		"ALLOW_LOGS":          "false",
		"ALLOW_ACCESS_REVIEW": "false",
		"METRICS_ENABLED":     "false",
		"HISTORY_ENABLED":     "false",
	} {
		if got[name] != want {
			t.Fatalf("env %s = %q, want %q", name, got[name], want)
		}
	}
}

// TestConsoleEnvIsDeterministic guards the no-op-reconcile-writes-nothing
// rule: the same spec must render the same slice, in the same order.
func TestConsoleEnvIsDeterministic(t *testing.T) {
	console := testConsole()
	console.Spec.Console.EventsMaxAge = &metav1.Duration{Duration: time.Hour}
	console.Spec.Console.Metrics.Interval = &metav1.Duration{Duration: 10 * time.Second}

	first, second := consoleEnv(console), consoleEnv(console)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("consoleEnv is not deterministic:\n%+v\n%+v", first, second)
	}

	names := envNames(console)
	if names[0] != "CLUSTER_NAME" || names[3] != "TRUSTED_LEVEL_HEADER" {
		t.Fatalf("env order changed: %v", names)
	}
}

// TestConsolePgAdminURL: the link is root-relative, so it is correct on
// every origin the console can be reached on — an ingress hostname, a
// Route, or a port-forward. It used to be built from the console's base
// URL, which without an exposure hostname falls back to the proxy's own
// in-Pod loopback address; that rendered a link to localhost:8080 that
// resolved to nothing.
func TestConsolePgAdminURL(t *testing.T) {
	exposed := testConsole()
	exposed.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type:     pgtoolboxv1alpha1.ExposureTypeIngress,
		Hostname: "pgconsole.apps.example.com",
	}
	if got := consolePgAdminURL(exposed); got != "/pgadmin" {
		t.Fatalf("exposed pgAdmin URL = %q, want the same-origin path", got)
	}

	// The case that used to render nothing, and before that rendered a
	// broken absolute URL: a clusterIP console reached by port-forward.
	unexposed := testConsole()
	if got := consolePgAdminURL(unexposed); got != "/pgadmin" {
		t.Fatalf("unexposed pgAdmin URL = %q, want the same-origin path", got)
	}
	if got := envMap(unexposed)["PGADMIN_URL"]; got != "/pgadmin" {
		t.Fatalf("PGADMIN_URL = %q", got)
	}

	// Nothing to link to when pgAdmin is not composed.
	noPgAdmin := testConsole()
	noPgAdmin.Spec.PgAdmin.Enabled = ptrTo(false)
	if got := consolePgAdminURL(noPgAdmin); got != "" {
		t.Fatalf("pgAdmin URL = %q with pgAdmin disabled", got)
	}
	if _, present := envMap(noPgAdmin)["PGADMIN_URL"]; present {
		t.Fatalf("PGADMIN_URL rendered with pgAdmin disabled")
	}
}

// TestValidateConsoleSpec checks each bound the application enforces, so a
// rejected value is reported on the resource instead of crash-looping a Pod.
func TestValidateConsoleSpec(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*pgtoolboxv1alpha1.ConsoleSpec)
		wantErr bool
	}{
		{name: "defaults", mutate: func(*pgtoolboxv1alpha1.ConsoleSpec) {}},
		{
			name: "events max age at the floor",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.EventsMaxAge = &metav1.Duration{Duration: time.Minute}
			},
		},
		{
			name: "events max age below the floor",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.EventsMaxAge = &metav1.Duration{Duration: 30 * time.Second}
			},
			wantErr: true,
		},
		{
			name: "events max age above the ceiling",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.EventsMaxAge = &metav1.Duration{Duration: 48 * time.Hour}
			},
			wantErr: true,
		},
		{
			name: "api request timeout above the ceiling",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.APIRequestTimeout = &metav1.Duration{Duration: 2 * time.Minute}
			},
			wantErr: true,
		},
		{
			name: "metrics interval below the floor",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.Metrics.Interval = &metav1.Duration{Duration: time.Second}
			},
			wantErr: true,
		},
		{
			name: "metrics retention above the ceiling",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.Metrics.Retention = &metav1.Duration{Duration: 60 * 24 * time.Hour}
			},
			wantErr: true,
		},
		{
			name: "history coalesce window above the ceiling",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.History.CoalesceWindow = &metav1.Duration{Duration: 2 * time.Hour}
			},
			wantErr: true,
		},
		{
			name: "http monitoring URL without the opt-in",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.MonitoringURL = "http://grafana.internal/d/pg"
			},
			wantErr: true,
		},
		{
			name: "http monitoring URL with the opt-in",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.MonitoringURL = "http://grafana.internal/d/pg"
				s.AllowInsecureLinks = ptrTo(true)
			},
		},
		{
			name: "https monitoring URL",
			mutate: func(s *pgtoolboxv1alpha1.ConsoleSpec) {
				s.MonitoringURL = "https://grafana.example.com/d/pg"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			console := testConsole()
			test.mutate(&console.Spec.Console)
			err := validateConsoleSpec(console)
			if test.wantErr && err == nil {
				t.Fatalf("expected a validation error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
