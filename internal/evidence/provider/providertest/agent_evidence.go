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

package providertest

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/fyannk/pgToolBox/internal/evidence/evidenceapi"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// journeyToken is the bearer token the journeys present; the handler's
// authenticator accepts any well-formed token, so its value is irrelevant.
const journeyToken = "providertest-journey-token-0123456789abcdef"

type allowAll struct{}

func (allowAll) Authenticate(context.Context, evidencev1alpha1.BearerToken) evidenceapi.AuthDecision {
	return evidenceapi.AuthDecision{Outcome: evidenceapi.AuthAllowed, User: "providertest"}
}

// AgentEvidence is a scanned snapshot served as the agent serves it: the
// evidence engine's projection behind the evidence/v1alpha1 HTTP handler.
type AgentEvidence struct {
	t       *testing.T
	handler http.Handler
	// Snapshot is the decoded, validated response of the snapshot route.
	Snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	// SnapshotBody is that response as the agent wrote it.
	SnapshotBody []byte
}

// ServeAgentEvidence publishes source through an evidence engine with
// options, reads the snapshot route and fails t unless it answers 200 with
// a valid snapshot.
func ServeAgentEvidence(t *testing.T, source inventory.Snapshot, options evidenceapi.Options, probe *readiness.ProbeState) AgentEvidence {
	t.Helper()
	engine, err := evidenceapi.NewEngine(evidenceapi.EngineOptions{Projection: options, Initial: source})
	if err != nil {
		t.Fatal(err)
	}
	audience, err := evidencev1alpha1.TokenAudience(options.ClusterNamespace)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := evidenceapi.NewAgentHandler(evidenceapi.AgentHandlerOptions{
		Engine: engine, Readiness: probe, Authenticator: allowAll{}, ProducerEpoch: 1,
		Logger: slog.New(slog.DiscardHandler), Audience: audience,
		ParseAuthorization: evidencev1alpha1.ParseSharedAuthorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	served := AgentEvidence{t: t, handler: handler}
	served.SnapshotBody = served.Get(evidencev1alpha1.SnapshotPath, &served.Snapshot)
	if err := served.Snapshot.Validate(); err != nil {
		t.Fatalf("served snapshot is invalid: %v", err)
	}
	return served
}

// Page reads the first page of a collection route at the snapshot's
// revision into page and returns the body.
func (a AgentEvidence) Page(path string, page any) []byte {
	a.t.Helper()
	return a.Get(path+"?limit="+strconv.Itoa(evidenceapi.MaximumPageSize)+"&revision="+strconv.FormatUint(a.Snapshot.Revision, 10), page)
}

// Get reads target, fails the test unless it answers 200, decodes the body
// into value and returns it.
func (a AgentEvidence) Get(target string, value any) []byte {
	a.t.Helper()
	request := httptest.NewRequestWithContext(a.t.Context(), http.MethodGet, target, http.NoBody)
	request.Header.Set("Authorization", "Bearer "+journeyToken)
	response := httptest.NewRecorder()
	a.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		a.t.Fatalf("GET %s = %d: %s", target, response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	if err := json.Unmarshal(body, value); err != nil {
		a.t.Fatalf("GET %s: %v", target, err)
	}
	return body
}
