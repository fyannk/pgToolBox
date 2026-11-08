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

package repository

import (
	"reflect"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

func TestRegistryKeepsFormatDescriptorsNeutral(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(
		StaticFormat{Value: Descriptor{ID: "barman-cloud", DisplayName: "Barman Cloud", ScopeKind: "server"}},
		StaticFormat{Value: Descriptor{ID: "pgbackrest", DisplayName: "pgBackRest", ScopeKind: "stanza"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := registry.IDs(), []string{"barman-cloud", "pgbackrest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	tests := []struct {
		id        string
		scopeKind string
	}{
		{id: "barman-cloud", scopeKind: "server"},
		{id: "pgbackrest", scopeKind: "stanza"},
	}
	for _, test := range tests {
		format, selectErr := registry.Select(test.id)
		if selectErr != nil {
			t.Fatalf("Select(%q) error = %v", test.id, selectErr)
		}
		if format.Descriptor().ScopeKind != test.scopeKind {
			t.Fatalf("Select(%q) scope = %q", test.id, format.Descriptor().ScopeKind)
		}
	}
}

func TestRegistryAcceptsFakeSecondFormatWithoutBarmanShape(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(StaticFormat{Value: Descriptor{
		ID: "synthetic", DisplayName: "Synthetic Repository", ScopeKind: "vault",
		Capabilities: []evidence.CapabilityID{evidence.CatalogListing},
	}})
	if err != nil {
		t.Fatal(err)
	}
	format, err := registry.Select("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := evidence.InitialSnapshot(format.Descriptor().ID, format.Descriptor().ScopeKind, format.Descriptor().Capabilities)
	if snapshot.Scope.Kind != "vault" || snapshot.State != evidence.Unknown {
		t.Fatalf("fake format was forced into another format shape: %#v", snapshot)
	}
}

func TestRegistrySelectionNeverFallsBack(t *testing.T) {
	t.Parallel()
	registry, err := NewRegistry(StaticFormat{Value: Descriptor{
		ID: "barman-cloud", DisplayName: "Barman Cloud", ScopeKind: "server",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Select("pgbackrest"); err == nil {
		t.Fatal("Select(pgbackrest) unexpectedly fell back to registered format")
	}
}
