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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const agentTestToken = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"

// fakeAuthenticator returns a fixed decision and counts calls.
type fakeAuthenticator struct {
	decision AuthDecision
	calls    atomic.Int64
	seen     string
	mu       sync.Mutex
}

func (f *fakeAuthenticator) Authenticate(_ context.Context, token evidencev1alpha1.BearerToken) AuthDecision {
	f.calls.Add(1)
	f.mu.Lock()
	f.seen = token.Value()
	f.mu.Unlock()
	return f.decision
}

func newTestAgentHandler(t *testing.T, ready bool, decision AuthOutcome) (*AgentHandler, *fakeAuthenticator) {
	t.Helper()
	authenticator := &fakeAuthenticator{decision: AuthDecision{Outcome: decision, User: "system:serviceaccount:team-a:pgtoolbox-console"}}
	handler, err := NewAgentHandler(AgentHandlerOptions{
		Engine:    newTestEngine(t, sourceWithBackups(t, 3), 0x91),
		Readiness: testReadiness(ready), Authenticator: authenticator, ProducerEpoch: 5,
		Audience: "pgtoolbox-evidence-agent/team-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, authenticator
}

func agentRequest(handler http.Handler, authorization, method, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertAgentError(t *testing.T, response *httptest.ResponseRecorder, status int, code evidencev1alpha1.ErrorCode) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d, body %s", response.Code, status, response.Body.String())
	}
	if response.Header().Get("Content-Type") != evidencev1alpha1.MediaType || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers = %v", response.Header())
	}
	var value evidencev1alpha1.EvidenceAPIError
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.Validate() != nil || value.Code != code {
		t.Fatalf("error body = %s (%v)", response.Body.String(), err)
	}
}

func TestAgentHandlerServesSnapshotWithEpochToAuthorizedCallers(t *testing.T) {
	handler, authenticator := newTestAgentHandler(t, true, AuthAllowed)
	response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != evidencev1alpha1.MediaType {
		t.Fatalf("snapshot = %d %s", response.Code, response.Body.String())
	}
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || snapshot.Validate() != nil {
		t.Fatalf("snapshot invalid: %v %s", err, response.Body.String())
	}
	if snapshot.APIVersion != evidencev1alpha1.APIVersion || snapshot.ProducerEpoch != 5 || !evidencev1alpha1.ValidProducerInstance(snapshot.ProducerInstance) {
		t.Fatalf("snapshot header = %s %d %q", snapshot.APIVersion, snapshot.ProducerEpoch, snapshot.ProducerInstance)
	}
	if authenticator.seen != agentTestToken {
		t.Fatal("the authenticator did not receive the presented token")
	}
	revision := snapshot.Revision
	for _, test := range []struct {
		path     string
		validate func([]byte) error
	}{
		{evidencev1alpha1.BackupsPath, func(body []byte) error {
			var page evidencev1alpha1.BarmanBackupPage
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			return page.Validate()
		}},
		{evidencev1alpha1.WALRangesPath, func(body []byte) error {
			var page evidencev1alpha1.BarmanWALRangePage
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			return page.Validate()
		}},
		{evidencev1alpha1.WALGapsPath, func(body []byte) error {
			var page evidencev1alpha1.BarmanWALGapPage
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			return page.Validate()
		}},
		{evidencev1alpha1.RecoveryPathsPath, func(body []byte) error {
			var page evidencev1alpha1.BarmanRecoveryPathPage
			if err := json.Unmarshal(body, &page); err != nil {
				return err
			}
			return page.Validate()
		}},
	} {
		target := test.path + "?revision=" + jsonNumber(revision) + "&limit=2"
		response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, target)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", test.path, response.Code, response.Body.String())
		}
		if err := test.validate(response.Body.Bytes()); err != nil {
			t.Fatalf("%s: %v", test.path, err)
		}
		if !bytes.Contains(response.Body.Bytes(), []byte(`"api_version":"`+evidencev1alpha1.APIVersion+`"`)) {
			t.Fatalf("%s is not an evidence page", test.path)
		}
	}
	// A cursor from page one continues on page two, through the same engine.
	first := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?revision="+jsonNumber(revision)+"&limit=1")
	var page evidencev1alpha1.BarmanBackupPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || page.NextCursor == nil {
		t.Fatalf("first page = %s", first.Body.String())
	}
	next := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?revision="+jsonNumber(revision)+"&limit=1&cursor="+*page.NextCursor)
	if next.Code != http.StatusOK {
		t.Fatalf("second page = %d %s", next.Code, next.Body.String())
	}
}

func jsonNumber(value uint64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAgentHandlerProbesAreUnauthenticatedAndRevealNothing(t *testing.T) {
	for _, ready := range []bool{true, false} {
		handler, authenticator := newTestAgentHandler(t, ready, AuthUnauthenticated)
		health := agentRequest(handler, "", http.MethodGet, evidencev1alpha1.HealthPath)
		readiness := agentRequest(handler, "", http.MethodGet, evidencev1alpha1.ReadinessPath)
		if health.Code != http.StatusOK || (readiness.Code == http.StatusOK) != ready {
			t.Fatalf("probes = %d/%d for ready=%v", health.Code, readiness.Code, ready)
		}
		for _, response := range []*httptest.ResponseRecorder{health, readiness} {
			var status evidencev1alpha1.ServiceStatus
			if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.Validate() != nil {
				t.Fatalf("probe body = %s", response.Body.String())
			}
		}
		if authenticator.calls.Load() != 0 {
			t.Fatal("a probe spent a token review")
		}
	}
}

func TestAgentHandlerRefusesWithoutAValidDecision(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		outcome       AuthOutcome
		status        int
		code          evidencev1alpha1.ErrorCode
		reviews       int64
	}{
		{name: "no header", outcome: AuthAllowed, status: http.StatusUnauthorized, code: evidencev1alpha1.ErrorUnauthenticated},
		{name: "basic", authorization: "Basic dXNlcjpwYXNz", outcome: AuthAllowed, status: http.StatusUnauthorized, code: evidencev1alpha1.ErrorUnauthenticated},
		{name: "opaque 43-character token", authorization: "Bearer AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", outcome: AuthAllowed, status: http.StatusUnauthorized, code: evidencev1alpha1.ErrorUnauthenticated},
		{name: "review says unauthenticated", authorization: "Bearer " + agentTestToken, outcome: AuthUnauthenticated, status: http.StatusUnauthorized, code: evidencev1alpha1.ErrorUnauthenticated, reviews: 1},
		{name: "SAR denies", authorization: "Bearer " + agentTestToken, outcome: AuthForbidden, status: http.StatusForbidden, code: evidencev1alpha1.ErrorForbidden, reviews: 1},
		{name: "review unavailable", authorization: "Bearer " + agentTestToken, outcome: AuthUnavailable, status: http.StatusServiceUnavailable, code: evidencev1alpha1.ErrorAuthenticationUnavailable, reviews: 1},
		{name: "another namespace's audience", authorization: "Bearer " + agentTestToken, outcome: AuthAudienceMismatch, status: http.StatusUnauthorized, code: evidencev1alpha1.ErrorAudienceMismatch, reviews: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, authenticator := newTestAgentHandler(t, true, test.outcome)
			response := agentRequest(handler, test.authorization, http.MethodGet, evidencev1alpha1.SnapshotPath)
			assertAgentError(t, response, test.status, test.code)
			if test.status == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != `Bearer realm="pgtoolbox-evidence-agent/team-a"` {
				t.Fatalf("401 challenge = %q, want the agent's own audience as realm", response.Header().Get("WWW-Authenticate"))
			}
			if authenticator.calls.Load() != test.reviews {
				t.Fatalf("reviews = %d, want %d", authenticator.calls.Load(), test.reviews)
			}
			if strings.Contains(response.Body.String(), "system:serviceaccount") || strings.Contains(response.Body.String(), "eyJ") {
				t.Fatal("refusal discloses the subject or the token")
			}
		})
	}
	handler, _ := newTestAgentHandler(t, true, AuthAllowed)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, evidencev1alpha1.SnapshotPath, nil)
	request.Header.Add("Authorization", "Bearer "+agentTestToken)
	request.Header.Add("Authorization", "Bearer "+agentTestToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assertAgentError(t, response, http.StatusUnauthorized, evidencev1alpha1.ErrorUnauthenticated)
}

func TestAgentHandlerClosedRouteSetMethodsAndQueries(t *testing.T) {
	handler, authenticator := newTestAgentHandler(t, true, AuthAllowed)
	for _, test := range []struct {
		method, target string
		status         int
		code           evidencev1alpha1.ErrorCode
	}{
		{http.MethodPost, evidencev1alpha1.SnapshotPath, http.StatusMethodNotAllowed, evidencev1alpha1.ErrorMethodNotAllowed},
		{http.MethodGet, "/api/v1beta1/snapshot", http.StatusNotFound, evidencev1alpha1.ErrorNotFound},
		{http.MethodGet, "/api/v1alpha1/", http.StatusNotFound, evidencev1alpha1.ErrorNotFound},
		{http.MethodGet, evidencev1alpha1.SnapshotPath + "?x=1", http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest},
		{http.MethodGet, evidencev1alpha1.HealthPath + "?x=1", http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest},
		{http.MethodGet, evidencev1alpha1.BackupsPath, http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest},
		{http.MethodGet, evidencev1alpha1.BackupsPath + "?revision=1&limit=201", http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest},
		{http.MethodGet, evidencev1alpha1.BackupsPath + "?revision=999", http.StatusConflict, evidencev1alpha1.ErrorPublicationChanged},
	} {
		response := agentRequest(handler, "Bearer "+agentTestToken, test.method, test.target)
		assertAgentError(t, response, test.status, test.code)
	}
	// Method, route and probe errors are answered before a review is spent;
	// the four evidence-route requests are authenticated before their query
	// is judged, so an anonymous caller learns nothing about the route set.
	if got := authenticator.calls.Load(); got != 4 {
		t.Fatalf("reviews = %d, want 4", got)
	}
}

func TestAgentHandlerBoundsConcurrentReviews(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 2)
	authenticator := blockingAuthenticator{entered: entered, release: block}
	handler, err := newAgentHandler(engineSource{engine: newTestEngine(t, sourceWithBackups(t, 1), 0x91)}, func() bool { return true },
		authenticator, 1, slog.New(slog.DiscardHandler), handlerLimits{requestTimeout: RequestTimeout, maximumSnapshot: MaximumSnapshotBytes, maximumCollection: MaximumCollectionBytes, maximumConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath)
	}()
	<-entered
	busy := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath)
	assertAgentError(t, busy, http.StatusTooManyRequests, evidencev1alpha1.ErrorBusy)
	close(block)
	if response := <-done; response.Code != http.StatusOK {
		t.Fatalf("first request = %d", response.Code)
	}
}

type blockingAuthenticator struct {
	entered chan struct{}
	release chan struct{}
}

func (b blockingAuthenticator) Authenticate(ctx context.Context, _ evidencev1alpha1.BearerToken) AuthDecision {
	b.entered <- struct{}{}
	select {
	case <-b.release:
		return AuthDecision{Outcome: AuthAllowed}
	case <-ctx.Done():
		return AuthDecision{Outcome: AuthUnavailable}
	}
}

func TestNewAgentHandlerRejectsIncompleteConfiguration(t *testing.T) {
	engine := newTestEngine(t, sourceWithBackups(t, 1), 0x91)
	authenticator := &fakeAuthenticator{}
	for _, options := range []AgentHandlerOptions{
		{Readiness: testReadiness(true), Authenticator: authenticator, ProducerEpoch: 1},
		{Engine: engine, Authenticator: authenticator, ProducerEpoch: 1},
		{Engine: engine, Readiness: testReadiness(true), ProducerEpoch: 1},
		{Engine: engine, Readiness: testReadiness(true), Authenticator: authenticator},
		{Engine: engine, Readiness: testReadiness(true), Authenticator: authenticator, ProducerEpoch: 1, Audience: "pgtoolbox-evidence-agent"},
	} {
		if options.Audience == "" {
			options.Audience = "pgtoolbox-evidence-agent/team-a"
		}
		if _, err := NewAgentHandler(options); err == nil {
			t.Fatalf("accepted %+v", options)
		}
	}
}

// FuzzAgentHandlerRequest drives the agent handler with arbitrary routes,
// queries and Authorization values. Invariant: evidence (anything but the two
// probes) is served only to a request whose header parses as a bound token
// and whose review allowed it; every response is a valid evidence body.
func FuzzAgentHandlerRequest(f *testing.F) {
	for _, seed := range [][2]string{
		{"/api/v1alpha1/snapshot", "Bearer " + agentTestToken},
		{"/api/v1alpha1/backups?revision=1&limit=2", "Bearer " + agentTestToken},
		{"/api/v1alpha1/snapshot", "Bearer a.b"},
		{"/healthz", ""},
		{"/readyz?x", "Basic x"},
		{"/api/v1beta1/snapshot", "Bearer " + agentTestToken},
	} {
		f.Add(seed[0], seed[1], true)
		f.Add(seed[0], seed[1], false)
	}
	engine := newTestEngine(f, sourceWithBackups(f, 2), 0x91)
	f.Fuzz(func(t *testing.T, target, authorization string, allow bool) {
		if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, " \t\r\n#") || len(target) > 8192 {
			return
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://agent"+target, nil)
		if err != nil {
			return
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		outcome := AuthForbidden
		if allow {
			outcome = AuthAllowed
		}
		authenticator := &fakeAuthenticator{decision: AuthDecision{Outcome: outcome}}
		handler, err := newAgentHandler(engineSource{engine: engine}, func() bool { return true }, authenticator, 1,
			slog.New(slog.DiscardHandler), productionHandlerLimits())
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		probe := request.URL.Path == evidencev1alpha1.HealthPath || request.URL.Path == evidencev1alpha1.ReadinessPath
		_, parseErr := evidencev1alpha1.ParseAuthorization(authorization)
		if response.Code == http.StatusOK && !probe && (parseErr != nil || !allow) {
			t.Fatalf("evidence served without an allowed bound token: %q %q", target, authorization)
		}
		if authenticator.calls.Load() > 0 && parseErr != nil {
			t.Fatal("a review was spent on an unparseable header")
		}
		if response.Header().Get("Content-Type") != evidencev1alpha1.MediaType {
			t.Fatal("response without the evidence media type")
		}
		if response.Code != http.StatusOK {
			var refusal evidencev1alpha1.EvidenceAPIError
			if err := json.Unmarshal(response.Body.Bytes(), &refusal); err != nil || refusal.Validate() != nil {
				// readyz not-ready is a ServiceStatus, never reached here with ready=true.
				t.Fatalf("invalid error body %d: %s", response.Code, response.Body.String())
			}
		}
	})
}

func TestAgentHandlerStrictlyParsesBoundedPageQueries(t *testing.T) {
	handler, _ := newTestAgentHandler(t, true, AuthAllowed)
	invalid := []string{
		"", "revision=0", "revision=+7", "revision=-7", "revision=18446744073709551616",
		"revision=7&revision=7", "revision=7&unknown=x", "revision=7&limit=-1",
		"revision=7&limit=201", "revision=7&limit=1&limit=1", "revision=7&cursor=",
		"revision=7&cursor=a&cursor=a", "revision=7;limit=1",
	}
	for _, query := range invalid {
		response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?"+query)
		assertAgentError(t, response, http.StatusBadRequest, evidencev1alpha1.ErrorInvalidRequest)
	}
}

func TestAgentHandlerEnforcesResponseBounds(t *testing.T) {
	allow := &fakeAuthenticator{decision: AuthDecision{Outcome: AuthAllowed}}
	snapshotLimits := productionHandlerLimits()
	snapshotLimits.maximumSnapshot = 1
	handler, err := newAgentHandler(engineSource{engine: newTestEngine(t, completeSource(t), 0x93)}, func() bool { return true }, allow, 1, discardLogger(), snapshotLimits)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentError(t, agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath), http.StatusRequestEntityTooLarge, evidencev1alpha1.ErrorResponseLimit)

	collectionLimits := productionHandlerLimits()
	collectionLimits.maximumCollection = 1
	handler, err = newAgentHandler(engineSource{engine: newTestEngine(t, completeSource(t), 0x94)}, func() bool { return true }, allow, 1, discardLogger(), collectionLimits)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentError(t, agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?revision=7"), http.StatusRequestEntityTooLarge, evidencev1alpha1.ErrorResponseLimit)
}

func TestAgentHandlerRejectsInvalidCollectionBeforeEncoding(t *testing.T) {
	engine := newTestEngine(t, completeSource(t), 0x95)
	allow := &fakeAuthenticator{decision: AuthDecision{Outcome: AuthAllowed}}
	handler, err := newAgentHandler(invalidCollectionSource{engineSource: engineSource{engine: engine}}, func() bool { return true }, allow, 1, discardLogger(), productionHandlerLimits())
	if err != nil {
		t.Fatal(err)
	}
	assertAgentError(t, agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.BackupsPath+"?revision=7"), http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication)
}

func TestAgentHandlerAppliesDeadline(t *testing.T) {
	source := &deadlineSource{}
	limits := productionHandlerLimits()
	limits.requestTimeout = 20 * time.Millisecond
	allow := &fakeAuthenticator{decision: AuthDecision{Outcome: AuthAllowed}}
	handler, err := newAgentHandler(source, func() bool { return true }, allow, 1, discardLogger(), limits)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	response := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, evidencev1alpha1.SnapshotPath)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("deadline took %s", elapsed)
	}
	if !source.sawDeadline.Load() {
		t.Fatal("the source saw no deadline")
	}
	assertAgentError(t, response, http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication)
}

func TestAgentHandlerReturnsMutationIsolatedDeterministicJSON(t *testing.T) {
	handler, _ := newTestAgentHandler(t, true, AuthAllowed)
	target := evidencev1alpha1.BackupsPath + "?revision=7&limit=1"
	first := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, target)
	second := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, target)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("same publication request produced different JSON (%d)", first.Code)
	}
	var page evidencev1alpha1.BarmanBackupPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	*page.Items[0].Status = "MUTATED"
	third := agentRequest(handler, "Bearer "+agentTestToken, http.MethodGet, target)
	if !bytes.Equal(first.Body.Bytes(), third.Body.Bytes()) {
		t.Fatal("consumer mutation changed publication JSON")
	}
}

func testReadiness(ready bool) *readiness.ProbeState {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	state := readiness.New(true, time.Hour, func() time.Time { return now })
	if ready {
		state.MarkReachable(now)
	}
	return state
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

type invalidCollectionSource struct{ engineSource }

func (invalidCollectionSource) Backups(context.Context, PageRequest) (evidencev1alpha1.BarmanBackupPage, error) {
	return evidencev1alpha1.BarmanBackupPage{}, nil
}

type deadlineSource struct {
	sawDeadline atomic.Bool
}

func (s *deadlineSource) Snapshot(ctx context.Context) (evidencev1alpha1.RepositoryEvidenceSnapshot, error) {
	if _, ok := ctx.Deadline(); ok {
		s.sawDeadline.Store(true)
	}
	<-ctx.Done()
	return evidencev1alpha1.RepositoryEvidenceSnapshot{}, ctx.Err()
}

func (s *deadlineSource) Backups(context.Context, PageRequest) (evidencev1alpha1.BarmanBackupPage, error) {
	return evidencev1alpha1.BarmanBackupPage{}, errors.New("unexpected call")
}

func (s *deadlineSource) WALRanges(context.Context, PageRequest) (evidencev1alpha1.BarmanWALRangePage, error) {
	return evidencev1alpha1.BarmanWALRangePage{}, errors.New("unexpected call")
}

func (s *deadlineSource) WALGaps(context.Context, PageRequest) (evidencev1alpha1.BarmanWALGapPage, error) {
	return evidencev1alpha1.BarmanWALGapPage{}, errors.New("unexpected call")
}

func (s *deadlineSource) RecoveryPaths(context.Context, PageRequest) (evidencev1alpha1.BarmanRecoveryPathPage, error) {
	return evidencev1alpha1.BarmanRecoveryPathPage{}, errors.New("unexpected call")
}
