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

package console

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestPrometheusProjectedTokenAudience: the projected Prometheus token
// carries spec.prometheus.tokenAudience, and the API server's audience
// (empty) when it is omitted.
func TestPrometheusProjectedTokenAudience(t *testing.T) {
	for name, tc := range map[string]struct{ audience, want string }{
		"default: the API server's audience": {},
		"a dedicated audience":               {audience: "prometheus.example.com", want: "prometheus.example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{
				ObjectMeta: metav1.ObjectMeta{Name: "team-a", Namespace: "team-a"},
				Spec: pgtoolboxv1alpha1.PgToolBoxConsoleSpec{Prometheus: &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{
					URL: "https://prometheus.example", ProjectedToken: true, TokenAudience: tc.audience,
				}},
			}
			_, volumes := consoleVolumes(c)
			found := false
			for _, v := range volumes {
				if v.Name != "prometheus-token" {
					continue
				}
				found = true
				if v.Projected == nil || len(v.Projected.Sources) != 1 || v.Projected.Sources[0].ServiceAccountToken == nil {
					t.Fatalf("prometheus-token is not a projected ServiceAccount token: %+v", v)
				}
				if got := v.Projected.Sources[0].ServiceAccountToken.Audience; got != tc.want {
					t.Errorf("audience = %q, want %q", got, tc.want)
				}
			}
			if !found {
				t.Fatal("no prometheus-token volume")
			}
		})
	}
}
