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
	"slices"
	"strings"
	"testing"
	"time"
)

func lookupOf(env map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func TestLoadConsoleDefaults(t *testing.T) {
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a"}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.WatchNamespaces, []string{"team-a"}) {
		t.Errorf("namespaces = %v", cfg.WatchNamespaces)
	}
	if !cfg.ClusterSelector.Empty() {
		t.Errorf("selector = %q, want everything", cfg.ClusterSelector)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"instance status on", cfg.InstanceStatus.Enabled, true},
		{"interval", cfg.InstanceStatus.Interval, 30 * time.Second},
		{"timeout", cfg.InstanceStatus.Timeout, 5 * time.Second},
		{"concurrency", cfg.InstanceStatus.Concurrency, 8},
		{"no CA file", cfg.InstanceStatus.CAFile, ""},
		{"metrics on", cfg.MetricsEnabled, true},
		{"ring 1h", cfg.RingRetention, time.Hour},
		{"diagnostics on", cfg.DiagnosticsEnabled, true},
		{"history in memory", cfg.History.Persistent(), false},
		{"history enabled", cfg.History.Enabled, true},
		{"logs allowed", cfg.AllowLogs, true},
		{"no cluster catalogs", cfg.AllowClusterCatalogs, false},
		{"log level", cfg.LogLevel, "info"},
		{"evidence on", cfg.Evidence.Enabled, true},
		{"evidence interval", cfg.Evidence.Interval, time.Minute},
		{"evidence token default", cfg.Evidence.TokenDirectory, ""},
		{"operations on", cfg.OperationsEnabled, true},
		{"log follow on", cfg.LogFollow, true},
		{"log match age", cfg.LogMatchMaxAge, 6 * time.Hour},
		{"no monitoring link", cfg.MonitoringURL, ""},
		{"no fixed pgAdmin link", cfg.PgAdminURL, ""},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestLoadConsoleChartEnvironment loads what charts/pgtools renders with
// every option on, plus the auth variables it also sets (read by the auth
// stream), which must not trip the read-side validation.
func TestLoadConsoleChartEnvironment(t *testing.T) {
	env := map[string]string{
		EnvPodNamespace:         "team-a",
		EnvWatchNamespaces:      "team-a,team-a-staging",
		EnvClusterSelector:      "app in (orders,billing),tier!=test",
		"LISTEN_ADDR":           ":8080",
		"PROBE_LISTEN_ADDR":     ":8081",
		"AUTH_PROVIDER":         "oidc",
		"SESSION_KEY_FILE":      "/var/run/console/session/key",
		"REVOCATION_CONFIGMAP":  "pgtoolbox-console-revocations",
		EnvInstanceStatusCAFile: "/var/run/console/instance-ca/ca.crt",
		EnvAllowClusterCatalogs: "true",
		EnvAllowLogs:            "false",
		EnvHistoryEnabled:       "true",
		EnvHistoryPath:          "/var/lib/console/history.db",
		EnvPgAdminURL:           "https://pgadmin.apps.example.com/",
		EnvStateConfigMap:       "pgtoolbox-console-state",
	}
	cfg, err := LoadConsole(lookupOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.WatchNamespaces, []string{"team-a", "team-a-staging"}) {
		t.Errorf("namespaces = %v", cfg.WatchNamespaces)
	}
	if cfg.ClusterSelector.Empty() {
		t.Error("selector not parsed")
	}
	if cfg.InstanceStatus.CAFile != "/var/run/console/instance-ca/ca.crt" || !cfg.AllowClusterCatalogs || cfg.AllowLogs || !cfg.History.Persistent() {
		t.Errorf("chart values not honoured: %+v", cfg)
	}
	if cfg.VolumeTrend != (VolumeTrend{Interval: 15 * time.Minute, Retention: 30 * 24 * time.Hour, MaxSeries: 16384}) {
		t.Errorf("volume trend defaults = %+v", cfg.VolumeTrend)
	}
	if cfg.StateConfigMap != "pgtoolbox-console-state" {
		t.Errorf("state ConfigMap = %q", cfg.StateConfigMap)
	}
	if cfg.PgAdminURL != "https://pgadmin.apps.example.com/" {
		t.Errorf("pgAdmin link = %q", cfg.PgAdminURL)
	}
	if info := cfg.Info(); info[EnvWatchNamespaces] != "team-a,team-a-staging" || info[EnvHistoryPath] == "" {
		t.Errorf("info = %v", info)
	}
}

func TestLoadConsoleWatchNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    []string
		wantErr string
	}{
		{name: "defaults to the pod namespace when unset", env: map[string]string{EnvPodNamespace: "team-a"}, want: []string{"team-a"}},
		{name: "unset and no pod namespace", env: map[string]string{}, wantErr: "WATCH_NAMESPACES: is required"},
		{name: "explicitly empty is never all", env: map[string]string{EnvWatchNamespaces: "", EnvPodNamespace: "team-a"}, wantErr: "an empty value is not all namespaces"},
		{name: "only commas", env: map[string]string{EnvWatchNamespaces: " , ,"}, wantErr: "an empty value is not all namespaces"},
		{name: "star refused", env: map[string]string{EnvWatchNamespaces: "*"}, wantErr: "no all-namespaces console"},
		{name: "all refused", env: map[string]string{EnvWatchNamespaces: "team-a,ALL"}, wantErr: "no all-namespaces console"},
		{name: "invalid label", env: map[string]string{EnvWatchNamespaces: "Team_A"}, wantErr: "DNS-1123"},
		{name: "deduplicated and trimmed", env: map[string]string{EnvWatchNamespaces: " a , b,a ,"}, want: []string{"a", "b"}},
		{name: "too many", env: map[string]string{EnvWatchNamespaces: "a,b,c,d,e,f,g,h,i,j,k,l,m,n,o,p,q"}, wantErr: "at most 16"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConsole(lookupOf(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.WatchNamespaces, tt.want) {
				t.Errorf("got %v, want %v", cfg.WatchNamespaces, tt.want)
			}
		})
	}
}

func TestLoadConsoleRejects(t *testing.T) {
	base := func(extra map[string]string) map[string]string {
		env := map[string]string{EnvWatchNamespaces: "team-a"}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"bad selector", base(map[string]string{EnvClusterSelector: "a in (b"}), EnvClusterSelector},
		{"relative CA file", base(map[string]string{EnvInstanceStatusCAFile: "ca.crt"}), EnvInstanceStatusCAFile},
		{"unclean CA file", base(map[string]string{EnvInstanceStatusCAFile: "/a/../b/ca.crt"}), EnvInstanceStatusCAFile},
		{"insecure mode is gone", base(map[string]string{EnvInstanceStatusInsecure: "true"}), EnvInstanceStatusInsecure},
		{"timeout not below interval", base(map[string]string{EnvInstanceStatusInterval: "5s", EnvInstanceStatusTimeout: "5s"}), EnvInstanceStatusTimeout},
		{"interval too short", base(map[string]string{EnvInstanceStatusInterval: "1s"}), EnvInstanceStatusInterval},
		{"concurrency out of range", base(map[string]string{EnvInstanceStatusConcurrency: "0"}), EnvInstanceStatusConcurrency},
		{"ring shorter than four sweeps", base(map[string]string{EnvInstanceStatusInterval: "5m", EnvRingRetention: "10m"}), EnvRingRetention},
		{"volume trend slot above an hour", base(map[string]string{EnvVolumeTrendInterval: "2h"}), EnvVolumeTrendInterval},
		{"volume trend over its memory budget", base(map[string]string{EnvVolumeTrendInterval: "1m", EnvVolumeTrendRetention: "2160h", EnvVolumeTrendMaxSeries: "100000"}), EnvVolumeTrendMaxSeries},
		{"volume trend slot below the sweep", base(map[string]string{EnvInstanceStatusInterval: "5m", EnvRingRetention: "1h", EnvVolumeTrendInterval: "2m"}), EnvVolumeTrendInterval},
		{"volume trend shorter than the fit window", base(map[string]string{EnvVolumeTrendRetention: "72h"}), EnvVolumeTrendRetention},
		{"volume trend series out of range", base(map[string]string{EnvVolumeTrendMaxSeries: "1"}), EnvVolumeTrendMaxSeries},
		{"history path without history", base(map[string]string{EnvHistoryEnabled: "false", EnvHistoryPath: "/var/lib/h.db"}), EnvHistoryPath},
		{"relative history path", base(map[string]string{EnvHistoryPath: "h.db"}), EnvHistoryPath},
		{"bad bool", base(map[string]string{EnvDiagnosticsEnabled: "yes"}), EnvDiagnosticsEnabled},
		{"bad log level", base(map[string]string{EnvLogLevel: "trace"}), EnvLogLevel},
		{"relative evidence token directory", base(map[string]string{EnvEvidenceTokenDir: "tokens"}), EnvEvidenceTokenDir},
		{"unclean evidence token directory", base(map[string]string{EnvEvidenceTokenDir: "/a/../b"}), EnvEvidenceTokenDir},
		{"retired evidence token path", base(map[string]string{EnvEvidenceTokenPathRetired: "/var/run/secrets/pgtoolbox.fyannk.dev/evidence-agent/token"}), EnvEvidenceTokenPathRetired},
		{"retired evidence token path, empty", base(map[string]string{EnvEvidenceTokenPathRetired: ""}), EnvEvidenceTokenPathRetired},
		{"evidence interval too short", base(map[string]string{EnvEvidenceInterval: "1s"}), EnvEvidenceInterval},
		{"bad operations bool", base(map[string]string{EnvOperationsEnabled: "maybe"}), EnvOperationsEnabled},
		{"plain-http monitoring link", base(map[string]string{EnvMonitoringURL: "http://grafana.example.com"}), EnvMonitoringURL},
		{"plain-http pgAdmin link", base(map[string]string{EnvPgAdminURL: "http://pgadmin.example.com"}), EnvPgAdminURL},
		{"protocol-relative pgAdmin link", base(map[string]string{EnvPgAdminURL: "//evil.example.com/"}), EnvPgAdminURL},
		{"pgAdmin link with user information", base(map[string]string{EnvPgAdminURL: "https://u:p@pgadmin.example.com/"}), EnvPgAdminURL},
		{"state ConfigMap that is no name", base(map[string]string{EnvStateConfigMap: "Not_A/Name"}), EnvStateConfigMap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConsole(lookupOf(tt.env))
			if err == nil {
				t.Fatal("want an error")
			}
			var varErr *VarError
			found := false
			for _, e := range unwrapAll(err) {
				if errors.As(e, &varErr) && varErr.Name == tt.wantVar {
					found = true
				}
			}
			if !found {
				t.Errorf("no VarError for %s in %v", tt.wantVar, err)
			}
		})
	}
}

// TestLoadConsoleNeverEchoesValues guards the rule that an error names the
// variable and the constraint, never what was supplied.
func TestLoadConsoleNeverEchoesValues(t *testing.T) {
	secretish := "s3cr3t-value"
	env := map[string]string{
		EnvWatchNamespaces:      secretish + "_",
		EnvClusterSelector:      secretish + " in (",
		EnvInstanceStatusCAFile: secretish,
		EnvHistoryPath:          secretish,
		EnvLogLevel:             secretish,
	}
	_, err := LoadConsole(lookupOf(env))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secretish) {
		t.Errorf("error echoes a value: %v", err)
	}
}

// TestLoadConsoleIsTotal reports every problem at once.
func TestLoadConsoleIsTotal(t *testing.T) {
	_, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "", EnvLogLevel: "x", EnvDiagnosticsEnabled: "x"}))
	if got := len(unwrapAll(err)); got != 3 {
		t.Errorf("got %d problems, want 3: %v", got, err)
	}
}

func unwrapAll(err error) []error {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}

func TestLogFollowNeedsLogAccess(t *testing.T) {
	cfg, err := LoadConsole(lookupOf(map[string]string{EnvWatchNamespaces: "team-a", EnvAllowLogs: "false"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogFollow {
		t.Error("log following on without ALLOW_LOGS")
	}
}

func TestLoadRestoreDrill(t *testing.T) {
	t.Parallel()
	base := map[string]string{EnvWatchNamespaces: "team-a,drills"}
	with := func(kv ...string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	cfg, err := LoadConsole(lookupOf(base))
	if err != nil {
		t.Fatal(err)
	}
	if d := cfg.RestoreDrill; d.Enabled || d.Prefix != "drill-" || d.TimeLimit != 2*time.Hour || d.MaxConcurrent != 3 || d.NamespaceFor("team-a") != "team-a" ||
		d.ServerCA != DrillServerCAOwn || d.ServerCASecret != "" {
		t.Errorf("defaults = %+v", d)
	}
	cfg, err = LoadConsole(lookupOf(with(EnvRestoreDrillEnabled, "true", EnvRestoreDrillNamespace, "drills", EnvRestoreDrillPrefix, "rd-",
		EnvRestoreDrillTimeLimit, "45m", EnvRestoreDrillMemoryRequest, "1Gi", EnvRestoreDrillMemoryLimit, "2Gi", EnvRestoreDrillCPURequest, "500m",
		EnvRestoreDrillStorageSize, "20Gi", EnvRestoreDrillStorageClass, "fast.ssd", EnvRestoreDrillMaxConcurrent, "5")))
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.RestoreDrill
	if !d.Enabled || d.NamespaceFor("team-a") != "drills" || d.Prefix != "rd-" || d.TimeLimit != 45*time.Minute ||
		d.MemoryRequest != "1Gi" || d.MemoryLimit != "2Gi" || d.CPURequest != "500m" || d.StorageSize != "20Gi" || d.StorageClass != "fast.ssd" || d.MaxConcurrent != 5 {
		t.Errorf("set = %+v", d)
	}
	if d.SealKeyFile != "" {
		t.Errorf("seal key file = %q, want the session-derived default", d.SealKeyFile)
	}
	if cfg, err := LoadConsole(lookupOf(with(EnvRestoreDrillSealKeyFile, "/etc/pgconsole/drill-seal/../drill-seal/keys"))); err != nil ||
		cfg.RestoreDrill.SealKeyFile != "/etc/pgconsole/drill-seal/keys" {
		t.Errorf("seal key file = %q, %v", cfg.RestoreDrill.SealKeyFile, err)
	}
	if info := cfg.Info(); info[EnvRestoreDrillEnabled] != "true" || info[EnvRestoreDrillNamespace] != "drills" ||
		info[EnvRestoreDrillServerCA] != DrillServerCAOwn {
		t.Errorf("info = %v", info)
	}
	for _, tt := range []struct {
		env          map[string]string
		mode, secret string
	}{
		{with(EnvRestoreDrillServerCA, "source"), DrillServerCASource, ""},
		{with(EnvRestoreDrillServerCA, "own"), DrillServerCAOwn, ""},
		{with(EnvRestoreDrillServerCA, "secret", EnvRestoreDrillServerCASecret, "drill-ca"), DrillServerCASecret, "drill-ca"},
		{with(EnvRestoreDrillServerCA, "secret", EnvRestoreDrillServerCASecret, "drill-ca", EnvRestoreDrillNamespace, "drills"), DrillServerCASecret, "drill-ca"},
	} {
		cfg, err := LoadConsole(lookupOf(tt.env))
		if err != nil {
			t.Fatalf("%v: %v", tt.env, err)
		}
		if d := cfg.RestoreDrill; d.ServerCA != tt.mode || d.ServerCASecret != tt.secret {
			t.Errorf("%v: server CA %q %q, want %q %q", tt.env, d.ServerCA, d.ServerCASecret, tt.mode, tt.secret)
		}
	}

	bad := []struct {
		name, variable string
		env            map[string]string
	}{
		{"unwatched namespace", EnvRestoreDrillNamespace, with(EnvRestoreDrillNamespace, "elsewhere")},
		{"namespace shape", EnvRestoreDrillNamespace, with(EnvRestoreDrillNamespace, "Team_A")},
		{"prefix shape", EnvRestoreDrillPrefix, with(EnvRestoreDrillPrefix, "-x")},
		{"empty prefix", EnvRestoreDrillPrefix, with(EnvRestoreDrillPrefix, "")},
		{"prefix too long", EnvRestoreDrillPrefix, with(EnvRestoreDrillPrefix, strings.Repeat("a", MaxRestoreDrillPrefix+1))},
		{"time limit too short", EnvRestoreDrillTimeLimit, with(EnvRestoreDrillTimeLimit, "1m")},
		{"not a quantity", EnvRestoreDrillMemoryRequest, with(EnvRestoreDrillMemoryRequest, "lots")},
		{"zero quantity", EnvRestoreDrillStorageSize, with(EnvRestoreDrillStorageSize, "0")},
		{"limit below request", EnvRestoreDrillCPULimit, with(EnvRestoreDrillCPURequest, "2", EnvRestoreDrillCPULimit, "1")},
		{"no concurrent drill", EnvRestoreDrillMaxConcurrent, with(EnvRestoreDrillMaxConcurrent, "0")},
		{"too many concurrent drills", EnvRestoreDrillMaxConcurrent, with(EnvRestoreDrillMaxConcurrent, "21")},
		{"storage class shape", EnvRestoreDrillStorageClass, with(EnvRestoreDrillStorageClass, "Fast_SSD")},
		{"unknown server CA mode", EnvRestoreDrillServerCA, with(EnvRestoreDrillServerCA, "mirror")},
		{"source CA in a drill namespace", EnvRestoreDrillServerCA, with(EnvRestoreDrillServerCA, "source", EnvRestoreDrillNamespace, "drills")},
		{"secret mode without a Secret", EnvRestoreDrillServerCASecret, with(EnvRestoreDrillServerCA, "secret")},
		{"a Secret without secret mode", EnvRestoreDrillServerCASecret, with(EnvRestoreDrillServerCASecret, "drill-ca")},
		{"relative seal key file", EnvRestoreDrillSealKeyFile, with(EnvRestoreDrillSealKeyFile, "seal/keys")},
		{"Secret name shape", EnvRestoreDrillServerCASecret, with(EnvRestoreDrillServerCA, "secret", EnvRestoreDrillServerCASecret, "Drill_CA")},
		{"needs operations", EnvRestoreDrillEnabled, with(EnvRestoreDrillEnabled, "true", EnvOperationsEnabled, "false")},
	}
	for _, tt := range bad {
		_, err := LoadConsole(lookupOf(tt.env))
		var ve *VarError
		if err == nil || !errors.As(err, &ve) || !strings.Contains(err.Error(), tt.variable) {
			t.Errorf("%s: err = %v, want one naming %s", tt.name, err, tt.variable)
		}
	}
}
