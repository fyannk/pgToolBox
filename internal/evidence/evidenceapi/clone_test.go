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

package evidenceapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestClonesShareNothingWithTheOriginal walks the wire types the engine
// clones by hand and fails when a pointer, slice or map of the clone
// still points into the original. The engine hands clones to readers
// while it keeps the original; a field added to evidencev1alpha1 and
// forgotten in clonePublication would alias the two silently, and a
// reader mutating its copy (or the engine replacing its own) would be
// seen across the boundary. Every field is filled by reflection, so a
// new field is covered the day it is added.
func TestClonesShareNothingWithTheOriginal(t *testing.T) {
	t.Parallel()
	t.Run("barman cloud publication", func(t *testing.T) {
		t.Parallel()
		var original Publication
		fillAll(reflect.ValueOf(&original).Elem(), 0)
		original.PGBackRest = nil // the hand-written path; pgBackRest is the next case
		// Details is a tagged union: one payload, named by its type.
		original.Snapshot.Details.Type, original.Snapshot.Details.PGBackRest = evidencev1alpha1.BarmanDetailsType, nil
		clone, err := clonePublication(original)
		if err != nil {
			t.Fatal(err)
		}
		assertDeepCopy(t, original, clone)
	})
	t.Run("pgbackrest publication", func(t *testing.T) {
		t.Parallel()
		var native PGBackRestPublication
		fillAll(reflect.ValueOf(&native).Elem(), 0)
		native.Snapshot.Details.Type, native.Snapshot.Details.BarmanCloud = evidencev1alpha1.PGBackRestDetailsType, nil
		original := Publication{PGBackRest: &native}
		clone, err := clonePublication(original)
		if err != nil {
			t.Fatal(err)
		}
		assertDeepCopy(t, original, clone)
	})
	t.Run("options", func(t *testing.T) {
		t.Parallel()
		var original Options
		fillAll(reflect.ValueOf(&original).Elem(), 0)
		assertDeepCopy(t, original, cloneOptions(original))
	})
}

func assertDeepCopy[T any](t *testing.T, original, clone T) {
	t.Helper()
	if !reflect.DeepEqual(original, clone) {
		t.Fatal("the clone differs from the original")
	}
	for _, alias := range aliases(reflect.ValueOf(original), reflect.ValueOf(clone), reflect.TypeFor[T]().Name()) {
		t.Errorf("%s is shared with the original: deep-copy it in the clone", alias)
	}
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// fillAll sets every exported field reachable from v to a non-zero
// value: pointers allocated, slices of two, maps of one entry.
func fillAll(v reflect.Value, depth int) {
	if depth > 12 {
		return
	}
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(time.Date(2026, 11, 9, 7, 0, 0, 0, time.UTC)))
		return
	case v.Type() == rawType:
		v.Set(reflect.ValueOf(json.RawMessage(`{"k":1}`)))
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillAll(v.Elem(), depth+1)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillAll(v.Field(i), depth+1)
			}
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := range 2 {
			fillAll(v.Index(i), depth+1)
		}
	case reflect.Array:
		for i := range v.Len() {
			fillAll(v.Index(i), depth+1)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key, value := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillAll(key, depth+1)
		fillAll(value, depth+1)
		v.SetMapIndex(key, value)
	case reflect.String:
		v.SetString("s")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	default:
		// Interfaces, channels and functions are not wire types: left zero.
	}
}

// aliases lists the paths at which b shares memory with a: the same
// pointer, the same non-empty slice backing array or the same map.
func aliases(a, b reflect.Value, path string) []string {
	var out []string
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		out = append(out, aliases(a.Elem(), b.Elem(), path)...)
	case reflect.Slice:
		if a.Len() == 0 || b.Len() == 0 {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for i := range min(a.Len(), b.Len()) {
			out = append(out, aliases(a.Index(i), b.Index(i), path+"[]")...)
		}
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return nil
		}
		if a.Pointer() == b.Pointer() {
			out = append(out, path)
		}
		for _, key := range a.MapKeys() {
			if bv := b.MapIndex(key); bv.IsValid() {
				out = append(out, aliases(a.MapIndex(key), bv, path+"[key]")...)
			}
		}
	case reflect.Struct:
		if a.Type() == timeType {
			return nil
		}
		for i := range a.NumField() {
			out = append(out, aliases(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name)...)
		}
	case reflect.Array:
		for i := range a.Len() {
			out = append(out, aliases(a.Index(i), b.Index(i), path+"[]")...)
		}
	case reflect.Interface:
		if !a.IsNil() && !b.IsNil() {
			out = append(out, aliases(a.Elem(), b.Elem(), path)...)
		}
	default:
		// A scalar is copied by value: nothing to share.
	}
	return out
}

// TestAliasDetectionFindsAShallowCopy is the walker's own negative: a
// shallow copy is reported, at the field that is shared.
func TestAliasDetectionFindsAShallowCopy(t *testing.T) {
	t.Parallel()
	var original Publication
	fillAll(reflect.ValueOf(&original).Elem(), 0)
	shallow := original
	found := aliases(reflect.ValueOf(original), reflect.ValueOf(shallow), "Publication")
	joined := strings.Join(found, "\n")
	for _, want := range []string{"Publication.Backups", "Publication.Snapshot.StartedAt", "Publication.PGBackRest"} {
		if !strings.Contains(joined, want+"\n") && !strings.HasSuffix(joined, want) {
			t.Errorf("a shallow copy shares %s, and the walker did not say so:\n%s", want, joined)
		}
	}
}
