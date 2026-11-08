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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
)

const shapeGolden = "testdata/golden/shape.json"

// The full report of each committed repository, byte for byte.
func TestReportGoldens(t *testing.T) {
	t.Parallel()
	for _, variant := range fixtureVariants {
		t.Run(variant, func(t *testing.T) {
			t.Parallel()
			report := analyzeFixture(t, loadFixture(t, fixtureRoot, variant), variant)
			compareJSONGolden(t, filepath.Join("testdata/golden", variant+".report.json"), report)
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(fixturePassphrase)) || bytes.Contains(data, []byte("cipher-pass")) {
				t.Fatal("the report carries a passphrase")
			}
		})
	}
}

// The shape of the committed repositories' reports (see testdata/README.md).
func TestCommittedFixturesShapeGolden(t *testing.T) {
	t.Parallel()
	compareJSONGolden(t, shapeGolden, shapeOfTree(t, fixtureRoot))
}

// TestRegeneratedFixturesMatchCommittedShape is the fixture regeneration
// check: hack/evidence/test-pgbackrest-fixtures.sh runs it with the tree the
// generator just wrote. Without that tree it is skipped.
func TestRegeneratedFixturesMatchCommittedShape(t *testing.T) {
	root := os.Getenv("PGBACKREST_REGENERATED_FIXTURES")
	if root == "" {
		t.Skip("PGBACKREST_REGENERATED_FIXTURES is not set; make test-pgbackrest-fixtures sets it")
	}
	var committed map[string]reportShape
	readJSONGolden(t, shapeGolden, &committed)
	regenerated := shapeOfTree(t, root)
	for _, variant := range fixtureVariants {
		if !reflect.DeepEqual(committed[variant], regenerated[variant]) {
			want, _ := json.MarshalIndent(committed[variant], "", "  ")
			got, _ := json.MarshalIndent(regenerated[variant], "", "  ")
			t.Errorf("%s: the regenerated repository has another shape\n%s", variant, firstDifference(want, got))
		}
	}
	version, err := os.ReadFile(filepath.Join(root, "pgbackrest-version.txt"))
	if err != nil || string(bytes.TrimSpace(version)) != "pgBackRest 2.60.0" {
		t.Errorf("generator version %q, %v", version, err)
	}
}

func shapeOfTree(t testing.TB, root string) map[string]reportShape {
	t.Helper()
	result := make(map[string]reportShape)
	for _, variant := range fixtureVariants {
		repository := loadFixture(t, root, variant)
		result[variant] = shapeOf(analyzeFixture(t, repository, variant), repository)
	}
	return result
}

// reportShape is the part of a report that does not change between two runs
// of the generator. Labels become "<type>#<n>" in timestamp order; system
// ids, checksums, salts, timestamps, sizes, WAL names and segment counts are
// dropped; states, reasons, problems, finding codes, chain structure,
// timelines and the window's ends (as symbolic labels and a timeline) are
// kept.
type reportShape struct {
	State             evidence.State
	Reason            string
	Encrypted         bool
	BackupInfo        pairShape
	ArchiveInfo       pairShape
	Database          *databaseShape
	ArchiveIDs        []string
	SegmentSize       int64
	SegmentSizeSource SegmentSizeSource
	Backups           []backupShape
	WAL               []walShape
	Timelines         []timelineShape
	Window            *windowShape
	Findings          []findingShape
	ListedKinds       map[string]int
}

type pairShape struct {
	Used                     FileSource
	Agree                    bool
	MainProblem, CopyProblem FileProblem
	MainEncrypted            bool
	MainFormat               int
}

type databaseShape struct {
	Version                        string
	VersionNumber                  int
	ID, CatalogVersion, ControlVer uint32
}

type backupShape struct {
	Name, Type, Prior    string
	References           []string
	ChainState           evidence.State
	ManifestState        evidence.State
	ManifestUsed         FileSource
	ManifestAgree        bool
	PathState            evidence.State
	PathStop             CoverageStop
	OffTarget            bool
	Consistent           bool
	TargetTimeline       uint32
	Timelines            []uint32
	StartTimeline        string
	FrontierTimeline     uint32
	Usable               bool
	Annotation           string
	HasArchiveAndLSN     bool
	BackrestVersion      string
	ArchiveID            string
	OptionCompress       bool
	OptionArchiveCheck   bool
	Error                bool
	ReferenceCountInInfo int
}

type walShape struct {
	ArchiveID       string
	State           evidence.State
	RangeTimelines  []uint32
	Gaps            int
	History         int64
	BackupHistory   int64
	Partials        int64
	Unknown         int64
	Duplicates      int64
	SegmentsPresent bool
}

type timelineShape struct {
	Timeline uint32
	State    evidence.State
	Edges    [][2]uint32
}

type windowShape struct {
	OldestBackup, NewestBackup string
	EndTimeline                uint32
	UsableBackups              int
	Assumptions                []string
}

type findingShape struct {
	Code     FindingCode
	Severity Severity
}

func shapeOf(report Report, repository *fixtureRepository) reportShape {
	names := symbolicNames(report.Backups)
	shape := reportShape{
		State: report.State, Reason: report.Reason, Encrypted: report.Encrypted,
		BackupInfo: pairShapeOf(report.BackupInfo), ArchiveInfo: pairShapeOf(report.ArchiveInfo),
		ArchiveIDs: report.ArchiveIDs, SegmentSize: report.SegmentSize, SegmentSizeSource: report.SegmentSizeSource,
		ListedKinds: make(map[string]int),
	}
	if report.Database != nil {
		shape.Database = &databaseShape{Version: report.Database.Version, VersionNumber: report.Database.VersionNumber, ID: report.Database.ID, CatalogVersion: report.Database.CatalogVersion, ControlVer: report.Database.ControlVersion}
	}
	ordered := append([]BackupReport(nil), report.Backups...)
	sort.SliceStable(ordered, func(i, j int) bool { return names[ordered[i].Backup.Label] < names[ordered[j].Backup.Label] })
	for _, backup := range ordered {
		references := make([]string, 0, len(backup.Backup.Reference))
		for _, reference := range backup.Backup.Reference {
			references = append(references, names[reference])
		}
		value := backupShape{
			Name: names[backup.Backup.Label], Type: string(backup.Backup.Type), Prior: names[backup.Backup.Prior], References: references,
			ChainState: backup.ChainState, ManifestState: backup.Manifest.State, ManifestUsed: backup.Manifest.Pair.Used, ManifestAgree: backup.Manifest.Pair.Agree,
			PathState: backup.Path.State, PathStop: backup.Path.Stop, OffTarget: backup.Path.OffTarget, Consistent: backup.Path.Consistent,
			TargetTimeline: backup.Path.TargetTimeline, Timelines: backup.Path.Timelines, FrontierTimeline: backup.Path.FrontierTimeline,
			Usable: backup.Usable, Annotation: backup.Backup.Annotations["cnpg-backup-name"],
			HasArchiveAndLSN: backup.Backup.ArchiveStart != "" && backup.Backup.LSNStop != "", BackrestVersion: backup.Backup.BackrestVersion,
			ArchiveID: backup.ArchiveID, OptionCompress: backup.Backup.OptionCompress, OptionArchiveCheck: backup.Backup.OptionArchiveCheck,
			Error: backup.Backup.Error != nil && *backup.Backup.Error, ReferenceCountInInfo: len(backup.Backup.Reference),
		}
		if len(backup.Backup.ArchiveStart) == 24 {
			value.StartTimeline = backup.Backup.ArchiveStart[:8]
		}
		shape.Backups = append(shape.Backups, value)
	}
	for _, wal := range report.WAL {
		value := walShape{
			ArchiveID: wal.ArchiveID, State: wal.State, Gaps: len(wal.Gaps), History: wal.Counts.History, BackupHistory: wal.Counts.BackupHistory,
			Partials: wal.Counts.Partials, Unknown: wal.Counts.Unknown, Duplicates: wal.Counts.Duplicates, SegmentsPresent: wal.Counts.Segments > 0,
		}
		for _, rangeValue := range wal.Ranges {
			value.RangeTimelines = append(value.RangeTimelines, rangeValue.Timeline)
		}
		shape.WAL = append(shape.WAL, value)
	}
	for _, history := range report.Timelines {
		value := timelineShape{Timeline: history.Timeline, State: history.State}
		for _, edge := range history.Edges {
			value.Edges = append(value.Edges, [2]uint32{edge.Parent, edge.Child})
		}
		shape.Timelines = append(shape.Timelines, value)
	}
	if report.Window != nil {
		shape.Window = &windowShape{OldestBackup: names[report.Window.OldestBackup], NewestBackup: names[report.Window.NewestBackup], EndTimeline: report.Window.EndTimeline, UsableBackups: report.Window.UsableBackups, Assumptions: report.Window.Assumptions}
	}
	for _, finding := range report.Findings {
		shape.Findings = append(shape.Findings, findingShape{Code: finding.Code, Severity: finding.Severity})
	}
	for _, object := range repository.objects {
		classified, belongs := ClassifyWALObject(object)
		kind := "other"
		if belongs {
			kind = "archive:" + string(classified.Class)
		}
		if kind != "archive:segment" {
			shape.ListedKinds[kind]++
		}
	}
	return shape
}

func pairShapeOf(pair PairCheck) pairShape {
	return pairShape{Used: pair.Used, Agree: pair.Agree, MainProblem: pair.Main.Problem, CopyProblem: pair.Copy.Problem, MainEncrypted: pair.Main.Encrypted, MainFormat: pair.Main.Format}
}

// symbolicNames names backups "<type>#<n>", numbered per type in the order
// of their backup-timestamp-stop.
func symbolicNames(backups []BackupReport) map[string]string {
	ordered := append([]BackupReport(nil), backups...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].Backup.TimestampStop.Equal(ordered[j].Backup.TimestampStop) {
			return ordered[i].Backup.TimestampStop.Before(ordered[j].Backup.TimestampStop)
		}
		return ordered[i].Backup.Label < ordered[j].Backup.Label
	})
	names := map[string]string{"": ""}
	counts := make(map[BackupType]int)
	for index, backup := range ordered {
		counts[backup.Backup.Type]++
		names[backup.Backup.Label] = strconv.Itoa(index+1) + ":" + string(backup.Backup.Type) + "#" + strconv.Itoa(counts[backup.Backup.Type])
	}
	return names
}
