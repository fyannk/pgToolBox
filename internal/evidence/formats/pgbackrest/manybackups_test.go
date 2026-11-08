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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// manyFixtureRoot is a stanza of 260 backups written by real pgBackRest
// 2.60.0 (hack/evidence/pgbackrest-generator, mode pgbackrest-many-backups).
const manyFixtureRoot = "testdata/pgbackrest-2.60.0-many"

// infoRepository serves a stanza's backup.info and archive.info, each with a
// byte-identical .copy, as pgBackRest writes them; nothing else is listed.
func infoRepository(t testing.TB, backupInfo, archiveInfo []byte) ([]store.Object, store.Reader) {
	t.Helper()
	content := map[string][]byte{
		"backup/demo/backup.info":        backupInfo,
		"backup/demo/backup.info.copy":   backupInfo,
		"archive/demo/archive.info":      archiveInfo,
		"archive/demo/archive.info.copy": archiveInfo,
	}
	objects := make([]store.Object, 0, len(content))
	for key, data := range content {
		objects = append(objects, store.Object{Key: key, Size: int64(len(data))})
	}
	reader := &storetest.Fake{OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
		data, ok := content[request.Key]
		if !ok {
			t.Errorf("the reader opened %s, which is not listed", request.Key)
			return nil, errors.New("not listed")
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
	return objects, reader
}

func countFindings(findings []Finding, code FindingCode) int {
	count := 0
	for _, finding := range findings {
		if finding.Code == code {
			count++
		}
	}
	return count
}

func statedChecksum(t testing.TB, data []byte) string {
	t.Helper()
	index := bytes.LastIndex(data, []byte("\nbackrest-checksum=\""))
	if index < 0 {
		t.Fatal("no backrest-checksum line")
	}
	rest := data[index+len("\nbackrest-checksum=\""):]
	return string(rest[:bytes.IndexByte(rest, '"')])
}

// A long-lived stanza's backup.info, as real pgBackRest writes it, verifies
// and is read in full: 260 backups, 1.1 MiB, one line longer than the
// reader's 64 KiB buffer, annotations with escaped and non-ASCII
// characters. Regression test for the pentest's 200-backup
// ChecksumMismatch, which was a tooling artefact (test/pentest/README.md):
// the reader's hashing has no size-dependent step, and this pins it.
func TestManyRealBackupsVerify(t *testing.T) {
	t.Parallel()
	compressed, err := os.ReadFile(filepath.Join(manyFixtureRoot, "backup/demo/backup.info.gz"))
	if err != nil {
		t.Fatal(err)
	}
	unzip, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	backupInfo, err := io.ReadAll(unzip)
	if err != nil {
		t.Fatal(err)
	}
	archiveInfo, err := os.ReadFile(filepath.Join(manyFixtureRoot, "archive/demo/archive.info"))
	if err != nil {
		t.Fatal(err)
	}
	labels, err := os.ReadFile(filepath.Join(manyFixtureRoot, "labels.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(string(labels))

	// The fixture keeps exercising what it was made for.
	longest := 0
	for line := range bytes.SplitSeq(backupInfo, []byte{'\n'}) {
		longest = max(longest, len(line))
	}
	if len(want) < 250 || len(backupInfo) < 1<<20 || longest <= 64*1024 {
		t.Fatalf("fixture: %d backups, %d bytes, longest line %d; want >= 250, >= 1 MiB, > 64 KiB", len(want), len(backupInfo), longest)
	}
	for _, escaped := range []string{`\"`, `\\`, `\t`, "€"} {
		if !bytes.Contains(backupInfo, []byte(escaped)) {
			t.Errorf("fixture: no %q in backup.info", escaped)
		}
	}

	checksum, err := INIChecksum(bytes.NewReader(backupInfo), MaxInfoFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if stated := statedChecksum(t, backupInfo); checksum != stated {
		t.Fatalf("INIChecksum = %s, pgBackRest wrote %s", checksum, stated)
	}
	info, err := ParseBackupInfo(bytes.NewReader(backupInfo))
	if err != nil {
		t.Fatalf("ParseBackupInfo: %v", err)
	}
	got := make([]string, 0, len(info.Backups))
	for _, backup := range info.Backups {
		got = append(got, backup.Label)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("backup.info has %d backups, pgbackrest info lists %d", len(got), len(want))
	}

	objects, reader := infoRepository(t, backupInfo, archiveInfo)
	report := Analyze(context.Background(), reader, objects, Options{Stanza: "demo"})
	for _, code := range []FindingCode{FindingChecksumMismatch, FindingInfoUnreadable, FindingCopyMismatch} {
		if count := countFindings(report.Findings, code); count != 0 {
			t.Errorf("%d %s findings on a real backup.info", count, code)
		}
	}
	if !report.BackupInfo.Agree || len(report.Backups) != len(want) {
		t.Fatalf("backup.info pair agree=%v, %d backups reported; want agreement and %d", report.BackupInfo.Agree, len(report.Backups), len(want))
	}
}

// craftedChain is test/pentest/fixtures/make_budget_backupinfo.py in Go: one
// full and count-1 incrementals in a single linear chain, signed.
func craftedChain(t testing.TB, count int) []byte {
	t.Helper()
	var body strings.Builder
	body.WriteString("[backrest]\nbackrest-format=5\nbackrest-version=\"2.60.0\"\n\n[backup:current]\n")
	full := "20261107-000000F"
	prior := ""
	for index := range count {
		label, kind := full, "full"
		if index > 0 {
			label, kind = fmt.Sprintf("%s_20261107-%06dI", full, index), "incr"
		}
		entry := map[string]any{
			"backrest-format": 5, "backrest-version": "2.60.0",
			"backup-archive-start": "000000010000000000000003", "backup-archive-stop": "000000010000000000000004",
			"backup-error": false, "backup-info-repo-size": 1000, "backup-info-repo-size-delta": 1000,
			"backup-info-size": 1000, "backup-info-size-delta": 1000,
			"backup-lsn-start": "0/3000028", "backup-lsn-stop": "0/4000050",
			"backup-timestamp-start": 1794039046 + index*10, "backup-timestamp-stop": 1794039048 + index*10,
			"backup-type": kind, "db-id": 1,
			"option-archive-check": true, "option-archive-copy": false, "option-backup-standby": false,
			"option-checksum-page": false, "option-compress": true, "option-hardlink": false, "option-online": true,
		}
		if prior != "" {
			entry["backup-prior"] = prior
			entry["backup-reference"] = []string{prior}
		}
		value, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(label + "=" + string(value) + "\n")
		prior = label
	}
	body.WriteString("\n[db]\ndb-catalog-version=202406281\ndb-control-version=1700\ndb-id=1\ndb-system-id=7705338956479107096\ndb-version=\"17\"\n\n")
	body.WriteString("[db:history]\n1={\"db-catalog-version\":202406281,\"db-control-version\":1700,\"db-system-id\":7705338956479107096,\"db-version\":\"17\"}\n")
	return withChecksum(t, body.String())
}

// Pentest attack 5 (test/pentest/05-manifest-budget.sh): a crafted
// backup.info of 200 backups in one chain verifies, and the scan reads 128
// manifests (the newest DefaultMaxManifests and as many of their chain as
// the cap allows), then stops with ManifestBudgetExceeded.
func TestCraftedChainPastManifestCap(t *testing.T) {
	t.Parallel()
	const count = 200
	backupInfo := craftedChain(t, count)
	checksum, err := INIChecksum(bytes.NewReader(backupInfo), MaxInfoFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	if stated := statedChecksum(t, backupInfo); checksum != stated {
		t.Fatalf("INIChecksum = %s, the independent oracle %s", checksum, stated)
	}
	archiveInfo := withChecksum(t, "[backrest]\nbackrest-format=5\nbackrest-version=\"2.60.0\"\n\n[db]\ndb-id=1\ndb-system-id=7705338956479107096\ndb-version=\"17\"\n\n[db:history]\n1={\"db-id\":7705338956479107096,\"db-version\":\"17\"}\n")

	objects, reader := infoRepository(t, backupInfo, archiveInfo)
	report := Analyze(context.Background(), reader, objects, Options{Stanza: "demo"})
	if count := countFindings(report.Findings, FindingChecksumMismatch); count != 0 {
		t.Fatalf("%d ChecksumMismatch findings on a correctly signed crafted file", count)
	}
	if len(report.Backups) != count {
		t.Fatalf("%d backups reported, want %d", len(report.Backups), count)
	}
	maxReads := 2 * DefaultMaxManifests
	if got := countFindings(report.Findings, FindingManifestBudgetExceeded); got != 1 {
		t.Fatalf("%d ManifestBudgetExceeded findings, want 1; findings %v", got, report.Findings)
	}
	if got := countFindings(report.Findings, FindingManifestMissing); got != maxReads {
		t.Errorf("%d manifests looked up, want the cap %d", got, maxReads)
	}
	notRead := 0
	for _, backup := range report.Backups {
		if strings.HasPrefix(backup.Manifest.Reason, "not read: the scan reads at most") {
			notRead++
		}
	}
	if notRead != count-maxReads {
		t.Errorf("%d backups past the cap, want %d", notRead, count-maxReads)
	}
}

// INIChecksum computes what ParseINI verifies, whatever the file states.
func TestINIChecksum(t *testing.T) {
	t.Parallel()
	signed := withChecksum(t, "[backrest]\nbackrest-format=5\n\n[db]\ndb-id=1\n")
	want := statedChecksum(t, signed)
	zero := bytes.Replace(signed, []byte(want), []byte(strings.Repeat("0", 40)), 1)
	unsigned := signed[:bytes.LastIndex(signed, []byte("[backrest]\nbackrest-checksum="))]
	tests := []struct {
		name    string
		data    []byte
		wantErr bool
	}{
		{name: "signed", data: signed},
		{name: "zero checksum", data: zero},
		{name: "no checksum", data: unsigned},
		{name: "malformed", data: []byte("[db]\nkey=not json\n"), wantErr: true},
	}
	for _, test := range tests {
		got, err := INIChecksum(bytes.NewReader(test.data), MaxInfoFileBytes)
		if test.wantErr {
			if err == nil {
				t.Errorf("%s: INIChecksum succeeded", test.name)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: INIChecksum = %q, %v; want %q", test.name, got, err, want)
		}
	}
	if _, err := ParseINI(bytes.NewReader(zero), MaxInfoFileBytes, nil); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("ParseINI of the zero checksum = %v, want ErrChecksumMismatch", err)
	}
}
