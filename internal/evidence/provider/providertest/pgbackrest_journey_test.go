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
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestPGBackRestManifestMatchesJourneyProfile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/pgbackrest-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Profile      string `json:"profile"`
		Format       string `json:"format"`
		Stanza       string `json:"stanza"`
		Repositories []struct {
			RepoPath    string `json:"repo_path"`
			StorePrefix string `json:"store_prefix"`
			Encrypted   bool   `json:"encrypted"`
		} `json:"repositories"`
		Scenarios []struct {
			Name string `json:"name"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Profile != PGBackRestFixtureProfile || manifest.Format != "pgbackrest" || manifest.Stanza != "demo" || len(manifest.Repositories) != 3 || len(manifest.Scenarios) != 3 {
		t.Fatalf("manifest %+v", manifest)
	}
	for _, repository := range manifest.Repositories {
		// The mapping the journeys proved: the store prefix is the repo path
		// without its leading slash.
		if strings.TrimPrefix(repository.RepoPath, "/") != repository.StorePrefix {
			t.Fatalf("repository %+v", repository)
		}
		want := PGBackRestRepositoryPrefix
		if repository.Encrypted {
			want = PGBackRestEncryptedPrefix
		}
		if repository.StorePrefix != want {
			t.Fatalf("repository %+v", repository)
		}
	}
}
