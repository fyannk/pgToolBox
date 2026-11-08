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

// Package pgbackrestfixture serves the reduced pgBackRest 2.60.0
// repositories of ../testdata to the tests of other packages (inventory,
// evidence API, web) as a paginated, read-only store.Reader. Tests only.
package pgbackrestfixture

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// Passphrase is the repository passphrase of the encrypted and format-6
// fixtures (hack/evidence/pgbackrest-generator). TEST ONLY.
//
// #nosec G101 -- a published test-only value that protects only fixtures.
const Passphrase = "pgtoolbox-fixture-test-only-passphrase"

// Stanza is the fixtures' stanza.
const Stanza = "demo"

// Repository is one reduced repository: its listing and the content of the
// metadata files. WAL segments are listed without content.
type Repository struct {
	Objects []store.Object
	Content map[string][]byte
}

// Load reads one variant: "plain", "encrypted" or "format6".
func Load(t testing.TB, variant string) *Repository {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the fixtures")
	}
	base := filepath.Join(filepath.Dir(file), "..", "testdata", "pgbackrest-2.60.0", variant)
	repository := &Repository{Content: make(map[string][]byte)}
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
		// #nosec G122 -- the committed testdata tree of this repository, read
		// by tests only.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		repository.Content[key] = data
		if object, ok := listed[key]; ok {
			object.Size = int64(len(data))
			listed[key] = object
		} else {
			listed[key] = store.Object{Key: key, Size: int64(len(data)), LastModified: time.Unix(1_780_000_000, 0).UTC()}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range listed {
		repository.Objects = append(repository.Objects, object)
	}
	sort.Slice(repository.Objects, func(i, j int) bool { return repository.Objects[i].Key < repository.Objects[j].Key })
	return repository
}

// PassphraseFor returns the passphrase of variant, nil for "plain".
func PassphraseFor(variant string) []byte {
	if variant == "plain" {
		return nil
	}
	return []byte(Passphrase)
}

// Without returns a copy without keys.
func (r *Repository) Without(keys ...string) *Repository {
	drop := make(map[string]bool, len(keys))
	for _, key := range keys {
		drop[key] = true
	}
	clone := &Repository{Content: make(map[string][]byte, len(r.Content))}
	for _, object := range r.Objects {
		if !drop[object.Key] {
			clone.Objects = append(clone.Objects, object)
		}
	}
	for key, data := range r.Content {
		if !drop[key] {
			clone.Content[key] = data
		}
	}
	return clone
}

// Prefixed returns a copy whose keys sit under prefix (for example
// "other/"), to test confinement.
func (r *Repository) Prefixed(prefix string) *Repository {
	clone := &Repository{Content: make(map[string][]byte, len(r.Content))}
	for _, object := range r.Objects {
		object.Key = prefix + object.Key
		clone.Objects = append(clone.Objects, object)
	}
	for key, data := range r.Content {
		clone.Content[prefix+key] = data
	}
	return clone
}

// Merge returns the union of the repositories, sorted by key.
func Merge(repositories ...*Repository) *Repository {
	merged := &Repository{Content: make(map[string][]byte)}
	for _, repository := range repositories {
		merged.Objects = append(merged.Objects, repository.Objects...)
		for key, data := range repository.Content {
			merged.Content[key] = data
		}
	}
	sort.Slice(merged.Objects, func(i, j int) bool { return merged.Objects[i].Key < merged.Objects[j].Key })
	return merged
}

// Reader serves the repository: List pages by key order honouring Prefix,
// Cursor (the last key returned) and Limit; Open serves metadata only and
// fails the test when asked for anything else.
func (r *Repository) Reader(t testing.TB) store.Reader {
	return &storetest.Fake{
		ListFunc: func(_ context.Context, request store.ListRequest) (store.Page, error) {
			page := store.Page{}
			for _, object := range r.Objects {
				if !strings.HasPrefix(object.Key, request.Prefix) || (request.Cursor != "" && object.Key <= request.Cursor) {
					continue
				}
				if len(page.Objects) == request.Limit {
					page.NextCursor = page.Objects[len(page.Objects)-1].Key
					break
				}
				page.Objects = append(page.Objects, object)
			}
			return page, nil
		},
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			data, ok := r.Content[request.Key]
			if !ok {
				t.Errorf("opened %s, which is not a metadata file", request.Key)
				return nil, errors.New("not in the fixture")
			}
			return io.NopCloser(bytes.NewReader(data)), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			for _, object := range r.Objects {
				if object.Key == key {
					return object, nil
				}
			}
			return store.Object{}, errors.New("not in the fixture")
		},
	}
}
