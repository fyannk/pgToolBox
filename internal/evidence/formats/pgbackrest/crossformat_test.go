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
	"io"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

// TestBarmanAndPGBackRestAgreeOnAnOffTargetBackup: the same backup, WAL and
// timeline history, read as a pgBackRest repository and as a Barman Cloud
// one, give the same answer. Timeline 2 branched one byte before the backup
// stopped, so a default restore (recovery_target_timeline 'latest') fails
// from it: both formats publish a warning, off-target path on the backup's
// own timeline, and neither calls coverage healthy; with the switchpoint at
// the stop LSN both follow the child.
func TestBarmanAndPGBackRestAgreeOnAnOffTargetBackup(t *testing.T) {
	t.Parallel()
	plain := loadFixture(t, fixtureRoot, "plain")
	for _, test := range []struct {
		switchpoint string
		offTarget   bool
	}{
		{switchpoint: "0/E00004F", offTarget: true},
		{switchpoint: "0/E000050"},
	} {
		t.Run(test.switchpoint, func(t *testing.T) {
			t.Parallel()
			history := "1\t" + test.switchpoint + "\tno recovery target specified\n"
			report := analyzeFixture(t, plain.with(switchHistoryKey, []byte(history)), "plain")
			pgbackrest := backupLabelled(t, report, incrAtSwitch)
			if pgbackrest.Path.OffTarget != test.offTarget {
				t.Fatalf("pgBackRest path %+v", pgbackrest.Path)
			}

			var wal ArchiveWAL
			for _, value := range report.WAL {
				if value.ArchiveID == pgbackrest.ArchiveID {
					wal = value
				}
			}
			server := barmancloud.ServerWAL{Server: "demo", State: evidence.Healthy, PostgreSQLVersion: 170000, SegmentSize: wal.SegmentSize}
			for _, value := range wal.Ranges {
				server.Ranges = append(server.Ranges, barmancloud.WALRange(value))
			}
			backup := barmancloud.Backup{
				Server: "demo", ID: incrAtSwitch, State: evidence.Healthy, BeginWAL: pgbackrest.Backup.ArchiveStart, EndWAL: pgbackrest.Backup.ArchiveStop,
				EndLSN: pgbackrest.Backup.LSNStop, SegmentSize: wal.SegmentSize, PostgreSQLVersion: 170000, Timeline: 1, EndAt: pgbackrest.Backup.TimestampStop,
			}
			reader := &storetest.Fake{OpenFunc: func(context.Context, store.OpenRequest) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader([]byte(history))), nil
			}}
			objects := []barmancloud.HistoryObject{{Key: "demo/wals/00000002.history", Server: "demo", Name: "00000002.history", Compression: "none", Timeline: 2}}
			catalog := barmancloud.AnalyzeRecovery(context.Background(), reader, []barmancloud.Backup{backup}, barmancloud.WALCatalog{Servers: []barmancloud.ServerWAL{server}}, objects, false, barmancloud.RecoveryOptions{})
			if err := catalog.Validate(); err != nil {
				t.Fatal(err)
			}
			var barman barmancloud.RecoveryPath
			for _, path := range catalog.Servers[0].Paths {
				if path.TargetTimeline == pgbackrest.Path.TargetTimeline {
					barman = path
				}
			}
			if barman.State != pgbackrest.Path.State || barman.OffTarget != pgbackrest.Path.OffTarget || barman.FrontierTimeline != pgbackrest.Path.FrontierTimeline {
				t.Fatalf("Barman path (state %s, off-target %v, frontier TL%d, %q) differs from pgBackRest (state %s, off-target %v, frontier TL%d, %q)",
					barman.State, barman.OffTarget, barman.FrontierTimeline, barman.Reason, pgbackrest.Path.State, pgbackrest.Path.OffTarget, pgbackrest.Path.FrontierTimeline, pgbackrest.Path.Reason)
			}
			if test.offTarget {
				if barman.State != evidence.Warning || catalog.Servers[0].CoverageState == evidence.Healthy {
					t.Fatalf("Barman off-target path %s, coverage %s", barman.State, catalog.Servers[0].CoverageState)
				}
				if assessment := assessmentLabelled(t, report, incrAtSwitch); assessment.State != evidence.Warning || assessment.Usable {
					t.Fatalf("pgBackRest assessment %+v", assessment)
				}
			}
		})
	}
}
