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

package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadConsolePrometheus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		want    Prometheus
		errVars []string
	}{
		{name: "unset", want: Prometheus{Range: time.Hour}},
		{
			name: "https with token and CA",
			env: map[string]string{
				EnvPrometheusURL: "https://thanos-querier.openshift-monitoring.svc:9092", EnvPrometheusTokenFile: "/var/run/secrets/prometheus/token",
				EnvPrometheusCAFile: "/etc/prometheus-ca/service-ca.crt", EnvPrometheusNamespaced: "true", EnvPrometheusRange: "3h",
			},
			want: Prometheus{
				URL: "https://thanos-querier.openshift-monitoring.svc:9092", TokenFile: "/var/run/secrets/prometheus/token",
				CAFile: "/etc/prometheus-ca/service-ca.crt", Namespaced: true, Range: 3 * time.Hour,
			},
		},
		{
			name: "http without a token",
			env:  map[string]string{EnvPrometheusURL: "http://prometheus-operated.monitoring.svc:9090"},
			want: Prometheus{URL: "http://prometheus-operated.monitoring.svc:9090", Range: time.Hour},
		},
		{
			name:    "a token never goes over http",
			env:     map[string]string{EnvPrometheusURL: "http://prometheus:9090", EnvPrometheusTokenFile: "/t"},
			errVars: []string{EnvPrometheusURL},
		},
		{name: "credentials in the URL", env: map[string]string{EnvPrometheusURL: "https://u:p@prometheus"}, errVars: []string{EnvPrometheusURL}},
		{name: "query in the URL", env: map[string]string{EnvPrometheusURL: "https://prometheus/?x=1"}, errVars: []string{EnvPrometheusURL}},
		{name: "relative URL", env: map[string]string{EnvPrometheusURL: "/prometheus"}, errVars: []string{EnvPrometheusURL}},
		{name: "other scheme", env: map[string]string{EnvPrometheusURL: "ftp://prometheus"}, errVars: []string{EnvPrometheusURL}},
		{name: "relative token path", env: map[string]string{EnvPrometheusURL: "https://p", EnvPrometheusTokenFile: "token"}, errVars: []string{EnvPrometheusTokenFile}},
		{name: "token without URL", env: map[string]string{EnvPrometheusTokenFile: "/t"}, errVars: []string{EnvPrometheusURL}},
		{name: "range out of bounds", env: map[string]string{EnvPrometheusURL: "https://p", EnvPrometheusRange: "1m"}, errVars: []string{EnvPrometheusRange}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{EnvWatchNamespaces: "team-a"}
			for k, v := range tt.env {
				env[k] = v
			}
			cfg, err := LoadConsole(lookupOf(env))
			if len(tt.errVars) > 0 {
				if err == nil {
					t.Fatal("want an error")
				}
				for _, name := range tt.errVars {
					found := false
					for _, e := range unwrapAll(err) {
						var ve *VarError
						if errors.As(e, &ve) && ve.Name == name {
							found = true
						}
					}
					if !found {
						t.Errorf("no error names %s: %v", name, err)
					}
				}
				if strings.Contains(err.Error(), "u:p") {
					t.Error("the error echoes the value")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Prometheus != tt.want {
				t.Errorf("Prometheus = %+v, want %+v", cfg.Prometheus, tt.want)
			}
			if cfg.Prometheus.Enabled() != (tt.want.URL != "") {
				t.Error("Enabled disagrees with URL")
			}
			if cfg.Info()[EnvPrometheusURL] != tt.want.URL {
				t.Error("Info does not report the URL")
			}
		})
	}
}
