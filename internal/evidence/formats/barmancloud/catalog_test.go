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
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

func TestParseBackupInfoAcceptsHistoricalFieldsWithoutRetainingUnknownValues(t *testing.T) {
	t.Parallel()
	backup, err := ParseBackupInfo([]byte("status=DONE\nbegin_time=2026-07-27 10:00:00 +0000\nend_time=2026-07-27 10:01:00 +0000\nbegin_xlog=000000010000000000000001\nend_xlog=000000010000000000000002\nxlog_segment_size=16777216\nversion=180000\nsize=42\ndeduplicated_size=21\ncompression=gzip\nprivate_note=do-not-render\n"), "alpha", "20260727T100000")
	if err != nil {
		t.Fatal(err)
	}
	if backup.Status != "DONE" || backup.PostgreSQLVersion != 180000 || backup.LogicalBytes != 42 || backup.DeduplicatedBytes == nil || *backup.DeduplicatedBytes != 21 || backup.BeginWAL == "" || backup.BeginAt.IsZero() {
		t.Fatalf("parsed backup = %#v", backup)
	}
}

func TestParseBackupInfoRetainsAllowlistedRecoveryAnchors(t *testing.T) {
	t.Parallel()
	backup, err := ParseBackupInfo([]byte("status=DONE\nbegin_wal=000000020000000100000000\nend_wal=000000020000000100000001\nbegin_lsn=1/00000000\nend_lsn=1/01000000\ntimeline=2\nsystemid=7612345678901234567\nxlog_segment_size=16777216\nversion=180000\n"), "alpha", "recovery-anchor")
	if err != nil {
		t.Fatal(err)
	}
	if backup.BeginWAL != "000000020000000100000000" || backup.EndWAL != "000000020000000100000001" || backup.BeginLSN != "1/00000000" || backup.EndLSN != "1/01000000" || backup.Timeline != 2 || backup.SystemID != "7612345678901234567" {
		t.Fatalf("recovery anchors = %#v", backup)
	}
}

func TestParseBackupInfoRejectsMalformedRecoveryIdentity(t *testing.T) {
	t.Parallel()
	for _, metadata := range []string{
		"status=DONE\ntimeline=0\n",
		"status=DONE\ntimeline=not-a-timeline\n",
		"status=DONE\nsystemid=secret-like-free-text\n",
	} {
		if _, err := ParseBackupInfo([]byte(metadata), "alpha", "invalid"); err == nil {
			t.Fatalf("ParseBackupInfo(%q) succeeded", metadata)
		}
	}
}

// Values Barman never writes, which the evidence contract cannot carry, are
// dropped at parse time and named, instead of making the whole publication
// unprojectable later. The kept values must stay byte-identical.
func TestParseBackupInfoWithholdsValuesOutsideTheEvidenceConstraints(t *testing.T) {
	t.Parallel()
	const anchors = "begin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nbegin_lsn=0/1000000\nend_lsn=0/2000000\nxlog_segment_size=16777216\n"
	tests := []struct {
		name     string
		metadata string
		withheld []string
	}{
		{name: "valid anchors kept", metadata: "status=DONE\n" + anchors},
		{name: "lower-case WAL name", metadata: "status=DONE\nbegin_wal=00000001000000000000000a\n", withheld: []string{"begin_wal"}},
		{name: "short WAL name", metadata: "status=DONE\nend_wal=0000000100000000\n", withheld: []string{"end_wal"}},
		{name: "LSN without slash", metadata: "status=DONE\nbegin_lsn=01000000\n", withheld: []string{"begin_lsn"}},
		{name: "lower-case LSN", metadata: "status=DONE\nend_lsn=0/2a000000\n", withheld: []string{"end_lsn"}},
		{name: "oversized LSN half", metadata: "status=DONE\nend_lsn=0/123456789\n", withheld: []string{"end_lsn"}},
		{name: "segment size not a power of two", metadata: "status=DONE\nxlog_segment_size=16777215\n", withheld: []string{"wal_segment_size"}},
		{name: "segment size too large", metadata: "status=DONE\nxlog_segment_size=2147483648\n", withheld: []string{"wal_segment_size"}},
		{name: "end before begin", metadata: "status=DONE\nbegin_time=2026-07-27 10:01:00 +0000\nend_time=2026-07-27 10:00:00 +0000\n", withheld: []string{"end_time"}},
		{name: "oversized compression text", metadata: "status=DONE\ncompression=" + strings.Repeat("z", 65) + "\n", withheld: []string{"compression"}},
		{name: "invalid UTF-8 encryption", metadata: "status=DONE\nencryption=\xff\n", withheld: []string{"encryption"}},
		{name: "control character in backup type", metadata: "status=DONE\nbackup_type=a\x01b\n", withheld: []string{"backup_type"}},
		{name: "oversized status", metadata: "status=" + strings.Repeat("D", 65) + "\n", withheld: []string{"status"}},
		{name: "several fields", metadata: "status=DONE\nbegin_wal=x\nend_lsn=y\n", withheld: []string{"begin_wal", "end_lsn"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backup, err := ParseBackupInfo([]byte(test.metadata), "alpha", "withheld")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(backup.WithheldFields, ",") != strings.Join(test.withheld, ",") {
				t.Fatalf("withheld = %v; want %v", backup.WithheldFields, test.withheld)
			}
			for _, field := range test.withheld {
				var value string
				switch field {
				case "begin_wal":
					value = backup.BeginWAL
				case "end_wal":
					value = backup.EndWAL
				case "begin_lsn":
					value = backup.BeginLSN
				case "end_lsn":
					value = backup.EndLSN
				case "compression":
					value = backup.Compression
				case "encryption":
					value = backup.Encryption
				case "backup_type":
					value = backup.Type
				case "status":
					value = backup.Status
				case "wal_segment_size":
					if backup.SegmentSize != 0 {
						value = "retained"
					}
				case "end_time":
					if !backup.BeginAt.IsZero() || !backup.EndAt.IsZero() {
						value = "retained"
					}
				}
				if value != "" {
					t.Fatalf("withheld field %s retained %q", field, value)
				}
			}
			if test.withheld == nil && (backup.BeginWAL != "000000010000000000000001" || backup.EndLSN != "0/2000000" || backup.SegmentSize != 16<<20) {
				t.Fatalf("valid anchors changed: %#v", backup)
			}
		})
	}
}

func TestAnalyzeMakesBackupsWithWithheldMetadataUnknown(t *testing.T) {
	t.Parallel()
	objects := []store.Object{{Key: "alpha/base/odd/backup.info"}, {Key: "alpha/base/odd/data.tar.gz", Size: 10240}}
	reader := &storetest.Fake{
		OpenFunc: func(_ context.Context, _ store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("status=DONE\nbegin_wal=not-a-wal\n")), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			return store.Object{Key: key, Size: 10240}, nil
		},
	}
	catalog := Analyze(context.Background(), reader, objects)
	if len(catalog.Backups) != 1 || catalog.Backups[0].State != evidence.Unknown || !strings.Contains(catalog.Backups[0].Reason, "begin_wal") || strings.Contains(catalog.Backups[0].Reason, "not-a-wal") {
		t.Fatalf("catalog = %#v", catalog.Backups)
	}
}

func TestParseBackupInfoAcceptsPinnedBarmanGeneratedGolden(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/barman-3.19.1/completed/backup.info")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := ParseBackupInfo(data, "alpha", "20260727T100000")
	if err != nil {
		t.Fatal(err)
	}
	if backup.Status != "DONE" || backup.Type != "postgres" || backup.Compression != "gzip" || len(backup.TablespaceOIDs) != 1 || backup.TablespaceOIDs[0] != "16384" {
		t.Fatalf("generated metadata = %#v", backup)
	}
	if got := backup.BeginAt.UTC().String(); got != "2026-07-27 10:00:00 +0000 UTC" {
		t.Fatalf("begin time = %s", got)
	}
}

func TestAnalyzeNeverMakesMalformedOrIncompleteBackupsUsable(t *testing.T) {
	t.Parallel()
	objects := []store.Object{{Key: "alpha/base/good/backup.info"}, {Key: "alpha/base/good/data.tar.gz", Size: 10240}, {Key: "alpha/base/missing/backup.info"}, {Key: "alpha/base/bad/backup.info"}}
	metadata := map[string]string{
		"alpha/base/good/backup.info":    "status=DONE\nsize=4\n",
		"alpha/base/missing/backup.info": "status=DONE\n",
		"alpha/base/bad/backup.info":     "this is not metadata\n",
	}
	reader := &storetest.Fake{
		OpenFunc: func(_ context.Context, request store.OpenRequest) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(metadata[request.Key])), nil
		},
		StatFunc: func(_ context.Context, key string) (store.Object, error) {
			return store.Object{Key: key, Size: 10240}, nil
		},
	}
	catalog := Analyze(context.Background(), reader, objects)
	if len(catalog.Backups) != 3 {
		t.Fatalf("catalog = %#v", catalog)
	}
	if catalog.Backups[0].State != evidence.Unknown || catalog.Backups[1].State != evidence.Healthy || catalog.Backups[2].State != evidence.Unhealthy {
		t.Fatalf("backup states = %#v", catalog.Backups)
	}
}

func TestAnalyzeLeavesSnapshotAndIncrementalMetadataUnknown(t *testing.T) {
	t.Parallel()
	objects := []store.Object{{Key: "alpha/base/snap/backup.info"}, {Key: "alpha/base/snap/data.tar", Size: 1}}
	reader := &storetest.Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("status=DONE\nbackup_type=snapshot\n")), nil
	}}
	catalog := Analyze(context.Background(), reader, objects)
	if len(catalog.Backups) != 1 || catalog.Backups[0].State != evidence.Unknown {
		t.Fatalf("snapshot backup = %#v", catalog)
	}
}

func TestAnalyzePathologicalBarmanCatalogStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		metadata string
		objects  []store.Object
		want     evidence.State
		reason   string
	}{
		{name: "completed", metadata: "status=DONE\nmode=postgres\ncompression=gzip\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.gz", Size: 10240}}, want: evidence.Healthy, reason: "stat-able"},
		{name: "cloud gzip with null compression metadata", metadata: "status=DONE\ncompression=None\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.gz", Size: 10240}}, want: evidence.Healthy, reason: "stat-able"},
		{name: "started is never promoted by an artifact", metadata: "status=STARTED\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Warning, reason: "in progress"},
		{name: "failed", metadata: "status=FAILED\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Unhealthy, reason: "failed"},
		{name: "malformed", metadata: "not key value\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}}, want: evidence.Unknown, reason: "malformed"},
		{name: "missing metadata", objects: []store.Object{{Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Unknown, reason: "metadata is missing"},
		{name: "missing main data", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/16384.tar", Size: 10240}}, want: evidence.Unhealthy, reason: "main-data"},
		{name: "missing tablespace", metadata: "status=DONE\nmode=postgres\ntablespaces=[('analytics', 16384, '/ts')]\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Unhealthy, reason: "tablespace"},
		{name: "split main data and tablespace", metadata: "status=DONE\nmode=postgres\ncompression=lz4\ntablespaces=[('analytics', 16384, '/ts')]\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.lz4", Size: 10240}, {Key: "alpha/base/id/data_0001.tar.lz4", Size: 10240}, {Key: "alpha/base/id/16384.tar.lz4", Size: 10240}, {Key: "alpha/base/id/16384_0001.tar.lz4", Size: 10240}}, want: evidence.Healthy, reason: "stat-able"},
		{name: "unknown compression", metadata: "status=DONE\nmode=postgres\ncompression=zstd\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.zst", Size: 10240}}, want: evidence.Unknown, reason: "compression is unsupported"},
		{name: "future status", metadata: "status=COMPLETED\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Unknown, reason: "status is unsupported"},
		{name: "malformed tablespace list", metadata: "status=DONE\nmode=postgres\ntablespaces=[('analytics', 16384, '/ts'), garbage]\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}, {Key: "alpha/base/id/16384.tar", Size: 10240}}, want: evidence.Unknown, reason: "metadata is malformed"},
		{name: "duplicate object", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}, {Key: "alpha/base/id/data.tar", Size: 10240}}, want: evidence.Unknown, reason: "duplicate"},
		// An artifact smaller than any archive of its kind (an empty upload)
		// is not usable; the floors are the smallest valid stream of each.
		{name: "empty data tar", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 0}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "tar below two end blocks", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 1023}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "tar of exactly two end blocks", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 1024}}, want: evidence.Healthy, reason: "stat-able"},
		{name: "empty gzip artifact under null compression metadata", metadata: "status=DONE\ncompression=None\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.gz", Size: 0}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "gzip below header and trailer", metadata: "status=DONE\ncompression=gzip\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.gz", Size: 17}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "gzip of header and trailer", metadata: "status=DONE\ncompression=gzip\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.gz", Size: 18}}, want: evidence.Healthy, reason: "stat-able"},
		{name: "bzip2 below its empty stream", metadata: "status=DONE\ncompression=bzip2\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.bz2", Size: 13}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "snappy below its stream identifier", metadata: "status=DONE\ncompression=snappy\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.snappy", Size: 9}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "empty split part", metadata: "status=DONE\nmode=postgres\ncompression=lz4\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar.lz4", Size: 10240}, {Key: "alpha/base/id/data_0001.tar.lz4", Size: 10}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "empty tablespace artifact", metadata: "status=DONE\nmode=postgres\ntablespaces=[('analytics', 16384, '/ts')]\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 10240}, {Key: "alpha/base/id/16384.tar", Size: 0}}, want: evidence.Unhealthy, reason: "smaller than any tar archive"},
		{name: "a duplicate listing stays unknown beside an empty artifact", metadata: "status=DONE\nmode=postgres\n", objects: []store.Object{{Key: "alpha/base/id/backup.info"}, {Key: "alpha/base/id/data.tar", Size: 0}, {Key: "alpha/base/id/data.tar", Size: 0}}, want: evidence.Unknown, reason: "duplicate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &storetest.Fake{
				OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader(test.metadata)), nil
				},
				StatFunc: func(_ context.Context, key string) (store.Object, error) {
					for _, object := range test.objects {
						if object.Key == key {
							return object, nil
						}
					}
					return store.Object{}, errors.New("not listed")
				},
			}
			catalog := Analyze(context.Background(), reader, test.objects)
			if len(catalog.Backups) != 1 || catalog.Backups[0].State != test.want || !strings.Contains(catalog.Backups[0].Reason, test.reason) {
				t.Fatalf("catalog = %#v, want %s containing %q", catalog, test.want, test.reason)
			}
		})
	}
}
