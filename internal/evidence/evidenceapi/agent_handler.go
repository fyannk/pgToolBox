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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Response limits of the evidence API. All but MaximumConcurrentRequests are
// re-exported from evidencev1alpha1, where each is documented.
const (
	MediaType              = evidencev1alpha1.MediaType
	MaximumSnapshotBytes   = evidencev1alpha1.MaximumSnapshotBytes
	MaximumCollectionBytes = evidencev1alpha1.MaximumCollectionBytes
	// MaximumConcurrentRequests is how many requests are served at once;
	// one more is refused with 429 rather than queued.
	MaximumConcurrentRequests = 16
	RequestTimeout            = evidencev1alpha1.RequestTimeout
	maximumQueryBytes         = MaximumCursorBytes + 256
)

// publicationSource is what the handler reads: the engine's Barman
// publication, page by page. The pgBackRest publication is reached through
// the optional nativeSource side.
type publicationSource interface {
	Snapshot(context.Context) (evidencev1alpha1.RepositoryEvidenceSnapshot, error)
	Backups(context.Context, PageRequest) (evidencev1alpha1.BarmanBackupPage, error)
	WALRanges(context.Context, PageRequest) (evidencev1alpha1.BarmanWALRangePage, error)
	WALGaps(context.Context, PageRequest) (evidencev1alpha1.BarmanWALGapPage, error)
	RecoveryPaths(context.Context, PageRequest) (evidencev1alpha1.BarmanRecoveryPathPage, error)
}

type engineSource struct {
	engine *Engine
}

func (s engineSource) Snapshot(ctx context.Context) (evidencev1alpha1.RepositoryEvidenceSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return evidencev1alpha1.RepositoryEvidenceSnapshot{}, err
	}
	return s.engine.Snapshot(), nil
}

func (s engineSource) Backups(ctx context.Context, request PageRequest) (evidencev1alpha1.BarmanBackupPage, error) {
	if err := ctx.Err(); err != nil {
		return evidencev1alpha1.BarmanBackupPage{}, err
	}
	return s.engine.Backups(request)
}

func (s engineSource) WALRanges(ctx context.Context, request PageRequest) (evidencev1alpha1.BarmanWALRangePage, error) {
	if err := ctx.Err(); err != nil {
		return evidencev1alpha1.BarmanWALRangePage{}, err
	}
	return s.engine.WALRanges(request)
}

func (s engineSource) WALGaps(ctx context.Context, request PageRequest) (evidencev1alpha1.BarmanWALGapPage, error) {
	if err := ctx.Err(); err != nil {
		return evidencev1alpha1.BarmanWALGapPage{}, err
	}
	return s.engine.WALGaps(request)
}

func (s engineSource) RecoveryPaths(ctx context.Context, request PageRequest) (evidencev1alpha1.BarmanRecoveryPathPage, error) {
	if err := ctx.Err(); err != nil {
		return evidencev1alpha1.BarmanRecoveryPathPage{}, err
	}
	return s.engine.RecoveryPaths(request)
}

type handlerLimits struct {
	requestTimeout    time.Duration
	maximumSnapshot   int
	maximumCollection int
	maximumConcurrent int
}

func productionHandlerLimits() handlerLimits {
	return handlerLimits{
		requestTimeout: RequestTimeout, maximumSnapshot: MaximumSnapshotBytes,
		maximumCollection: MaximumCollectionBytes, maximumConcurrent: MaximumConcurrentRequests,
	}
}

type handlerResponse struct {
	status  int
	body    []byte
	code    string
	headers map[string]string
}

// AuthOutcome is the closed result of authenticating one agent request.
type AuthOutcome int

const (
	// AuthUnauthenticated means there is no valid token for the agent's audience.
	AuthUnauthenticated AuthOutcome = iota
	// AuthAllowed means TokenReview and SubjectAccessReview both passed.
	AuthAllowed
	// AuthForbidden means the token is valid but its subject may not get the
	// agent.
	AuthForbidden
	// AuthUnavailable means a review could not be completed; the request fails
	// closed.
	AuthUnavailable
	// AuthAudienceMismatch means the token was not authenticated for the
	// agent's audience and its claims name another evidence-agent audience
	// (another namespace's, or the bare cluster-wide one). Refused like
	// AuthUnauthenticated, with its own error code.
	AuthAudienceMismatch
)

// AuthDecision is an authentication outcome plus the reviewed username,
// which is not secret and is logged for audit.
type AuthDecision struct {
	Outcome AuthOutcome
	User    string
}

// Authenticator decides one bound-token request. The agent's implementation
// is TokenReview (audience-checked) followed by SubjectAccessReview.
type Authenticator interface {
	Authenticate(ctx context.Context, token evidencev1alpha1.BearerToken) AuthDecision
}

// AgentHandlerOptions configures the handler: the in-memory engine,
// readiness, the authenticator and the epoch stamped into every snapshot.
// It cannot reach a store or start a scan.
type AgentHandlerOptions struct {
	Engine        *Engine
	Readiness     *readiness.ProbeState
	Authenticator Authenticator
	ProducerEpoch uint64
	Logger        *slog.Logger
	// Audience is the agent's token audience,
	// evidencev1alpha1.TokenAudience(namespace); it is the realm of the
	// WWW-Authenticate challenge.
	Audience string
	// ParseAuthorization reads the Authorization header before a
	// concurrency slot is taken. Nil is evidencev1alpha1.ParseAuthorization
	// (a bound ServiceAccount token, a compact JWS); the shared-token
	// authenticator passes evidencev1alpha1.ParseSharedAuthorization.
	ParseAuthorization func(header string) (evidencev1alpha1.BearerToken, error)
}

// AgentHandler serves the closed evidence/v1alpha1 route set over HTTPS.
type AgentHandler struct {
	source        publicationSource
	readiness     func() bool
	authenticator Authenticator
	epoch         uint64
	logger        *slog.Logger
	concurrency   chan struct{}
	requestID     atomic.Uint64
	limits        handlerLimits
	// realm is the challenge's realm, the agent's audience.
	realm string
	// parse reads the Authorization header (AgentHandlerOptions.ParseAuthorization).
	parse func(string) (evidencev1alpha1.BearerToken, error)
}

// NewAgentHandler constructs the token-authenticated evidence handler.
func NewAgentHandler(options AgentHandlerOptions) (*AgentHandler, error) {
	if options.Engine == nil || options.Readiness == nil {
		return nil, errors.New("evidence agent handler configuration is invalid")
	}
	if _, ok := evidencev1alpha1.ParseTokenAudience(options.Audience); !ok {
		return nil, errors.New("evidence agent handler configuration is invalid: the audience is not a per-namespace evidence-agent audience")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	handler, err := newAgentHandler(engineSource{engine: options.Engine}, func() bool { return options.Readiness.Result().Ready },
		options.Authenticator, options.ProducerEpoch, logger, productionHandlerLimits())
	if err != nil {
		return nil, err
	}
	handler.realm = options.Audience
	if options.ParseAuthorization != nil {
		handler.parse = options.ParseAuthorization
	}
	return handler, nil
}

func newAgentHandler(source publicationSource, ready func() bool, authenticator Authenticator, epoch uint64, logger *slog.Logger, limits handlerLimits) (*AgentHandler, error) {
	if source == nil || ready == nil || authenticator == nil || !evidencev1alpha1.ValidProducerEpoch(epoch) || logger == nil ||
		limits.requestTimeout <= 0 || limits.maximumSnapshot <= 0 || limits.maximumCollection <= 0 || limits.maximumConcurrent <= 0 {
		return nil, errors.New("evidence agent handler configuration is invalid")
	}
	return &AgentHandler{
		source: source, readiness: ready, authenticator: authenticator, epoch: epoch, logger: logger,
		concurrency: make(chan struct{}, limits.maximumConcurrent), limits: limits,
		parse: evidencev1alpha1.ParseAuthorization,
	}, nil
}

// ServeHTTP answers the two unauthenticated probe routes, then requires a
// bound token for everything else. The token's syntax is checked before a
// concurrency slot is taken; the reviews run inside the slot and the request
// deadline, so a flood of tokens cannot queue unbounded API server calls.
func (h *AgentHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	entry := agentLogEntry{requestID: fmt.Sprintf("evidence-%016x", h.requestID.Add(1)), route: agentRouteName(request.URL.Path), started: started}
	requestCtx := request.Context()
	respond := func(response handlerResponse) {
		h.write(writer, response)
		h.log(requestCtx, entry, response)
	}

	if request.Method != http.MethodGet {
		response := agentError(http.StatusMethodNotAllowed, evidencev1alpha1.ErrorMethodNotAllowed)
		response.headers = map[string]string{"Allow": http.MethodGet}
		respond(response)
		return
	}
	if request.ContentLength != 0 || len(request.TransferEncoding) != 0 || len(request.URL.RawQuery) > maximumQueryBytes {
		respond(agentError(http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest))
		return
	}
	switch request.URL.Path {
	case evidencev1alpha1.HealthPath, evidencev1alpha1.ReadinessPath:
		respond(h.probe(request))
		return
	}
	if entry.route == "not-found" {
		respond(agentError(http.StatusNotFound, evidencev1alpha1.ErrorNotFound))
		return
	}

	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		respond(h.unauthenticated(evidencev1alpha1.ErrorUnauthenticated))
		return
	}
	token, err := h.parse(values[0])
	if err != nil {
		respond(h.unauthenticated(evidencev1alpha1.ErrorUnauthenticated))
		return
	}

	select {
	case h.concurrency <- struct{}{}:
		defer func() { <-h.concurrency }()
	default:
		respond(agentError(http.StatusTooManyRequests, evidencev1alpha1.ErrorBusy))
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), h.limits.requestTimeout)
	defer cancel()
	decision := h.authenticator.Authenticate(ctx, token)
	entry.user = decision.User
	switch decision.Outcome {
	case AuthAllowed:
	case AuthForbidden:
		respond(agentError(http.StatusForbidden, evidencev1alpha1.ErrorForbidden))
		return
	case AuthUnavailable:
		response := agentError(http.StatusServiceUnavailable, evidencev1alpha1.ErrorAuthenticationUnavailable)
		response.headers = map[string]string{"Retry-After": "5"}
		respond(response)
		return
	case AuthAudienceMismatch:
		respond(h.unauthenticated(evidencev1alpha1.ErrorAudienceMismatch))
		return
	default:
		respond(h.unauthenticated(evidencev1alpha1.ErrorUnauthenticated))
		return
	}

	response, revision := h.dispatch(request.WithContext(ctx))
	entry.revision = revision
	if errors.Is(request.Context().Err(), context.Canceled) {
		return
	}
	respond(response)
}

// unauthenticated is the 401 of a refused token, its challenge naming the
// one audience the agent accepts.
func (h *AgentHandler) unauthenticated(code evidencev1alpha1.ErrorCode) handlerResponse {
	response := agentError(http.StatusUnauthorized, code)
	challenge := "Bearer"
	if h.realm != "" {
		challenge += ` realm="` + h.realm + `"`
	}
	response.headers = map[string]string{"WWW-Authenticate": challenge}
	return response
}

func (h *AgentHandler) probe(request *http.Request) handlerResponse {
	if request.URL.RawQuery != "" {
		return agentError(http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest)
	}
	if request.URL.Path == evidencev1alpha1.HealthPath {
		return agentJSON(http.StatusOK, evidencev1alpha1.ServiceStatus{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.HealthKind, Status: evidencev1alpha1.HealthLive}, h.limits.maximumSnapshot)
	}
	status := http.StatusOK
	body := evidencev1alpha1.ServiceStatus{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ReadinessKind, Status: evidencev1alpha1.ReadinessReady}
	if !h.readiness() {
		status = http.StatusServiceUnavailable
		body.Status = evidencev1alpha1.ReadinessNotReady
	}
	return agentJSON(status, body, h.limits.maximumSnapshot)
}

func (h *AgentHandler) dispatch(request *http.Request) (handlerResponse, uint64) {
	switch request.URL.Path {
	case evidencev1alpha1.SnapshotPath:
		if request.URL.RawQuery != "" {
			return agentError(http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest), 0
		}
		if native, ok := h.source.(nativeSource); ok {
			value, isNative, err := native.nativeSnapshot(request.Context(), h.epoch)
			if isNative {
				if err != nil {
					return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication), 0
				}
				return agentJSON(http.StatusOK, value, h.limits.maximumSnapshot), value.Revision
			}
		}
		value, err := h.source.Snapshot(request.Context())
		if err != nil {
			return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication), 0
		}
		value.ProducerEpoch = h.epoch
		if value.Validate() != nil {
			return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication), 0
		}
		return agentJSON(http.StatusOK, value, h.limits.maximumSnapshot), value.Revision
	default:
		pageRequest, err := parsePageRequest(request.URL.RawQuery)
		if err != nil {
			return agentError(http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest), 0
		}
		return h.collection(request.Context(), request.URL.Path, pageRequest), pageRequest.Revision
	}
}

func (h *AgentHandler) collection(ctx context.Context, path string, request PageRequest) handlerResponse {
	if native, ok := h.source.(nativeSource); ok {
		value, isNative, err := native.nativeCollection(ctx, path, request)
		if isNative {
			if errors.Is(err, errRouteNotFound) {
				return agentError(http.StatusNotFound, evidencev1alpha1.ErrorNotFound)
			}
			if err != nil {
				return collectionError(err)
			}
			return agentJSON(http.StatusOK, value, h.limits.maximumCollection)
		}
	}
	var value any
	var err error
	switch path {
	case evidencev1alpha1.BackupsPath:
		value, err = h.source.Backups(ctx, request)
	case evidencev1alpha1.WALRangesPath:
		value, err = h.source.WALRanges(ctx, request)
	case evidencev1alpha1.WALGapsPath:
		value, err = h.source.WALGaps(ctx, request)
	case evidencev1alpha1.RecoveryPathsPath:
		value, err = h.source.RecoveryPaths(ctx, request)
	default:
		return agentError(http.StatusNotFound, evidencev1alpha1.ErrorNotFound)
	}
	if err != nil {
		return collectionError(err)
	}
	if validateCollection(value) != nil {
		return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication)
	}
	return agentJSON(http.StatusOK, value, h.limits.maximumCollection)
}

func validateCollection(value any) error {
	switch page := value.(type) {
	case evidencev1alpha1.BarmanBackupPage:
		return page.Validate()
	case evidencev1alpha1.BarmanWALRangePage:
		return page.Validate()
	case evidencev1alpha1.BarmanWALGapPage:
		return page.Validate()
	case evidencev1alpha1.BarmanRecoveryPathPage:
		return page.Validate()
	default:
		return ErrInvalidPublication
	}
}

func parsePageRequest(rawQuery string) (PageRequest, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return PageRequest{}, ErrInvalidRequest
	}
	for key, values := range query {
		if key != "revision" && key != "limit" && key != "cursor" || len(values) != 1 {
			return PageRequest{}, ErrInvalidRequest
		}
	}
	revision, ok := singleDecimal(query, "revision", true)
	if !ok || revision == 0 {
		return PageRequest{}, ErrInvalidRequest
	}
	limit, ok := singleDecimal(query, "limit", false)
	if !ok || limit > MaximumPageSize {
		return PageRequest{}, ErrInvalidRequest
	}
	cursor := ""
	if values, exists := query["cursor"]; exists {
		if len(values) != 1 || values[0] == "" || len(values[0]) > MaximumCursorBytes {
			return PageRequest{}, ErrInvalidRequest
		}
		cursor = values[0]
	}
	return PageRequest{Revision: revision, Limit: int(limit), Cursor: cursor}, nil
}

func singleDecimal(query url.Values, key string, required bool) (uint64, bool) {
	values, exists := query[key]
	if !exists {
		return 0, !required
	}
	if len(values) != 1 || values[0] == "" || strings.IndexFunc(values[0], func(value rune) bool { return value < '0' || value > '9' }) != -1 {
		return 0, false
	}
	value, err := strconv.ParseUint(values[0], 10, 64)
	return value, err == nil
}

func collectionError(err error) handlerResponse {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		return agentError(http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest)
	case errors.Is(err, ErrPublicationChanged):
		return agentError(http.StatusConflict, evidencev1alpha1.ErrorPublicationChanged)
	default:
		return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication)
	}
}

func agentJSON(status int, value any, maximum int) handlerResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return agentError(http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication)
	}
	if len(body) > maximum {
		return agentError(http.StatusRequestEntityTooLarge, evidencev1alpha1.ErrorResponseLimit)
	}
	return handlerResponse{status: status, body: body}
}

var agentErrorMessages = map[evidencev1alpha1.ErrorCode]string{
	evidencev1alpha1.ErrorInvalidRequest:            "invalid evidence request",
	evidencev1alpha1.ErrorUnauthenticated:           "a bound ServiceAccount token for this agent's namespace audience (" + evidencev1alpha1.TokenAudiencePrefix + "<namespace>) is required",
	evidencev1alpha1.ErrorAudienceMismatch:          "the token was issued for another evidence-agent audience; present the token of this agent's namespace (" + evidencev1alpha1.TokenAudiencePrefix + "<namespace>)",
	evidencev1alpha1.ErrorNotFound:                  "evidence route not found",
	evidencev1alpha1.ErrorMethodNotAllowed:          "method not allowed",
	evidencev1alpha1.ErrorPublicationChanged:        "repository evidence changed; restart from snapshot",
	evidencev1alpha1.ErrorResponseLimit:             "evidence response exceeds safety limit",
	evidencev1alpha1.ErrorBusy:                      "evidence API is busy; retry later",
	evidencev1alpha1.ErrorInvalidPublication:        "repository evidence unavailable",
	evidencev1alpha1.ErrorForbidden:                 "the token's subject may not get this EvidenceAgent",
	evidencev1alpha1.ErrorAuthenticationUnavailable: "token review unavailable; retry later",
}

func agentError(status int, code evidencev1alpha1.ErrorCode) handlerResponse {
	value := evidencev1alpha1.EvidenceAPIError{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: code, Message: agentErrorMessages[code]}
	body, _ := json.Marshal(value)
	return handlerResponse{status: status, body: body, code: string(code)}
}

func (h *AgentHandler) write(writer http.ResponseWriter, response handlerResponse) {
	writer.Header().Set("Content-Type", evidencev1alpha1.MediaType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("Content-Length", strconv.Itoa(len(response.body)))
	for key, value := range response.headers {
		writer.Header().Set(key, value)
	}
	writer.WriteHeader(response.status)
	_, _ = writer.Write(response.body)
}

type agentLogEntry struct {
	requestID string
	route     string
	user      string
	revision  uint64
	started   time.Time
}

func (h *AgentHandler) log(ctx context.Context, entry agentLogEntry, response handlerResponse) {
	attributes := []any{
		slog.String("request_id", entry.requestID), slog.String("route", entry.route),
		slog.Int("status", response.status), slog.Duration("duration", time.Since(entry.started)),
	}
	if entry.user != "" {
		attributes = append(attributes, slog.String("user", entry.user))
	}
	if entry.revision != 0 {
		attributes = append(attributes, slog.Uint64("revision", entry.revision))
	}
	if response.code != "" {
		attributes = append(attributes, slog.String("error_code", response.code))
	}
	level := slog.LevelInfo
	// Probes, and refusals of anonymous or malformed callers (which anyone
	// on the Pod network can produce at will), are DEBUG: logging them at
	// INFO would let a peer flood the agent's log.
	if (entry.route == "healthz" || entry.route == "readyz") && response.status == http.StatusOK || response.status == http.StatusUnauthorized {
		level = slog.LevelDebug
	}
	h.logger.Log(ctx, level, "evidence API request", attributes...)
}

func agentRouteName(path string) string {
	switch path {
	case evidencev1alpha1.HealthPath:
		return "healthz"
	case evidencev1alpha1.ReadinessPath:
		return "readyz"
	case evidencev1alpha1.SnapshotPath:
		return "snapshot"
	case evidencev1alpha1.BackupsPath:
		return "backups"
	case evidencev1alpha1.WALRangesPath:
		return "wal-ranges"
	case evidencev1alpha1.WALGapsPath:
		return "wal-gaps"
	case evidencev1alpha1.RecoveryPathsPath:
		return "recovery-paths"
	default:
		return "not-found"
	}
}
