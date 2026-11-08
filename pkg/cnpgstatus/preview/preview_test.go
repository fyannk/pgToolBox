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

package preview

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// def builds a Cluster definition: spec fields, annotations and the
// status the plan reads (orders-1 primary of three).
func def(t *testing.T, spec map[string]any, annotations map[string]string) []byte {
	t.Helper()
	obj := map[string]any{
		"metadata": map[string]any{"name": "orders", "annotations": annotations},
		"spec":     spec,
		"status": map[string]any{
			"currentPrimary":  "orders-1",
			"instanceNames":   []string{"orders-1", "orders-2", "orders-3"},
			"pgDataImageInfo": map[string]any{"majorVersion": 17},
		},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pg(params map[string]string) map[string]any {
	return map[string]any{"postgresql": map[string]any{"parameters": params}}
}

func merge(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestCompute(t *testing.T) {
	base := map[string]any{"instances": 3, "imageName": "ghcr.io/cloudnative-pg/postgresql:17.6-standard-trixie"}
	tests := []struct {
		name          string
		before, after map[string]any
		annB, annA    map[string]string
		minor         cnpgstatus.Minor
		major         int
		effect        Effect
		atLeast       Effect
		primary       string
		firstStep     string
		// steps, when set, is the whole order as "role:action".
		steps        []string
		paramEffect  map[string]Effect
		paramContext map[string]string
		fieldReasons []string
		decrease     bool
		verified     bool
		unknown      int
	}{
		{
			name:        "work_mem is a reload",
			before:      merge(base, pg(map[string]string{"work_mem": "4MB"})),
			after:       merge(base, pg(map[string]string{"work_mem": "64MB"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectReload,
			primary:     PrimaryReload,
			firstStep:   "all",
			paramEffect: map[string]Effect{"work_mem": EffectReload},
			verified:    true,
		},
		{
			name:        "shared_buffers restarts replicas first, then the primary in place",
			before:      merge(base, pg(map[string]string{"shared_buffers": "1GB"})),
			after:       merge(base, pg(map[string]string{"shared_buffers": "2GB"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartInPlace,
			firstStep:   "replicas",
			paramEffect: map[string]Effect{"shared_buffers": EffectRestart},
			verified:    true,
		},
		{
			name:        "a max_connections decrease restarts the primary first",
			before:      merge(base, pg(map[string]string{"max_connections": "200"})),
			after:       merge(base, pg(map[string]string{"max_connections": "100"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartFirst,
			firstStep:   "primary",
			paramEffect: map[string]Effect{"max_connections": EffectRestart},
			decrease:    true,
			verified:    true,
		},
		{
			name:        "a max_connections increase follows the usual order",
			before:      merge(base, pg(map[string]string{"max_connections": "100"})),
			after:       merge(base, pg(map[string]string{"max_connections": "200"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartInPlace,
			firstStep:   "replicas",
			paramEffect: map[string]Effect{"max_connections": EffectRestart},
			verified:    true,
		},
		{
			name:         "resources roll the Pods; switchover method promotes a replica",
			before:       merge(base, map[string]any{"primaryUpdateMethod": "switchover"}),
			after:        merge(base, map[string]any{"primaryUpdateMethod": "switchover", "resources": map[string]any{"limits": map[string]string{"memory": "4Gi"}}}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRollout,
			primary:      PrimarySwitchover,
			firstStep:    "replicas",
			fieldReasons: []string{ReasonPodTemplate},
			verified:     true,
		},
		{
			name:         "supervised waits for a person",
			before:       merge(base, map[string]any{"primaryUpdateStrategy": "supervised"}),
			after:        merge(base, map[string]any{"primaryUpdateStrategy": "supervised", "imageName": "ghcr.io/cloudnative-pg/postgresql:17.7-standard-trixie"}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRollout,
			primary:      PrimaryWaitForUser,
			firstStep:    "replicas",
			fieldReasons: []string{ReasonImage},
			verified:     true,
		},
		{
			name:         "a new major is an offline upgrade",
			before:       base,
			after:        merge(base, map[string]any{"imageName": "ghcr.io/cloudnative-pg/postgresql:18.1-standard-trixie"}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectMajorUpgrade,
			primary:      PrimaryMajorUpgrade,
			firstStep:    "all",
			fieldReasons: []string{ReasonMajorVersion},
			verified:     true,
		},
		{
			name:        "a fixed parameter is refused, not restarted",
			before:      base,
			after:       merge(base, pg(map[string]string{"port": "5433"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectNone,
			primary:     PrimaryNone,
			paramEffect: map[string]Effect{"port": EffectRefused},
			verified:    true,
		},
		{
			name:        "ssl_ecdh_curve is fixed on 1.30 and free on 1.31",
			before:      base,
			after:       merge(base, pg(map[string]string{"ssl_ecdh_curve": "prime256v1"})),
			minor:       cnpgstatus.Minor131,
			effect:      EffectReload,
			primary:     PrimaryReload,
			firstStep:   "all",
			paramEffect: map[string]Effect{"ssl_ecdh_curve": EffectReload},
			verified:    true,
		},
		{
			name:        "an unknown extension parameter is unknown, not assumed, and never 'no restart'",
			before:      base,
			after:       merge(base, pg(map[string]string{"timescaledb.max_background_workers": "8"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectUnknown,
			atLeast:     EffectNone,
			primary:     PrimaryUnknown,
			paramEffect: map[string]Effect{"timescaledb.max_background_workers": EffectUnknown},
			unknown:     1,
		},
		{
			name:         "a new tablespace attaches a PVC and rolls the Pods",
			before:       base,
			after:        merge(base, map[string]any{"tablespaces": []map[string]any{{"name": "hist"}}}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRollout,
			primary:      PrimaryRecreate,
			firstStep:    "replicas",
			fieldReasons: []string{ReasonNewVolume},
			verified:     true,
		},
		{
			name:         "a storage resize restarts nothing",
			before:       merge(base, map[string]any{"storage": map[string]any{"size": "10Gi"}}),
			after:        merge(base, map[string]any{"storage": map[string]any{"size": "20Gi"}}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectNone,
			primary:      PrimaryNone,
			fieldReasons: []string{ReasonVolumeResize},
			verified:     true,
		},
		{
			name:         "hibernation on shuts everything down",
			before:       base,
			after:        base,
			annA:         map[string]string{"cnpg.io/hibernation": "on"},
			minor:        cnpgstatus.Minor130,
			effect:       EffectShutdown,
			primary:      PrimaryShutdown,
			firstStep:    "all",
			fieldReasons: []string{ReasonHibernation},
			verified:     true,
		},
		{
			name:        "an unverified operator minor is computed and flagged",
			before:      merge(base, pg(map[string]string{"work_mem": "4MB"})),
			after:       merge(base, pg(map[string]string{"work_mem": "8MB"})),
			minor:       "",
			effect:      EffectReload,
			primary:     PrimaryReload,
			firstStep:   "all",
			paramEffect: map[string]Effect{"work_mem": EffectReload},
		},
		{
			name:         "an unknown spec field is unknown",
			before:       base,
			after:        merge(base, map[string]any{"futureKnob": true}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectUnknown,
			atLeast:      EffectNone,
			primary:      PrimaryUnknown,
			fieldReasons: []string{ReasonUnknownField},
			unknown:      1,
		},
		// an unknown change makes the whole preview unknown, with
		// the classified effects as a floor and the steps of that floor.
		{
			name:        "an unknown parameter beside a restart is 'unknown, at least restart'",
			before:      merge(base, pg(map[string]string{"shared_buffers": "1GB"})),
			after:       merge(base, pg(map[string]string{"shared_buffers": "2GB", "timescaledb.max_background_workers": "8"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectUnknown,
			atLeast:     EffectRestart,
			primary:     PrimaryUnknown,
			steps:       []string{"replicas:recreate", "primary:restartInPlace"},
			paramEffect: map[string]Effect{"shared_buffers": EffectRestart, "timescaledb.max_background_workers": EffectUnknown},
			unknown:     1,
		},
		{
			name:    "an unknown field under hibernation still shuts everything down",
			before:  base,
			after:   merge(base, map[string]any{"futureKnob": true}),
			annA:    map[string]string{"cnpg.io/hibernation": "on"},
			minor:   cnpgstatus.Minor130,
			effect:  EffectUnknown,
			atLeast: EffectShutdown,
			primary: PrimaryShutdown,
			steps:   []string{"all:shutdown"},
			unknown: 1,
		},
		// fencing, hibernation off and managed extensions.
		{
			name:         "fencing a replica stops PostgreSQL there; the primary is untouched",
			before:       base,
			after:        base,
			annA:         map[string]string{"cnpg.io/fencedInstances": `["orders-2"]`},
			minor:        cnpgstatus.Minor130,
			effect:       EffectShutdown,
			primary:      PrimaryNone,
			steps:        []string{"replicas:shutdown"},
			fieldReasons: []string{ReasonFencing},
			verified:     true,
		},
		{
			name:     "fencing every instance stops PostgreSQL everywhere",
			before:   base,
			after:    base,
			annA:     map[string]string{"cnpg.io/fencedInstances": `["*"]`},
			minor:    cnpgstatus.Minor130,
			effect:   EffectShutdown,
			primary:  PrimaryShutdown,
			steps:    []string{"all:shutdown"},
			verified: true,
		},
		{
			name:     "lifting the primary's fence starts PostgreSQL again",
			before:   base,
			after:    base,
			annB:     map[string]string{"cnpg.io/fencedInstances": `["orders-1"]`},
			minor:    cnpgstatus.Minor130,
			effect:   EffectStart,
			primary:  PrimaryStart,
			steps:    []string{"primary:start"},
			verified: true,
		},
		{
			name:         "fencing the primary during a rollout: the replicas roll, the primary stays down",
			before:       base,
			after:        merge(base, map[string]any{"resources": map[string]any{"limits": map[string]string{"memory": "4Gi"}}}),
			annA:         map[string]string{"cnpg.io/fencedInstances": `["orders-1"]`},
			minor:        cnpgstatus.Minor130,
			effect:       EffectShutdown,
			primary:      PrimaryShutdown,
			steps:        []string{"primary:shutdown", "replicas:recreate"},
			fieldReasons: []string{ReasonFencing, ReasonPodTemplate},
			verified:     true,
		},
		{
			name:     "an unreadable fencing annotation is unknown",
			before:   base,
			after:    base,
			annA:     map[string]string{"cnpg.io/fencedInstances": `orders-1`},
			minor:    cnpgstatus.Minor130,
			effect:   EffectUnknown,
			atLeast:  EffectNone,
			primary:  PrimaryUnknown,
			unknown:  1,
			verified: false,
		},
		{
			name:         "hibernation off starts every instance",
			before:       base,
			after:        base,
			annB:         map[string]string{"cnpg.io/hibernation": "on"},
			annA:         map[string]string{"cnpg.io/hibernation": "off"},
			minor:        cnpgstatus.Minor130,
			effect:       EffectStart,
			primary:      PrimaryStart,
			steps:        []string{"all:start"},
			fieldReasons: []string{ReasonHibernation},
			verified:     true,
		},
		{
			name:         "the first pg_stat_statements parameter loads the library: a restart",
			before:       base,
			after:        merge(base, pg(map[string]string{"pg_stat_statements.track": "all"})),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRestart,
			primary:      PrimaryRestartInPlace,
			steps:        []string{"replicas:recreate", "primary:restartInPlace"},
			paramEffect:  map[string]Effect{"pg_stat_statements.track": EffectReload},
			fieldReasons: []string{ReasonManagedExtension},
			verified:     true,
		},
		{
			name:         "removing the last auto_explain parameter unloads the library: a restart",
			before:       merge(base, pg(map[string]string{"auto_explain.log_min_duration": "1s"})),
			after:        base,
			minor:        cnpgstatus.Minor130,
			effect:       EffectRestart,
			primary:      PrimaryRestartInPlace,
			fieldReasons: []string{ReasonManagedExtension},
			firstStep:    "replicas",
			verified:     true,
		},
		{
			name:        "a second pgaudit parameter keeps the library: a reload",
			before:      merge(base, pg(map[string]string{"pgaudit.log": "ddl"})),
			after:       merge(base, pg(map[string]string{"pgaudit.log": "ddl", "pgaudit.log_relation": "on"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectReload,
			primary:     PrimaryReload,
			steps:       []string{"all:reload"},
			paramEffect: map[string]Effect{"pgaudit.log_relation": EffectReload},
			verified:    true,
		},
		{
			name:        "a library the user preloads anyway changes nothing",
			before:      merge(base, map[string]any{"postgresql": map[string]any{"shared_preload_libraries": []string{"pg_stat_statements"}}}),
			after:       merge(base, map[string]any{"postgresql": map[string]any{"shared_preload_libraries": []string{"pg_stat_statements"}, "parameters": map[string]string{"pg_stat_statements.track": "all"}}}),
			minor:       cnpgstatus.Minor130,
			effect:      EffectReload,
			primary:     PrimaryReload,
			steps:       []string{"all:reload"},
			paramEffect: map[string]Effect{"pg_stat_statements.track": EffectReload},
			verified:    true,
		},
		// a decrease under supervised waits for a person.
		{
			name:        "a decrease under supervised waits for a person before anything restarts",
			before:      merge(base, map[string]any{"primaryUpdateStrategy": "supervised"}, pg(map[string]string{"max_connections": "200"})),
			after:       merge(base, map[string]any{"primaryUpdateStrategy": "supervised"}, pg(map[string]string{"max_connections": "100"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryWaitForUser,
			steps:       []string{"primary:waitForUser", "replicas:recreate"},
			paramEffect: map[string]Effect{"max_connections": EffectRestart},
			decrease:    true,
			verified:    true,
		},
		// P2.
		{
			name:        "a decrease restarts the primary first, then the replica Pods are recreated",
			before:      merge(base, pg(map[string]string{"max_wal_senders": "20"})),
			after:       merge(base, pg(map[string]string{"max_wal_senders": "12"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartFirst,
			steps:       []string{"primary:restartFirst", "replicas:recreate"},
			paramEffect: map[string]Effect{"max_wal_senders": EffectRestart},
			decrease:    true,
			verified:    true,
		},
		{
			name:        "removing max_worker_processes returns it to CloudNativePG's 32: a decrease from 64",
			before:      merge(base, pg(map[string]string{"max_worker_processes": "64"})),
			after:       base,
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartFirst,
			firstStep:   "primary",
			paramEffect: map[string]Effect{"max_worker_processes": EffectRestart},
			decrease:    true,
			verified:    true,
		},
		{
			name:        "removing max_connections 50 returns it to 100: an increase",
			before:      merge(base, pg(map[string]string{"max_connections": "50"})),
			after:       base,
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartInPlace,
			firstStep:   "replicas",
			paramEffect: map[string]Effect{"max_connections": EffectRestart},
			verified:    true,
		},
		{
			name:        "setting max_prepared_transactions above PostgreSQL's 0 is no decrease",
			before:      base,
			after:       merge(base, pg(map[string]string{"max_prepared_transactions": "10"})),
			minor:       cnpgstatus.Minor130,
			effect:      EffectRestart,
			primary:     PrimaryRestartInPlace,
			firstStep:   "replicas",
			paramEffect: map[string]Effect{"max_prepared_transactions": EffectRestart},
			verified:    true,
		},
		{
			name:         "a decrease with a Pod change: primary first, replicas recreated, then the primary's Pod",
			before:       merge(base, map[string]any{"primaryUpdateMethod": "switchover"}, pg(map[string]string{"max_connections": "200"})),
			after:        merge(base, map[string]any{"primaryUpdateMethod": "switchover", "resources": map[string]any{"limits": map[string]string{"memory": "4Gi"}}}, pg(map[string]string{"max_connections": "100"})),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRollout,
			primary:      PrimaryRestartFirst,
			steps:        []string{"primary:restartFirst", "replicas:recreate", "primary:switchover"},
			paramEffect:  map[string]Effect{"max_connections": EffectRestart},
			fieldReasons: []string{ReasonPodTemplate},
			decrease:     true,
			verified:     true,
		},
		{
			name:         "a decrease with a Pod change under supervised: replicas roll, the primary waits",
			before:       merge(base, map[string]any{"primaryUpdateStrategy": "supervised"}, pg(map[string]string{"max_connections": "200"})),
			after:        merge(base, map[string]any{"primaryUpdateStrategy": "supervised", "resources": map[string]any{"limits": map[string]string{"memory": "4Gi"}}}, pg(map[string]string{"max_connections": "100"})),
			minor:        cnpgstatus.Minor130,
			effect:       EffectRollout,
			primary:      PrimaryWaitForUser,
			steps:        []string{"replicas:recreate", "primary:waitForUser"},
			paramEffect:  map[string]Effect{"max_connections": EffectRestart},
			fieldReasons: []string{ReasonPodTemplate},
			decrease:     true,
			verified:     true,
		},
		{
			name:         "summarize_wal does not exist on 16: unknown, absent",
			before:       base,
			after:        merge(base, pg(map[string]string{"summarize_wal": "on"})),
			minor:        cnpgstatus.Minor130,
			major:        16,
			effect:       EffectUnknown,
			atLeast:      EffectNone,
			primary:      PrimaryUnknown,
			paramEffect:  map[string]Effect{"summarize_wal": EffectUnknown},
			paramContext: map[string]string{"summarize_wal": ContextAbsent},
			unknown:      1,
		},
		{
			name:         "io_workers does not exist on 17: unknown, absent",
			before:       base,
			after:        merge(base, pg(map[string]string{"io_workers": "8"})),
			minor:        cnpgstatus.Minor130,
			effect:       EffectUnknown,
			atLeast:      EffectNone,
			primary:      PrimaryUnknown,
			paramEffect:  map[string]Effect{"io_workers": EffectUnknown},
			paramContext: map[string]string{"io_workers": ContextAbsent},
			unknown:      1,
		},
		{
			name:         "summarize_wal on 17 is a reload",
			before:       base,
			after:        merge(base, pg(map[string]string{"summarize_wal": "on"})),
			minor:        cnpgstatus.Minor130,
			effect:       EffectReload,
			primary:      PrimaryReload,
			steps:        []string{"all:reload"},
			paramEffect:  map[string]Effect{"summarize_wal": EffectReload},
			paramContext: map[string]string{"summarize_wal": ContextReload},
			verified:     true,
		},
		{
			name:         "new certificates are reloaded",
			before:       base,
			after:        merge(base, map[string]any{"certificates": map[string]any{"serverTLSSecret": "orders-tls"}}),
			minor:        cnpgstatus.Minor130,
			effect:       EffectReload,
			primary:      PrimaryReload,
			steps:        []string{"all:reload"},
			fieldReasons: []string{ReasonCertificates},
			verified:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Compute(Input{Before: def(t, tt.before, tt.annB), After: def(t, tt.after, tt.annA), CNPGMinor: tt.minor, PostgresMajor: tt.major})
			if err != nil {
				t.Fatal(err)
			}
			if got.Effect != tt.effect || got.AtLeast != tt.atLeast || got.Primary != tt.primary || got.Verified != tt.verified || got.Unknown != tt.unknown {
				t.Fatalf("effect=%s atLeast=%s primary=%s verified=%v unknown=%d, want %s %s %s %v %d",
					got.Effect, got.AtLeast, got.Primary, got.Verified, got.Unknown, tt.effect, tt.atLeast, tt.primary, tt.verified, tt.unknown)
			}
			if tt.steps != nil {
				var order []string
				for _, st := range got.Steps {
					order = append(order, st.Role+":"+st.Action)
				}
				if !slices.Equal(order, tt.steps) {
					t.Fatalf("steps %v, want %v", order, tt.steps)
				}
			}
			for name, want := range tt.paramContext {
				i := slices.IndexFunc(got.Parameters, func(c ParameterChange) bool { return c.Name == name })
				if i < 0 || got.Parameters[i].Context != want {
					t.Fatalf("parameter %s: %+v, want context %s", name, got.Parameters, want)
				}
			}
			if tt.firstStep != "" && (len(got.Steps) == 0 || got.Steps[0].Role != tt.firstStep) {
				t.Fatalf("steps %+v, want first %s", got.Steps, tt.firstStep)
			}
			if tt.firstStep == "" && tt.steps == nil && len(got.Steps) != 0 {
				t.Fatalf("steps %+v, want none", got.Steps)
			}
			for name, want := range tt.paramEffect {
				i := slices.IndexFunc(got.Parameters, func(c ParameterChange) bool { return c.Name == name })
				if i < 0 || got.Parameters[i].Effect != want || got.Parameters[i].Decrease != tt.decrease {
					t.Fatalf("parameter %s: %+v, want %s decrease=%v", name, got.Parameters, want, tt.decrease)
				}
			}
			for _, reason := range tt.fieldReasons {
				if !slices.ContainsFunc(got.Fields, func(f FieldChange) bool { return f.Reason == reason }) {
					t.Fatalf("fields %+v, want reason %s", got.Fields, reason)
				}
			}
		})
	}
}

func TestComputeOrderNamesInstances(t *testing.T) {
	base := map[string]any{"imageName": "pg:17.6"}
	got, err := Compute(Input{
		Before: def(t, merge(base, pg(map[string]string{"shared_buffers": "1GB"})), nil),
		After:  def(t, merge(base, pg(map[string]string{"shared_buffers": "2GB"})), nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Role: "replicas", Instances: []string{"orders-2", "orders-3"}, Action: "recreate"},
		{Role: "primary", Instances: []string{"orders-1"}, Action: PrimaryRestartInPlace},
	}
	b, _ := json.Marshal(got.Steps)
	w, _ := json.Marshal(want)
	if string(b) != string(w) {
		t.Fatalf("steps %s, want %s", b, w)
	}
}

func TestGUCMajorGates(t *testing.T) {
	tests := []struct {
		name  string
		major int
		want  Effect
	}{
		{"restore_command", 13, EffectRestart},
		{"restore_command", 14, EffectReload},
		{"autovacuum_max_workers", 17, EffectRestart},
		{"autovacuum_max_workers", 18, EffectReload},
		{"autovacuum_max_workers", 0, EffectUnknown},
		{"shared_buffers", 0, EffectRestart},
		{"work_mem", 0, EffectReload},
		{"stats_temp_directory", 14, EffectReload},
		{"pg_stat_statements.max", 16, EffectRestart},
		{"pg_stat_statements.track", 16, EffectReload},
		{"Shared_Buffers", 16, EffectRestart},
		{"DateStyle", 16, EffectReload},
		{"IntervalStyle", 0, EffectReload},
		{"TimeZone", 18, EffectReload},
		{"summarize_wal", 16, EffectUnknown},
		{"summarize_wal", 17, EffectReload},
		{"summarize_wal", 0, EffectUnknown},
		{"io_workers", 17, EffectUnknown},
		{"io_workers", 18, EffectReload},
		{"io_method", 17, EffectUnknown},
		{"io_method", 18, EffectRestart},
		{"compute_query_id", 13, EffectUnknown},
		{"compute_query_id", 14, EffectReload},
		{"stats_temp_directory", 16, EffectUnknown},
		{"db_user_namespace", 16, EffectReload},
		{"db_user_namespace", 17, EffectUnknown},
	}
	for _, tt := range tests {
		if got, _ := guc(tt.name, tt.major); got != tt.want {
			t.Errorf("guc(%s, %d) = %s, want %s", tt.name, tt.major, got, tt.want)
		}
	}
	if !slices.IsSorted(reloadGUCs18) {
		t.Fatal("reloadGUCs18 must stay sorted: guc binary-searches it")
	}
	// guc lowercases the name before looking up: a mixed-case entry
	// would never match.
	for _, n := range reloadGUCs18 {
		if n != strings.ToLower(n) {
			t.Errorf("reloadGUCs18 entry %q is not lowercase", n)
		}
	}
	for _, table := range []map[string]span{postmasterGUCs, reloadRemoved} {
		for n := range table {
			if n != strings.ToLower(n) {
				t.Errorf("table entry %q is not lowercase", n)
			}
		}
	}
	for n := range reloadSince {
		if _, ok := slices.BinarySearch(reloadGUCs18, n); !ok {
			t.Errorf("reloadSince names %q, which is not in reloadGUCs18", n)
		}
	}
}

func TestComputeRejectsBadInput(t *testing.T) {
	if _, err := Compute(Input{}); err == nil {
		t.Fatal("an empty definition must be refused")
	}
	if _, err := Compute(Input{After: []byte("{")}); err == nil {
		t.Fatal("a malformed definition must be refused")
	}
}
