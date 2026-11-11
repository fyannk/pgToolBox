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
	"slices"
	"strings"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
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
			want: []string{"login.corp.example.com", "EXPOSURE_ALLOWED_HOST_SUFFIXES: apps.example.com"},
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
