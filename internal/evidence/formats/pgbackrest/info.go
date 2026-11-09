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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// The repository format this package reads.
const supportedFormat = 5

// Bounds on what one info file may describe.
const (
	// MaxBackups bounds the [backup:current] entries of one backup.info.
	MaxBackups = 10_000
	// MaxDBHistory bounds the [db:history] entries of one info file (one per
	// stanza-upgrade).
	MaxDBHistory = 256
	// MaxAnnotations bounds the annotations kept per backup.
	MaxAnnotations = 32
	maxTextBytes   = 256
)

// BackupType is a pgBackRest backup type.
type BackupType string

// The pgBackRest backup types.
const (
	BackupFull         BackupType = "full"
	BackupDifferential BackupType = "diff"
	BackupIncremental  BackupType = "incr"
)

// Labels (src/command/backup/common.c backupRegExp): YYYYMMDD-HHMMSSF for a
// full backup, <full>_YYYYMMDD-HHMMSS{D,I} for the others. The time in a
// label is the server's local time and is never parsed: the epochs in
// backup-timestamp-start/stop are.
var (
	fullLabelPattern  = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}F$`)
	childLabelPattern = regexp.MustCompile(`^([0-9]{8}-[0-9]{6}F)_[0-9]{8}-[0-9]{6}([DI])$`)
)

// LabelType returns the backup type a label names and its full backup's
// label, or false for a string that is not a pgBackRest label.
func LabelType(label string) (BackupType, string, bool) {
	if fullLabelPattern.MatchString(label) {
		return BackupFull, label, true
	}
	match := childLabelPattern.FindStringSubmatch(label)
	if match == nil {
		return "", "", false
	}
	if match[2] == "D" {
		return BackupDifferential, match[1], true
	}
	return BackupIncremental, match[1], true
}

// PGHistory is one [db:history] entry: a PostgreSQL cluster the stanza has
// held (one per stanza-create or stanza-upgrade). CatalogVersion and
// ControlVersion are only in backup.info.
type PGHistory struct {
	ID             uint32
	SystemID       uint64
	Version        string
	VersionNumber  int
	CatalogVersion uint32
	ControlVersion uint32
}

// ArchiveID is the directory WAL for this history entry is archived under:
// "<db-version>-<db-id>", for example "17-1".
func (h PGHistory) ArchiveID() string {
	return h.Version + "-" + strconv.FormatUint(uint64(h.ID), 10)
}

// Backup is one [backup:current] entry of backup.info, with every field
// pgBackRest 2.60.0 writes (src/info/infoBackup.c). Optional fields are zero
// when absent; the timestamps are UTC instants from the stored epochs.
type Backup struct {
	Label                     string
	Type                      BackupType
	Prior                     string
	Reference                 []string
	ArchiveStart, ArchiveStop string
	LSNStart, LSNStop         string
	TimestampStart            time.Time
	TimestampStop             time.Time
	Error                     *bool
	BackrestFormat            int
	BackrestVersion           string
	DBID                      uint32
	Size, SizeDelta           uint64
	RepoSize, RepoSizeDelta   uint64
	RepoSizeMap               *uint64
	RepoSizeMapDelta          *uint64
	Annotations               map[string]string
	AnnotationsWithheld       int
	OptionArchiveCheck        bool
	OptionArchiveCopy         bool
	OptionBackupStandby       bool
	OptionChecksumPage        bool
	OptionCompress            bool
	OptionHardlink            bool
	OptionOnline              bool
}

// BackupInfo is a parsed, checksum-verified backup.info.
type BackupInfo struct {
	Format            int
	PGBackRestVersion string
	Checksum          string
	Current           PGHistory
	History           []PGHistory
	Backups           []Backup
	// Encrypted reports a [cipher] cipher-pass: the repository is encrypted
	// and the manifests open with that passphrase, which is kept unexported
	// and never rendered.
	Encrypted  bool
	cipherPass []byte
}

// ArchiveInfo is a parsed, checksum-verified archive.info.
type ArchiveInfo struct {
	Format            int
	PGBackRestVersion string
	Checksum          string
	Current           PGHistory
	History           []PGHistory
	// Encrypted reports a [cipher] cipher-pass, which opens WAL and history
	// files; it is kept unexported and never rendered.
	Encrypted  bool
	cipherPass []byte
}

// ArchiveIDs returns the archive id of every history entry, oldest first.
func (a *ArchiveInfo) ArchiveIDs() []string {
	result := make([]string, 0, len(a.History))
	for _, history := range a.History {
		result = append(result, history.ArchiveID())
	}
	return result
}

// ParseBackupInfo parses and verifies a decrypted backup.info. A file written
// at a repository format other than 5 is a FormatError once its checksum
// verified.
func ParseBackupInfo(source io.Reader) (*BackupInfo, error) {
	file, err := ParseINI(source, MaxInfoFileBytes, nil)
	if err != nil {
		return nil, err
	}
	if file.Format != supportedFormat {
		return nil, FormatError{Format: file.Format}
	}
	info := &BackupInfo{Format: file.Format, PGBackRestVersion: file.Version, Checksum: file.Checksum}
	if info.History, err = parseDBHistory(file, true); err != nil {
		return nil, err
	}
	if info.Current, err = currentDB(file, info.History, true); err != nil {
		return nil, err
	}
	if info.Encrypted, info.cipherPass, err = parseCipherPass(file); err != nil {
		return nil, err
	}
	entries := file.Section("backup:current")
	if len(entries) > MaxBackups {
		return nil, fmt.Errorf("%w: more than %d backups", errMalformedINI, MaxBackups)
	}
	for _, entry := range entries {
		backup, err := parseBackupEntry(entry.Key, entry.Raw)
		if err != nil {
			return nil, err
		}
		info.Backups = append(info.Backups, backup)
	}
	sort.Slice(info.Backups, func(i, j int) bool { return info.Backups[i].Label < info.Backups[j].Label })
	return info, nil
}

// ParseArchiveInfo parses and verifies a decrypted archive.info.
func ParseArchiveInfo(source io.Reader) (*ArchiveInfo, error) {
	file, err := ParseINI(source, MaxInfoFileBytes, nil)
	if err != nil {
		return nil, err
	}
	if file.Format != supportedFormat {
		return nil, FormatError{Format: file.Format}
	}
	info := &ArchiveInfo{Format: file.Format, PGBackRestVersion: file.Version, Checksum: file.Checksum}
	if info.History, err = parseDBHistory(file, false); err != nil {
		return nil, err
	}
	if info.Current, err = currentDB(file, info.History, false); err != nil {
		return nil, err
	}
	if info.Encrypted, info.cipherPass, err = parseCipherPass(file); err != nil {
		return nil, err
	}
	return info, nil
}

func malformed(what string) error { return fmt.Errorf("%w: %s", errMalformedINI, what) }

func parseDBHistory(file *INIFile, backup bool) ([]PGHistory, error) {
	entries := file.Section("db:history")
	if len(entries) == 0 || len(entries) > MaxDBHistory {
		return nil, malformed("db:history is empty or too long")
	}
	history := make([]PGHistory, 0, len(entries))
	for _, entry := range entries {
		id, err := strconv.ParseUint(entry.Key, 10, 32)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != entry.Key {
			return nil, malformed("db:history id")
		}
		fields, err := objectFields(entry.Raw)
		if err != nil {
			return nil, malformed("db:history entry")
		}
		value := PGHistory{ID: uint32(id)}
		systemKey := "db-id"
		if backup {
			systemKey = "db-system-id"
			if value.CatalogVersion, err = requiredUint32(fields, "db-catalog-version"); err != nil {
				return nil, err
			}
			if value.ControlVersion, err = requiredUint32(fields, "db-control-version"); err != nil {
				return nil, err
			}
		}
		if value.SystemID, err = requiredUint64(fields, systemKey); err != nil {
			return nil, err
		}
		if value.Version, value.VersionNumber, err = requiredPGVersion(fields, "db-version"); err != nil {
			return nil, err
		}
		history = append(history, value)
	}
	sort.Slice(history, func(i, j int) bool { return history[i].ID < history[j].ID })
	return history, nil
}

// currentDB reads [db]. pgBackRest takes only db-id from it and the rest from
// the history entry; the reader also requires the copies to agree.
func currentDB(file *INIFile, history []PGHistory, backup bool) (PGHistory, error) {
	raw, ok := file.Get("db", "db-id")
	if !ok {
		return PGHistory{}, malformed("no db-id in [db]")
	}
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return PGHistory{}, malformed("db-id in [db]")
	}
	index := slices.IndexFunc(history, func(value PGHistory) bool { return uint64(value.ID) == id })
	if index < 0 {
		return PGHistory{}, malformed("db-id in [db] is not in db:history")
	}
	current := history[index]
	if raw, ok := file.Get("db", "db-system-id"); ok {
		if system, err := strconv.ParseUint(raw, 10, 64); err != nil || system != current.SystemID {
			return PGHistory{}, malformed("[db] disagrees with db:history")
		}
	}
	if raw, ok := file.Get("db", "db-version"); ok {
		var version string
		if err := json.Unmarshal([]byte(raw), &version); err != nil || version != current.Version {
			return PGHistory{}, malformed("[db] disagrees with db:history")
		}
	}
	if backup {
		for key, want := range map[string]uint32{"db-catalog-version": current.CatalogVersion, "db-control-version": current.ControlVersion} {
			if raw, ok := file.Get("db", key); ok {
				if value, err := strconv.ParseUint(raw, 10, 32); err != nil || uint32(value) != want {
					return PGHistory{}, malformed("[db] disagrees with db:history")
				}
			}
		}
	}
	return current, nil
}

func parseCipherPass(file *INIFile) (bool, []byte, error) {
	raw, ok := file.Get("cipher", "cipher-pass")
	if !ok {
		return false, nil, nil
	}
	var pass string
	if err := json.Unmarshal([]byte(raw), &pass); err != nil || pass == "" || len(pass) > MaxPassphraseBytes {
		return false, nil, malformed("cipher-pass")
	}
	return true, []byte(pass), nil
}

// parseBackupEntry reads one [backup:current] value. Required and optional
// keys follow infoBackupLoadCallback; unknown keys are ignored, as a later
// pgBackRest may add some.
func parseBackupEntry(label, raw string) (Backup, error) {
	backup := Backup{Label: label}
	labelType, _, ok := LabelType(label)
	if !ok {
		return backup, malformed("backup label")
	}
	fields, err := objectFields(raw)
	if err != nil {
		return backup, malformed("backup entry")
	}
	var format uint32
	if format, err = requiredUint32(fields, "backrest-format"); err != nil {
		return backup, err
	}
	if format > math.MaxInt32 {
		return backup, malformed("backrest-format")
	}
	backup.BackrestFormat = int(format)
	if backup.BackrestVersion, err = requiredText(fields, "backrest-version"); err != nil {
		return backup, err
	}
	var kind string
	if kind, err = requiredText(fields, "backup-type"); err != nil {
		return backup, err
	}
	backup.Type = BackupType(kind)
	if backup.Type != labelType {
		return backup, malformed("backup type disagrees with its label")
	}
	for _, field := range []struct {
		name   string
		target *string
		valid  func(string) bool
	}{
		{"backup-archive-start", &backup.ArchiveStart, canonicalWALName},
		{"backup-archive-stop", &backup.ArchiveStop, canonicalWALName},
		{"backup-lsn-start", &backup.LSNStart, canonicalLSN},
		{"backup-lsn-stop", &backup.LSNStop, canonicalLSN},
		{"backup-prior", &backup.Prior, validLabel},
	} {
		if *field.target, err = optionalText(fields, field.name); err != nil {
			return backup, err
		}
		if *field.target != "" && !field.valid(*field.target) {
			return backup, malformed(field.name)
		}
	}
	backup.LSNStart, backup.LSNStop = normalizedOptionalLSN(backup.LSNStart), normalizedOptionalLSN(backup.LSNStop)
	if referenceRaw, ok := fields["backup-reference"]; ok {
		if err := json.Unmarshal(referenceRaw, &backup.Reference); err != nil || len(backup.Reference) > MaxBackups {
			return backup, malformed("backup-reference")
		}
		for _, reference := range backup.Reference {
			if !validLabel(reference) {
				return backup, malformed("backup-reference")
			}
		}
	}
	if errorRaw, ok := fields["backup-error"]; ok {
		var value bool
		if err := json.Unmarshal(errorRaw, &value); err != nil {
			return backup, malformed("backup-error")
		}
		backup.Error = &value
	}
	for _, field := range []struct {
		name   string
		target *uint64
	}{
		{"backup-info-size", &backup.Size},
		{"backup-info-size-delta", &backup.SizeDelta},
		{"backup-info-repo-size", &backup.RepoSize},
		{"backup-info-repo-size-delta", &backup.RepoSizeDelta},
	} {
		if *field.target, err = requiredUint64(fields, field.name); err != nil {
			return backup, err
		}
	}
	if _, ok := fields["backup-info-repo-size-map"]; ok {
		value, err := requiredUint64(fields, "backup-info-repo-size-map")
		if err != nil {
			return backup, err
		}
		delta, err := requiredUint64(fields, "backup-info-repo-size-map-delta")
		if err != nil {
			return backup, err
		}
		backup.RepoSizeMap, backup.RepoSizeMapDelta = &value, &delta
	}
	start, err := requiredUint64(fields, "backup-timestamp-start")
	if err != nil {
		return backup, err
	}
	stop, err := requiredUint64(fields, "backup-timestamp-stop")
	if err != nil {
		return backup, err
	}
	const maxEpoch = 1 << 40
	if start == 0 || start > maxEpoch || stop < start || stop > maxEpoch {
		return backup, malformed("backup timestamps")
	}
	backup.TimestampStart, backup.TimestampStop = time.Unix(int64(start), 0).UTC(), time.Unix(int64(stop), 0).UTC()
	if backup.DBID, err = requiredUint32(fields, "db-id"); err != nil {
		return backup, err
	}
	for _, field := range []struct {
		name   string
		target *bool
	}{
		{"option-archive-check", &backup.OptionArchiveCheck},
		{"option-archive-copy", &backup.OptionArchiveCopy},
		{"option-backup-standby", &backup.OptionBackupStandby},
		{"option-checksum-page", &backup.OptionChecksumPage},
		{"option-compress", &backup.OptionCompress},
		{"option-hardlink", &backup.OptionHardlink},
		{"option-online", &backup.OptionOnline},
	} {
		value, ok := fields[field.name]
		if !ok || json.Unmarshal(value, field.target) != nil {
			return backup, malformed(field.name)
		}
	}
	if annotationRaw, ok := fields["backup-annotation"]; ok {
		backup.Annotations, backup.AnnotationsWithheld, err = parseAnnotations(annotationRaw)
		if err != nil {
			return backup, err
		}
	}
	return backup, nil
}

// parseAnnotations keeps the string annotations that are short, printable
// text, up to MaxAnnotations of them, and counts the ones it drops. An
// annotation is free text an operator (or a plugin, such as Opera's
// cnpg-backup-name) chose.
func parseAnnotations(raw json.RawMessage) (map[string]string, int, error) {
	fields, err := objectFields(string(raw))
	if err != nil {
		return nil, 0, malformed("backup-annotation")
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make(map[string]string)
	withheld := 0
	for _, key := range keys {
		var value string
		if json.Unmarshal(fields[key], &value) != nil || !boundedText(key) || !boundedText(value) || len(result) >= MaxAnnotations {
			withheld++
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		result = nil
	}
	return result, withheld, nil
}

func objectFields(raw string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return nil, errMalformedINI
	}
	return fields, nil
}

func requiredUint64(fields map[string]json.RawMessage, name string) (uint64, error) {
	raw, ok := fields[name]
	if !ok {
		return 0, malformed("no " + name)
	}
	value, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, malformed(name)
	}
	return value, nil
}

func requiredUint32(fields map[string]json.RawMessage, name string) (uint32, error) {
	value, err := requiredUint64(fields, name)
	if err != nil {
		return 0, err
	}
	if value > 1<<32-1 {
		return 0, malformed(name)
	}
	return uint32(value), nil
}

func requiredText(fields map[string]json.RawMessage, name string) (string, error) {
	if _, ok := fields[name]; !ok {
		return "", malformed("no " + name)
	}
	value, err := optionalText(fields, name)
	if err != nil || value == "" {
		return "", malformed(name)
	}
	return value, nil
}

func optionalText(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (value != "" && !boundedText(value)) {
		return "", malformed(name)
	}
	return value, nil
}

func requiredPGVersion(fields map[string]json.RawMessage, name string) (string, int, error) {
	version, err := requiredText(fields, name)
	if err != nil {
		return "", 0, err
	}
	number, ok := PostgreSQLVersionNumber(version)
	if !ok {
		return "", 0, malformed(name)
	}
	return version, number, nil
}

// PostgreSQLVersionNumber turns pgBackRest's db-version ("9.6", "17") into
// PostgreSQL's server_version_num (90600, 170000).
func PostgreSQLVersionNumber(version string) (int, bool) {
	major, minor, hasMinor := strings.Cut(version, ".")
	majorNumber, err := strconv.Atoi(major)
	if err != nil || majorNumber <= 0 || majorNumber > 99 || strconv.Itoa(majorNumber) != major {
		return 0, false
	}
	if majorNumber >= 10 {
		if hasMinor {
			return 0, false
		}
		return majorNumber * 10_000, true
	}
	minorNumber, err := strconv.Atoi(minor)
	if !hasMinor || err != nil || minorNumber < 0 || minorNumber > 9 || strconv.Itoa(minorNumber) != minor {
		return 0, false
	}
	return majorNumber*10_000 + minorNumber*100, true
}

func validLabel(value string) bool {
	_, _, ok := LabelType(value)
	return ok
}

func boundedText(value string) bool {
	return value != "" && len(value) <= maxTextBytes && strings.ToValidUTF8(value, "") == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

// canonicalWALName is PostgreSQL's 24 upper-case hexadecimal digits.
func canonicalWALName(value string) bool {
	return len(value) == 24 && upperHex(value)
}

// canonicalLSN accepts the two spellings PostgreSQL prints: %X/%X (up to
// PostgreSQL 18) and %X/%08X (PostgreSQL 19 zero-pads the low half). The
// high half is never padded, and a padded low half has exactly eight digits;
// any other leading zero is not something PostgreSQL or pgBackRest writes.
func canonicalLSN(value string) bool {
	high, low, ok := strings.Cut(value, "/")
	return ok && len(high) >= 1 && len(high) <= 8 && len(low) >= 1 && len(low) <= 8 && upperHex(high) && upperHex(low) &&
		(len(high) == 1 || high[0] != '0') && (len(low) == 1 || len(low) == 8 || low[0] != '0')
}

// normalizeLSN returns a valid LSN in the %X/%X spelling, so the evidence and
// the comparisons between backup.info and a manifest do not depend on the
// PostgreSQL version that printed it; ok is false for an invalid LSN.
func normalizeLSN(value string) (string, bool) {
	lsn, ok := ParseLSN(value)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%X/%X", lsn>>32, lsn&0xFFFFFFFF), true
}

// normalizedOptionalLSN normalizes an LSN that canonicalLSN already accepted;
// an empty value stays empty.
func normalizedOptionalLSN(value string) string {
	if normalized, ok := normalizeLSN(value); ok {
		return normalized
	}
	return value
}

// ParseLSN parses an LSN in either spelling canonicalLSN accepts.
func ParseLSN(value string) (uint64, bool) {
	if !canonicalLSN(value) {
		return 0, false
	}
	high, low, _ := strings.Cut(value, "/")
	highValue, errHigh := strconv.ParseUint(high, 16, 32)
	lowValue, errLow := strconv.ParseUint(low, 16, 32)
	if errHigh != nil || errLow != nil {
		return 0, false
	}
	return highValue<<32 | lowValue, true
}

func upperHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return value != ""
}

// FileSource says which file of an info pair a result was read from.
type FileSource string

// The two files of a pair.
const (
	SourceMain FileSource = "main"
	SourceCopy FileSource = "copy"
)

// FileProblem is why one file of a pair was not used. It is a category; it
// never carries file content or a passphrase.
type FileProblem string

// The file problems.
const (
	ProblemNone              FileProblem = ""
	ProblemMissing           FileProblem = "missing"
	ProblemUnreadable        FileProblem = "unreadable"
	ProblemTooLarge          FileProblem = "too-large"
	ProblemChecksum          FileProblem = "checksum-mismatch"
	ProblemMalformed         FileProblem = "malformed"
	ProblemEncrypted         FileProblem = "encrypted-without-passphrase"
	ProblemDecryption        FileProblem = "decryption-failed"
	ProblemFormatUnsupported FileProblem = "format-unsupported"
)

// FileCheck is the outcome of reading one file of a pair.
type FileCheck struct {
	Key       string
	Present   bool
	Problem   FileProblem
	Checksum  string
	Encrypted bool
	// Format is the repository format the file declared, or 0.
	Format int
}

// PairCheck is the outcome of reading a file and its .copy: which one was
// used (the main file when it verifies, as pgBackRest does) and whether both
// verified with the same checksum.
type PairCheck struct {
	Used       FileSource
	Main, Copy FileCheck
	// Agree is true when both files verified and their checksums are equal.
	Agree bool
}

// Usable reports whether one file of the pair verified.
func (p PairCheck) Usable() bool { return p.Used != "" }

// readPair reads key and key+".copy" (only when listed), opening each through
// openFile, and returns the parsed value of the file it used.
func readPair[T any](ctx context.Context, reader store.Reader, listed map[string]store.Object, key string, limit int64, passphrase []byte, parse func(io.Reader) (T, string, int, error)) (T, PairCheck) {
	var zero, mainValue, copyValue T
	check := PairCheck{Main: FileCheck{Key: key}, Copy: FileCheck{Key: key + ".copy"}}
	mainValue, check.Main = readOne(ctx, reader, listed, check.Main, limit, passphrase, parse)
	copyValue, check.Copy = readOne(ctx, reader, listed, check.Copy, limit, passphrase, parse)
	mainOK, copyOK := check.Main.Present && check.Main.Problem == ProblemNone, check.Copy.Present && check.Copy.Problem == ProblemNone
	check.Agree = mainOK && copyOK && check.Main.Checksum == check.Copy.Checksum
	switch {
	case mainOK:
		check.Used = SourceMain
		return mainValue, check
	case copyOK:
		check.Used = SourceCopy
		return copyValue, check
	default:
		return zero, check
	}
}

func readOne[T any](ctx context.Context, reader store.Reader, listed map[string]store.Object, check FileCheck, limit int64, passphrase []byte, parse func(io.Reader) (T, string, int, error)) (T, FileCheck) {
	var zero T
	object, ok := listed[check.Key]
	if !ok {
		check.Problem = ProblemMissing
		return zero, check
	}
	check.Present = true
	// Encryption adds a 16-byte header and up to one block of padding.
	stored := limit + int64(cipherHeaderBytes) + 16
	if object.Size > stored {
		check.Problem = ProblemTooLarge
		return zero, check
	}
	// One byte over the bound: adapters cut a body at MaxBytes, so an
	// oversized file must reach the bounded reader to be rejected rather
	// than parsed from a truncated prefix.
	stream, err := reader.Open(ctx, store.OpenRequest{Key: check.Key, MaxBytes: stored + 1})
	if err != nil {
		check.Problem = ProblemUnreadable
		return zero, check
	}
	defer func() { _ = stream.Close() }()
	source, encrypted, err := openEnvelope(&boundedReader{source: stream, remaining: stored}, passphrase)
	check.Encrypted = encrypted
	if err != nil {
		check.Problem = problemOf(err)
		var formatErr FormatError
		if errors.As(err, &formatErr) {
			check.Format = formatErr.Format
		}
		return zero, check
	}
	value, checksum, format, err := parse(source)
	check.Format = format
	if err != nil {
		check.Problem = problemOf(err)
		if encrypted && (check.Problem == ProblemMalformed || check.Problem == ProblemChecksum) {
			// A wrong passphrase that happened to unpad decrypts to noise. A
			// file over the bound stays too large: its size, not its key, is
			// what is wrong.
			check.Problem = ProblemDecryption
		}
		var formatErr FormatError
		if errors.As(err, &formatErr) {
			check.Format = formatErr.Format
		}
		return zero, check
	}
	check.Checksum = checksum
	return value, check
}

// openEnvelope looks at the first bytes of a stored file: a format header is
// a FormatError, the salted magic is decrypted with passphrase (or refused
// without one), anything else is read as plain text.
func openEnvelope(source io.Reader, passphrase []byte) (io.Reader, bool, error) {
	buffered := bufio.NewReader(source)
	prefix, err := buffered.Peek(cipherHeaderBytes)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, false, err
	}
	switch DetectEnvelope(prefix) {
	case EnvelopeFormatHeader:
		return nil, true, FormatError{Format: formatHeaderFormat(prefix)}
	case EnvelopeSalted:
		if len(passphrase) == 0 {
			return nil, true, errEncryptedWithoutPassphrase
		}
		plain, err := NewDecryptReader(buffered, passphrase)
		return plain, true, err
	default:
		return buffered, false, nil
	}
}

var errEncryptedWithoutPassphrase = errors.New("pgbackrest: file is encrypted and no passphrase is configured")

func problemOf(err error) FileProblem {
	switch {
	case err == nil:
		return ProblemNone
	case errors.Is(err, ErrRepositoryFormatUnsupported):
		return ProblemFormatUnsupported
	case errors.Is(err, errEncryptedWithoutPassphrase):
		return ProblemEncrypted
	case errors.Is(err, ErrDecryption):
		return ProblemDecryption
	case errors.Is(err, errTooLarge):
		return ProblemTooLarge
	case errors.Is(err, ErrChecksumMismatch):
		return ProblemChecksum
	case errors.Is(err, errMalformedINI):
		return ProblemMalformed
	default:
		return ProblemUnreadable
	}
}

// LoadBackupInfo reads backup/<stanza>/backup.info, falling back to its
// .copy, decrypting with the repository passphrase when the file is
// encrypted. Only listed keys are opened.
func LoadBackupInfo(ctx context.Context, reader store.Reader, listed map[string]store.Object, stanza string, passphrase []byte) (*BackupInfo, PairCheck) {
	return readPair(ctx, reader, listed, "backup/"+stanza+"/backup.info", MaxInfoFileBytes, passphrase, func(source io.Reader) (*BackupInfo, string, int, error) {
		info, err := ParseBackupInfo(source)
		if err != nil {
			return nil, "", 0, err
		}
		return info, info.Checksum, info.Format, nil
	})
}

// LoadArchiveInfo reads archive/<stanza>/archive.info as LoadBackupInfo does.
func LoadArchiveInfo(ctx context.Context, reader store.Reader, listed map[string]store.Object, stanza string, passphrase []byte) (*ArchiveInfo, PairCheck) {
	return readPair(ctx, reader, listed, "archive/"+stanza+"/archive.info", MaxInfoFileBytes, passphrase, func(source io.Reader) (*ArchiveInfo, string, int, error) {
		info, err := ParseArchiveInfo(source)
		if err != nil {
			return nil, "", 0, err
		}
		return info, info.Checksum, info.Format, nil
	})
}
