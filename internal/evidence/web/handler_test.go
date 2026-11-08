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
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
)

func TestHandlerRendersUnknownEmptyShellAndEscapesValues(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	snapshot := evidence.InitialSnapshot("synthetic", "vault", []evidence.CapabilityID{evidence.CatalogListing})
	snapshot.Reason = `<script>alert("store")</script>`
	handler := newTestHandler(t, logs, snapshot, readiness.Result{})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	request.Header.Set("X-Forwarded-User", `<img src=x onerror=alert("user")>`)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{"unknown", "no-completed-scan", "Synthetic Repository", "vault", "none", "never", "not applicable", "does not verify that a restore will succeed"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body does not contain %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{`<script>alert`, `<img src=x`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("body contains unescaped value %q: %s", forbidden, body)
		}
	}
	if strings.Contains(body, "DESTINATION_PATH") || strings.Contains(body, "s3://") {
		t.Fatalf("body disclosed topology: %s", body)
	}
}

func TestHandlerRendersDeterministicGenerationAndEvidenceAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	snapshot := evidence.Snapshot{
		RepositoryFormat: "synthetic", Compatibility: evidence.Healthy,
		Scope: evidence.Scope{Kind: "vault"}, Generation: 7,
		StartedAt: now.Add(-2 * time.Minute), CompletedAt: now.Add(-90 * time.Second),
		Completeness: evidence.Complete, State: evidence.Healthy, Reason: "complete structural evidence",
	}
	handler := newTestHandlerAt(t, &bytes.Buffer{}, snapshot, readiness.Result{}, now)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	for _, expected := range []string{"<dd>7</dd>", "2026-07-27T11:58:30Z", "1m30s", "<dd>false</dd>"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("body does not contain %q: %s", expected, response.Body)
		}
	}
}

func TestHandlerHealthAndReadinessMeanings(t *testing.T) {
	t.Parallel()
	snapshot := evidence.InitialSnapshot("synthetic", "vault", nil)
	tests := []struct {
		name       string
		path       string
		readiness  readiness.Result
		wantStatus int
		wantBody   string
	}{
		{name: "liveness ignores catalog", path: "/healthz", wantStatus: http.StatusOK, wantBody: "live\n"},
		{name: "unknown reachability is not ready", path: "/readyz", readiness: readiness.Result{Category: fault.Unavailable}, wantStatus: http.StatusServiceUnavailable, wantBody: "not ready\n"},
		{name: "recent reachability is ready", path: "/readyz", readiness: readiness.Result{Ready: true}, wantStatus: http.StatusOK, wantBody: "ready\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := newTestHandler(t, &bytes.Buffer{}, snapshot, test.readiness)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.path, nil))
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody {
				t.Fatalf("response = (%d, %q), want (%d, %q)", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if strings.Contains(response.Body.String(), "bucket") || strings.Contains(response.Body.String(), "authorization") {
				t.Fatalf("health response disclosed details: %q", response.Body.String())
			}
		})
	}
}

func TestHandlerAppliesSecurityHeaders(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &bytes.Buffer{}, evidence.InitialSnapshot("synthetic", "vault", nil), readiness.Result{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	want := map[string]string{
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"X-Request-ID":            "test-request-id",
	}
	for header, expected := range want {
		if got := response.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
}

func TestHandlerHasNoDownloadRouteAndRejectsOtherMethods(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t, &bytes.Buffer{}, evidence.InitialSnapshot("synthetic", "vault", nil), readiness.Result{})
	tests := []struct {
		method string
		path   string
		status int
	}{
		{method: http.MethodGet, path: "/download/object", status: http.StatusNotFound},
		{method: http.MethodPost, path: "/", status: http.StatusMethodNotAllowed},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), test.method, test.path, nil))
		if response.Code != test.status {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.status)
		}
		if strings.Contains(response.Body.String(), "href") && strings.Contains(response.Body.String(), "download") {
			t.Errorf("%s %s exposed a download link", test.method, test.path)
		}
	}
}

func TestHandlerLogsOnlyStableRouteData(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	handler := newTestHandler(t, logs, evidence.InitialSnapshot("synthetic", "vault", nil), readiness.Result{})
	canary := "credential-canary-never-log"
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+canary+"?token="+canary, nil)
	request.Header.Set("Authorization", "Bearer "+canary)
	request.Header.Set("X-Forwarded-User", canary)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if strings.Contains(logs.String(), canary) {
		t.Fatalf("log disclosed request canary: %s", logs)
	}
	for _, expected := range []string{`"route":"not_found"`, `"request_id":"test-request-id"`} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("log does not contain %s: %s", expected, logs)
		}
	}
}

func TestHandlerWrongFormatSnapshotFailsUnknown(t *testing.T) {
	t.Parallel()
	snapshot := evidence.Snapshot{
		RepositoryFormat: "barman-cloud",
		Compatibility:    evidence.Healthy,
		Scope:            evidence.Scope{Kind: "server", Name: "wrong-format"},
		Generation:       1,
		Completeness:     evidence.Complete,
		State:            evidence.Healthy,
		Reason:           "catalog accepted",
	}
	handler := newTestHandler(t, &bytes.Buffer{}, snapshot, readiness.Result{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, "unknown: invalid or incompatible evidence snapshot") {
		t.Fatalf("wrong-format snapshot was not forced to unknown: %s", body)
	}
	if strings.Contains(body, "wrong-format") || strings.Contains(body, "catalog accepted") {
		t.Fatalf("wrong-format facts crossed the configured format boundary: %s", body)
	}
}

func TestHandlerRendersCompleteCachedInventoryAndEscapesStoreValues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	envelope := evidence.InitialSnapshot("synthetic", "vault", []evidence.CapabilityID{evidence.ObjectInventory, evidence.CatalogListing})
	envelope.Generation = 4
	envelope.StartedAt = now.Add(-time.Minute)
	envelope.CompletedAt = now.Add(-30 * time.Second)
	envelope.Completeness = evidence.Complete
	envelope.State = evidence.Unknown
	envelope.Reason = "backup semantics not evaluated"
	envelope.Capabilities[1].Support = evidence.Supported
	envelope.Capabilities[1].State = evidence.Healthy
	envelope.Capabilities[1].Reason = "complete object listing"
	snapshot := inventory.Snapshot{
		Evidence: envelope, RefreshGeneration: 4, LastRefreshAt: envelope.CompletedAt,
		TotalsKnown: true, ObjectCount: 2, StoredBytes: 1536, UnscopedObjectCount: 1,
		Scopes:        []inventory.Scope{{Name: `<scope&one>`, Recognized: true, ObjectCount: 1, StoredBytes: 512}},
		RecentObjects: []inventory.RecentObject{{Key: `<script>alert("key")</script>`, Scope: `<scope&one>`, Size: 512, LastModified: now.Add(-time.Minute)}},
	}
	handler := newTestInventoryHandler(t, &bytes.Buffer{}, snapshot, readiness.Result{}, now)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, expected := range []string{"<dt>Format compatibility</dt><dd>unknown</dd>", "<dd>2</dd>", "1536 bytes (1.5 KiB)", "<dd>1</dd>", "complete object listing", "2026-07-27T11:59:00Z"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body does not contain %q: %s", expected, body)
		}
	}
	for _, forbidden := range []string{`<scope&one>`, `<script>alert`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("store-derived value was not escaped: %q", forbidden)
		}
	}
}

func TestHandlerShowsRetainedTotalsAsStaleAfterFailedRefresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	envelope := evidence.InitialSnapshot("synthetic", "vault", nil)
	envelope.Generation = 2
	envelope.StartedAt = now.Add(-2 * time.Minute)
	envelope.CompletedAt = now.Add(-time.Minute)
	envelope.Completeness = evidence.Complete
	envelope.Stale = true
	envelope.State = evidence.Unknown
	envelope.Reason = "refresh failed: throttled"
	snapshot := inventory.Snapshot{
		Evidence: envelope, RefreshGeneration: 3, LastRefreshAt: now,
		RefreshFailure: fault.Throttled, TotalsKnown: true, ObjectCount: 17, StoredBytes: 34, UnscopedObjectCount: 17,
	}
	handler := newTestInventoryHandler(t, &bytes.Buffer{}, snapshot, readiness.Result{}, now)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, expected := range []string{"<dd>true</dd>", "failed: throttled", "<dd>17</dd>", "34 bytes (34 B)"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body does not contain %q: %s", expected, body)
		}
	}
}

func TestHandlerInvalidPartialInventoryFailsToUnknown(t *testing.T) {
	t.Parallel()
	snapshot := inventory.Initial(repository.Descriptor{ID: "synthetic", DisplayName: "Synthetic Repository", ScopeKind: "vault"})
	snapshot.ObjectCount = 99
	handler := newTestInventoryHandler(t, &bytes.Buffer{}, snapshot, readiness.Result{}, time.Now())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, "unknown: invalid or incompatible evidence snapshot") || strings.Contains(body, "<dd>99</dd>") {
		t.Fatalf("invalid partial inventory crossed the cache boundary: %s", body)
	}
}

func TestHandlerRendersCachedBarmanWALSummaryAndFilteredGapBrowser(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	handler := newBarmanWALHandler(t, now, 10)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, expected := range []string{"Barman WAL continuity", "candidate gaps", "2026-07-27T11:59:00Z", "Archive receipt is provider modification time", "Recovery-path relevance is evaluated separately"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("summary does not contain %q: %s", expected, body)
		}
	}
	// Expected non-segment classes are counted, not listed, so the summary
	// must show their counts.
	for _, expected := range []string{"<th>Backup history files</th>", "<td>1</td><td>7</td><td>312</td><td>0</td><td>0</td>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("summary does not show WAL class counts %q: %s", expected, body)
		}
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/wals?server=alpha&class=gap&timeline=1", nil))
	body = response.Body.String()
	for _, expected := range []string{"bounded missing segment-name range", "candidate", "000000010000000000000003", "000000010000000000000004", "not transaction time"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("WAL browser does not contain %q: %s", expected, body)
		}
	}
	if strings.Contains(body, `<script>alert("wal")</script>`) {
		t.Fatalf("WAL diagnostic key was not escaped: %s", body)
	}
}

func TestHandlerRendersConservativeRecoveryCoverageAndEscapesValues(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	handler := newBarmanWALHandler(t, now, 10)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"Observed recovery coverage",
		"Conservative lower time bound",
		"Latest contiguous archived WAL received at",
		"candidate-limited",
		"Archive receipt is not transaction time",
		"configured but not interpreted",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("recovery summary does not contain %q: %s", expected, body)
		}
	}
	if strings.Contains(body, `<script>alert("backup")</script>`) || !strings.Contains(body, "&lt;script&gt;alert") {
		t.Fatalf("backup identifier was not safely escaped: %s", body)
	}
	lower := strings.ToLower(body)
	for _, prohibited := range []string{"exact pitr window", "restorable until", "restore guaranteed"} {
		if strings.Contains(lower, prohibited) {
			t.Fatalf("recovery UI used prohibited claim %q: %s", prohibited, body)
		}
	}
}

func TestHandlerBoundsWALBrowserRowsAndRejectsUntrustedFilters(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	handler := newBarmanWALHandler(t, now, 1)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/wals?server=alpha&class=segment", nil))
	body := response.Body.String()
	if strings.Count(body, "complete segment-name range") != 1 || !strings.Contains(body, "Next page") || !strings.Contains(body, "Rows: 2") {
		t.Fatalf("bounded WAL page = %s", body)
	}

	for _, target := range []string{
		"/wals?server=%3Cscript%3Ecanary%3C%2Fscript%3E",
		"/wals?class=raw-content",
		"/wals?start=000000010000000000000001",
		"/wals?server=alpha&start=000000010000000000000005&end=000000010000000000000001",
		"/wals?server=alpha&class=gap&page=0",
	} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest || response.Body.String() != "invalid WAL filter\n" || strings.Contains(response.Body.String(), "canary") {
			t.Fatalf("%s response = %d %q", target, response.Code, response.Body.String())
		}
	}
}

func BenchmarkHandlerCachedSummary(b *testing.B) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	envelope := evidence.InitialSnapshot("synthetic", "vault", nil)
	envelope.Generation = 1
	envelope.StartedAt = now.Add(-time.Minute)
	envelope.CompletedAt = now
	envelope.Completeness = evidence.Complete
	envelope.State = evidence.Unknown
	envelope.Reason = "backup semantics not evaluated"
	snapshot := inventory.Snapshot{Evidence: envelope, RefreshGeneration: 1, LastRefreshAt: now, TotalsKnown: true, ObjectCount: inventory.MaxScopes, StoredBytes: inventory.MaxScopes}
	for index := range inventory.MaxScopes {
		snapshot.Scopes = append(snapshot.Scopes, inventory.Scope{Name: fmt.Sprintf("scope-%04d", index), Recognized: true, ObjectCount: 1, StoredBytes: 1})
	}
	for index := range inventory.MaxRecentObjects {
		snapshot.RecentObjects = append(snapshot.RecentObjects, inventory.RecentObject{Key: fmt.Sprintf("scope-0000/object-%04d", index), Scope: "scope-0000", Size: 1, LastModified: now})
	}
	handler, err := New(Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "s3",
		Format:    repository.Descriptor{ID: "synthetic", DisplayName: "Synthetic Repository", ScopeKind: "vault"},
		Inventory: func() inventory.Snapshot { return snapshot }, Readiness: func() readiness.Result { return readiness.Result{Ready: true} },
		RequestID: func() string { return "benchmark" }, Now: func() time.Time { return now },
	})
	if err != nil {
		b.Fatal(err)
	}
	routes := handler.Routes()
	request := httptest.NewRequestWithContext(b.Context(), http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			b.Fatalf("status = %d", response.Code)
		}
	}
}

func newTestHandler(t *testing.T, logs *bytes.Buffer, snapshot evidence.Snapshot, ready readiness.Result) http.Handler {
	t.Helper()
	return newTestHandlerAt(t, logs, snapshot, ready, time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC))
}

func newTestHandlerAt(t *testing.T, logs *bytes.Buffer, snapshot evidence.Snapshot, ready readiness.Result, now time.Time) http.Handler {
	t.Helper()
	return newTestInventoryHandler(t, logs, inventory.Snapshot{Evidence: snapshot}, ready, now)
}

func newTestInventoryHandler(t *testing.T, logs *bytes.Buffer, snapshot inventory.Snapshot, ready readiness.Result, now time.Time) http.Handler {
	t.Helper()
	handler, err := New(Options{
		Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
		Provider: "s3",
		Format: repository.Descriptor{
			ID: "synthetic", DisplayName: "Synthetic Repository", ScopeKind: "vault",
		},
		Inventory:         func() inventory.Snapshot { return snapshot },
		Readiness:         func() readiness.Result { return ready },
		TrustedUserHeader: "X-Forwarded-User",
		RequestID:         func() string { return "test-request-id" },
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler.Routes()
}

func newBarmanWALHandler(t *testing.T, now time.Time, pageSize int) http.Handler {
	t.Helper()
	descriptor := barmancloud.New().Descriptor()
	envelope := evidence.InitialSnapshot(descriptor.ID, descriptor.ScopeKind, descriptor.Capabilities)
	envelope.Generation = 2
	envelope.StartedAt = now.Add(-2 * time.Minute)
	envelope.CompletedAt = now.Add(-time.Minute)
	envelope.Completeness = evidence.Complete
	envelope.State = evidence.Warning
	envelope.Reason = "Barman WAL continuity contains candidate gaps"
	first, _ := barmancloud.WALName(1, 1, 16<<20)
	second, _ := barmancloud.WALName(1, 2, 16<<20)
	third, _ := barmancloud.WALName(1, 3, 16<<20)
	fourth, _ := barmancloud.WALName(1, 4, 16<<20)
	fifth, _ := barmancloud.WALName(1, 5, 16<<20)
	snapshot := inventory.Snapshot{
		Evidence: envelope, RefreshGeneration: 2, LastRefreshAt: envelope.CompletedAt, TotalsKnown: true,
		BarmanWAL: barmancloud.WALCatalog{Servers: []barmancloud.ServerWAL{{
			Server: "alpha", State: evidence.Warning, Reason: "candidate segment-name gaps observed", PostgreSQLVersion: 180000, SegmentSize: 16 << 20,
			Counts: barmancloud.WALCounts{Segments: 4, Partials: 1, History: 7, BackupHistory: 312},
			Ranges: []barmancloud.WALRange{
				{Timeline: 1, Start: 1, End: 2, Count: 2, First: first, Last: second, LatestReceipt: now.Add(-2 * time.Minute)},
				{Timeline: 1, Start: 5, End: 5, Count: 1, First: fifth, Last: fifth, LatestReceipt: now.Add(-time.Minute)},
			},
			Gaps:                 []barmancloud.WALGap{{Timeline: 1, Start: 3, End: 4, Count: 2, First: third, Last: fourth, Status: barmancloud.GapCandidate, FirstObservedGeneration: 2, LastObservedGeneration: 2}},
			Diagnostics:          []barmancloud.WALDiagnostic{{Key: `alpha/wals/<script>alert("wal")</script>`, Name: first + ".partial", Class: barmancloud.WALPartial, Timeline: 1, LastModified: now.Add(-3 * time.Minute), Reason: "partial WAL does not fill continuity"}},
			LatestArchiveReceipt: now.Add(-time.Minute),
		}}},
		BarmanRecovery: barmancloud.RecoveryCatalog{Servers: []barmancloud.ServerRecovery{{
			Server: "alpha", TimelineState: evidence.Healthy, TimelineReason: "timeline ancestry parsed",
			CoverageState: evidence.Warning, CoverageReason: "one recovery path stops at a candidate required gap",
			Histories: []barmancloud.TimelineHistory{{
				Server: "alpha", Key: "alpha/wals/00000002.history", Timeline: 2, State: evidence.Healthy, Reason: "timeline ancestry parsed",
				Edges: []barmancloud.HistoryEdge{{Parent: 1, Child: 2, SwitchLSN: 3 * uint64(16<<20), SwitchPosition: 3, SwitchWAL: "000000020000000000000003"}},
			}},
			Paths: []barmancloud.RecoveryPath{{
				Server: "alpha", BackupID: `<script>alert("backup")</script>`, TargetTimeline: 1,
				State: evidence.Warning, Reason: "candidate required WAL gap stops coverage", Stop: barmancloud.CoverageCandidateLimited,
				LowerBound: now.Add(-time.Hour), StartTimeline: 1, FrontierTimeline: 1, StartPosition: 1, FrontierPosition: 2,
				StartWAL: first, FrontierWAL: second, FrontierReceipt: now.Add(-2 * time.Minute),
				Assumptions: []string{"segment-name presence only", "WAL bytes and restore execution not verified"},
			}},
			Retention: barmancloud.RetentionSummary{
				VisibleBackups: 1, StructurallyUsable: 1, OldestCompletion: now.Add(-time.Hour), NewestCompletion: now.Add(-time.Hour),
				PolicyConfigured: true, State: evidence.Unknown, Reason: "retention policy syntax is not interpreted in this slice",
			},
		}}},
	}
	handler, err := New(Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "s3", Format: descriptor,
		Inventory: func() inventory.Snapshot { return snapshot }, Readiness: func() readiness.Result { return readiness.Result{Ready: true} },
		RequestID: func() string { return "test-request-id" }, Now: func() time.Time { return now }, WALPageSize: pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler.Routes()
}
