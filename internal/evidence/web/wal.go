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
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
)

type walSummaryView struct {
	Server, State, Reason, PostgreSQLVersion, SegmentSize string
	Segments, Ranges, CandidateGaps, ConfirmedGaps        string
	LatestReceipt                                         string
	// Expected non-segment classes are counted, not listed as rows.
	Partials, History, BackupHistory, Unrecognized, Duplicates string
}

func walSummaryViews(servers []barmancloud.ServerWAL) []walSummaryView {
	result := make([]walSummaryView, 0, len(servers))
	for _, server := range servers {
		candidate, confirmed := 0, 0
		for _, gap := range server.Gaps {
			if gap.Status == barmancloud.GapCandidate {
				candidate++
			} else if gap.Status == barmancloud.GapConfirmed {
				confirmed++
			}
		}
		version := "unknown"
		if server.PostgreSQLVersion > 0 {
			version = strconv.FormatInt(server.PostgreSQLVersion, 10)
		}
		segmentSize := "unknown"
		if server.SegmentSize > 0 {
			segmentSize = bytesText(true, server.SegmentSize)
		}
		result = append(result, walSummaryView{
			Server: server.Server, State: string(server.State), Reason: server.Reason,
			PostgreSQLVersion: version, SegmentSize: segmentSize,
			Segments: strconv.FormatInt(server.Counts.Segments, 10), Ranges: strconv.Itoa(len(server.Ranges)),
			CandidateGaps: strconv.Itoa(candidate), ConfirmedGaps: strconv.Itoa(confirmed),
			LatestReceipt: archiveReceiptText(server.LatestArchiveReceipt),
			Partials:      strconv.FormatInt(server.Counts.Partials, 10), History: strconv.FormatInt(server.Counts.History, 10),
			BackupHistory: strconv.FormatInt(server.Counts.BackupHistory, 10), Unrecognized: strconv.FormatInt(server.Counts.Unknown, 10),
			Duplicates: strconv.FormatInt(server.Counts.Duplicates, 10),
		})
	}
	return result
}

type walFilter struct {
	server, class, start, end string
	timeline                  uint32
	hasTimeline               bool
	page                      int
}

type walRowView struct {
	Server, Class, Timeline, Start, End, Count, Status, Receipt, Reason, Key string
	positionStart, positionEnd                                               uint64
	hasPosition                                                              bool
	timelineValue                                                            uint32
}

type walPageView struct {
	Title                               string
	Generation, Completeness, Stale     string
	Server, Class, Timeline, Start, End string
	Rows                                []walRowView
	Page, TotalPages, TotalRows         int
	PreviousURL, NextURL                string
	DiagnosticsNotice, EvidenceLimit    string
}

func (h *Handler) wals(writer http.ResponseWriter, request *http.Request) {
	if h.format.ID != "barman-cloud" && !h.isPGBackRest() {
		http.NotFound(writer, request)
		return
	}
	snapshot := h.safeInventory()
	servers := h.walServers(snapshot)
	filter, err := parseWALFilter(request.URL.Query(), servers)
	if err != nil {
		http.Error(writer, "invalid WAL filter", http.StatusBadRequest)
		return
	}
	pageStart := (filter.page - 1) * h.walPageSize
	pageRows, totalRows, diagnosticsTruncated := filterWALRows(servers, filter, pageStart, h.walPageSize)
	totalPages := 0
	if totalRows > 0 {
		totalPages = (totalRows + h.walPageSize - 1) / h.walPageSize
	}
	previousURL, nextURL := "", ""
	if filter.page > 1 {
		previousURL = walPageURL(filter, filter.page-1)
	}
	if pageStart+len(pageRows) < totalRows {
		nextURL = walPageURL(filter, filter.page+1)
	}
	timeline := ""
	if filter.hasTimeline {
		timeline = strconv.FormatUint(uint64(filter.timeline), 10)
	}
	notice := ""
	if diagnosticsTruncated {
		notice = "Diagnostic objects are bounded; class counts remain exact for the complete scan."
	}
	title := "Barman"
	if h.isPGBackRest() {
		title = "pgBackRest"
	}
	data := walPageView{
		Title:      title,
		Generation: generationText(snapshot.Evidence), Completeness: string(snapshot.Evidence.Completeness), Stale: staleText(snapshot.Evidence),
		Server: filter.server, Class: filter.class, Timeline: timeline, Start: filter.start, End: filter.end,
		Rows: pageRows, Page: filter.page, TotalPages: totalPages, TotalRows: totalRows,
		PreviousURL: previousURL, NextURL: nextURL, DiagnosticsNotice: notice,
		EvidenceLimit: "Segment-name continuity does not validate WAL bytes or prove recovery. Archive receipt is provider modification time, not transaction time.",
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.walsTemplate.Execute(writer, data); err != nil {
		h.logger.ErrorContext(request.Context(), "response rendering failed",
			"category", "render", "request_id", requestIDFromContext(request.Context()))
	}
}

func parseWALFilter(values url.Values, servers []barmancloud.ServerWAL) (walFilter, error) {
	allowed := map[string]bool{"server": true, "class": true, "timeline": true, "start": true, "end": true, "page": true}
	for key, value := range values {
		if !allowed[key] || len(value) != 1 {
			return walFilter{}, errors.New("unsupported filter")
		}
	}
	result := walFilter{server: values.Get("server"), class: values.Get("class"), start: strings.ToUpper(values.Get("start")), end: strings.ToUpper(values.Get("end")), page: 1}
	if result.class == "" {
		result.class = "all"
	}
	validClass := map[string]bool{"all": true, "segment": true, "gap": true, "partial": true, "history": true, "backup-history": true, "unknown": true, "duplicate": true}
	if !validClass[result.class] {
		return walFilter{}, errors.New("invalid class")
	}
	if result.server != "" {
		found := false
		for _, server := range servers {
			if server.Server == result.server {
				found = true
				break
			}
		}
		if !found {
			return walFilter{}, errors.New("unknown server")
		}
	}
	if raw := values.Get("timeline"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || parsed == 0 {
			return walFilter{}, errors.New("invalid timeline")
		}
		result.timeline, result.hasTimeline = uint32(parsed), true
	}
	if result.start != "" || result.end != "" {
		if result.server == "" {
			return walFilter{}, errors.New("range requires server")
		}
		server := findWALServer(servers, result.server)
		if server.SegmentSize == 0 {
			return walFilter{}, errors.New("range context unavailable")
		}
		var startPosition, endPosition uint64
		var rangeTimeline uint32
		if result.start != "" {
			timeline, position, err := barmancloud.ParseWALName(result.start, server.SegmentSize)
			if err != nil || (result.hasTimeline && result.timeline != timeline) {
				return walFilter{}, errors.New("invalid start")
			}
			startPosition, rangeTimeline = position, timeline
		}
		if result.end != "" {
			timeline, position, err := barmancloud.ParseWALName(result.end, server.SegmentSize)
			if err != nil || (result.hasTimeline && result.timeline != timeline) {
				return walFilter{}, errors.New("invalid end")
			}
			endPosition = position
			if rangeTimeline != 0 && rangeTimeline != timeline {
				return walFilter{}, errors.New("range crosses timelines")
			}
			rangeTimeline = timeline
		}
		if result.start != "" && result.end != "" && startPosition > endPosition {
			return walFilter{}, errors.New("reversed range")
		}
		if !result.hasTimeline {
			result.timeline, result.hasTimeline = rangeTimeline, true
		}
	}
	if raw := values.Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1_000_000 {
			return walFilter{}, errors.New("invalid page")
		}
		result.page = parsed
	}
	return result, nil
}

// filterWALRows retains at most one requested page. It materializes and sorts
// only one server's bounded compact facts at a time; it never expands gaps or
// accumulates every matching row across a multi-server snapshot.
func filterWALRows(servers []barmancloud.ServerWAL, filter walFilter, pageStart, pageSize int) ([]walRowView, int, bool) {
	result := make([]walRowView, 0, pageSize)
	totalRows := 0
	diagnosticsTruncated := false
	for _, server := range servers {
		if filter.server != "" && filter.server != server.Server {
			continue
		}
		diagnosticsTruncated = diagnosticsTruncated || server.DiagnosticsTruncated || server.RangesTruncated
		serverRows := make([]walRowView, 0, len(server.Ranges)+len(server.Gaps)+len(server.Diagnostics))
		for _, value := range server.Ranges {
			row := walRowView{Server: server.Server, Class: "segment", Timeline: strconv.FormatUint(uint64(value.Timeline), 10), Start: value.First, End: value.Last, Count: strconv.FormatUint(value.Count, 10), Status: string(server.State), Receipt: archiveReceiptText(value.LatestReceipt), Reason: "complete segment-name range", positionStart: value.Start, positionEnd: value.End, hasPosition: true, timelineValue: value.Timeline}
			if includeWALRow(row, value.Timeline, server, filter) {
				serverRows = append(serverRows, row)
			}
		}
		for _, value := range server.Gaps {
			row := walRowView{Server: server.Server, Class: "gap", Timeline: strconv.FormatUint(uint64(value.Timeline), 10), Start: value.First, End: value.Last, Count: strconv.FormatUint(value.Count, 10), Status: string(value.Status), Receipt: "not applicable", Reason: "bounded missing segment-name range", positionStart: value.Start, positionEnd: value.End, hasPosition: true, timelineValue: value.Timeline}
			if includeWALRow(row, value.Timeline, server, filter) {
				serverRows = append(serverRows, row)
			}
		}
		for _, value := range server.Diagnostics {
			row := walRowView{Server: server.Server, Class: string(value.Class), Timeline: timelineText(value.Timeline), Start: value.Name, End: value.Name, Count: "1", Status: "diagnostic", Receipt: archiveReceiptText(value.LastModified), Reason: value.Reason, Key: value.Key, timelineValue: value.Timeline}
			timeline := value.Timeline
			if len(value.Name) >= 24 && server.SegmentSize > 0 {
				parsedTimeline, position, err := barmancloud.ParseWALName(value.Name[:24], server.SegmentSize)
				if err == nil {
					timeline, row.positionStart, row.positionEnd, row.hasPosition = parsedTimeline, position, position, true
					row.timelineValue = parsedTimeline
				}
			}
			if includeWALRow(row, timeline, server, filter) {
				serverRows = append(serverRows, row)
			}
		}
		sort.Slice(serverRows, func(i, j int) bool {
			left, right := serverRows[i], serverRows[j]
			if left.timelineValue != right.timelineValue {
				return left.timelineValue < right.timelineValue
			}
			if left.hasPosition != right.hasPosition {
				return left.hasPosition
			}
			if left.positionStart != right.positionStart {
				return left.positionStart < right.positionStart
			}
			if left.Class != right.Class {
				return left.Class < right.Class
			}
			return left.Key < right.Key
		})
		serverStart, serverEnd := totalRows, totalRows+len(serverRows)
		pageEnd := pageStart + pageSize
		if serverEnd > pageStart && serverStart < pageEnd {
			from := max(pageStart-serverStart, 0)
			to := min(pageEnd-serverStart, len(serverRows))
			result = append(result, serverRows[from:to]...)
		}
		totalRows = serverEnd
	}
	return result, totalRows, diagnosticsTruncated
}

func includeWALRow(row walRowView, timeline uint32, server barmancloud.ServerWAL, filter walFilter) bool {
	if filter.class != "all" && filter.class != row.Class {
		return false
	}
	if filter.hasTimeline && filter.timeline != timeline {
		return false
	}
	if filter.start == "" && filter.end == "" {
		return true
	}
	if !row.hasPosition {
		return false
	}
	if filter.start != "" {
		_, start, _ := barmancloud.ParseWALName(filter.start, server.SegmentSize)
		if row.positionEnd < start {
			return false
		}
	}
	if filter.end != "" {
		_, end, _ := barmancloud.ParseWALName(filter.end, server.SegmentSize)
		if row.positionStart > end {
			return false
		}
	}
	return true
}

func findWALServer(servers []barmancloud.ServerWAL, name string) barmancloud.ServerWAL {
	for _, server := range servers {
		if server.Server == name {
			return server
		}
	}
	return barmancloud.ServerWAL{}
}

func walPageURL(filter walFilter, page int) string {
	values := url.Values{}
	if filter.server != "" {
		values.Set("server", filter.server)
	}
	if filter.class != "all" {
		values.Set("class", filter.class)
	}
	if filter.hasTimeline {
		values.Set("timeline", strconv.FormatUint(uint64(filter.timeline), 10))
	}
	if filter.start != "" {
		values.Set("start", filter.start)
	}
	if filter.end != "" {
		values.Set("end", filter.end)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/wals?" + encoded
	}
	return "/wals"
}

func archiveReceiptText(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}

func timelineText(value uint32) string {
	if value == 0 {
		return "unknown"
	}
	return strconv.FormatUint(uint64(value), 10)
}
