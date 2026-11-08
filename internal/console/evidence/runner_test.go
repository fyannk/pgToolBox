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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// TestRunnerFetch drives one pass against a scripted agent per case and
// checks the published Result.
func TestRunnerFetch(t *testing.T) {
	serverName := evidencev1alpha1.ServerName(testService, testNamespace)
	ca := newTestCA(t, serverName)
	otherCA := newTestCA(t, serverName)
	wrongNameCA := newTestCA(t, evidencev1alpha1.ServerName("someone-else", testNamespace))

	tests := []struct {
		name string
		// ca is the serving certificate; agentCA the bundle in the status
		// (defaults: ca for both).
		ca, agentCA *testCA
		script      func(t *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot)
		agent       func(*pgtoolboxv1alpha1.EvidenceAgent)
		wantState   viewmodel.EvidenceState
		wantReason  string
		wantIDs     []string
		wantDone    bool
		wantOutcome string
		wantSnap    bool
		wantNoCalls bool
	}{
		{
			name: "happy path", wantState: viewmodel.EvidenceLive, wantIDs: []string{"b1"}, wantDone: true,
			wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "pagination reads every page",
			script: func(t *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				items := []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1"), fixtureBackup(t, "b2"), fixtureBackup(t, "b3"), fixtureBackup(t, "b4"), fixtureBackup(t, "b5")}
				a.pages = backupPages(*s, items, 2)
			},
			wantState: viewmodel.EvidenceLive, wantIDs: []string{"b1", "b2", "b3", "b4", "b5"}, wantDone: true,
			wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "other servers' backups are not this agent's",
			script: func(t *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				other := fixtureBackup(t, "x1")
				other.Server = "billing"
				a.pages = backupPages(*s, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1"), other}, 5)
			},
			wantState: viewmodel.EvidenceLive, wantIDs: []string{"b1"}, wantDone: true, wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "a total larger than what was read is a truncated collection",
			script: func(t *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.pages = backupPages(*s, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
				total := uint64(9)
				a.pages[0].TotalItems = &total
			},
			wantState: viewmodel.EvidenceLive, wantIDs: []string{"b1"}, wantDone: false, wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "a chain longer than the page bound is cut",
			script: func(t *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				items := make([]evidencev1alpha1.BarmanBackup, 0, maximumBackupPages+5)
				for index := range maximumBackupPages + 5 {
					items = append(items, fixtureBackup(t, "b"+itoa(1000+index)))
				}
				a.pages = backupPages(*s, items, 1)
			},
			wantState: viewmodel.EvidenceLive, wantDone: false, wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "a snapshot without a completed generation has no collection",
			script: func(_ *testing.T, a *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				s.EvidenceGeneration, s.StartedAt, s.CompletedAt = 0, nil, nil
				s.Completeness, s.State = evidencev1alpha1.Unscanned, evidencev1alpha1.Unknown
				s.Details.BarmanCloud = nil
				s.Details.Type = "other/v1"
				a.pages = nil
			},
			wantState: viewmodel.EvidenceLive, wantDone: false, wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "a publication change is retried once from the snapshot",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.conflictOnce = true
			},
			wantState: viewmodel.EvidenceLive, wantIDs: []string{"b1"}, wantDone: true, wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "the agent's own stale flag is stale",
			script: func(_ *testing.T, _ *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				failed := evidencev1alpha1.FailureTimeout
				s.Stale, s.State = true, evidencev1alpha1.Unknown
				s.Inventory.LastFailureCategory = &failed
				for index := range s.Capabilities {
					s.Capabilities[index].State = evidencev1alpha1.Unknown
				}
			},
			wantState: viewmodel.EvidenceStale, wantReason: reasonAgentStale, wantIDs: []string{"b1"}, wantDone: true,
			wantOutcome: outcomeSuccess, wantSnap: true,
		},
		{
			name: "wrong media type",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.contentType = "application/json"
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureContract.reason(), wantOutcome: string(failureContract),
		},
		{
			name: "oversize snapshot",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.snapshotBody = bytes.Repeat([]byte(" "), evidencev1alpha1.MaximumSnapshotBytes+1)
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureTooLarge.reason(), wantOutcome: string(failureTooLarge),
		},
		{
			name: "snapshot failing the contract",
			script: func(_ *testing.T, _ *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				s.ProducerInstance = "not-hex"
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureContract.reason(), wantOutcome: string(failureContract),
		},
		{
			name: "backup page from another publication",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.pages[0].EvidenceGeneration--
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureContract.reason(), wantOutcome: string(failureContract),
		},
		{
			name: "redirect is refused",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.redirect = true
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureContract.reason(), wantOutcome: string(failureContract),
		},
		{
			name: "CA bundle of another CA", agentCA: &otherCA,
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureTLS.reason(), wantOutcome: string(failureTLS),
		},
		{
			name: "certificate for another server name", ca: &wrongNameCA, agentCA: &wrongNameCA,
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureTLS.reason(), wantOutcome: string(failureTLS),
		},
		{
			// Defense in depth: the endpoint and CA were set by
			// another field manager than the operator's; the console sends
			// nothing to them.
			name:      "status written by another field manager",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.ManagedFields = operatorManagedFields("kubectl-patch") },
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureStatusWriter.reason(), wantOutcome: string(failureStatusWriter),
			wantNoCalls: true,
		},
		{
			name:      "status without managedFields",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.ManagedFields = nil },
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureStatusWriter.reason(), wantOutcome: string(failureStatusWriter),
			wantNoCalls: true,
		},
		{
			name: "agent refuses the namespace's audience",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.status, a.errorCode = http.StatusUnauthorized, evidencev1alpha1.ErrorAudienceMismatch
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureAudienceMismatch.reason(), wantOutcome: string(failureAudienceMismatch),
		},
		{
			name:      "CA bundle without a certificate",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.CABundle = []byte("not a PEM bundle") },
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureCABundle.reason(), wantOutcome: string(failureCABundle),
			wantNoCalls: true,
		},
		{
			name: "endpoint in another namespace",
			agent: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
				a.Status.Endpoint = evidencev1alpha1.Endpoint(testService, "team-b")
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureEndpoint.reason(), wantOutcome: string(failureEndpoint),
			wantNoCalls: true,
		},
		{
			name: "endpoint outside .svc",
			agent: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
				a.Status.Endpoint = "https://" + testService + "." + testNamespace + ".svc.cluster.local:8443"
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureEndpoint.reason(), wantOutcome: string(failureEndpoint),
			wantNoCalls: true,
		},
		{
			name: "401",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.status, a.errorCode = http.StatusUnauthorized, evidencev1alpha1.ErrorUnauthenticated
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureUnauthorized.reason(), wantOutcome: string(failureUnauthorized),
		},
		{
			name: "403",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.status, a.errorCode = http.StatusForbidden, evidencev1alpha1.ErrorForbidden
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureForbidden.reason(), wantOutcome: string(failureForbidden),
		},
		{
			name: "token review unavailable",
			script: func(_ *testing.T, a *fakeAgent, _ *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				a.status, a.errorCode = http.StatusServiceUnavailable, evidencev1alpha1.ErrorAuthenticationUnavailable
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureAuthUnavailable.reason(), wantOutcome: string(failureAuthUnavailable),
		},
		{
			name: "identity: another namespace",
			script: func(_ *testing.T, _ *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				s.Identity.Cluster.Namespace = "team-b"
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureIdentity.reason(), wantOutcome: string(failureIdentity),
		},
		{
			name: "identity: another cluster name",
			script: func(_ *testing.T, _ *fakeAgent, s *evidencev1alpha1.RepositoryEvidenceSnapshot) {
				other := "billing"
				s.Identity.Cluster.Name = &other
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureIdentity.reason(), wantOutcome: string(failureIdentity),
		},
		{
			name: "identity: another repository",
			agent: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
				a.Status.Fingerprint = "sha256:" + strings.Repeat("0", 64)
			},
			wantState: viewmodel.EvidenceUnavailable, wantReason: failureIdentity.reason(), wantOutcome: string(failureIdentity),
		},
		{
			name:      "epoch mismatch is stale",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.ProducerEpoch = testEpoch + 1 },
			wantState: viewmodel.EvidenceStale, wantReason: reasonOtherEpoch, wantIDs: []string{"b1"}, wantDone: true,
			wantOutcome: outcomeStaleEpoch, wantSnap: true,
		},
		{
			name:      "no published epoch is stale",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.ProducerEpoch = 0 },
			wantState: viewmodel.EvidenceStale, wantReason: reasonNoEpoch, wantIDs: []string{"b1"}, wantDone: true,
			wantOutcome: outcomeStaleEpoch, wantSnap: true,
		},
		{
			name:      "no endpoint yet is pending",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.Endpoint = "" },
			wantState: viewmodel.EvidencePending, wantReason: reasonNoEndpoint, wantNoCalls: true,
		},
		{
			name:      "no CA bundle yet is pending",
			agent:     func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Status.CABundle = nil },
			wantState: viewmodel.EvidencePending, wantReason: reasonNoCABundle, wantNoCalls: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serving, bundle := ca, ca
			if tc.ca != nil {
				serving = *tc.ca
			}
			if tc.agentCA != nil {
				bundle = *tc.agentCA
			}
			fake := newFakeAgent(t, serving)
			snapshot := fixtureSnapshot(t)
			fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
			if tc.script != nil {
				tc.script(t, fake, &snapshot)
			}
			fake.snapshot = snapshot
			object := testEvidenceAgent(testAgent, bundle, fixtureSnapshot(t), testNow.Add(-time.Hour))
			if tc.agent != nil {
				tc.agent(object)
			}
			var logs bytes.Buffer
			runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken+"\n"), func(o *Options) {
				o.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			var before float64
			if tc.wantOutcome != "" {
				before = testutil.ToFloat64(fetches.WithLabelValues(tc.wantOutcome))
			}

			runner.refresh(context.Background())
			got := runner.ForCluster(testNamespace, testCluster)

			if got.State != tc.wantState || got.Reason != tc.wantReason {
				t.Fatalf("state %q reason %q, want %q %q", got.State, got.Reason, tc.wantState, tc.wantReason)
			}
			if got.Agent != testAgent {
				t.Errorf("agent %q", got.Agent)
			}
			if (got.Snapshot != nil) != tc.wantSnap {
				t.Fatalf("snapshot present %v, want %v", got.Snapshot != nil, tc.wantSnap)
			}
			if got.Snapshot != nil {
				if err := got.Snapshot.Validate(); err != nil {
					t.Errorf("retained snapshot fails Validate: %v", err)
				}
				if got.ObservedAt == nil || !got.ObservedAt.Equal(testNow) {
					t.Errorf("observedAt %v", got.ObservedAt)
				}
			}
			if tc.wantIDs != nil {
				ids := slices.Sorted(maps.Keys(got.BackupIDs))
				if !slices.Equal(ids, tc.wantIDs) {
					t.Errorf("backup ids %v, want %v", ids, tc.wantIDs)
				}
			}
			if got.BackupsComplete != tc.wantDone {
				t.Errorf("backups complete %v, want %v", got.BackupsComplete, tc.wantDone)
			}
			if tc.wantOutcome != "" {
				if after := testutil.ToFloat64(fetches.WithLabelValues(tc.wantOutcome)); after != before+1 {
					t.Errorf("counter %q moved by %v, want 1", tc.wantOutcome, after-before)
				}
			}
			fake.set(func(a *fakeAgent) {
				if tc.wantNoCalls && a.snapshots != 0 {
					t.Errorf("the agent was called %d times", a.snapshots)
				}
				if a.badAuth {
					t.Error("a request lacked the bound token or the media type")
				}
			})
			if strings.Contains(logs.String(), testToken) || strings.Contains(logs.String(), "eyJ") {
				t.Error("the token reached the logs")
			}
			if state := runner.State(); state.State != viewmodel.SourceLive {
				t.Errorf("source %+v", state)
			}
			for _, state := range agentStates {
				want := 0.0
				if state == tc.wantState {
					want = 1
				}
				if got := testutil.ToFloat64(agentStatus.WithLabelValues(testNamespace, testAgent, string(state))); got != want {
					t.Errorf("pgtoolbox_console_evidence_agent_status{state=%q} = %v, want %v", state, got, want)
				}
			}
		})
	}
}

// TestRunnerFailedRefreshKeepsSnapshot checks that a failure after a good
// answer keeps the snapshot, its time and its backups, stale.
func TestRunnerFailedRefreshKeepsSnapshot(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	fake := newFakeAgent(t, ca)
	snapshot := fixtureSnapshot(t)
	fake.snapshot = snapshot
	fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	object := testEvidenceAgent(testAgent, ca, snapshot, testNow)
	now := testNow
	runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken), func(o *Options) {
		o.Now = func() time.Time { return now }
	})

	runner.refresh(context.Background())
	first := runner.ForCluster(testNamespace, testCluster)
	if first.State != viewmodel.EvidenceLive {
		t.Fatalf("first pass %+v", first)
	}

	for _, failure := range []struct {
		script func(*fakeAgent)
		reason string
	}{
		{func(a *fakeAgent) {
			a.status, a.errorCode = http.StatusInternalServerError, evidencev1alpha1.ErrorInvalidPublication
		}, failureUnavailable.reason()},
		{func(a *fakeAgent) { a.status, a.contentType = 0, "text/plain" }, failureContract.reason()},
		{func(a *fakeAgent) {
			a.contentType = evidencev1alpha1.MediaType
			a.snapshot.Identity.Cluster.Namespace = "elsewhere"
		}, failureIdentity.reason()},
	} {
		now = now.Add(time.Minute)
		fake.set(failure.script)
		runner.refresh(context.Background())
		got := runner.ForCluster(testNamespace, testCluster)
		if got.State != viewmodel.EvidenceStale || got.Reason != failure.reason {
			t.Fatalf("after failure: %q %q, want stale %q", got.State, got.Reason, failure.reason)
		}
		if got.Snapshot != first.Snapshot || !got.ObservedAt.Equal(testNow) || !got.BackupIDs["b1"] || !got.BackupsComplete {
			t.Fatalf("the retained evidence changed: %+v", got)
		}
	}

	// Recovery replaces it with a fresh observation.
	now = now.Add(time.Minute)
	fake.set(func(a *fakeAgent) { a.snapshot = snapshot })
	runner.refresh(context.Background())
	if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceLive || !got.ObservedAt.Equal(now) {
		t.Fatalf("after recovery %+v", got)
	}

	// The endpoint disappears (rollout): the snapshot is kept, stale.
	object.Status.Endpoint = ""
	runner.lister = newFakeLister(t, object)
	runner.refresh(context.Background())
	if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceStale || got.Reason != reasonNoEndpoint || got.Snapshot == nil {
		t.Fatalf("after endpoint loss %+v", got)
	}
}

// TestRunnerEpochKeepsCurrentSnapshot checks that an answer from a
// superseded configuration never displaces a snapshot of the current one.
func TestRunnerEpochKeepsCurrentSnapshot(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	fake := newFakeAgent(t, ca)
	current := fixtureSnapshot(t)
	fake.snapshot = current
	fake.pages = backupPages(current, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	object := testEvidenceAgent(testAgent, ca, current, testNow)
	runner := newTestRunner(t, newFakeLister(t, object), fake, writeToken(t, testToken))
	runner.refresh(context.Background())

	superseded := current
	superseded.ProducerEpoch = testEpoch - 1
	superseded.ProducerInstance = strings.Repeat("f", evidencev1alpha1.ProducerInstanceLength)
	fake.set(func(a *fakeAgent) { a.snapshot = superseded })
	runner.refresh(context.Background())
	got := runner.ForCluster(testNamespace, testCluster)
	if got.State != viewmodel.EvidenceStale || got.Reason != reasonOtherEpoch || got.Snapshot.ProducerEpoch != testEpoch {
		t.Fatalf("got %q %q epoch %d", got.State, got.Reason, got.Snapshot.ProducerEpoch)
	}
}

// TestRunnerToken covers a missing and a malformed token file.
func TestRunnerToken(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	snapshot := fixtureSnapshot(t)
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
		want failure
	}{
		{"missing", func(t *testing.T) string { return t.TempDir() + "/absent" }, failureTokenMissing},
		{"malformed", func(t *testing.T) string { return writeToken(t, "not a token") }, failureTokenInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeAgent(t, ca)
			fake.snapshot = snapshot
			runner := newTestRunner(t, newFakeLister(t, testEvidenceAgent(testAgent, ca, snapshot, testNow)), fake, tc.path(t))
			runner.refresh(context.Background())
			got := runner.ForCluster(testNamespace, testCluster)
			if got.State != viewmodel.EvidenceUnavailable || got.Reason != tc.want.reason() {
				t.Fatalf("result %q %q", got.State, got.Reason)
			}
			state := runner.State()
			if state.State != viewmodel.SourceUnavailable || state.Reason != tc.want.reason() || state.Since == nil {
				t.Fatalf("source %+v", state)
			}
			fake.set(func(a *fakeAgent) {
				if a.snapshots != 0 {
					t.Error("the agent was called without a token")
				}
			})
		})
	}
	if failureTokenMissing.reason() != "the console has no evidence-agent token (chart projection missing)" {
		t.Error("the missing-token reason is the documented phrase")
	}
}

// TestForCluster covers discovery: no agent, several agents, unreadable
// and unwatched namespaces, before the first pass, and disabled.
func TestForCluster(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	snapshot := fixtureSnapshot(t)
	pendingAgent := func(name string, created time.Time) client.Object {
		object := testEvidenceAgent(name, ca, snapshot, created)
		object.Status.Endpoint = ""
		return object
	}
	token := writeToken(t, testToken)

	t.Run("before the first pass", func(t *testing.T) {
		runner := newTestRunner(t, newFakeLister(t), nil, token)
		if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidencePending || got.Reason != reasonNotListed {
			t.Fatalf("%+v", got)
		}
		if state := runner.State(); state.State != viewmodel.SourcePending || state.Source != SourceKey {
			t.Fatalf("%+v", state)
		}
	})
	t.Run("no agent names the cluster", func(t *testing.T) {
		other := testEvidenceAgent("billing", ca, snapshot, testNow)
		other.Spec.ClusterRef.Name = "billing"
		other.Status.Endpoint = ""
		runner := newTestRunner(t, newFakeLister(t, other), nil, token)
		runner.refresh(context.Background())
		if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceNoAgent || got.Reason != reasonNoAgent || got.Agent != "" {
			t.Fatalf("%+v", got)
		}
		if got := runner.ForCluster(testNamespace, "billing"); got.State != viewmodel.EvidencePending || got.Agent != "billing" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("several agents: the oldest, said so", func(t *testing.T) {
		runner := newTestRunner(t, newFakeLister(t,
			pendingAgent("zeta", testNow.Add(-2*time.Hour)), pendingAgent("alpha", testNow.Add(-time.Hour))), nil, token)
		runner.refresh(context.Background())
		got := runner.ForCluster(testNamespace, testCluster)
		if got.Agent != "zeta" || !strings.HasPrefix(got.Reason, "2 EvidenceAgents name this Cluster; the oldest, zeta, is used") {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("unreadable namespace is noAgent", func(t *testing.T) {
		runner := newTestRunner(t, newFakeLister(t, pendingAgent(testAgent, testNow)), nil, token, func(o *Options) {
			o.Readable = func(string) bool { return false }
		})
		runner.refresh(context.Background())
		if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceNoAgent || got.Reason != reasonUnreadable {
			t.Fatalf("%+v", got)
		}
		if state := runner.State(); state.State != viewmodel.SourceLive {
			t.Fatalf("an unreadable kind is not a fault: %+v", state)
		}
	})
	t.Run("unwatched namespace", func(t *testing.T) {
		runner := newTestRunner(t, newFakeLister(t), nil, token)
		runner.refresh(context.Background())
		if got := runner.ForCluster("team-z", testCluster); got.State != viewmodel.EvidenceNoAgent || got.Reason != reasonNotWatched {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("list failure keeps the previous listing", func(t *testing.T) {
		lister := &flakyLister{inner: newFakeLister(t, pendingAgent(testAgent, testNow))}
		runner := newTestRunner(t, lister, nil, token)
		runner.refresh(context.Background())
		lister.fail = true
		runner.refresh(context.Background())
		if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidencePending || got.Agent != testAgent {
			t.Fatalf("%+v", got)
		}
		if state := runner.State(); state.State != viewmodel.SourceUnavailable || state.Reason != reasonListFailed {
			t.Fatalf("%+v", state)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		runner, err := New(Options{Disabled: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceDisabled {
			t.Fatalf("%+v", got)
		}
		if state := runner.State(); state.State != viewmodel.SourceDisabled || state.Source != SourceKey {
			t.Fatalf("%+v", state)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := runner.Run(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

type flakyLister struct {
	inner AgentLister
	fail  bool
}

func (f *flakyLister) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if f.fail {
		return errors.New("cache not synced")
	}
	return f.inner.List(ctx, list, opts...)
}

// TestNewValidates covers the constructor's refusals.
func TestNewValidates(t *testing.T) {
	lister := newFakeLister(t)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no lister", Options{Namespaces: []string{"a"}}},
		{"no namespace", Options{Lister: lister}},
		{"empty namespace", Options{Lister: lister, Namespaces: []string{""}}},
		{"relative token directory", Options{Lister: lister, Namespaces: []string{"a"}, TokenDirectory: "tokens"}},
		{"unclean token directory", Options{Lister: lister, Namespaces: []string{"a"}, TokenDirectory: "/var/../tokens"}},
		{"namespace not a DNS label", Options{Lister: lister, Namespaces: []string{"a/b"}}},
		{"interval below the floor", Options{Lister: lister, Namespaces: []string{"a"}, Interval: time.Second}},
		{"negative concurrency", Options{Lister: lister, Namespaces: []string{"a"}, Concurrency: -1}},
		{"excessive concurrency", Options{Lister: lister, Namespaces: []string{"a"}, Concurrency: MaximumConcurrency + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	runner, err := New(Options{Lister: lister, Namespaces: []string{"b", "a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.namespaces, []string{"a", "b"}) || runner.tokenPaths["b"] != evidencev1alpha1.DefaultTokenDirectory+"/b/token" ||
		runner.interval != DefaultInterval || runner.concurrency != DefaultConcurrency {
		t.Fatalf("defaults %+v", runner)
	}
}

// TestRunLoop checks that Run reads at once, answers Trigger, and stops
// on cancellation.
func TestRunLoop(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	fake := newFakeAgent(t, ca)
	snapshot := fixtureSnapshot(t)
	fake.snapshot = snapshot
	fake.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	runner := newTestRunner(t, newFakeLister(t, testEvidenceAgent(testAgent, ca, snapshot, testNow)), fake, writeToken(t, testToken))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for runner.ForCluster(testNamespace, testCluster).State != viewmodel.EvidenceLive {
		if time.Now().After(deadline) {
			t.Fatal("no pass within 10 s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	runner.Trigger()
	runner.Trigger()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}
