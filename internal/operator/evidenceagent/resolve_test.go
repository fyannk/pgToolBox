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

package evidenceagent

import (
	"context"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/pkg/discovery"
)

// s3Configuration is an ObjectStore spec.configuration of the supported
// shape, with keys overridden or (nil) removed.
func s3Configuration(overrides map[string]any) map[string]any {
	raw := map[string]any{
		"destinationPath": "s3://backups/team-a",
		"endpointURL":     "https://minio.storage.svc:9000",
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
		},
	}
	for key, value := range overrides {
		if value == nil {
			delete(raw, key)
			continue
		}
		raw[key] = value
	}
	return raw
}

// TestResolveRecordsTheObjectStoreAndGatesOnTheProbe covers the steps the
// operator adds around the shared resolver: the ObjectStore name is recorded
// in status before the ObjectStore is read (its watch maps back through it),
// and an unprobed ObjectStore API is a problem, not a read. The resolution
// itself is tested in internal/backupresolve.
func TestResolveRecordsTheObjectStoreAndGatesOnTheProbe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
		Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: barmanPluginName, Parameters: map[string]string{barmanObjectParameter: "store"},
		}}},
	}
	store := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": s3Configuration(nil)}}}
	store.SetGroupVersionKind(discovery.ObjectStoreGVK)
	store.SetNamespace("team-a")
	store.SetName("store")
	reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cnpgv1.Cluster{}).WithObjects(cluster).Build()
	if err := reader.Create(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
	}

	r := &Reconciler{APIReader: reader}
	_, whyNot, err := r.resolve(context.Background(), agent)
	if err != nil || whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable {
		t.Fatalf("unprobed API: problem = %+v, err = %v", whyNot, err)
	}
	if agent.Status.ObjectStoreName != "store" {
		t.Fatalf("status.objectStoreName = %q, want it recorded before the read", agent.Status.ObjectStoreName)
	}

	// Listable in other watched namespaces, refused in the agent's: the
	// agent's namespace decides (its RBAC there alone).
	r.ObjectStoreAPI = discovery.Result{GVK: discovery.ObjectStoreGVK, State: discovery.Available, RefusedIn: []string{"team-a"}}
	_, whyNot, err = r.resolve(context.Background(), agent)
	if err != nil || whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable || !strings.Contains(whyNot.message, "in team-a") {
		t.Fatalf("API refused in the agent's namespace: problem = %+v, err = %v", whyNot, err)
	}

	r.ObjectStoreAPI = discovery.Result{GVK: discovery.ObjectStoreGVK, State: discovery.Available, RefusedIn: []string{"team-b"}}
	resolved, whyNot, err := r.resolve(context.Background(), agent)
	if err != nil || whyNot != nil {
		t.Fatalf("resolve = %+v, %v", whyNot, err)
	}
	if resolved.ClusterName != "orders" || resolved.ClusterUID != "uid-1" || resolved.ServerName != "orders" || resolved.ObjectStoreName != "store" {
		t.Fatalf("composition = %+v", resolved)
	}

	agent.Spec.ClusterRef.Name = "absent"
	if _, whyNot, _ := r.resolve(context.Background(), agent); whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonClusterNotFound {
		t.Fatalf("missing Cluster: problem = %+v", whyNot)
	}
}

// TestResolveInTreeNeedsNoObjectStoreAPI: a Cluster that backs up through
// its in-tree spec.backup.barmanObjectStore resolves without the plugin's
// ObjectStore API, and records no ObjectStore in status.
func TestResolveInTreeNeedsNoObjectStoreAPI(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configuration := &cnpgv1.BarmanObjectStoreConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(s3Configuration(nil), configuration); err != nil {
		t.Fatal(err)
	}
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
		Spec:       cnpgv1.ClusterSpec{Backup: &cnpgv1.BackupConfiguration{BarmanObjectStore: configuration}},
	}
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
	}
	r := &Reconciler{APIReader: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cnpgv1.Cluster{}).WithObjects(cluster).Build()}
	// Without spec.allowedSecrets the Cluster's own configuration (which
	// whoever may patch the Cluster writes) names no Secret the agent reads.
	if _, whyNot, err := r.resolve(context.Background(), agent); err != nil || whyNot == nil ||
		whyNot.reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore || !strings.Contains(whyNot.message, "spec.allowedSecrets") {
		t.Fatalf("unlisted in-tree: problem = %+v, %v", whyNot, err)
	}
	agent.Spec.AllowedSecrets = []string{"minio"}
	resolved, whyNot, err := r.resolve(context.Background(), agent)
	if err != nil || whyNot != nil {
		t.Fatalf("resolve = %+v, %v", whyNot, err)
	}
	if resolved.ObjectStoreName != "" || agent.Status.ObjectStoreName != "" || resolved.ServerName != "orders" || resolved.Origin.String() != "Cluster orders spec.backup.barmanObjectStore" {
		t.Fatalf("composition = %+v, status.objectStoreName = %q", resolved, agent.Status.ObjectStoreName)
	}
}

// TestResolveAppliesTheAuthorsPolicy: the live configuration is checked
// against the EvidenceAgent's own choices (allowedSecrets,
// allowInsecureEndpoint, workloadIdentity) before anything is rendered.
func TestResolveAppliesTheAuthorsPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	plainHTTP := map[string]any{"endpointURL": "http://garage.storage.svc:3900"}
	iamRole := map[string]any{"endpointURL": nil, "s3Credentials": map[string]any{
		"inheritFromIAMRole": true, "region": map[string]any{"name": "region", "key": "REGION"},
	}}
	for _, test := range []struct {
		name      string
		overrides map[string]any
		spec      func(*pgtoolboxv1alpha1.EvidenceAgentSpec)
		refusal   string
	}{
		{name: "no list, ObjectStore, plain http", overrides: plainHTTP},
		{name: "listed", spec: func(s *pgtoolboxv1alpha1.EvidenceAgentSpec) { s.AllowedSecrets = []string{"minio"} }},
		{
			name: "not listed", spec: func(s *pgtoolboxv1alpha1.EvidenceAgentSpec) { s.AllowedSecrets = []string{"other"} },
			refusal: "Secret minio, which spec.allowedSecrets does not list",
		},
		{
			name: "listed, plain http", overrides: plainHTTP,
			spec:    func(s *pgtoolboxv1alpha1.EvidenceAgentSpec) { s.AllowedSecrets = []string{"minio"} },
			refusal: "spec.allowInsecureEndpoint",
		},
		{name: "listed, plain http admitted", overrides: plainHTTP, spec: func(s *pgtoolboxv1alpha1.EvidenceAgentSpec) {
			s.AllowedSecrets, s.AllowInsecureEndpoint = []string{"minio"}, true
		}},
		{name: "key slot swap", overrides: map[string]any{"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
			"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
		}}, refusal: "for both accessKeyId and secretAccessKey"},
		{name: "IAM role without workload identity", overrides: iamRole, refusal: "spec.workloadIdentity"},
		{name: "IAM role with workload identity", overrides: iamRole, spec: func(s *pgtoolboxv1alpha1.EvidenceAgentSpec) {
			s.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := s3Configuration(test.overrides)
			cluster := &cnpgv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
				Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
					Name: barmanPluginName, Parameters: map[string]string{barmanObjectParameter: "store"},
				}}},
			}
			store := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": configuration}}}
			store.SetGroupVersionKind(discovery.ObjectStoreGVK)
			store.SetNamespace("team-a")
			store.SetName("store")
			reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cnpgv1.Cluster{}).WithObjects(cluster).Build()
			if err := reader.Create(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			agent := &pgtoolboxv1alpha1.EvidenceAgent{
				ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
				Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
			}
			if test.spec != nil {
				test.spec(&agent.Spec)
			}
			if agent.Spec.WorkloadIdentity != nil && len(agent.Spec.AllowedSecrets) == 0 {
				agent.Spec.AllowedSecrets = []string{"region"}
			}
			r := &Reconciler{APIReader: reader, ObjectStoreAPI: discovery.Result{State: discovery.Available}}
			_, whyNot, err := r.resolve(context.Background(), agent)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case test.refusal == "" && whyNot != nil:
				t.Fatalf("refused: %+v", whyNot)
			case test.refusal != "" && (whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore || !strings.Contains(whyNot.message, test.refusal)):
				t.Fatalf("problem = %+v, want %q", whyNot, test.refusal)
			}
		})
	}
}

// TestResolvePgBackRestGatesOnTheProbeAndRecordsTheObject: a Cluster
// backed up by Opera's plugin records its Archive in status before the read
// (the Archive watch maps back through it), an unprobed Archive API is a
// problem, and the composition carries the split stanza.
func TestResolvePgBackRestGatesOnTheProbeAndRecordsTheObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "uid-1"},
		Spec: cnpgv1.ClusterSpec{Plugins: []cnpgv1.PluginConfiguration{{
			Name: "pgbackrest.cnpg.opera.com", Parameters: map[string]string{"pgbackrestObjectName": "orders-archive"},
		}}},
	}
	archive := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": map[string]any{
		"stanza": "orders-wal",
		"repositories": []any{map[string]any{
			"destinationPath": "/orders", "bucket": "backups", "endpointURL": "minio.storage.svc:9000",
			"s3Credentials": map[string]any{
				"region":          "us-east-1",
				"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
				"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
			},
		}},
	}}}}
	archive.SetGroupVersionKind(discovery.ArchiveGVK)
	archive.SetNamespace("team-a")
	archive.SetName("orders-archive")
	reader := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&cnpgv1.Cluster{}).WithObjects(cluster).Build()
	if err := reader.Create(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}, AllowedSecrets: []string{"minio"},
		},
	}

	// The Stanza API being usable does not stand in for the Archive's.
	r := &Reconciler{APIReader: reader, StanzaAPI: discovery.Result{State: discovery.Available}}
	_, whyNot, err := r.resolve(context.Background(), agent)
	if err != nil || whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonObjectStoreAPIUnavailable ||
		!strings.Contains(whyNot.message, "Archive API") {
		t.Fatalf("unprobed API: problem = %+v, err = %v", whyNot, err)
	}
	if status := agent.Status.PgBackRest; status == nil || status.Kind != "Archive" || status.ObjectName != "orders-archive" ||
		status.Plugin != "pgbackrest.cnpg.opera.com" || agent.Status.ObjectStoreName != "" {
		t.Fatalf("status.pgbackrest = %+v, objectStoreName = %q, want the Archive recorded before the read", status, agent.Status.ObjectStoreName)
	}

	r.ArchiveAPI = discovery.Result{State: discovery.Available}
	resolved, whyNot, err := r.resolve(context.Background(), agent)
	if err != nil || whyNot != nil {
		t.Fatalf("resolve = %+v, %v", whyNot, err)
	}
	if resolved.Format != "pgbackrest" || resolved.Stanza != "orders" || resolved.ArchiveStanza != "orders-wal" || resolved.Repository != 1 ||
		resolved.AgentDestination != "s3://backups/orders" || resolved.EndpointURL != "https://minio.storage.svc:9000" || resolved.ClusterUID != "uid-1" {
		t.Fatalf("composition = %+v", resolved)
	}
	if got := resolvedMessage(resolved); got != "Cluster orders, Archive orders-archive, repo1, stanza orders, WAL under stanza orders-wal" {
		t.Fatalf("message = %q", got)
	}

	// The author's endpoint pin: the Archive's
	// endpoint must be it.
	agent.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Endpoint: "minio.storage.svc:9000"}
	if _, whyNot, err := r.resolve(context.Background(), agent); err != nil || whyNot != nil {
		t.Fatalf("matching pin: problem = %+v, err = %v", whyNot, err)
	}
	agent.Spec.PgBackRest.Endpoint = "minio.other.svc:9000"
	if _, whyNot, _ := r.resolve(context.Background(), agent); whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore ||
		!strings.Contains(whyNot.message, "is not the pinned endpoint minio.other.svc:9000") {
		t.Fatalf("another pin: problem = %+v", whyNot)
	}
	agent.Spec.PgBackRest = nil

	// Barman's knobs on a pgBackRest Cluster: refused, not ignored.
	agent.Spec.ServerName = "orders"
	if _, whyNot, _ := r.resolve(context.Background(), agent); whyNot == nil || whyNot.reason != pgtoolboxv1alpha1.ReasonUnsupportedObjectStore {
		t.Fatalf("serverName on a pgBackRest Cluster: problem = %+v", whyNot)
	}
}

// TestPgBackRestWatchMapsBackThroughStatus: a Stanza event enqueues the
// agents whose status names that Stanza, and not those naming an Archive
// of the same name.
func TestPgBackRestWatchMapsBackThroughStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	named := func(name, kind string) *pgtoolboxv1alpha1.EvidenceAgent {
		agent := &pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}}
		if kind != "" {
			agent.Status.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestStatus{Kind: kind, ObjectName: "backups"}
		}
		return agent
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&pgtoolboxv1alpha1.EvidenceAgent{}).
		WithObjects(named("orders", "Stanza"), named("billing", "Archive"), named("barman", "")).
		WithIndex(&pgtoolboxv1alpha1.EvidenceAgent{}, indexPgBackRest, pgBackRestObjectOf).Build()
	r := &Reconciler{Client: c}
	stanza := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "backups", Namespace: "team-a"}}
	requests := r.agentsForPgBackRest("Stanza")(context.Background(), stanza)
	if len(requests) != 1 || requests[0].Name != "orders" {
		t.Fatalf("Stanza backups enqueued %v", requests)
	}
	if requests := r.agentsForPgBackRest("Archive")(context.Background(), stanza); len(requests) != 1 || requests[0].Name != "billing" {
		t.Fatalf("Archive backups enqueued %v", requests)
	}
}
