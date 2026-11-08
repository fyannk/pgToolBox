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

package pgbackrest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixturePassphrase is the repository passphrase the generator gives the
// encrypted and format-6 repositories (hack/evidence/pgbackrest-generator).
// TEST ONLY: it protects nothing but fixtures.
const fixturePassphrase = "pgtoolbox-fixture-test-only-passphrase"

const fixtureRoot = "testdata/pgbackrest-2.60.0"

var fixtureVariants = []string{"plain", "encrypted", "format6"}

// fixtureRepository is one reduced repository: its listed objects (the
// archive listing plus every copied file) and the content of the copied
// files. WAL segments are listed but have no content: opening one fails the
// test, which is how the "names only" rule is enforced.
type fixtureRepository struct {
	objects []store.Object
	content map[string][]byte

	mu     sync.Mutex
	opened []string
}

func loadFixture(t testing.TB, root, variant string) *fixtureRepository {
	t.Helper()
	base := filepath.Join(root, variant)
	repository := &fixtureRepository{content: make(map[string][]byte)}
	listed := make(map[string]store.Object)
	if listing, err := os.ReadFile(filepath.Join(base, "listing.tsv")); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(listing))
		for scanner.Scan() {
			fields := strings.Split(scanner.Text(), "\t")
			if len(fields) != 3 {
				t.Fatalf("listing line %q", scanner.Text())
			}
			size, errSize := strconv.ParseInt(fields[1], 10, 64)
			epoch, errTime := strconv.ParseInt(fields[2], 10, 64)
			if errSize != nil || errTime != nil {
				t.Fatalf("listing line %q", scanner.Text())
			}
			listed[fields[0]] = store.Object{Key: fields[0], Size: size, LastModified: time.Unix(epoch, 0).UTC()}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		key, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		key = filepath.ToSlash(key)
		if key == "listing.tsv" || key == "pgbackrest-info.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		repository.content[key] = data
		if _, ok := listed[key]; !ok {
			listed[key] = store.Object{Key: key, Size: int64(len(data))}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range listed {
		repository.objects = append(repository.objects, object)
	}
	sort.Slice(repository.objects, func(i, j int) bool { return repository.objects[i].Key < repository.objects[j].Key })
	return repository
}

func (r *fixtureRepository) reader(t testing.TB) store.Reader {
	return &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		r.mu.Lock()
		r.opened = append(r.opened, request.Key)
		r.mu.Unlock()
		data, ok := r.content[request.Key]
		if !ok {
			t.Errorf("the reader opened %s, which is not a metadata file", request.Key)
			return nil, errors.New("not in the fixture")
		}
		if int64(len(data)) > request.MaxBytes {
			t.Errorf("the reader opened %s with MaxBytes %d below its size %d", request.Key, request.MaxBytes, len(data))
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
}

func (r *fixtureRepository) without(keys ...string) *fixtureRepository {
	clone := &fixtureRepository{content: make(map[string][]byte)}
	drop := make(map[string]bool)
	for _, key := range keys {
		drop[key] = true
	}
	for _, object := range r.objects {
		if !drop[object.Key] {
			clone.objects = append(clone.objects, object)
		}
	}
	for key, data := range r.content {
		if !drop[key] {
			clone.content[key] = data
		}
	}
	return clone
}

func (r *fixtureRepository) with(key string, data []byte) *fixtureRepository {
	clone := r.without()
	clone.content[key] = data
	found := false
	for index := range clone.objects {
		if clone.objects[index].Key == key {
			clone.objects[index].Size = int64(len(data))
			found = true
		}
	}
	if !found {
		clone.objects = append(clone.objects, store.Object{Key: key, Size: int64(len(data))})
		sort.Slice(clone.objects, func(i, j int) bool { return clone.objects[i].Key < clone.objects[j].Key })
	}
	return clone
}

func fixturePassphraseFor(variant string) []byte {
	if variant == "plain" || strings.HasPrefix(variant, "retention-") {
		return nil
	}
	return []byte(fixturePassphrase)
}

func analyzeFixture(t testing.TB, repository *fixtureRepository, variant string) Report {
	t.Helper()
	return Analyze(context.Background(), repository.reader(t), repository.objects, Options{Stanza: "demo", Passphrase: fixturePassphraseFor(variant), Generation: 1})
}

func readJSONGolden(t testing.TB, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func compareJSONGolden(t testing.TB, path string, value any) {
	t.Helper()
	actual, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual = append(actual, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	expected, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(expected, actual) {
		t.Fatalf("%s is stale; run go test ./internal/evidence/formats/pgbackrest -update and review the diff\n%s", path, firstDifference(expected, actual))
	}
}

func firstDifference(expected, actual []byte) string {
	left, right := strings.Split(string(expected), "\n"), strings.Split(string(actual), "\n")
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			return "line " + strconv.Itoa(index+1) + ":\n  golden: " + left[index] + "\n  actual: " + right[index]
		}
	}
	return "lengths differ"
}

type storeObject = store.Object

func formatUint64(value uint64) string { return strconv.FormatUint(value, 10) }
