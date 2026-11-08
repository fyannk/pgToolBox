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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
)

type recoveryServerView struct {
	Server, TimelineState, TimelineReason, CoverageState, CoverageReason string
	Histories                                                            []timelineView
	Paths                                                                []recoveryPathView
	Retention                                                            retentionView
}

type timelineView struct {
	Timeline, Parent, Child, SwitchLSN, SwitchWAL, State, Reason string
}

type recoveryPathView struct {
	BackupID, TargetTimeline, State, Stop, LowerBound, StartWAL, FrontierWAL, FrontierReceipt, Reason, Assumptions string
}

type retentionView struct {
	Visible, Usable, Oldest, Newest, ExpectedMinimum, Policy, State, Reason string
}

func recoveryServerViews(servers []barmancloud.ServerRecovery) []recoveryServerView {
	result := make([]recoveryServerView, 0, len(servers))
	for _, server := range servers {
		view := recoveryServerView{Server: server.Server, TimelineState: string(server.TimelineState), TimelineReason: server.TimelineReason, CoverageState: string(server.CoverageState), CoverageReason: server.CoverageReason}
		for _, history := range server.Histories {
			if len(history.Edges) == 0 {
				view.Histories = append(view.Histories, timelineView{Timeline: timelineText(history.Timeline), State: string(history.State), Reason: history.Reason, Parent: "unknown", Child: timelineText(history.Timeline), SwitchLSN: "unknown", SwitchWAL: "unknown"})
				continue
			}
			for _, edge := range history.Edges {
				view.Histories = append(view.Histories, timelineView{Timeline: timelineText(history.Timeline), Parent: timelineText(edge.Parent), Child: timelineText(edge.Child), SwitchLSN: formatLSN(edge.SwitchLSN), SwitchWAL: edge.SwitchWAL, State: string(history.State), Reason: history.Reason})
			}
		}
		for _, path := range server.Paths {
			view.Paths = append(view.Paths, recoveryPathView{BackupID: path.BackupID, TargetTimeline: timelineText(path.TargetTimeline), State: string(path.State), Stop: string(path.Stop), LowerBound: timeText(path.LowerBound), StartWAL: unknownText(path.StartWAL), FrontierWAL: unknownText(path.FrontierWAL), FrontierReceipt: archiveReceiptText(path.FrontierReceipt), Reason: path.Reason, Assumptions: strings.Join(path.Assumptions, "; ")})
		}
		retention := server.Retention
		expected := "not configured"
		if retention.MinimumConfigured {
			expected = strconv.Itoa(retention.MinimumRedundancy)
		}
		policy := "not configured"
		if retention.PolicyConfigured {
			policy = "configured but not interpreted"
		}
		view.Retention = retentionView{Visible: strconv.Itoa(retention.VisibleBackups), Usable: strconv.Itoa(retention.StructurallyUsable), Oldest: timeText(retention.OldestCompletion), Newest: timeText(retention.NewestCompletion), ExpectedMinimum: expected, Policy: policy, State: string(retention.State), Reason: retention.Reason}
		result = append(result, view)
	}
	return result
}

func formatLSN(value uint64) string { return fmt.Sprintf("%X/%X", value>>32, value&0xffffffff) }

func timeText(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}
