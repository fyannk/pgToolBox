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

package store

import (
	"reflect"
	"testing"
)

func TestReaderSurface(t *testing.T) {
	t.Parallel()
	reader := reflect.TypeOf((*Reader)(nil)).Elem()
	want := []string{"List", "Open", "Stat"}
	if reader.NumMethod() != len(want) {
		t.Fatalf("Reader has %d methods, want exactly %d", reader.NumMethod(), len(want))
	}
	for index, name := range want {
		if got := reader.Method(index).Name; got != name {
			t.Fatalf("Reader method %d = %s, want %s", index, got, name)
		}
	}
}
