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

package evidenceapi

import (
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
)

// FuzzBackupInfoProjection runs untrusted backup.info bytes through the Barman
// parser and the evidence projection. Every metadata file the parser accepts
// must project into a publication that the shared api validators accept:
// otherwise one malformed object in the repository would make the agent
// unable to publish anything.
func FuzzBackupInfoProjection(f *testing.F) {
	f.Add([]byte("status=DONE\nbegin_time=2026-07-27 10:00:00 +0000\nend_time=2026-07-27 10:01:00 +0000\nbegin_wal=000000010000000000000001\nend_wal=000000010000000000000002\nbegin_lsn=0/01000000\nend_lsn=0/02000000\ntimeline=1\nsystemid=7612345678901234567\nxlog_segment_size=16777216\nversion=180000\nsize=42\ndeduplicated_size=21\ncompression=gzip\n"))
	f.Add([]byte("status=DONE\nbegin_wal=00000001000000000000000a\nend_lsn=0/zz\nxlog_segment_size=3\n"))
	f.Add([]byte("status=DONE\nbegin_time=2026-07-27 10:01:00 +0000\nend_time=2026-07-27 10:00:00 +0000\n"))
	f.Add([]byte("status=\xff\xfe\nbackup_type=a\x01b\nencryption=\xff\n"))
	f.Add([]byte("status=STARTED\nbackup_type=postgres\nencryption=none\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := barmancloud.ParseBackupInfo(data, "orders", "20260728T100000")
		if err != nil {
			return
		}
		source := completeSource(t)
		backup := source.BarmanCatalog.Backups[0]
		parsed.Artifacts, parsed.StoredBytes = backup.Artifacts, backup.StoredBytes
		parsed.State, parsed.Reason = evidence.Unknown, "fuzzed backup metadata"
		if len(parsed.WithheldFields) == 0 && parsed.Status == "DONE" {
			parsed.State, parsed.Reason = evidence.Warning, "fuzzed backup metadata awaiting validation"
		}
		source.BarmanCatalog.Backups[0] = parsed
		source.Evidence.State = evidence.Unknown
		source.Evidence.Reason = "fuzzed backup metadata"
		publication, err := Project(source, testProjectOptions())
		if err != nil {
			t.Fatalf("accepted backup.info cannot be projected: %v\n%q", err, data)
		}
		if err := publication.Validate(); err != nil {
			t.Fatalf("projected snapshot is invalid: %v", err)
		}
		for _, item := range publication.Backups {
			if err := item.Validate(); err != nil {
				t.Fatalf("projected backup is invalid: %v\n%q", err, data)
			}
		}
	})
}
