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

package evidence

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestParseEndpoint(t *testing.T) {
	tests := []struct {
		raw, namespace     string
		wantOrigin, wantSN string
	}{
		{evidencev1alpha1.Endpoint("orders-evidence", "team-a"), "team-a", "https://orders-evidence.team-a.svc:8443", "orders-evidence.team-a.svc"},
		{"https://svc.team-a.svc", "team-a", "https://svc.team-a.svc", "svc.team-a.svc"},
		{"https://svc.team-a.svc:8443/", "team-a", "https://svc.team-a.svc:8443", "svc.team-a.svc"},
		{evidencev1alpha1.Endpoint("orders-evidence", "team-b"), "team-a", "", ""},
		{"https://svc.team-a.svc.cluster.local:8443", "team-a", "", ""},
		{"https://svc.team-a:8443", "team-a", "", ""},
		{"https://team-a.svc:8443", "team-a", "", ""},
		{"https://a.svc.team-a.svc:8443", "team-a", "", ""},
		{"http://svc.team-a.svc:8443", "team-a", "", ""},
		{"https://user@svc.team-a.svc:8443", "team-a", "", ""},
		{"https://svc.team-a.svc:8443/api", "team-a", "", ""},
		{"https://svc.team-a.svc:8443?x=1", "team-a", "", ""},
		{"https://svc.team-a.svc:8443#x", "team-a", "", ""},
		{"https://svc.team-a.svc:0", "team-a", "", ""},
		{"https://svc.team-a.svc:08443", "team-a", "", ""},
		{"https://svc.team-a.svc:99999", "team-a", "", ""},
		{"https://SVC.team-a.svc:8443", "team-a", "", ""},
		{"https://svc_x.team-a.svc:8443", "team-a", "", ""},
		{"https://10.0.0.1:8443", "team-a", "", ""},
		{"https://[::1]:8443", "team-a", "", ""},
		{"https://svc.team-a.svc.:8443", "team-a", "", ""},
		{"https://svc..svc:8443", "", "", ""},
		{"svc.team-a.svc:8443", "team-a", "", ""},
		{"", "team-a", "", ""},
		{"https://" + strings.Repeat("a", 600) + ".team-a.svc", "team-a", "", ""},
	}
	for _, tc := range tests {
		origin, serverName, err := parseEndpoint(tc.raw, tc.namespace)
		if tc.wantOrigin == "" {
			if err == nil {
				t.Errorf("%q in %q accepted as %q", tc.raw, tc.namespace, origin)
			}
			continue
		}
		if err != nil || origin != tc.wantOrigin || serverName != tc.wantSN {
			t.Errorf("%q: got %q %q %v, want %q %q", tc.raw, origin, serverName, err, tc.wantOrigin, tc.wantSN)
		}
	}
}

func TestStatusKind(t *testing.T) {
	errorBody := func(code evidencev1alpha1.ErrorCode) []byte {
		body, _ := json.Marshal(evidencev1alpha1.EvidenceAPIError{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: code, Message: "m"})
		return body
	}
	tests := []struct {
		status int
		body   []byte
		want   failure
	}{
		{http.StatusUnauthorized, nil, failureUnauthorized},
		{http.StatusUnauthorized, errorBody(evidencev1alpha1.ErrorUnauthenticated), failureUnauthorized},
		{http.StatusUnauthorized, errorBody(evidencev1alpha1.ErrorAudienceMismatch), failureAudienceMismatch},
		{http.StatusForbidden, errorBody(evidencev1alpha1.ErrorAudienceMismatch), failureForbidden},
		{http.StatusForbidden, nil, failureForbidden},
		{http.StatusTooManyRequests, nil, failureBusy},
		{http.StatusConflict, nil, failurePublicationChanged},
		{http.StatusRequestEntityTooLarge, nil, failureTooLarge},
		{http.StatusServiceUnavailable, errorBody(evidencev1alpha1.ErrorAuthenticationUnavailable), failureAuthUnavailable},
		{http.StatusServiceUnavailable, errorBody(evidencev1alpha1.ErrorBusy), failureUnavailable},
		{http.StatusServiceUnavailable, []byte("garbage"), failureUnavailable},
		{http.StatusInternalServerError, nil, failureUnavailable},
		{http.StatusBadRequest, nil, failureContract},
		{http.StatusNotFound, nil, failureContract},
		{http.StatusFound, nil, failureContract},
	}
	for _, tc := range tests {
		if got := statusKind(tc.status, tc.body); got != tc.want {
			t.Errorf("%d: %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestCheckIdentity(t *testing.T) {
	ca := newTestCA(t, "x.team-a.svc")
	snapshot := fixtureSnapshot(t)
	other := "billing"
	tests := []struct {
		name   string
		mutate func(*evidencev1alpha1.RepositoryEvidenceSnapshot)
		status func(fingerprint, server *string)
		ok     bool
	}{
		{name: "matching", ok: true},
		{name: "no cluster name is fine", mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Identity.Cluster.Name = nil }, ok: true},
		{name: "status without fingerprint or server", status: func(f, s *string) { *f, *s = "", "" }, ok: true},
		{name: "namespace", mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Identity.Cluster.Namespace = "team-b" }},
		{name: "cluster name", mutate: func(s *evidencev1alpha1.RepositoryEvidenceSnapshot) { s.Identity.Cluster.Name = &other }},
		{name: "fingerprint", status: func(f, _ *string) { *f = "sha256:" + strings.Repeat("1", 64) }},
		{name: "server", status: func(_, s *string) { *s = "billing" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			copied := snapshot
			if tc.mutate != nil {
				tc.mutate(&copied)
			}
			agent := testEvidenceAgent(testAgent, ca, snapshot, time.Now())
			if tc.status != nil {
				tc.status(&agent.Status.Fingerprint, &agent.Status.ServerName)
			}
			err := checkIdentity(agent, &copied)
			if (err == nil) != tc.ok {
				t.Fatalf("err %v, want ok %v", err, tc.ok)
			}
			if err != nil && kindOf(err) != failureIdentity {
				t.Fatalf("kind %q", kindOf(err))
			}
		})
	}
}

func TestDecoders(t *testing.T) {
	snapshot := fixtureSnapshot(t)
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSnapshot(body); err != nil {
		t.Fatalf("golden snapshot refused: %v", err)
	}
	for name, bad := range map[string][]byte{
		"trailing data": append(append([]byte{}, body...), []byte(` {}`)...),
		"two values":    append(append([]byte{}, body...), body...),
		"not JSON":      []byte("<html>"),
		"empty":         nil,
		"null":          []byte("null"),
	} {
		if _, err := decodeSnapshot(bad); err == nil || kindOf(err) != failureContract {
			t.Errorf("%s: %v", name, err)
		}
	}
	page := backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)[0]
	empty := ""
	page.NextCursor = &empty
	encoded, _ := json.Marshal(page)
	if _, err := decodeBackupPage(encoded); err == nil {
		t.Error("an empty cursor was accepted")
	}
	if _, ok := decodeAPIError([]byte(`{"api_version":"x","kind":"EvidenceAPIError","code":"busy","message":"m"}`)); ok {
		t.Error("an invalid error body was accepted")
	}
}

func TestMediaType(t *testing.T) {
	for header, want := range map[string]bool{
		evidencev1alpha1.MediaType:                     true,
		evidencev1alpha1.MediaType + "; charset=utf-8": true,
		"application/json":                             false,
		"":                                             false,
		evidencev1alpha1.MediaType + "x":               false,
	} {
		if got := mediaTypeOK(header); got != want {
			t.Errorf("%q: %v", header, got)
		}
	}
}

func TestFailureReasonsAreClosed(t *testing.T) {
	for _, outcome := range outcomes {
		if outcome == outcomeSuccess || outcome == outcomeStaleEpoch {
			continue
		}
		if _, ok := failureReasons[failure(outcome)]; !ok {
			t.Errorf("outcome %q has no reason", outcome)
		}
	}
	if len(failureReasons) != len(outcomes)-2 {
		t.Errorf("%d reasons for %d failure outcomes", len(failureReasons), len(outcomes)-2)
	}
}
