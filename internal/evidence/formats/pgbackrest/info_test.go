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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// infoOracle is the part of `pgbackrest info --output=json` the reader is
// compared with: what pgBackRest itself makes of the same repository.
type infoOracle struct {
	Archive []struct {
		ID  string `json:"id"`
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"archive"`
	Backup []struct {
		Annotation map[string]string `json:"annotation"`
		Archive    struct {
			Start string `json:"start"`
			Stop  string `json:"stop"`
		} `json:"archive"`
		Backrest struct {
			Format  int    `json:"format"`
			Version string `json:"version"`
		} `json:"backrest"`
		Database struct {
			ID uint32 `json:"id"`
		} `json:"database"`
		Error bool `json:"error"`
		Info  struct {
			Delta      uint64 `json:"delta"`
			Size       uint64 `json:"size"`
			Repository struct {
				Delta uint64 `json:"delta"`
				Size  uint64 `json:"size"`
			} `json:"repository"`
		} `json:"info"`
		Label string `json:"label"`
		LSN   struct {
			Start string `json:"start"`
			Stop  string `json:"stop"`
		} `json:"lsn"`
		Prior     *string  `json:"prior"`
		Reference []string `json:"reference"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		Type string `json:"type"`
	} `json:"backup"`
	Cipher string `json:"cipher"`
	DB     []struct {
		ID       uint32      `json:"id"`
		SystemID json.Number `json:"system-id"`
		Version  string      `json:"version"`
	} `json:"db"`
}

func readOracle(t testing.TB, variant string) infoOracle {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureRoot, variant, "pgbackrest-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var stanzas []infoOracle
	if err := decoder.Decode(&stanzas); err != nil || len(stanzas) != 1 {
		t.Fatalf("oracle: %v (%d stanzas)", err, len(stanzas))
	}
	return stanzas[0]
}

func loadInfoPair(t testing.TB, variant string) (*BackupInfo, *ArchiveInfo) {
	t.Helper()
	repository := loadFixture(t, fixtureRoot, variant)
	listed := make(map[string]storeObject)
	for _, object := range repository.objects {
		listed[object.Key] = object
	}
	backup, backupCheck := LoadBackupInfo(context.Background(), repository.reader(t), listed, "demo", fixturePassphraseFor(variant))
	archive, archiveCheck := LoadArchiveInfo(context.Background(), repository.reader(t), listed, "demo", fixturePassphraseFor(variant))
	if backup == nil || archive == nil || backupCheck.Used != SourceMain || !backupCheck.Agree || archiveCheck.Used != SourceMain || !archiveCheck.Agree {
		t.Fatalf("%s: backup.info %+v, archive.info %+v", variant, backupCheck, archiveCheck)
	}
	return backup, archive
}

// The reader's view of backup.info and archive.info must be pgBackRest's own.
func TestInfoFilesMatchPgBackRestInfoOutput(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"plain", "encrypted"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			oracle := readOracle(t, variant)
			backup, archive := loadInfoPair(t, variant)
			wantCipher := map[string]string{"plain": "none", "encrypted": "aes-256-cbc"}[variant]
			if oracle.Cipher != wantCipher || backup.Encrypted != (variant == "encrypted") || archive.Encrypted != backup.Encrypted {
				t.Fatalf("cipher %q, backup.info encrypted %v, archive.info encrypted %v", oracle.Cipher, backup.Encrypted, archive.Encrypted)
			}
			if len(oracle.DB) != 1 || oracle.DB[0].SystemID.String() != formatUint64(backup.Current.SystemID) || oracle.DB[0].Version != backup.Current.Version || oracle.DB[0].ID != backup.Current.ID {
				t.Fatalf("db = %+v, reader %+v", oracle.DB, backup.Current)
			}
			if archive.Current.SystemID != backup.Current.SystemID || !reflect.DeepEqual(archive.ArchiveIDs(), []string{oracle.Archive[0].ID}) {
				t.Fatalf("archive ids %v, oracle %+v", archive.ArchiveIDs(), oracle.Archive)
			}
			if len(oracle.Backup) != len(backup.Backups) {
				t.Fatalf("%d backups, oracle %d", len(backup.Backups), len(oracle.Backup))
			}
			for index, want := range oracle.Backup {
				got := backup.Backups[index]
				prior := ""
				if want.Prior != nil {
					prior = *want.Prior
				}
				if got.Label != want.Label || string(got.Type) != want.Type || got.Prior != prior || !reflect.DeepEqual(got.Reference, want.Reference) ||
					got.ArchiveStart != want.Archive.Start || got.ArchiveStop != want.Archive.Stop || got.LSNStart != want.LSN.Start || got.LSNStop != want.LSN.Stop ||
					got.TimestampStart.Unix() != want.Timestamp.Start || got.TimestampStop.Unix() != want.Timestamp.Stop ||
					got.BackrestFormat != want.Backrest.Format || got.BackrestVersion != want.Backrest.Version || got.DBID != want.Database.ID ||
					got.Error == nil || *got.Error != want.Error || got.Size != want.Info.Size || got.SizeDelta != want.Info.Delta ||
					got.RepoSize != want.Info.Repository.Size || got.RepoSizeDelta != want.Info.Repository.Delta || !reflect.DeepEqual(got.Annotations, want.Annotation) {
					t.Fatalf("backup %d:\nreader %+v\noracle %+v", index, got, want)
				}
				if got.TimestampStart.Location() != time.UTC {
					t.Fatal("timestamps are not UTC")
				}
			}
		})
	}
}

func TestBackupEntryParserFollowsInfoBackupFields(t *testing.T) {
	t.Parallel()
	const label = "20261107-081046F_20261107-081101D"
	valid := `{"backrest-format":5,"backrest-version":"2.60.0","backup-annotation":{"cnpg-backup-name":"b","bad":"\u0001"},"backup-archive-start":"000000010000000000000008","backup-archive-stop":"000000010000000000000009","backup-error":false,"backup-info-repo-size":3551751,"backup-info-repo-size-delta":730489,"backup-info-size":24601678,"backup-info-size-delta":2261250,"backup-lsn-start":"0/8000028","backup-lsn-stop":"0/9009A88","backup-prior":"20261107-081046F","backup-reference":["20261107-081046F"],"backup-timestamp-start":1794038156,"backup-timestamp-stop":1794038159,"backup-type":"diff","db-id":1,"option-archive-check":true,"option-archive-copy":false,"option-backup-standby":false,"option-checksum-page":false,"option-compress":true,"option-hardlink":false,"option-online":true,"a-later-field":1}`
	backup, err := parseBackupEntry(label, valid)
	if err != nil {
		t.Fatal(err)
	}
	if backup.Type != BackupDifferential || backup.Prior != "20261107-081046F" || backup.AnnotationsWithheld != 1 || backup.Annotations["cnpg-backup-name"] != "b" || !backup.OptionCompress || backup.OptionHardlink {
		t.Fatalf("parsed %+v", backup)
	}
	tests := map[string]struct {
		label, raw string
	}{
		"label that is not pgBackRest's":   {"backup-1", valid},
		"type that disagrees with label":   {strings.Replace(label, "D", "I", 1), valid},
		"missing required size":            {label, strings.Replace(valid, `"backup-info-size":24601678,`, "", 1)},
		"missing an option":                {label, strings.Replace(valid, `"option-online":true,`, "", 1)},
		"lower-case WAL name":              {label, strings.Replace(valid, "000000010000000000000008", "00000001000000000000000a", 1)},
		"LSN with a leading zero":          {label, strings.Replace(valid, "0/8000028", "0/08000028", 1)},
		"prior that is not a label":        {label, strings.Replace(valid, `"backup-prior":"20261107-081046F"`, `"backup-prior":"x"`, 1)},
		"reference that is not a label":    {label, strings.Replace(valid, `["20261107-081046F"]`, `["../x"]`, 1)},
		"stop before start":                {label, strings.Replace(valid, "1794038159", "1794038150", 1)},
		"zero start":                       {label, strings.Replace(valid, "1794038156", "0", 1)},
		"start beyond any epoch":           {label, strings.Replace(valid, "1794038156", "18446744073709551615", 1)},
		"format beyond int32":              {label, strings.Replace(valid, `"backrest-format":5`, `"backrest-format":4294967295`, 1)},
		"negative size":                    {label, strings.Replace(valid, "3551751", "-1", 1)},
		"size map without its delta":       {label, strings.Replace(valid, `"backup-info-size":`, `"backup-info-repo-size-map":1,"backup-info-size":`, 1)},
		"not an object":                    {label, `[1]`},
		"backup-error that is not boolean": {label, strings.Replace(valid, `"backup-error":false`, `"backup-error":"no"`, 1)},
	}
	for name, test := range tests {
		if _, err := parseBackupEntry(test.label, test.raw); !errors.Is(err, errMalformedINI) {
			t.Errorf("%s: parseBackupEntry() error = %v", name, err)
		}
	}
}

func TestLabelTypeAndVersionNumber(t *testing.T) {
	t.Parallel()
	for label, want := range map[string]BackupType{
		"20261107-081046F":                  BackupFull,
		"20261107-081046F_20261107-081101D": BackupDifferential,
		"20261107-081046F_20261107-081105I": BackupIncremental,
		"20261107-081046D":                  "",
		"20261107-081046F_20261107-081105F": "",
		"2026117-081046F":                   "",
		"20261107-081046F_":                 "",
	} {
		got, _, ok := LabelType(label)
		if got != want || ok != (want != "") {
			t.Errorf("LabelType(%q) = %q, %v", label, got, ok)
		}
	}
	for version, want := range map[string]int{"17": 170000, "10": 100000, "9.6": 90600, "9.5": 90500, "17.1": 0, "9": 0, "09.6": 0, "": 0, "x": 0, "9.10": 0} {
		got, ok := PostgreSQLVersionNumber(version)
		if got != want || ok != (want != 0) {
			t.Errorf("PostgreSQLVersionNumber(%q) = %d, %v", version, got, ok)
		}
	}
}

// rewriteINI applies edit to a plain info file's body and recomputes its
// checksum, so the result is a different but valid file.
func rewriteINI(t testing.TB, data []byte, edit func(string) string) []byte {
	t.Helper()
	body := string(data)
	index := strings.Index(body, "\n[backrest]\nbackrest-checksum=")
	if index < 0 {
		t.Fatal("no checksum section")
	}
	return withChecksum(t, edit(body[:index]))
}

func TestInfoPairFallsBackToCopyAndReportsDisagreement(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	main := plain.content["backup/demo/backup.info"]
	other := rewriteINI(t, main, func(body string) string {
		return strings.Replace(body, `"backup-info-size":`, `"backup-info-size":1`, 1)
	})
	original, err := ParseBackupInfo(bytes.NewReader(main))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(main, []byte(`"db-id":1`), []byte(`"db-id":2`), 1)
	encrypted := loadFixture(t, fixtureRoot, "encrypted")
	tests := []struct {
		name        string
		repository  *fixtureRepository
		passphrase  []byte
		wantUsed    FileSource
		wantAgree   bool
		wantMain    FileProblem
		wantCopy    FileProblem
		wantChanged bool
	}{
		{name: "both verify and agree", repository: plain, wantUsed: SourceMain, wantAgree: true},
		{name: "copy missing", repository: plain.without("backup/demo/backup.info.copy"), wantUsed: SourceMain, wantCopy: ProblemMissing},
		{name: "main missing", repository: plain.without("backup/demo/backup.info"), wantUsed: SourceCopy, wantMain: ProblemMissing},
		{name: "main corrupt", repository: plain.with("backup/demo/backup.info", corrupt), wantUsed: SourceCopy, wantMain: ProblemChecksum},
		{name: "copy differs but verifies", repository: plain.with("backup/demo/backup.info.copy", other), wantUsed: SourceMain},
		{name: "main differs, copy used only when main fails", repository: plain.with("backup/demo/backup.info", other), wantUsed: SourceMain, wantChanged: true},
		{name: "both missing", repository: plain.without("backup/demo/backup.info", "backup/demo/backup.info.copy"), wantMain: ProblemMissing, wantCopy: ProblemMissing},
		{name: "encrypted without passphrase", repository: encrypted, wantMain: ProblemEncrypted, wantCopy: ProblemEncrypted},
		{name: "encrypted with a wrong passphrase", repository: encrypted, passphrase: []byte("wrong"), wantMain: ProblemDecryption, wantCopy: ProblemDecryption},
		{name: "encrypted with the passphrase", repository: encrypted, passphrase: []byte(fixturePassphrase), wantUsed: SourceMain, wantAgree: true},
		{name: "a passphrase on a plain repository is not needed", repository: plain, passphrase: []byte(fixturePassphrase), wantUsed: SourceMain, wantAgree: true},
		{name: "oversized object", repository: plain.with("backup/demo/backup.info", bytes.Repeat([]byte{'a'}, int(MaxInfoFileBytes)+64)), wantUsed: SourceCopy, wantMain: ProblemTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			listed := make(map[string]storeObject)
			for _, object := range test.repository.objects {
				listed[object.Key] = object
			}
			info, check := LoadBackupInfo(context.Background(), test.repository.reader(t), listed, "demo", test.passphrase)
			if check.Used != test.wantUsed || check.Agree != test.wantAgree || check.Main.Problem != test.wantMain || check.Copy.Problem != test.wantCopy {
				t.Fatalf("check = %+v", check)
			}
			if (info != nil) != (test.wantUsed != "") {
				t.Fatalf("info = %v", info)
			}
			if info != nil && test.wantChanged != (info.Backups[0].Size != original.Backups[0].Size) {
				t.Fatalf("the wrong file was used: size %d", info.Backups[0].Size)
			}
		})
	}
}

func TestInfoParsersRefuseOtherFormatsAndInconsistentDB(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	backupInfo := plain.content["backup/demo/backup.info"]
	archiveInfo := plain.content["archive/demo/archive.info"]
	formatSix := rewriteINI(t, backupInfo, func(body string) string { return strings.Replace(body, "backrest-format=5", "backrest-format=6", 1) })
	var formatErr FormatError
	if _, err := ParseBackupInfo(bytes.NewReader(formatSix)); !errors.As(err, &formatErr) || formatErr.Format != 6 {
		t.Fatalf("ParseBackupInfo(format 6) = %v", err)
	}
	for name, edit := range map[string]func(string) string{
		"db-id not in history": func(body string) string { return strings.Replace(body, "\ndb-id=1\n", "\ndb-id=2\n", 1) },
		"db disagrees": func(body string) string {
			return strings.Replace(body, "\ndb-version=\"17\"\n", "\ndb-version=\"16\"\n", 1)
		},
		"no history":              func(body string) string { before, _, _ := strings.Cut(body, "[db:history]"); return before },
		"history id not a number": func(body string) string { return strings.Replace(body, "\n1={\"db-catalog", "\nx={\"db-catalog", 1) },
		"empty cipher-pass":       func(body string) string { return body + "\n[cipher]\ncipher-pass=\"\"\n" },
	} {
		if _, err := ParseBackupInfo(bytes.NewReader(rewriteINI(t, backupInfo, edit))); !errors.Is(err, errMalformedINI) {
			t.Errorf("%s: ParseBackupInfo() = %v", name, err)
		}
	}
	archive, err := ParseArchiveInfo(bytes.NewReader(archiveInfo))
	if err != nil || !reflect.DeepEqual(archive.ArchiveIDs(), []string{"17-1"}) || archive.Current.VersionNumber != 170000 {
		t.Fatalf("ParseArchiveInfo() = %+v, %v", archive, err)
	}
	// archive.info names the system id db-id inside db:history; backup.info's key is refused there.
	swapped := rewriteINI(t, archiveInfo, func(body string) string { return strings.Replace(body, `{"db-id":`, `{"db-system-id":`, 1) })
	if _, err := ParseArchiveInfo(bytes.NewReader(swapped)); !errors.Is(err, errMalformedINI) {
		t.Fatalf("ParseArchiveInfo(backup.info history keys) = %v", err)
	}
}
