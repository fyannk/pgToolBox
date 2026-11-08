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

// Package web renders only the conservative evidence envelope. It performs no
// object-store operations and never handles provider or format-native models.
package web

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/fyannk/pgToolBox/internal/evidence/evidence"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/barmancloud"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	"github.com/fyannk/pgToolBox/internal/evidence/repository"
)

//go:embed templates/index.html
var indexTemplateText string

//go:embed templates/wals.html
var walsTemplateText string

const maxDisplayedUserBytes = 256

// Options configures New. Format, Inventory and Readiness are required;
// TrustedUserHeader names the header carrying the proxy-authenticated user
// to display; WALPageSize defaults to 200.
type Options struct {
	Logger            *slog.Logger
	Provider          string
	Format            repository.Descriptor
	Inventory         func() inventory.Snapshot
	Readiness         func() readiness.Result
	TrustedUserHeader string
	RequestID         func() string
	Now               func() time.Time
	WALPageSize       int
}

// Handler renders the standalone viewer's read-only HTML pages and health
// endpoints from the published inventory snapshot.
type Handler struct {
	logger            *slog.Logger
	provider          string
	format            repository.Descriptor
	inventory         func() inventory.Snapshot
	readiness         func() readiness.Result
	trustedUserHeader string
	requestID         func() string
	now               func() time.Time
	indexTemplate     *template.Template
	walsTemplate      *template.Template
	walPageSize       int
}

// New validates options and parses the page templates.
func New(options Options) (*Handler, error) {
	if options.Format.ID == "" || options.Format.DisplayName == "" || options.Format.ScopeKind == "" {
		return nil, errors.New("web handler requires a repository format descriptor")
	}
	if options.Inventory == nil || options.Readiness == nil {
		return nil, errors.New("web handler requires inventory and readiness sources")
	}
	indexTemplate, err := template.New("index").Parse(indexTemplateText)
	if err != nil {
		return nil, err
	}
	walsTemplate, err := template.New("wals").Parse(walsTemplateText)
	if err != nil {
		return nil, err
	}
	walPageSize := options.WALPageSize
	if walPageSize == 0 {
		walPageSize = 200
	}
	if walPageSize < 1 || walPageSize > 1_000 {
		return nil, errors.New("web handler requires a bounded WAL page size")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(ioDiscard{}, nil))
	}
	requestID := options.RequestID
	if requestID == nil {
		requestID = randomRequestID
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		logger: logger, provider: options.Provider, format: options.Format,
		inventory: options.Inventory, readiness: options.Readiness,
		trustedUserHeader: options.TrustedUserHeader, requestID: requestID, now: now,
		indexTemplate: indexTemplate, walsTemplate: walsTemplate, walPageSize: walPageSize,
	}, nil
}

// Routes returns the HTTP handler for /healthz, /readyz, / and /wals, with
// security headers and request logging applied.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("GET /", h.index)
	mux.HandleFunc("GET /wals", h.wals)
	return h.security(h.logging(mux))
}

func (h *Handler) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("live\n"))
}

func (h *Handler) ready(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	result := h.readiness()
	if !result.Ready {
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("not ready\n"))
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("ready\n"))
}

func (h *Handler) index(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	inventorySnapshot := h.safeInventory()
	snapshot := inventorySnapshot.Evidence
	data := struct {
		FormatName      string
		FormatID        string
		Provider        string
		ScopeKind       string
		Compatibility   evidence.State
		State           evidence.State
		Reason          string
		Completeness    evidence.Completeness
		Generation      string
		CompletedAt     string
		EvidenceAge     string
		Stale           string
		ObjectCount     string
		StoredBytes     string
		Unscoped        string
		Refresh         string
		Scopes          []inventory.Scope
		Recent          []recentObjectView
		Capabilities    []evidence.Capability
		Backups         []backupView
		WALServers      []walSummaryView
		RecoveryServers []recoveryServerView
		PGBackRest      bool
		Stanzas         []pgbackrestStanzaView
		User            string
	}{
		FormatName: h.format.DisplayName, FormatID: h.format.ID,
		Provider: h.provider, ScopeKind: snapshot.Scope.Kind,
		Compatibility: snapshot.Compatibility,
		State:         snapshot.State, Reason: snapshot.Reason,
		Completeness: snapshot.Completeness, Capabilities: snapshot.Capabilities,
		Generation: generationText(snapshot), CompletedAt: completedAtText(snapshot),
		EvidenceAge: evidenceAgeText(snapshot, h.now().UTC()), Stale: staleText(snapshot),
		ObjectCount: totalText(inventorySnapshot.TotalsKnown, inventorySnapshot.ObjectCount),
		StoredBytes: bytesText(inventorySnapshot.TotalsKnown, inventorySnapshot.StoredBytes),
		Unscoped:    totalText(inventorySnapshot.TotalsKnown, inventorySnapshot.UnscopedObjectCount),
		Refresh:     refreshText(inventorySnapshot), Scopes: inventorySnapshot.Scopes,
		Recent:          recentObjectViews(inventorySnapshot.RecentObjects),
		Backups:         backupViews(inventorySnapshot.BarmanCatalog.Backups),
		WALServers:      walSummaryViews(h.walServers(inventorySnapshot)),
		RecoveryServers: recoveryServerViews(inventorySnapshot.BarmanRecovery.Servers),
		PGBackRest:      h.isPGBackRest(),
		Stanzas:         pgbackrestStanzaViews(inventorySnapshot.PGBackRest),
		User:            displayedUser(request.Header.Get(h.trustedUserHeader), h.trustedUserHeader != ""),
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.indexTemplate.Execute(writer, data); err != nil {
		h.logger.ErrorContext(request.Context(), "response rendering failed",
			slog.String("category", "render"),
			slog.String("request_id", requestIDFromContext(request.Context())),
		)
	}
}

type backupView struct {
	Server, ID, Status, Type, State, Reason string
	Logical, Deduplicated, Stored           string
	Compression, Encryption                 string
}

func backupViews(backups []barmancloud.Backup) []backupView {
	result := make([]backupView, 0, len(backups))
	for _, backup := range backups {
		deduplicated := "unknown"
		if backup.DeduplicatedBytes != nil {
			deduplicated = bytesText(true, *backup.DeduplicatedBytes)
		}
		result = append(result, backupView{Server: backup.Server, ID: backup.ID, Status: backup.Status, Type: unknownText(backup.Type), State: string(backup.State), Reason: backup.Reason, Logical: bytesText(true, backup.LogicalBytes), Deduplicated: deduplicated, Stored: bytesText(true, backup.StoredBytes), Compression: unknownText(backup.Compression), Encryption: unknownText(backup.Encryption)})
	}
	return result
}

func unknownText(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func generationText(snapshot evidence.Snapshot) string {
	if snapshot.Generation == 0 {
		return "none"
	}
	return strconv.FormatUint(snapshot.Generation, 10)
}

func completedAtText(snapshot evidence.Snapshot) string {
	if snapshot.CompletedAt.IsZero() {
		return "never"
	}
	return snapshot.CompletedAt.UTC().Format(time.RFC3339)
}

func evidenceAgeText(snapshot evidence.Snapshot, now time.Time) string {
	if snapshot.CompletedAt.IsZero() || snapshot.CompletedAt.After(now) {
		return "unknown"
	}
	return now.Sub(snapshot.CompletedAt.UTC()).Truncate(time.Second).String()
}

func staleText(snapshot evidence.Snapshot) string {
	if snapshot.CompletedAt.IsZero() {
		return "not applicable"
	}
	return strconv.FormatBool(snapshot.Stale)
}

func totalText(known bool, value int64) string {
	if !known {
		return "unknown"
	}
	return strconv.FormatInt(value, 10)
}

func bytesText(known bool, value int64) string {
	if !known {
		return "unknown"
	}
	return strconv.FormatInt(value, 10) + " bytes (" + humanBytes(value) + ")"
}

func humanBytes(value int64) string {
	units := []struct {
		name   string
		factor int64
	}{
		{name: "PiB", factor: 1 << 50},
		{name: "TiB", factor: 1 << 40},
		{name: "GiB", factor: 1 << 30},
		{name: "MiB", factor: 1 << 20},
		{name: "KiB", factor: 1 << 10},
	}
	for _, unit := range units {
		if value >= unit.factor {
			whole := value / unit.factor
			tenth := (value % unit.factor) * 10 / unit.factor
			return fmt.Sprintf("%d.%d %s", whole, tenth, unit.name)
		}
	}
	return strconv.FormatInt(value, 10) + " B"
}

func refreshText(snapshot inventory.Snapshot) string {
	if snapshot.RefreshFailure != "" {
		return "failed: " + string(snapshot.RefreshFailure)
	}
	if snapshot.RefreshGeneration == 0 {
		return "not attempted"
	}
	return "complete"
}

type recentObjectView struct {
	Key          string
	Scope        string
	Size         string
	LastModified string
}

func recentObjectViews(objects []inventory.RecentObject) []recentObjectView {
	result := make([]recentObjectView, 0, len(objects))
	for _, object := range objects {
		modified := "unknown"
		if !object.LastModified.IsZero() {
			modified = object.LastModified.UTC().Format(time.RFC3339)
		}
		result = append(result, recentObjectView{
			Key: object.Key, Scope: object.Scope,
			Size: bytesText(true, object.Size), LastModified: modified,
		})
	}
	return result
}

func (h *Handler) safeInventory() inventory.Snapshot {
	inventorySnapshot := h.inventory()
	snapshot := inventorySnapshot.Evidence
	if snapshot.RepositoryFormat == h.format.ID && snapshot.Scope.Kind == h.format.ScopeKind && snapshot.Validate() == nil {
		if inventorySnapshot.Validate() == nil {
			return inventorySnapshot
		}
	}
	inventorySnapshot = inventory.Initial(h.format)
	inventorySnapshot.Evidence.Reason = "invalid or incompatible evidence snapshot"
	for index := range inventorySnapshot.Evidence.Capabilities {
		inventorySnapshot.Evidence.Capabilities[index].Reason = inventorySnapshot.Evidence.Reason
	}
	return inventorySnapshot
}

func (h *Handler) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(writer, request)
	})
}

func (h *Handler) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		requestID := h.requestID()
		writer.Header().Set("X-Request-ID", requestID)
		ctx := withRequestID(request.Context(), requestID)
		request = request.WithContext(ctx)
		recorder := &statusRecorder{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(recorder, request)
		h.logger.InfoContext(ctx, "http request",
			slog.String("request_id", requestID),
			slog.String("method", request.Method),
			slog.String("route", routeName(request)),
			slog.Int("status", recorder.status),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func routeName(request *http.Request) string {
	switch request.URL.Path {
	case "/":
		return "index"
	case "/healthz":
		return "healthz"
	case "/readyz":
		return "readyz"
	case "/wals":
		return "wals"
	default:
		return "not_found"
	}
}

func displayedUser(value string, enabled bool) string {
	if !enabled {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(value) <= maxDisplayedUserBytes {
		return value
	}
	value = value[:maxDisplayedUserBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func randomRequestID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "request-id-unavailable"
	}
	return hex.EncodeToString(buffer)
}

type requestIDKey struct{}

func withRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

type ioDiscard struct{}

func (ioDiscard) Write(value []byte) (int, error) { return len(value), nil }
