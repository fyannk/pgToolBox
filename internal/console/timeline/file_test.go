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

package timeline

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/timeline/boltfile"
)

// writeFile makes a timeline file holding n events and closes it.
func writeFile(t *testing.T, path string, c *clock, n int) {
	t.Helper()
	file, err := Open(path, c, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, err := NewPersisted(Limits{}, c, file)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		j.Record(Event{Kind: KindBackup, Action: ActionCompleted, At: c.Now(), Namespace: "a", Cluster: "orders", Key: "b/" + strings.Repeat("x", i%40) + string(rune('a'+i%26)), Detail: strings.Repeat("d", 200)})
	}
	if err := file.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSetAside(t *testing.T, path string, c *clock, want []byte) *File {
	t.Helper()
	modTime := time.Date(2026, 11, 2, 22, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	file, err := Open(path, c, c, nil)
	if err != nil {
		t.Fatalf("a corrupt file must not stop the console: %v", err)
	}
	if file.SetAside() == "" || !strings.HasPrefix(file.SetAside(), "timeline.db.corrupt-") {
		t.Fatalf("set aside as %q", file.SetAside())
	}
	kept, err := os.ReadFile(filepath.Join(filepath.Dir(path), file.SetAside()))
	if err != nil || !bytes.Equal(kept, want) {
		t.Fatalf("the corrupt file must be kept intact beside the new one (err %v)", err)
	}
	if !file.PreviousAlive().Equal(modTime) {
		t.Fatalf("previous life bounded by %v, want the file's last write %v", file.PreviousAlive(), modTime)
	}
	j, err := NewPersisted(Limits{}, c, file)
	if err != nil {
		t.Fatal(err)
	}
	NewRecorderWithStart(j, file.PreviousAlive(), "set aside")
	start := j.All()[0]
	if start.Kind != KindConsole || !start.Since.Equal(modTime) || start.Detail != "set aside" {
		t.Fatalf("the start must record the loss: %+v", start)
	}
	j.Record(Event{Kind: KindBackup, Action: ActionCompleted, At: c.Now(), Namespace: "a", Cluster: "orders", Key: "new"})
	if err := file.Flush(); err != nil {
		t.Fatalf("the new file must be writable: %v", err)
	}
	return file
}

// TestOpenSetsAsideAFileThatIsNotBbolt: garbage in place of the file.
func TestOpenSetsAsideAFileThatIsNotBbolt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeline.db")
	junk := bytes.Repeat([]byte("not a bbolt file "), 1000)
	if err := os.WriteFile(path, junk, 0o600); err != nil {
		t.Fatal(err)
	}
	file := assertSetAside(t, path, &clock{now: t0}, junk)
	_ = file.db.Close()
}

// TestOpenSetsAsideAFileWithADamagedPage: valid meta pages, a data page
// overwritten — the probe finds it at start, not a later panic.
func TestOpenSetsAsideAFileWithADamagedPage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeline.db")
	c := &clock{now: t0}
	writeFile(t, path, c, 300)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := os.Getpagesize()
	if len(raw) < 6*page {
		t.Fatalf("file too small to damage a data page: %d", len(raw))
	}
	for i := 2 * page; i < len(raw); i++ {
		raw[i] = 0xff
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	file := assertSetAside(t, path, c, raw)
	_ = file.db.Close()
}

// TestOpenNeverMovesALockedFile: another process holding the file is not
// corruption; the start fails and the file stays where it is.
func TestOpenNeverMovesALockedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "timeline.db")
	held, _, err := boltfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, err := Open(path, &clock{now: t0}, &clock{now: t0}, nil); err == nil {
		t.Fatal("a locked file must fail the start")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("a locked file must not be moved: %v", entries)
	}
}

// TestLoadSkipsAnUndecodableEvent: one bad record costs that record, and
// the journal then treats the file as evicted (no invented changes).
func TestLoadSkipsAnUndecodableEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeline.db")
	c := &clock{now: t0}
	writeFile(t, path, c, 2)
	db, _, err := boltfile.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	good, _ := json.Marshal(Event{Seq: 50, Kind: KindBackup, Action: ActionFailed, At: t0, Namespace: "a", Cluster: "orders"})
	if err := db.Write(boltfile.Batch{Events: map[uint64][]byte{40: []byte("{not json"), 50: good}}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	file, err := Open(path, c, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.db.Close() }()
	events, _, evicted, err := file.LoadEvents()
	if err != nil {
		t.Fatalf("an undecodable event must not fail the load: %v", err)
	}
	if len(events) != 3 || !evicted {
		t.Fatalf("got %d events, evicted %v; want 3, true", len(events), evicted)
	}
}
