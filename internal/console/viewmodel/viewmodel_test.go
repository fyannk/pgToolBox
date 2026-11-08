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

package viewmodel_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/console/topology"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func scenarios() map[string]any {
	return map[string]any{
		"cluster-list":          fixtures.ClusterList(),
		"overview-healthy":      fixtures.HealthyOverview(),
		"overview-failing-over": fixtures.FailingOverOverview(),
		"instances":             fixtures.Instances(),
		"events":                fixtures.Events(),
		"diagnostics":           fixtures.Diagnostics(),
		"triage":                fixtures.Triage(),
		"replication":           fixtures.Replication(),
		"comparison":            fixtures.Comparison(),
		"backups":               fixtures.Backups(),
		"storage":               fixtures.Storage(),
		"storage-forecasts":     fixtures.StorageForecasts(),
		"restore-check":         fixtures.RestoreCheck(),
		"objects":               fixtures.Objects(),
		"history":               fixtures.History(),
		"revision":              fixtures.Revision(),
		"timeline":              fixtures.Timeline(),
		"digest":                fixtures.Digests()[0],
		"metrics":               fixtures.Metrics(),
		"topology":              fixtures.Topology(),
		"log-tail":              fixtures.LogTail(),
		"admin":                 fixtures.Admin(),
		"operations":            fixtures.Operations(),
		"operation":             fixtures.Operation(),
		"vocabulary":            viewmodel.NewVocabulary(),
	}
}

// TestGolden pins the JSON wire shape: a change to a field name, a tag or
// omitempty shows up as a golden diff the reviewer must accept with
// -update, which is the point at which the additive rule is checked.
func TestGolden(t *testing.T) {
	for name, value := range scenarios() {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".golden.json")
			if *update {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from its golden file; run go test ./internal/console/viewmodel -update and review the diff", name)
			}
		})
	}
}

// TestEnumTags walks every scenario and checks that each string value
// carried by a field with an enum tag is one of the values the tag
// lists. The enum tags become the OpenAPI enums (Huma), so a value the
// tag does not list would be a contract the generated client rejects.
func TestEnumTags(t *testing.T) {
	for name, value := range scenarios() {
		walk(t, name, reflect.ValueOf(value), "")
	}
}

func walk(t *testing.T, scenario string, v reflect.Value, path string) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walk(t, scenario, v.Elem(), path)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walk(t, scenario, v.Index(i), path+"[]")
		}
	case reflect.Struct:
		for i := range v.NumField() {
			field := v.Type().Field(i)
			fv := v.Field(i)
			// SourceState.Health is filled from State by MarshalJSON.
			derived := v.Type() == reflect.TypeFor[viewmodel.SourceState]() && field.Name == "Health"
			if enum, ok := field.Tag.Lookup("enum"); ok && fv.Kind() == reflect.String && !derived {
				value := fv.String()
				omitempty := strings.Contains(field.Tag.Get("json"), "omitempty")
				if (value != "" || !omitempty) && !slices.Contains(strings.Split(enum, ","), value) {
					t.Errorf("%s: %s.%s = %q, not in enum %q", scenario, path, field.Name, value, enum)
				}
			}
			walk(t, scenario, fv, path+"."+field.Name)
		}
	default:
	}
}

// TestHealthEnumTagsListEveryState checks every Health-typed field tags
// exactly the seven states, so no field silently narrows the vocabulary.
func TestHealthEnumTagsListEveryState(t *testing.T) {
	want := make([]string, 0, len(viewmodel.AllHealth))
	for _, h := range viewmodel.AllHealth {
		want = append(want, string(h))
	}
	slices.Sort(want)
	healthType := reflect.TypeFor[viewmodel.Health]()
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			if field.Type == healthType {
				got := strings.Split(field.Tag.Get("enum"), ",")
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Errorf("%s.%s enum tag %v, want %v", typ.Name(), field.Name, got, want)
				}
			}
			visit(field.Type)
		}
	}
	for _, value := range scenarios() {
		visit(reflect.TypeOf(value))
	}
}

func TestWorst(t *testing.T) {
	tests := []struct {
		name string
		in   []viewmodel.Health
		want viewmodel.Health
	}{
		{"empty is unknown", nil, viewmodel.HealthUnknown},
		{"single ok", []viewmodel.Health{viewmodel.HealthOK}, viewmodel.HealthOK},
		{"info never raises", []viewmodel.Health{viewmodel.HealthInfo, viewmodel.HealthOK}, viewmodel.HealthOK},
		{"unknown beats ok", []viewmodel.Health{viewmodel.HealthOK, viewmodel.HealthUnknown, viewmodel.HealthOK}, viewmodel.HealthUnknown},
		{"unknown beats progressing", []viewmodel.Health{viewmodel.HealthProgressing, viewmodel.HealthUnknown}, viewmodel.HealthUnknown},
		{"warning beats unknown", []viewmodel.Health{viewmodel.HealthUnknown, viewmodel.HealthWarning}, viewmodel.HealthWarning},
		{"critical beats all", []viewmodel.Health{viewmodel.HealthWarning, viewmodel.HealthCritical, viewmodel.HealthUnknown}, viewmodel.HealthCritical},
		{"inactive beats ok", []viewmodel.Health{viewmodel.HealthOK, viewmodel.HealthInactive}, viewmodel.HealthInactive},
		{"progressing beats inactive", []viewmodel.Health{viewmodel.HealthInactive, viewmodel.HealthProgressing}, viewmodel.HealthProgressing},
		{"foreign state is unknown", []viewmodel.Health{viewmodel.HealthOK, "purple"}, viewmodel.HealthUnknown},
		{"foreign state alone is unknown", []viewmodel.Health{"purple"}, viewmodel.HealthUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := viewmodel.Worst(tt.in...); got != tt.want {
				t.Errorf("Worst(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSourceStateHealth(t *testing.T) {
	want := map[viewmodel.SourceStateKind]viewmodel.Health{
		viewmodel.SourceLive: viewmodel.HealthOK, viewmodel.SourcePending: viewmodel.HealthProgressing,
		viewmodel.SourceStale: viewmodel.HealthUnknown, viewmodel.SourceUnavailable: viewmodel.HealthWarning,
		viewmodel.SourceForbidden: viewmodel.HealthWarning, viewmodel.SourceNotInstalled: viewmodel.HealthInfo,
		viewmodel.SourceDisabled: viewmodel.HealthInactive, "purple": viewmodel.HealthUnknown,
	}
	for kind, h := range want {
		if got := kind.Health(); got != h {
			t.Errorf("%s.Health() = %s, want %s", kind, got, h)
		}
		b, err := json.Marshal(viewmodel.SourceState{Source: "x", State: kind})
		if err != nil {
			t.Fatal(err)
		}
		var back map[string]any
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back["health"] != string(h) {
			t.Errorf("marshalled %s carries health %v, want %s", kind, back["health"], h)
		}
	}
	if len(viewmodel.AllSourceStates) != len(want)-1 {
		t.Errorf("AllSourceStates has %d states, the table %d", len(viewmodel.AllSourceStates), len(want)-1)
	}
}

func TestHealthRankFollowsAllHealth(t *testing.T) {
	for i, h := range viewmodel.AllHealth {
		if h.Rank() != i {
			t.Errorf("%s.Rank() = %d, want %d", h, h.Rank(), i)
		}
	}
	if viewmodel.Health("purple").Rank() != viewmodel.HealthUnknown.Rank() {
		t.Error("a foreign state does not rank as unknown")
	}
}

// TestTopologyFixtureGeometry holds the fixture to the rules the read
// model's layout keeps (internal/console/topology.Check).
func TestTopologyFixtureGeometry(t *testing.T) {
	for _, v := range topology.Check(fixtures.Topology()) {
		t.Error(v)
	}
}
