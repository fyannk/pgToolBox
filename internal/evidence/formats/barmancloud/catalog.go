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

package barmancloud

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// MaxBackupInfoBytes bounds format metadata before parsing.  A bigger object
// is not evidence of a valid backup.info file.
const MaxBackupInfoBytes int64 = 256 * 1024

// Backup is Barman-native catalog evidence.  It intentionally is not shared
// with pgBackRest: Barman parents and artifact rules have different semantics.
type Backup struct {
	Server, ID, Status, Type string
	SystemID                 string
	State                    evidence.State
	Reason                   string
	BeginWAL, EndWAL         string
	BeginLSN, EndLSN         string
	SegmentSize              int64
	PostgreSQLVersion        int64
	Timeline                 uint32
	LogicalBytes             int64
	DeduplicatedBytes        *int64
	StoredBytes              int64
	Compression, Encryption  string
	TablespaceOIDs           []string
	BeginAt, EndAt           time.Time
	Artifacts                []string
	// WithheldFields names optional backup.info fields whose values were
	// present but outside the published evidence constraints. Their values
	// are dropped at parse time and the backup cannot be better than unknown.
	WithheldFields []string
}

// Catalog holds bounded Barman-only facts from one complete object listing.
type Catalog struct {
	Backups []Backup
}

// ParseBackupInfo accepts Barman's version-tolerant key=value field list and
// intentionally retains only allowlisted facts.  Unknown field values are
// discarded so they cannot leak through diagnostics or UI rendering.
func ParseBackupInfo(data []byte, server, id string) (Backup, error) {
	if len(data) == 0 || int64(len(data)) > MaxBackupInfoBytes || server == "" || id == "" {
		return Backup{}, errors.New("invalid bounded Barman backup metadata")
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), int(MaxBackupInfoBytes))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return Backup{}, errors.New("malformed Barman backup metadata")
		}
		values[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return Backup{}, errors.New("malformed Barman backup metadata")
	}
	backup := Backup{Server: server, ID: id, Status: strings.ToUpper(first(values, "status")), Type: strings.ToLower(first(values, "backup_type", "backup_method", "mode")), State: evidence.Unknown, Reason: "metadata status is unknown"}
	if backup.Status == "" {
		return backup, errors.New("no status in Barman backup metadata")
	}
	var err error
	if backup.BeginAt, err = parseBarmanTime(first(values, "begin_time", "begin_time_iso")); err != nil {
		return backup, errors.New("malformed begin time in Barman backup metadata")
	}
	if backup.EndAt, err = parseBarmanTime(first(values, "end_time", "end_time_iso")); err != nil {
		return backup, errors.New("malformed end time in Barman backup metadata")
	}
	backup.BeginWAL, backup.EndWAL = first(values, "begin_wal"), first(values, "end_wal")
	backup.BeginLSN, backup.EndLSN = first(values, "begin_lsn"), first(values, "end_lsn")
	assignLegacyXLog(&backup.BeginWAL, &backup.BeginLSN, first(values, "begin_xlog"))
	assignLegacyXLog(&backup.EndWAL, &backup.EndLSN, first(values, "end_xlog"))
	if backup.SegmentSize, err = parseOptionalInt(first(values, "xlog_segment_size", "wal_segment_size")); err != nil || backup.SegmentSize < 0 {
		return backup, errors.New("malformed WAL segment size in Barman backup metadata")
	}
	if backup.PostgreSQLVersion, err = parseOptionalInt(first(values, "version", "postgresql_version")); err != nil || backup.PostgreSQLVersion < 0 {
		return backup, errors.New("malformed PostgreSQL version in Barman backup metadata")
	}
	if timeline := first(values, "timeline"); timeline != "" {
		parsed, parseErr := strconv.ParseUint(timeline, 10, 32)
		if parseErr != nil || parsed == 0 {
			return backup, errors.New("malformed timeline in Barman backup metadata")
		}
		backup.Timeline = uint32(parsed)
	}
	backup.SystemID = first(values, "systemid", "system_id")
	if len(backup.SystemID) > 64 || strings.IndexFunc(backup.SystemID, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return backup, errors.New("malformed system ID in Barman backup metadata")
	}
	if backup.LogicalBytes, err = parseOptionalInt(first(values, "size", "backup_size")); err != nil || backup.LogicalBytes < 0 {
		return backup, errors.New("malformed logical size in Barman backup metadata")
	}
	if dedup := first(values, "deduplicated_size"); dedup != "" {
		value, parseErr := parseOptionalInt(dedup)
		if parseErr != nil || value < 0 {
			return backup, errors.New("malformed deduplicated size in Barman backup metadata")
		}
		backup.DeduplicatedBytes = &value
	}
	backup.Compression, backup.Encryption = first(values, "compression", "compression_type"), first(values, "encryption", "encryption_mode")
	if raw := firstIncludingNone(values, "encryption", "encryption_mode"); strings.EqualFold(raw, "none") {
		backup.Encryption = "none"
	}
	if backup.TablespaceOIDs, err = parseTablespaceOIDs(values["tablespaces"]); err != nil {
		return backup, errors.New("malformed tablespaces in Barman backup metadata")
	}
	backup.WithheldFields = withholdInvalidOptionalFacts(&backup)
	return backup, nil
}

const (
	maxBackupTextBytes = 64
	minWALSegmentBytes = 1 << 20
	maxWALSegmentBytes = 1 << 30
)

// withholdInvalidOptionalFacts drops optional values that Barman would never
// write and that the published evidence contract cannot carry: free text that
// is too long, not UTF-8, or contains control characters; WAL names and LSNs
// that are not PostgreSQL's upper-case hexadecimal spelling; a WAL segment
// size that is not a power of two between 1 MiB and 1 GiB; and an end time
// before the begin time. A dropped value is absent, never repaired, and the
// returned field names make the backup unknown with a reason. Without this, a
// single malformed backup.info would make the whole publication unprojectable.
func withholdInvalidOptionalFacts(backup *Backup) []string {
	var withheld []string
	withhold := func(name string, invalid bool, clear func()) {
		if invalid {
			clear()
			withheld = append(withheld, name)
		}
	}
	withhold("status", invalidBackupText(backup.Status), func() { backup.Status = "" })
	withhold("backup_type", invalidBackupText(backup.Type), func() { backup.Type = "" })
	withhold("begin_wal", backup.BeginWAL != "" && !canonicalWALName(backup.BeginWAL), func() { backup.BeginWAL = "" })
	withhold("end_wal", backup.EndWAL != "" && !canonicalWALName(backup.EndWAL), func() { backup.EndWAL = "" })
	withhold("begin_lsn", backup.BeginLSN != "" && !canonicalLSN(backup.BeginLSN), func() { backup.BeginLSN = "" })
	withhold("end_lsn", backup.EndLSN != "" && !canonicalLSN(backup.EndLSN), func() { backup.EndLSN = "" })
	withhold("wal_segment_size", backup.SegmentSize != 0 && !validSegmentSize(backup.SegmentSize), func() { backup.SegmentSize = 0 })
	withhold("compression", invalidBackupText(backup.Compression), func() { backup.Compression = "" })
	withhold("encryption", invalidBackupText(backup.Encryption), func() { backup.Encryption = "" })
	withhold("end_time", !backup.BeginAt.IsZero() && !backup.EndAt.IsZero() && backup.EndAt.Before(backup.BeginAt), func() {
		backup.BeginAt, backup.EndAt = time.Time{}, time.Time{}
	})
	return withheld
}

func invalidBackupText(value string) bool {
	return value != "" && (len(value) > maxBackupTextBytes || strings.ToValidUTF8(value, "") != value || strings.IndexFunc(value, unicode.IsControl) >= 0)
}

func canonicalWALName(value string) bool {
	return len(value) == 24 && upperHex(value)
}

func canonicalLSN(value string) bool {
	high, low, ok := strings.Cut(value, "/")
	return ok && len(high) >= 1 && len(high) <= 8 && len(low) >= 1 && len(low) <= 8 && upperHex(high) && upperHex(low)
}

func upperHex(value string) bool {
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return value != ""
}

func validSegmentSize(value int64) bool {
	return value >= minWALSegmentBytes && value <= maxWALSegmentBytes && value&(value-1) == 0
}

// Analyze reads only listed backup.info files, then stats each discovered
// Barman tar artifact. Any read/stat failure is represented as unknown for the
// affected backup; it must not make an otherwise complete inventory healthy.
func Analyze(ctx context.Context, reader store.Reader, objects []store.Object) Catalog {
	byDirectory := make(map[string][]store.Object)
	for _, object := range objects {
		if server, id, ok := backupInfoKey(object.Key); ok {
			byDirectory[server+"\x00"+id] = append(byDirectory[server+"\x00"+id], object)
			continue
		}
		if server, id, ok := backupObjectKey(object.Key); ok {
			byDirectory[server+"\x00"+id] = append(byDirectory[server+"\x00"+id], object)
		}
	}
	catalog := Catalog{}
	for composite, listed := range byDirectory {
		server, id, _ := strings.Cut(composite, "\x00")
		infoKey := server + "/base/" + id + "/backup.info"
		if !containsKey(listed, infoKey) {
			catalog.Backups = append(catalog.Backups, Backup{
				Server: server, ID: id, State: evidence.Unknown,
				Reason: "required backup metadata is missing",
			})
			continue
		}
		backup := readBackup(ctx, reader, infoKey, server, id)
		backup.Artifacts = artifacts(listed, infoKey, backup)
		backup.StoredBytes = artifactBytes(listed, backup.Artifacts)
		if backup.State == evidence.Warning && backup.Reason == "awaiting structural artifact validation" {
			backup.State, backup.Reason = structuralState(ctx, reader, backup)
		}
		catalog.Backups = append(catalog.Backups, backup)
	}
	sort.Slice(catalog.Backups, func(i, j int) bool {
		if catalog.Backups[i].Server != catalog.Backups[j].Server {
			return catalog.Backups[i].Server < catalog.Backups[j].Server
		}
		return catalog.Backups[i].ID < catalog.Backups[j].ID
	})
	return catalog
}

// AnalyzeCatalog lets the shared scanner invoke Barman-owned semantics without
// teaching provider or inventory code how Barman metadata is structured.
func (Format) AnalyzeCatalog(ctx context.Context, reader store.Reader, objects []store.Object) Catalog {
	return Analyze(ctx, reader, objects)
}

func readBackup(ctx context.Context, reader store.Reader, key, server, id string) Backup {
	// One byte over the limit, so an oversized backup.info is rejected
	// rather than truncated by the adapter's MaxBytes cut.
	stream, err := reader.Open(ctx, store.OpenRequest{Key: key, MaxBytes: MaxBackupInfoBytes + 1})
	if err != nil {
		return Backup{Server: server, ID: id, State: evidence.Unknown, Reason: "backup metadata could not be read"}
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, MaxBackupInfoBytes+1))
	if err != nil || int64(len(data)) > MaxBackupInfoBytes {
		return Backup{Server: server, ID: id, State: evidence.Unknown, Reason: "backup metadata is unreadable"}
	}
	backup, err := ParseBackupInfo(data, server, id)
	if err != nil {
		return Backup{Server: server, ID: id, State: evidence.Unknown, Reason: "backup metadata is malformed"}
	}
	if len(backup.WithheldFields) > 0 {
		backup.State = evidence.Unknown
		backup.Reason = "backup metadata has invalid values, withheld: " + strings.Join(backup.WithheldFields, ", ")
		return backup
	}
	switch backup.Status {
	case "DONE":
		backup.State, backup.Reason = evidence.Warning, "awaiting structural artifact validation"
	case "STARTED":
		backup.State, backup.Reason = evidence.Warning, "backup is in progress"
	case "FAILED":
		backup.State, backup.Reason = evidence.Unhealthy, "backup failed"
	default:
		backup.State, backup.Reason = evidence.Unknown, "backup status is unsupported"
	}
	if unsupportedType(backup.Type) {
		backup.State, backup.Reason = evidence.Unknown, "backup type is unsupported"
	}
	if !supportedCompression(backup.Compression) {
		backup.State, backup.Reason = evidence.Unknown, "backup compression is unsupported"
	}
	return backup
}

func structuralState(ctx context.Context, reader store.Reader, backup Backup) (evidence.State, string) {
	if len(backup.Artifacts) == 0 {
		return evidence.Unhealthy, "expected main-data artifact is missing"
	}
	expectedRoots := append([]string{"data"}, backup.TablespaceOIDs...)
	for _, root := range expectedRoots {
		if !containsArtifactRoot(backup.Artifacts, root, backup.Compression) {
			return evidence.Unhealthy, "expected main-data or tablespace artifact is missing"
		}
	}
	seen := make(map[string]struct{}, len(backup.Artifacts))
	empty := false
	for _, key := range backup.Artifacts {
		if _, duplicate := seen[key]; duplicate {
			return evidence.Unknown, "duplicate backup artifact listing"
		}
		seen[key] = struct{}{}
		object, err := reader.Stat(ctx, store.StatRequest{Key: key})
		if err != nil || object.Key != key || object.Size < 0 {
			return evidence.Unknown, "backup artifact could not be confirmed"
		}
		empty = empty || object.Size < minimumArtifactBytes(path.Base(key))
	}
	// Decided once every artifact is confirmed: an unconfirmed or duplicate
	// listing says the catalog itself is not known.
	if empty {
		return evidence.Unhealthy, "a backup data artifact is smaller than any tar archive can be"
	}
	return evidence.Healthy, "all discovered backup data artifacts are stat-able"
}

// minimumArtifactBytes is a lower bound on the size of one Barman Cloud data
// artifact: each one, every _NNNN part included, is a complete tar stream
// (barman-cloud-backup closes one tar and opens the next at
// --max-archive-size), and a tar stream ends with two 512-byte zero blocks.
// A compressed artifact is bounded by its codec's smallest possible frame:
// gzip's 10-byte header and 8-byte trailer, bzip2's 4-byte header and
// 10-byte end-of-stream marker, the 10-byte snappy framing stream
// identifier, and lz4's 4-byte magic, 3-byte minimal frame descriptor and
// 4-byte end mark. The bounds are far below any real artifact, so the floor
// only refuses objects that cannot be an archive at all (an empty upload);
// it says nothing about the content. The file's own extension decides, as
// barman-cloud writes compression=None for a gzip backup.
func minimumArtifactBytes(name string) int64 {
	switch {
	case strings.HasSuffix(name, ".tar"):
		return 1024
	case strings.HasSuffix(name, ".tar.gz"):
		return 18
	case strings.HasSuffix(name, ".tar.bz2"):
		return 14
	case strings.HasSuffix(name, ".tar.lz4"):
		return 11
	case strings.HasSuffix(name, ".tar.snappy"):
		return 10
	default:
		return 1
	}
}

func backupInfoKey(key string) (string, string, bool) {
	server, id, ok := backupObjectKey(key)
	return server, id, ok && path.Base(key) == "backup.info"
}

func backupObjectKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) < 4 || parts[1] != "base" || parts[0] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
}

func artifacts(objects []store.Object, infoKey string, backup Backup) []string {
	result := make([]string, 0)
	expectedRoots := append([]string{"data"}, backup.TablespaceOIDs...)
	for _, object := range objects {
		base := path.Base(object.Key)
		if object.Key == infoKey {
			continue
		}
		for _, root := range expectedRoots {
			if artifactName(base, root, backup.Compression) {
				result = append(result, object.Key)
				break
			}
		}
	}
	slices.Sort(result)
	return result
}

func artifactBytes(objects []store.Object, keys []string) int64 {
	var total int64
	for _, key := range keys {
		for _, object := range objects {
			if object.Key == key {
				total += object.Size
				break
			}
		}
	}
	return total
}

func containsKey(objects []store.Object, key string) bool {
	for _, object := range objects {
		if object.Key == key {
			return true
		}
	}
	return false
}

func unsupportedType(value string) bool {
	return strings.Contains(value, "snapshot") || strings.Contains(value, "increment") || (value != "" && value != "full" && value != "postgres" && value != "rsync" && value != "tar")
}

func supportedCompression(value string) bool {
	switch strings.ToLower(value) {
	case "", "none", "gzip", "gz", "bzip2", "bz2", "snappy", "lz4":
		return true
	default:
		return false
	}
}

func artifactName(name, root, compression string) bool {
	if name == "" || root == "" {
		return false
	}
	prefix := root
	remainder := strings.TrimPrefix(name, prefix)
	if remainder == name {
		return false
	}
	if strings.HasPrefix(remainder, "_") {
		part, rest, ok := strings.Cut(remainder[1:], ".tar")
		if !ok || len(part) != 4 {
			return false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return false
			}
		}
		remainder = ".tar" + rest
	}
	if !strings.HasPrefix(remainder, ".tar") {
		return false
	}
	extension := strings.TrimPrefix(remainder, ".tar")
	for _, allowed := range compressionExtensions(compression) {
		if extension == allowed {
			return true
		}
	}
	return false
}

func containsArtifactRoot(keys []string, root, compression string) bool {
	for _, key := range keys {
		name := path.Base(key)
		if strings.HasPrefix(name, root+".tar") && artifactName(name, root, compression) {
			return true
		}
	}
	return false
}

func compressionExtensions(compression string) []string {
	switch strings.ToLower(compression) {
	case "none":
		return []string{""}
	case "gzip", "gz":
		return []string{".gz"}
	case "bzip2", "bz2":
		return []string{".bz2"}
	case "snappy":
		return []string{".snappy"}
	case "lz4":
		return []string{".lz4"}
	default:
		return []string{"", ".gz", ".bz2", ".snappy", ".lz4"}
	}
}

var tablespaceTuplePattern = regexp.MustCompile(`\(\s*['"][^'"]*['"]\s*,\s*['"]?([0-9]+)['"]?\s*,\s*['"][^'"]*['"]\s*\)`)

func parseTablespaceOIDs(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "None" || value == "[]" {
		return nil, nil
	}
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, errors.New("invalid tablespace list")
	}
	matches := tablespaceTuplePattern.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return nil, errors.New("invalid tablespace list")
	}
	residual := tablespaceTuplePattern.ReplaceAllString(value, "")
	if strings.IndexFunc(residual, func(character rune) bool {
		return character != '[' && character != ']' && character != ',' && !unicode.IsSpace(character)
	}) >= 0 {
		return nil, errors.New("invalid tablespace list")
	}
	oids := make([]string, 0, len(matches))
	for _, match := range matches {
		if slices.Contains(oids, match[1]) {
			return nil, errors.New("duplicate tablespace OID")
		}
		oids = append(oids, match[1])
	}
	slices.Sort(oids)
	return oids, nil
}

func first(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := values[key]; value != "" && !strings.EqualFold(value, "none") {
			return value
		}
	}
	return ""
}

func firstIncludingNone(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := values[key]; value != "" {
			return value
		}
	}
	return ""
}

func assignLegacyXLog(wal, lsn *string, value string) {
	if value == "" {
		return
	}
	if *wal == "" && len(value) == 24 && isHex(value) {
		*wal = value
		return
	}
	if *lsn == "" {
		*lsn = value
	}
}

func parseOptionalInt(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

func parseBarmanTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05 -0700", "Mon Jan 2 15:04:05 2006"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, errors.New("invalid time")
}
