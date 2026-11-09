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
	"errors"
	"slices"
	"strings"
	"testing"
)

// Every manifest real pgBackRest wrote opens (with backup.info's cipher-pass
// when encrypted) and agrees with its backup.info entry.
func TestManifestsAgreeWithBackupInfo(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"plain", "encrypted"} {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			repository := loadFixture(t, fixtureRoot, variant)
			info, _ := loadInfoPair(t, variant)
			listed := make(map[string]storeObject)
			for _, object := range repository.objects {
				listed[object.Key] = object
			}
			for _, backup := range info.Backups {
				manifest, check := LoadManifest(context.Background(), repository.reader(t), listed, "demo", backup.Label, info)
				if manifest == nil || check.Used != SourceMain || !check.Agree || check.Main.Encrypted != (variant == "encrypted") {
					t.Fatalf("%s: %+v", backup.Label, check)
				}
				if mismatch := manifestDisagreement(manifest, backup, info.History); mismatch != "" {
					t.Fatalf("%s: manifest disagrees on %s", backup.Label, mismatch)
				}
				if !slices.Contains(manifest.Reference, backup.Label) || len(manifest.Reference) != len(backup.Reference)+1 {
					t.Fatalf("%s: manifest references %v, backup.info %v", backup.Label, manifest.Reference, backup.Reference)
				}
				if manifest.Annotations["cnpg-backup-name"] == "" || manifest.Annotations["cnpg-backup-name"] != backup.Annotations["cnpg-backup-name"] {
					t.Fatalf("%s: annotations %v and %v", backup.Label, manifest.Annotations, backup.Annotations)
				}
				if manifest.TimestampCopyStart.Before(manifest.TimestampStart) || manifest.DB.SystemID != info.Current.SystemID {
					t.Fatalf("%s: %+v", backup.Label, manifest)
				}
			}
		})
	}
}

func TestEncryptedManifestDoesNotOpenWithTheRepositoryPassphrase(t *testing.T) {
	t.Parallel()
	repository := loadFixture(t, fixtureRoot, "encrypted")
	info, _ := loadInfoPair(t, "encrypted")
	listed := make(map[string]storeObject)
	for _, object := range repository.objects {
		listed[object.Key] = object
	}
	wrong := *info
	wrong.cipherPass = []byte(fixturePassphrase)
	manifest, check := LoadManifest(context.Background(), repository.reader(t), listed, "demo", info.Backups[0].Label, &wrong)
	if manifest != nil || check.Main.Problem != ProblemDecryption {
		t.Fatalf("manifest opened with the repository passphrase: %+v", check)
	}
}

func TestManifestDisagreementNamesTheField(t *testing.T) {
	t.Parallel()
	repository := loadFixture(t, fixtureRoot, "plain")
	info, _ := loadInfoPair(t, "plain")
	backup := info.Backups[1]
	manifest, err := ParseManifest(bytes.NewReader(repository.content[ManifestKey("demo", backup.Label)]))
	if err != nil {
		t.Fatal(err)
	}
	for want, mutate := range map[string]func(*Backup){
		"prior":         func(b *Backup) { b.Prior = info.Backups[2].Label },
		"archive range": func(b *Backup) { b.ArchiveStop = "000000010000000000000099" },
		"LSN range":     func(b *Backup) { b.LSNStart = "0/1" },
		"timestamps":    func(b *Backup) { b.TimestampStop = b.TimestampStop.Add(1) },
		"db-id":         func(b *Backup) { b.DBID = 7 },
		"reference":     func(b *Backup) { b.Reference = []string{info.Backups[3].Label} },
	} {
		changed := backup
		mutate(&changed)
		if got := manifestDisagreement(manifest, changed, info.History); got != want {
			t.Errorf("manifestDisagreement() = %q, want %q", got, want)
		}
	}
}

func TestParseManifestRefusesMalformedSummary(t *testing.T) {
	t.Parallel()
	repository := loadFixture(t, fixtureRoot, "plain")
	info, _ := loadInfoPair(t, "plain")
	data := repository.content[ManifestKey("demo", info.Backups[1].Label)]
	for name, edit := range map[string]func(string) string{
		"no label":       func(body string) string { return strings.Replace(body, "backup-label=", "backup-labels=", 1) },
		"type and label": func(body string) string { return strings.Replace(body, `backup-type="diff"`, `backup-type="incr"`, 1) },
		"reference": func(body string) string {
			return strings.Replace(body, "backup-reference=\"", "backup-reference=\"x,", 1)
		},
	} {
		_, err := ParseManifest(bytes.NewReader(rewriteINI(t, data, edit)))
		if !errors.Is(err, errMalformedINI) {
			t.Errorf("%s: ParseManifest() = %v", name, err)
		}
	}
	// An extra key in [backup:db] is tolerated: a later pgBackRest may add one.
	extra := rewriteINI(t, data, func(body string) string {
		return strings.Replace(body, "\n[backup:db]\n", "\n[backup:db]\na-later-key=1\n", 1)
	})
	if _, err := ParseManifest(bytes.NewReader(extra)); err != nil {
		t.Errorf("extra key: ParseManifest() = %v", err)
	}
	// PostgreSQL 19 pads the low half (%X/%08X); the manifest keeps the
	// %X/%X spelling backup.info is compared in.
	padded := rewriteINI(t, data, func(body string) string {
		return strings.Replace(body, `backup-lsn-start="0/8000028"`, `backup-lsn-start="0/08000028"`, 1)
	})
	if manifest, err := ParseManifest(bytes.NewReader(padded)); err != nil || manifest.LSNStart != "0/8000028" {
		t.Errorf("padded LSN: ParseManifest() = %+v, %v", manifest, err)
	}
	reversed := rewriteINI(t, data, func(body string) string {
		return strings.Replace(body, "backup-timestamp-start=", "backup-timestamp-start=9", 1)
	})
	if _, err := ParseManifest(bytes.NewReader(reversed)); !errors.Is(err, errMalformedINI) {
		t.Errorf("start after stop: ParseManifest() = %v", err)
	}
}
