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

package web

import (
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
)

// pgbackrestStanzaView is one analysed stanza on the summary page.
type pgbackrestStanzaView struct {
	Stanza, ArchiveStanza, State, Reason   string
	Encryption, RepositoryFormat, Database string
	SegmentSize                            string
	Aspects                                []pgbackrestAspectView
	Backups                                []pgbackrestBackupView
	Window                                 *pgbackrestWindowView
	Findings                               []pgbackrestFindingView
	Recovery                               recoveryServerView
}

type pgbackrestAspectView struct{ Name, State, Reason string }

type pgbackrestBackupView struct {
	Label, Type, Prior, State, Reason, Usable string
	Size, RepositorySize, Start, Stop         string
	ArchiveStart, ArchiveStop, CNPGBackup     string
}

type pgbackrestWindowView struct {
	Oldest, Start, Newest, NewestStop, EndWAL, EndLSN, EndReceipt, Usable, Assumptions string
}

type pgbackrestFindingView struct{ Code, Severity, Subject, Message string }

// isPGBackRest reports whether the handler serves a pgBackRest repository.
func (h *Handler) isPGBackRest() bool { return h.format.ID == pgbackrest.FormatID }

func pgbackrestStanzaViews(reports []pgbackrest.Report) []pgbackrestStanzaView {
	result := make([]pgbackrestStanzaView, 0, len(reports))
	for _, report := range reports {
		aspects := report.Aspects()
		view := pgbackrestStanzaView{
			Stanza: report.Stanza, ArchiveStanza: report.ArchiveStanza,
			State: string(aspects.Overall.State), Reason: aspects.Overall.Reason,
			Encryption:       string(aspects.Encryption.State) + " — " + aspects.Encryption.Reason,
			RepositoryFormat: "unknown", Database: "unknown", SegmentSize: "unknown",
			Aspects: []pgbackrestAspectView{
				{"Catalog", string(aspects.Catalog.State), aspects.Catalog.Reason},
				{"Backup chains", string(aspects.Chains.State), aspects.Chains.Reason},
				{"WAL continuity", string(aspects.WAL.State), aspects.WAL.Reason},
				{"Timelines", string(aspects.Timeline.State), aspects.Timeline.Reason},
				{"Recovery coverage", string(aspects.Coverage.State), aspects.Coverage.Reason},
			},
			Recovery: pgbackrestRecoveryView(report, aspects),
		}
		if format := report.RepositoryFormat(); format > 0 {
			view.RepositoryFormat = strconv.Itoa(format)
		}
		if report.Database != nil {
			view.Database = "PostgreSQL " + report.Database.Version + ", system " + strconv.FormatUint(report.Database.SystemID, 10)
		}
		if report.SegmentSize > 0 {
			view.SegmentSize = bytesText(true, report.SegmentSize) + " (" + string(report.SegmentSizeSource) + ")"
		}
		for index, assessment := range report.Assess() {
			backup := report.Backups[index].Backup
			usable := "yes"
			if !assessment.Usable {
				reasons := make([]string, 0, len(assessment.Unusable))
				for _, reason := range assessment.Unusable {
					reasons = append(reasons, string(reason))
				}
				usable = "no: " + strings.Join(reasons, ", ")
			}
			view.Backups = append(view.Backups, pgbackrestBackupView{
				Label: backup.Label, Type: string(backup.Type), Prior: noneText(backup.Prior),
				State: string(assessment.State), Reason: assessment.Reason, Usable: usable,
				Size: unsignedBytesText(backup.Size), RepositorySize: unsignedBytesText(backup.RepoSize),
				Start: timeText(backup.TimestampStart), Stop: timeText(backup.TimestampStop),
				ArchiveStart: unknownText(backup.ArchiveStart), ArchiveStop: unknownText(backup.ArchiveStop),
				CNPGBackup: noneText(backup.Annotations["cnpg-backup-name"]),
			})
		}
		if window := report.Window; window != nil {
			view.Window = &pgbackrestWindowView{
				Oldest: window.OldestBackup, Start: timeText(window.Start), Newest: window.NewestBackup, NewestStop: timeText(window.NewestStop),
				EndWAL: window.EndWAL, EndLSN: window.EndLSN, EndReceipt: archiveReceiptText(window.EndReceipt),
				Usable: strconv.Itoa(window.UsableBackups), Assumptions: strings.Join(window.Assumptions, "; "),
			}
		}
		for _, finding := range report.Findings {
			view.Findings = append(view.Findings, pgbackrestFindingView{Code: string(finding.Code), Severity: string(finding.Severity), Subject: finding.Subject, Message: finding.Message})
		}
		result = append(result, view)
	}
	return result
}

func pgbackrestRecoveryView(report pgbackrest.Report, aspects pgbackrest.Aspects) recoveryServerView {
	view := recoveryServerView{
		Server: report.Stanza, TimelineState: string(aspects.Timeline.State), TimelineReason: aspects.Timeline.Reason,
		CoverageState: string(aspects.Coverage.State), CoverageReason: aspects.Coverage.Reason,
	}
	for _, history := range report.Timelines {
		if len(history.Edges) == 0 {
			view.Histories = append(view.Histories, timelineView{Timeline: timelineText(history.Timeline), State: string(history.State), Reason: history.Reason, Parent: "unknown", Child: timelineText(history.Timeline), SwitchLSN: "unknown", SwitchWAL: "unknown"})
			continue
		}
		for _, edge := range history.Edges {
			view.Histories = append(view.Histories, timelineView{Timeline: timelineText(history.Timeline), Parent: timelineText(edge.Parent), Child: timelineText(edge.Child), SwitchLSN: formatLSN(edge.SwitchLSN), SwitchWAL: switchWAL(edge, report.SegmentSize), State: string(history.State), Reason: history.Reason})
		}
	}
	for _, backup := range report.Backups {
		path := backup.Path
		if path.TargetTimeline == 0 {
			continue
		}
		view.Paths = append(view.Paths, recoveryPathView{
			BackupID: backup.Backup.Label, TargetTimeline: timelineText(path.TargetTimeline), State: string(path.State), Stop: string(path.Stop),
			LowerBound: timeText(backup.Backup.TimestampStop), StartWAL: unknownText(path.StartWAL), FrontierWAL: unknownText(path.FrontierWAL),
			FrontierReceipt: archiveReceiptText(path.FrontierReceipt), Reason: path.Reason, Assumptions: "segment-name presence only",
		})
	}
	return view
}

// pgbackrestWALServers presents each archive id of each stanza as one WAL
// "server" (<archive stanza>/<archive id>), so the WAL browser serves both
// formats with one filter and one paging code.
func pgbackrestWALServers(reports []pgbackrest.Report) []barmancloud.ServerWAL {
	var result []barmancloud.ServerWAL
	for _, report := range reports {
		for _, wal := range report.WAL {
			server := barmancloud.ServerWAL{
				Server: wal.Stanza + "/" + wal.ArchiveID, State: wal.State, Reason: wal.Reason,
				SegmentSize: wal.SegmentSize, Counts: barmancloud.WALCounts(wal.Counts),
				DiagnosticsTruncated: wal.DiagnosticsTruncated, RangesTruncated: wal.RangesTruncated,
				LatestArchiveReceipt: wal.LatestArchiveReceipt,
			}
			if major, _, ok := strings.Cut(wal.ArchiveID, "-"); ok {
				if version, err := strconv.ParseInt(major, 10, 64); err == nil {
					server.PostgreSQLVersion = version
				}
			}
			for _, value := range wal.Ranges {
				server.Ranges = append(server.Ranges, barmancloud.WALRange(value))
			}
			for _, value := range wal.Gaps {
				server.Gaps = append(server.Gaps, barmancloud.WALGap{
					Timeline: value.Timeline, Start: value.Start, End: value.End, Count: value.Count, First: value.First, Last: value.Last,
					Status: barmancloud.GapStatus(value.Status), FirstObservedGeneration: value.FirstObservedGeneration, LastObservedGeneration: value.LastObservedGeneration,
				})
			}
			for _, value := range wal.Diagnostics {
				server.Diagnostics = append(server.Diagnostics, barmancloud.WALDiagnostic{
					Key: value.Key, Name: value.Name, Reason: value.Reason, Class: barmancloud.WALClass(value.Class), Timeline: value.Timeline, LastModified: value.LastModified,
				})
			}
			result = append(result, server)
		}
	}
	return result
}

// walServers is the WAL evidence the browser shows, whichever the format.
func (h *Handler) walServers(snapshot inventory.Snapshot) []barmancloud.ServerWAL {
	if h.isPGBackRest() {
		return pgbackrestWALServers(snapshot.PGBackRest)
	}
	return snapshot.BarmanWAL.Servers
}

func noneText(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

func unsignedBytesText(value uint64) string {
	if value > 1<<62 {
		return strconv.FormatUint(value, 10) + " B"
	}
	return bytesText(true, int64(value))
}

func switchWAL(edge pgbackrest.HistoryEdge, segmentSize int64) string {
	name, err := pgbackrest.WALName(edge.Child, edge.SwitchPosition, segmentSize)
	if err != nil {
		return "unknown"
	}
	return name
}
