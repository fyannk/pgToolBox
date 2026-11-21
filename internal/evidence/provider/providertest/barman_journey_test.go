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

package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

type notFound struct{}

func (notFound) Error() string            { return "object not found" }
func (notFound) Category() fault.Category { return fault.NotFound }

var errNotFound error = notFound{}

// memoryStore serves objects from memory in one sorted page per prefix.
func memoryStore(objects []FixtureObject) store.Reader {
	data := map[string][]byte{}
	for _, object := range objects {
		data[object.Key] = object.Data
	}
	modified := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	stat := func(key string) (store.Object, error) {
		content, ok := data[key]
		if !ok {
			return store.Object{}, errNotFound
		}
		return store.Object{Key: key, Size: int64(len(content)), LastModified: modified, ETag: "etag"}, nil
	}
	return &storetest.Fake{
		ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			page := store.Page{}
			for key := range data {
				if strings.HasPrefix(key, request.Prefix) {
					object, _ := stat(key)
					page.Objects = append(page.Objects, object)
				}
			}
			sort.Slice(page.Objects, func(i, j int) bool { return page.Objects[i].Key < page.Objects[j].Key })
			return page, nil
		},
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			content, ok := data[request.Key]
			if !ok {
				return nil, errNotFound
			}
			return io.NopCloser(bytes.NewReader(content)), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) { return stat(key) },
	}
}

// The journey runs hermetically too: the provider integration tests add
// only the provider under it.
func TestBarmanCatalogJourneyInMemory(t *testing.T) {
	t.Parallel()
	result := BarmanCatalogJourney(t, memoryStore(BarmanFixture(t)))
	if !slices.Equal(result.ServedStates, []evidence.State{evidence.Healthy, evidence.Warning, evidence.Unhealthy, evidence.Unknown}) {
		t.Fatalf("served states = %v", result.ServedStates)
	}
}

func TestBarmanFixtureManifestMatchesJourneyProfile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Profile    string `json:"profile"`
		Format     string `json:"format"`
		WALContext struct {
			PostgreSQLVersion int64 `json:"postgresql_version"`
			SegmentSize       int64 `json:"segment_size"`
		} `json:"wal_context"`
		Scenarios []struct {
			Name          string `json:"name"`
			ExpectedState string `json:"expected_state"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Profile != FixtureProfile || manifest.Format != "barman-cloud" || manifest.WALContext.PostgreSQLVersion != 170005 || manifest.WALContext.SegmentSize != 16<<20 || len(manifest.Scenarios) != 6 {
		t.Fatalf("fixture manifest = %#v", manifest)
	}
	wantStates := map[string]string{"completed": "healthy", "started": "warning", "failed": "unhealthy", "malformed": "unknown", "missing-artifact": "unhealthy", "missing-info": "unknown"}
	for _, scenario := range manifest.Scenarios {
		if wantStates[scenario.Name] != scenario.ExpectedState {
			t.Fatalf("scenario %q state = %q", scenario.Name, scenario.ExpectedState)
		}
		delete(wantStates, scenario.Name)
	}
	if len(wantStates) != 0 {
		t.Fatalf("fixture manifest is missing scenarios: %#v", wantStates)
	}
	objects := BarmanFixture(t)
	if len(objects) != 12 {
		t.Fatalf("fixture objects = %d, want 12", len(objects))
	}
}
