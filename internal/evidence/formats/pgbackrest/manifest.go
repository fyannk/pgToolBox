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
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// Manifest is what the reader takes from a backup.manifest: the [backup],
// [backup:db] and [metadata] sections (src/info/manifest/serialize.c.inc).
// The file list, which is most of the file, is hashed for the checksum and
// otherwise skipped: the reader never looks at the data files of a backup.
type Manifest struct {
	Format            int
	PGBackRestVersion string
	Checksum          string
	Label             string
	Type              BackupType
	Prior             string
	// Reference is the manifest's own list, which unlike backup.info's
	// includes the backup itself.
	Reference                 []string
	ArchiveStart, ArchiveStop string
	LSNStart, LSNStop         string
	TimestampStart            time.Time
	TimestampStop             time.Time
	TimestampCopyStart        time.Time
	DB                        PGHistory
	Annotations               map[string]string
	AnnotationsWithheld       int
}

func manifestSection(section string) bool {
	return section == "backup" || section == "backup:db" || section == "metadata"
}

// ParseManifest parses and verifies a decrypted backup.manifest.
func ParseManifest(source io.Reader) (*Manifest, error) {
	file, err := ParseINI(source, MaxManifestBytes, manifestSection)
	if err != nil {
		return nil, err
	}
	if file.Format != supportedFormat {
		return nil, FormatError{Format: file.Format}
	}
	manifest := &Manifest{Format: file.Format, PGBackRestVersion: file.Version, Checksum: file.Checksum}
	fields := make(map[string]json.RawMessage)
	for _, value := range file.Section("backup") {
		fields[value.Key] = json.RawMessage(value.Raw)
	}
	if manifest.Label, err = requiredText(fields, "backup-label"); err != nil {
		return nil, err
	}
	labelType, _, ok := LabelType(manifest.Label)
	if !ok {
		return nil, malformed("backup-label")
	}
	var kind string
	if kind, err = requiredText(fields, "backup-type"); err != nil {
		return nil, err
	}
	if manifest.Type = BackupType(kind); manifest.Type != labelType {
		return nil, malformed("backup type disagrees with its label")
	}
	for _, field := range []struct {
		name   string
		target *string
		valid  func(string) bool
	}{
		{"backup-archive-start", &manifest.ArchiveStart, canonicalWALName},
		{"backup-archive-stop", &manifest.ArchiveStop, canonicalWALName},
		{"backup-lsn-start", &manifest.LSNStart, canonicalLSN},
		{"backup-lsn-stop", &manifest.LSNStop, canonicalLSN},
		{"backup-prior", &manifest.Prior, validLabel},
	} {
		if *field.target, err = optionalText(fields, field.name); err != nil {
			return nil, err
		}
		if *field.target != "" && !field.valid(*field.target) {
			return nil, malformed(field.name)
		}
	}
	manifest.LSNStart, manifest.LSNStop = normalizedOptionalLSN(manifest.LSNStart), normalizedOptionalLSN(manifest.LSNStop)
	if raw, ok := fields["backup-reference"]; ok {
		var joined string
		if json.Unmarshal(raw, &joined) != nil {
			return nil, malformed("backup-reference")
		}
		if joined != "" {
			manifest.Reference = strings.Split(joined, ",")
		}
		if len(manifest.Reference) > MaxBackups {
			return nil, malformed("backup-reference")
		}
		for _, reference := range manifest.Reference {
			if !validLabel(reference) {
				return nil, malformed("backup-reference")
			}
		}
	}
	for _, field := range []struct {
		name     string
		target   *time.Time
		required bool
	}{
		{"backup-timestamp-start", &manifest.TimestampStart, true},
		{"backup-timestamp-stop", &manifest.TimestampStop, true},
		{"backup-timestamp-copy-start", &manifest.TimestampCopyStart, false},
	} {
		if _, ok := fields[field.name]; !ok && !field.required {
			continue
		}
		epoch, err := requiredUint64(fields, field.name)
		if err != nil {
			return nil, err
		}
		if epoch > 1<<40 {
			return nil, malformed(field.name)
		}
		*field.target = time.Unix(int64(epoch), 0).UTC()
	}
	if manifest.TimestampStop.Before(manifest.TimestampStart) {
		return nil, malformed("backup timestamps")
	}
	db := make(map[string]json.RawMessage)
	for _, value := range file.Section("backup:db") {
		db[value.Key] = json.RawMessage(value.Raw)
	}
	if manifest.DB.ID, err = requiredUint32(db, "db-id"); err != nil {
		return nil, err
	}
	if manifest.DB.SystemID, err = requiredUint64(db, "db-system-id"); err != nil {
		return nil, err
	}
	if manifest.DB.Version, manifest.DB.VersionNumber, err = requiredPGVersion(db, "db-version"); err != nil {
		return nil, err
	}
	if manifest.DB.CatalogVersion, err = requiredUint32(db, "db-catalog-version"); err != nil {
		return nil, err
	}
	if _, ok := db["db-control-version"]; ok {
		if manifest.DB.ControlVersion, err = requiredUint32(db, "db-control-version"); err != nil {
			return nil, err
		}
	}
	if raw, ok := file.Get("metadata", "annotation"); ok {
		if manifest.Annotations, manifest.AnnotationsWithheld, err = parseAnnotations(json.RawMessage(raw)); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

// ManifestKey is where pgBackRest stores the manifest of a backup.
func ManifestKey(stanza, label string) string {
	return "backup/" + stanza + "/" + label + "/backup.manifest"
}

// LoadManifest reads a backup's manifest, falling back to its .copy. In an
// encrypted repository, manifests open with backup.info's cipher-pass
// (src/command/repo/get.c), not with the repository passphrase.
func LoadManifest(ctx context.Context, reader store.Reader, listed map[string]store.Object, stanza, label string, info *BackupInfo) (*Manifest, PairCheck) {
	var passphrase []byte
	if info != nil && info.Encrypted {
		passphrase = info.cipherPass
	}
	return readPair(ctx, reader, listed, ManifestKey(stanza, label), MaxManifestBytes, passphrase, func(source io.Reader) (*Manifest, string, int, error) {
		manifest, err := ParseManifest(source)
		if err != nil {
			return nil, "", 0, err
		}
		return manifest, manifest.Checksum, manifest.Format, nil
	})
}

// manifestDisagreement names the first field in which a manifest contradicts
// its backup.info entry, or "" when they agree.
func manifestDisagreement(manifest *Manifest, backup Backup, history []PGHistory) string {
	switch {
	case manifest.Label != backup.Label:
		return "label"
	case manifest.Type != backup.Type:
		return "type"
	case manifest.Prior != backup.Prior:
		return "prior"
	case manifest.ArchiveStart != backup.ArchiveStart || manifest.ArchiveStop != backup.ArchiveStop:
		return "archive range"
	case manifest.LSNStart != backup.LSNStart || manifest.LSNStop != backup.LSNStop:
		return "LSN range"
	case !manifest.TimestampStart.Equal(backup.TimestampStart) || !manifest.TimestampStop.Equal(backup.TimestampStop):
		return "timestamps"
	case manifest.DB.ID != backup.DBID:
		return "db-id"
	}
	for _, entry := range history {
		if entry.ID == backup.DBID && (entry.SystemID != manifest.DB.SystemID || entry.Version != manifest.DB.Version) {
			return "database"
		}
	}
	references := make(map[string]struct{}, len(manifest.Reference))
	for _, reference := range manifest.Reference {
		references[reference] = struct{}{}
	}
	for _, reference := range backup.Reference {
		if _, ok := references[reference]; !ok {
			return "reference"
		}
	}
	if len(manifest.Reference) > 0 {
		if _, ok := references[backup.Label]; !ok {
			return "reference"
		}
	}
	return ""
}
