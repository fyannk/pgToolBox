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

package formats

import (
	"reflect"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

func TestBuiltinsExposeBothFormatsAndObjectInventory(t *testing.T) {
	t.Parallel()
	registry, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := registry.IDs(), []string{"barman-cloud", "pgbackrest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs() = %v, want %v", got, want)
	}
	for _, id := range registry.IDs() {
		format, err := registry.Select(id)
		if err != nil {
			t.Fatal(err)
		}
		foundInventory := false
		for _, capability := range format.Descriptor().Capabilities {
			foundInventory = foundInventory || capability == evidence.ObjectInventory
		}
		if !foundInventory {
			t.Fatalf("format %s lacks object inventory capability", id)
		}
	}
}
