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

package exposure

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

func TestHostnameAllowed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hostname string
		suffixes []string
		want     bool
	}{
		{"no allow-list allows anything", "pgadmin.attacker.example", nil, true},
		{"no hostname publishes nothing", "", []string{"apps.example.com"}, true},
		{"a name below a suffix", "pgadmin.team-a.apps.example.com", []string{"apps.example.com"}, true},
		{"the suffix itself", "apps.example.com", []string{"apps.example.com"}, true},
		{"a leading dot on the suffix", "console.apps.example.com", []string{".apps.example.com"}, true},
		{"case and a trailing dot", "Console.Apps.Example.com.", []string{"apps.example.com."}, true},
		{"one of several", "console.lab.internal", []string{"apps.example.com", "lab.internal"}, true},
		// Squatting: a name outside every suffix.
		{"another domain", "login.corp.example.com", []string{"apps.example.com"}, false},
		{"a suffix that is not a label boundary", "evilapps.example.com", []string{"apps.example.com"}, false},
		{"the parent of a suffix", "example.com", []string{"apps.example.com"}, false},
		{"a suffix appended as a label", "apps.example.com.attacker.example", []string{"apps.example.com"}, false},
		{"an empty suffix allows nothing", "console.apps.example.com", []string{""}, false},
		{"a lone dot allows nothing", "console.apps.example.com", []string{"."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HostnameAllowed(tc.hostname, tc.suffixes); got != tc.want {
				t.Fatalf("HostnameAllowed(%q, %q) = %v, want %v", tc.hostname, tc.suffixes, got, tc.want)
			}
		})
	}
}

func TestHostnameRefusal(t *testing.T) {
	suffixes := []string{"apps.example.com"}
	for _, tc := range []struct {
		name string
		spec pgtoolboxv1alpha1.ExposureSpec
		also []string
		want []string // substrings; nil: no refusal
	}{
		{name: "clusterIP publishes no name", spec: pgtoolboxv1alpha1.ExposureSpec{Hostname: "login.corp.example.com"}},
		{name: "allowed", spec: pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "a.apps.example.com"}},
		{
			name: "refused", spec: pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "login.corp.example.com"},
			// Both names of the setting: the env variable and the chart value.
			want: []string{"login.corp.example.com", "EXPOSURE_ALLOWED_HOST_SUFFIXES", "chart value operator.exposure.allowedHostSuffixes: apps.example.com"},
		},
		{
			name: "refused with another problem", spec: pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "login.corp.example.com"},
			also: []string{"", "spec.image repository evil is not in the operator's allow-list"},
			want: []string{"login.corp.example.com", "; also spec.image repository evil"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := HostnameRefusal(tc.spec, suffixes, tc.also...)
			if tc.want == nil {
				if got != "" {
					t.Fatalf("HostnameRefusal = %q, want none", got)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("HostnameRefusal = %q, want it to contain %q", got, want)
				}
			}
			if strings.Count(got, "; also") != len(slices.DeleteFunc(slices.Clone(tc.also), func(s string) bool { return s == "" })) {
				t.Fatalf("HostnameRefusal = %q names an empty problem", got)
			}
		})
	}
}

// TestClearHostnameRefusal: only a condition that still says
// HostnameNotAllowed is turned Unknown (Reconciling); any other reason is
// left as it is, so a no-op loop changes nothing.
func TestClearHostnameRefusal(t *testing.T) {
	refused := pgtoolboxv1alpha1.ReasonHostnameNotAllowed.String()
	for _, tc := range []struct {
		name          string
		exposure      string
		configuration string
		wantChanged   []string
	}{
		{
			name: "both refused", exposure: refused, configuration: refused,
			wantChanged: []string{pgtoolboxv1alpha1.ConditionExposureReady, pgtoolboxv1alpha1.ConditionConfigurationValid},
		},
		{
			name: "configuration says another stall", exposure: refused, configuration: pgtoolboxv1alpha1.ReasonImageNotAllowed.String(),
			wantChanged: []string{pgtoolboxv1alpha1.ConditionExposureReady},
		},
		{name: "nothing refused", exposure: pgtoolboxv1alpha1.ReasonAsExpected.String(), configuration: pgtoolboxv1alpha1.ReasonAsExpected.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conds := []metav1.Condition{
				{Type: pgtoolboxv1alpha1.ConditionExposureReady, Status: metav1.ConditionFalse, Reason: tc.exposure, ObservedGeneration: 1},
				{Type: pgtoolboxv1alpha1.ConditionConfigurationValid, Status: metav1.ConditionFalse, Reason: tc.configuration, ObservedGeneration: 1},
			}
			before := slices.Clone(conds)
			ClearHostnameRefusal(&conds, 1)
			for i, c := range conds {
				changed := slices.Contains(tc.wantChanged, c.Type)
				switch {
				case changed && (c.Status != metav1.ConditionUnknown || c.Reason != pgtoolboxv1alpha1.ReasonReconciling.String() || c.Message != HostnameAllowedMessage):
					t.Errorf("%s = %+v, want Unknown/Reconciling", c.Type, c)
				case !changed && c != before[i]:
					t.Errorf("%s changed: %+v, was %+v", c.Type, c, before[i])
				}
			}
		})
	}
}

// TestExtraPaths: the extra paths of a Target go to their Service ahead of
// the catch-all, with the same stream timeout; none leaves each object as
// it was (one path, one rule without matches, no extra Route).
func TestExtraPaths(t *testing.T) {
	spec := &pgtoolboxv1alpha1.ExposureSpec{
		Hostname: "console.example",
		Gateway:  &pgtoolboxv1alpha1.GatewayExposureSpec{ParentRef: pgtoolboxv1alpha1.GatewayParentRef{Name: "gw"}},
	}
	extra := []PathBackend{
		{Name: "mcp", Path: "/mcp", Service: "console-mcp"},
		{Name: "logout", Path: "/logout", Exact: true, Service: "console-mcp"},
	}
	for _, tc := range []struct {
		name       string
		extra      []PathBackend
		wantPaths  []string // path type path -> Service
		wantRules  []string // match types and values -> Service
		wantRoutes []string // name path -> Service
	}{
		{
			name:      "none",
			wantPaths: []string{"Prefix / -> console"},
			wantRules: []string{"[] -> console"},
		},
		{
			name: "two", extra: extra,
			wantPaths:  []string{"Prefix /mcp -> console-mcp", "Exact /logout -> console-mcp", "Prefix / -> console"},
			wantRules:  []string{"[PathPrefix /mcp Exact /logout] -> console-mcp", "[] -> console"},
			wantRoutes: []string{"console-mcp-mcp /mcp -> console-mcp", "console-mcp-logout /logout -> console-mcp"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := Target{
				Meta: metav1.ObjectMeta{Name: "console", Namespace: "team-a"}, Service: "console", PortName: "http", Port: 80,
				StreamTimeout: time.Hour, Extra: tc.extra,
			}
			var paths, rules, routes []string
			for _, p := range Ingress(spec, target).Spec.Rules[0].HTTP.Paths {
				paths = append(paths, fmt.Sprintf("%s %s -> %s", *p.PathType, p.Path, p.Backend.Service.Name))
			}
			for _, r := range HTTPRoute(spec, target).Spec.Rules {
				var matches []string
				for _, m := range r.Matches {
					matches = append(matches, string(*m.Path.Type)+" "+*m.Path.Value)
				}
				if r.Timeouts == nil || *r.Timeouts.Request != "3600s" {
					t.Errorf("rule %v: timeouts %+v, want the stream's", matches, r.Timeouts)
				}
				rules = append(rules, fmt.Sprintf("[%s] -> %s", strings.Join(matches, " "), r.BackendRefs[0].Name))
			}
			for _, r := range ExtraRoutes(spec, target) {
				if r.Annotations["haproxy.router.openshift.io/timeout"] != "3600s" || specs.HashOf(r) != specs.Hash(r.Spec) {
					t.Errorf("Route %s: annotations %v, want the stream timeout and its own hash", r.Name, r.Annotations)
				}
				routes = append(routes, fmt.Sprintf("%s %s -> %s", r.Name, r.Spec.Path, r.Spec.To.Name))
			}
			if !slices.Equal(paths, tc.wantPaths) || !slices.Equal(rules, tc.wantRules) || !slices.Equal(routes, tc.wantRoutes) {
				t.Errorf("Ingress %v\nHTTPRoute %v\nRoutes %v\nwant %v\n%v\n%v", paths, rules, routes, tc.wantPaths, tc.wantRules, tc.wantRoutes)
			}
		})
	}
}
