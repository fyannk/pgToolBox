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
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func statusEntry(manager, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "status",
		FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(fields),
	}
}

func TestStatusWrittenByOperator(t *testing.T) {
	if OperatorStatusManager != "pgtoolbox-status" {
		t.Fatal("OperatorStatusManager mirrors internal/operator/reconcile.StatusFieldOwner (pinned there too)")
	}
	const both = `{"f:status":{"f:endpoint":{},"f:caBundle":{}}}`
	mainEntry := func(manager, fields string) metav1.ManagedFieldsEntry {
		entry := statusEntry(manager, fields)
		entry.Subresource = ""
		return entry
	}
	for _, tc := range []struct {
		name    string
		entries []metav1.ManagedFieldsEntry
		want    bool
	}{
		{"operator owns both", operatorManagedFields(OperatorStatusManager), true},
		{"operator owns both in two entries", []metav1.ManagedFieldsEntry{
			statusEntry(OperatorStatusManager, `{"f:status":{"f:endpoint":{}}}`),
			statusEntry(OperatorStatusManager, `{"f:status":{"f:caBundle":{}}}`),
		}, true},
		{"another manager owns other status fields", []metav1.ManagedFieldsEntry{
			statusEntry(OperatorStatusManager, both), statusEntry("kubectl-edit", `{"f:status":{"f:conditions":{}}}`),
		}, true},
		{"no managedFields", nil, false},
		{"another manager wrote both", operatorManagedFields("kubectl-patch"), false},
		{"another manager took the endpoint", []metav1.ManagedFieldsEntry{
			statusEntry(OperatorStatusManager, `{"f:status":{"f:caBundle":{}}}`), statusEntry("intruder", `{"f:status":{"f:endpoint":{}}}`),
		}, false},
		{"another manager co-owns the CA", []metav1.ManagedFieldsEntry{
			statusEntry(OperatorStatusManager, both), statusEntry("intruder", `{"f:status":{"f:caBundle":{}}}`),
		}, false},
		{"operator owns only the endpoint", []metav1.ManagedFieldsEntry{statusEntry(OperatorStatusManager, `{"f:status":{"f:endpoint":{}}}`)}, false},
		{"operator's entry is not on the status subresource", []metav1.ManagedFieldsEntry{mainEntry(OperatorStatusManager, both)}, false},
		{"undecodable fields", []metav1.ManagedFieldsEntry{statusEntry(OperatorStatusManager, `{`)}, false},
		{"entry without fields", []metav1.ManagedFieldsEntry{{Manager: OperatorStatusManager, Subresource: "status"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{ManagedFields: tc.entries}}
			if got := StatusWrittenByOperator(agent); got != tc.want {
				t.Fatalf("StatusWrittenByOperator = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCacheTransform: the EvidenceAgent transform keeps the status
// subresource's entries and drops every other one; every other kind loses
// all of its managedFields, as with the default transform.
func TestCacheTransform(t *testing.T) {
	entries := append(operatorManagedFields(OperatorStatusManager),
		metav1.ManagedFieldsEntry{Manager: "kubectl-client-side-apply", Operation: metav1.ManagedFieldsOperationUpdate},
		metav1.ManagedFieldsEntry{Manager: "other", Operation: metav1.ManagedFieldsOperationUpdate, Subresource: "scale"},
		statusEntry("intruder", `{"f:status":{"f:endpoint":{}}}`),
	)
	agent := &pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{Name: "a", ManagedFields: entries}}
	out, err := CacheTransform(agent)
	if err != nil {
		t.Fatal(err)
	}
	kept := out.(*pgtoolboxv1alpha1.EvidenceAgent).ManagedFields
	if len(kept) != 2 {
		t.Fatalf("kept %d entries, want the 2 status entries: %+v", len(kept), kept)
	}
	for _, entry := range kept {
		if entry.Subresource != "status" {
			t.Fatalf("kept a non-status entry: %+v", entry)
		}
	}
	if StatusWrittenByOperator(out.(*pgtoolboxv1alpha1.EvidenceAgent)) {
		t.Fatal("the transform hid the intruder's status entry")
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", ManagedFields: entries}}
	if out, err := CacheTransform(pod); err != nil || out.(*corev1.Pod).ManagedFields != nil {
		t.Fatalf("a Pod kept managedFields: %v", err)
	}
	tombstone := cache.DeletedFinalStateUnknown{Key: "ns/a", Obj: agent}
	if out, err := CacheTransform(tombstone); err != nil || out.(cache.DeletedFinalStateUnknown).Key != "ns/a" {
		t.Fatalf("tombstone = %+v, %v", out, err)
	}
	if out, err := CacheTransform("not an object"); err != nil || out != "not an object" {
		t.Fatalf("a non-object = %v, %v", out, err)
	}
}

// TestRunnerPresentsEachNamespaceItsOwnToken: with two watched
// namespaces, each agent receives the token projected for its own
// namespace, and a namespace without its token fails alone.
func TestRunnerPresentsEachNamespaceItsOwnToken(t *testing.T) {
	const otherNamespace = "team-b"
	otherToken := strings.Replace(testToken, "c2lnbmF0dXJl", "b3RoZXJzaWdu", 1)
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace), evidencev1alpha1.ServerName(testService, otherNamespace))
	snapshot := fixtureSnapshot(t)
	agentA := newFakeAgent(t, ca)
	agentA.snapshot = snapshot
	agentA.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	agentB := newFakeAgent(t, ca)
	agentB.token = otherToken
	snapshotB := fixtureSnapshot(t)
	snapshotB.Identity.Cluster.Namespace = otherNamespace
	agentB.snapshot = snapshotB
	agentB.pages = backupPages(snapshotB, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)

	objectA := testEvidenceAgent(testAgent, ca, snapshot, testNow)
	objectB := testEvidenceAgent(testAgent, ca, snapshotB, testNow)
	objectB.Namespace, objectB.UID = otherNamespace, "uid-b"
	objectB.Status.Endpoint = evidencev1alpha1.Endpoint(testService, otherNamespace)

	directory := writeToken(t, testToken)
	runner := newTestRunner(t, newFakeLister(t, objectA, objectB), nil, directory, func(o *Options) {
		o.Namespaces = []string{testNamespace, otherNamespace}
	})
	// Route each namespace's Service to its own fake agent.
	runner.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.Contains(address, "."+otherNamespace+".") {
			return agentB.dial(ctx, network, address)
		}
		return agentA.dial(ctx, network, address)
	}

	// team-b's token is not projected yet: only team-b fails.
	runner.refresh(context.Background())
	if got := runner.ForCluster(testNamespace, testCluster); got.State != viewmodel.EvidenceLive {
		t.Fatalf("team-a: %q %q", got.State, got.Reason)
	}
	if got := runner.ForCluster(otherNamespace, testCluster); got.State != viewmodel.EvidenceUnavailable || got.Reason != failureTokenMissing.reason() {
		t.Fatalf("team-b: %q %q", got.State, got.Reason)
	}
	if state := runner.State(); state.State != viewmodel.SourceUnavailable || state.Reason != failureTokenMissing.reason()+" for namespace team-b" {
		t.Fatalf("source %+v", state)
	}

	writeNamespaceToken(t, directory, otherNamespace, otherToken)
	runner.refresh(context.Background())
	if got := runner.ForCluster(otherNamespace, testCluster); got.State != viewmodel.EvidenceLive {
		t.Fatalf("team-b with its token: %q %q", got.State, got.Reason)
	}
	for name, agent := range map[string]*fakeAgent{testNamespace: agentA, otherNamespace: agentB} {
		agent.set(func(a *fakeAgent) {
			if a.badAuth || a.snapshots == 0 {
				t.Errorf("%s: badAuth=%v snapshots=%d (each agent must receive its own namespace's token)", name, a.badAuth, a.snapshots)
			}
		})
	}
	if state := runner.State(); state.State != viewmodel.SourceLive {
		t.Fatalf("source %+v", state)
	}
}

// TestAgentStatusGaugeFollowsTheListing: an agent's series disappear with
// it.
func TestAgentStatusGaugeFollowsTheListing(t *testing.T) {
	ca := newTestCA(t, evidencev1alpha1.ServerName(testService, testNamespace))
	snapshot := fixtureSnapshot(t)
	agent := newFakeAgent(t, ca)
	agent.snapshot = snapshot
	agent.pages = backupPages(snapshot, []evidencev1alpha1.BarmanBackup{fixtureBackup(t, "b1")}, 5)
	object := testEvidenceAgent("gauged", ca, snapshot, testNow)
	lister := newFakeLister(t, object)
	runner := newTestRunner(t, lister, agent, writeToken(t, testToken))
	runner.refresh(context.Background())
	if got := testutil.ToFloat64(agentStatus.WithLabelValues(testNamespace, "gauged", string(viewmodel.EvidenceLive))); got != 1 {
		t.Fatalf("live gauge = %v", got)
	}
	if err := lister.Delete(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	runner.refresh(context.Background())
	if n := testutil.CollectAndCount(agentStatus); n != 0 {
		for _, state := range agentStates {
			if agentStatus.DeleteLabelValues(testNamespace, "gauged", string(state)) {
				t.Fatalf("series of a deleted agent survived (state %q)", state)
			}
		}
	}
}

// writeNamespaceToken writes namespace's token file under directory.
func writeNamespaceToken(t *testing.T, directory, namespace, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, namespace), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, namespace, evidencev1alpha1.TokenFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
