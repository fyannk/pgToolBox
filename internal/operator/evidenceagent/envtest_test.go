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
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/reconcile"
	"github.com/fyannk/pgToolBox/pkg/discovery"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// The operator identity the suite reconciles as: a user bound to exactly the
// generated manager ClusterRole (config/rbac/role.yaml). A missing RBAC
// marker — or a ClusterRole the API server's escalation check refuses to
// let the operator create or bind — fails here, not first in a cluster.
const operatorUser = "system:serviceaccount:pgtoolbox-system:pgtoolbox-operator"

type envtestSuite struct {
	admin      client.Client
	operator   client.Client
	recorder   *events.FakeRecorder
	reconciler *Reconciler
}

func newEnvtestSuite(t *testing.T) *envtestSuite {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set (make test-envtest-evidenceagent)")
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "config", "crd", "bases"),
			cnpgCRDs(),
			filepath.Join("testdata", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = environment.Stop() })

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatal(err)
	}
	role.ResourceVersion = ""
	must(t, admin.Create(ctx, role))
	must(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-operator"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: operatorUser}},
	}))
	impersonated := rest.CopyConfig(config)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: operatorUser, Groups: []string{"system:authenticated"}}
	operator, err := client.New(impersonated, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	objectStoreAPI, err := discovery.ProbeObjectStoreAPI(ctx, operator, nil)
	if err != nil || !objectStoreAPI.Usable() {
		t.Fatalf("ObjectStore probe = %+v, %v", objectStoreAPI, err)
	}
	// The generated ClusterRole must let the operator list both pgBackRest
	// plugins' objects, or their probes turn the feature off.
	stanzaAPI, err := discovery.ProbeStanzaAPI(ctx, operator, nil)
	if err != nil || !stanzaAPI.Usable() {
		t.Fatalf("Stanza probe = %+v, %v", stanzaAPI, err)
	}
	archiveAPI, err := discovery.ProbeArchiveAPI(ctx, operator, nil)
	if err != nil || !archiveAPI.Usable() {
		t.Fatalf("Archive probe = %+v, %v", archiveAPI, err)
	}
	recorder := events.NewFakeRecorder(256)
	return &envtestSuite{
		admin: admin, operator: operator, recorder: recorder,
		reconciler: &Reconciler{
			Client: operator, APIReader: operator, Recorder: recorder,
			DefaultImage:   "registry.example/pgtoolbox-evidence-agent@sha256:" + strings.Repeat("0", 64),
			ObjectStoreAPI: objectStoreAPI, StanzaAPI: stanzaAPI, ArchiveAPI: archiveAPI,
		},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// suiteNamespace is the namespace every EvidenceAgent of the suite lives in.
const suiteNamespace = "team-a"

func (s *envtestSuite) reconcile(ctx context.Context, t *testing.T, name string) ctrl.Result {
	t.Helper()
	result, err := s.reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: suiteNamespace, Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", suiteNamespace, name, err)
	}
	return result
}

func (s *envtestSuite) agent(ctx context.Context, t *testing.T, name string) *pgtoolboxv1alpha1.EvidenceAgent {
	t.Helper()
	agent := &pgtoolboxv1alpha1.EvidenceAgent{}
	must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: name}, agent))
	return agent
}

func (s *envtestSuite) seedRepository(t *testing.T, namespace string) {
	t.Helper()
	ctx := context.Background()
	must(t, s.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}))
	must(t, s.admin.Create(ctx, &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: namespace},
		Spec: cnpgv1.ClusterSpec{
			Instances:            1,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"},
			Plugins: []cnpgv1.PluginConfiguration{{
				Name: barmanPluginName, Parameters: map[string]string{barmanObjectParameter: "store", barmanServerParameter: "orders-2026"},
			}},
		},
	}))
	store := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"configuration": s3Configuration(nil)}}}
	store.SetGroupVersionKind(discovery.ObjectStoreGVK)
	store.SetNamespace(namespace)
	store.SetName("store")
	must(t, s.admin.Create(ctx, store))
	must(t, s.admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: namespace},
		StringData: map[string]string{"ACCESS_KEY_ID": "synthetic", "ACCESS_SECRET_KEY": "synthetic-canary"},
	}))
}

func TestEvidenceAgentReconcilerOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx := context.Background()
	s.seedRepository(t, "team-a")
	must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
	}))

	t.Run("first reconcile renders the agent and publishes its contract", func(t *testing.T) {
		result := s.reconcile(ctx, t, "orders")
		agent := s.agent(ctx, t, "orders")
		if result.RequeueAfter < 200*24*time.Hour {
			t.Errorf("RequeueAfter = %s, want the certificate renewal timer", result.RequeueAfter)
		}
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionWorkloadReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonRolloutInProgress)
		if agent.Status.Endpoint != "https://orders-evidence-agent.team-a.svc:8443" || agent.Status.ServerName != "orders-2026" ||
			agent.Status.ObjectStoreName != "store" || agent.Status.ProducerEpoch != 0 || agent.Status.ObservedGeneration != agent.Generation {
			t.Fatalf("status = %+v", agent.Status)
		}
		fingerprint, _ := evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
			Endpoint: "https://minio.storage.svc:9000", Bucket: "backups", Prefix: "team-a",
			Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders-2026",
		})
		if agent.Status.Fingerprint != fingerprint {
			t.Fatalf("fingerprint = %s, want %s", agent.Status.Fingerprint, fingerprint)
		}
		// The published bundle verifies the serving certificate for the
		// Service name the console dials.
		serving := &corev1.Secret{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "orders-evidence-agent-tls"}, serving))
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(agent.Status.CABundle) {
			t.Fatal("status.caBundle holds no certificate")
		}
		block, _ := pem.Decode(serving.Data[corev1.TLSCertKey])
		leaf, err := x509.ParseCertificate(block.Bytes)
		must(t, err)
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: evidencev1alpha1.ServerName("orders-evidence-agent", "team-a"), Roots: roots}); err != nil {
			t.Fatalf("serving certificate does not verify against status.caBundle: %v", err)
		}
		if strings.Contains(string(agent.Status.CABundle), "PRIVATE KEY") {
			t.Fatal("status carries key material")
		}
		for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.ServiceAccount{}, &corev1.Secret{}} {
			name := "orders-evidence-agent"
			if _, isSecret := object.(*corev1.Secret); isSecret {
				name = "orders-evidence-agent-ca"
			}
			must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: name}, object))
			if owner := metav1.GetControllerOf(object); owner == nil || owner.UID != agent.UID {
				t.Fatalf("%T %s is not controlled by the agent", object, name)
			}
			// One field manager for every child the operator writes, the
			// one the PgAdmin and PgToolBoxConsole controllers use.
			for _, entry := range object.GetManagedFields() {
				if entry.Manager != reconcile.FieldOwner {
					t.Fatalf("%T %s has field manager %q, want only %q", object, name, entry.Manager, reconcile.FieldOwner)
				}
			}
		}
		binding := &rbacv1.ClusterRoleBinding{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Name: ClusterRoleBindingName}, binding))
		if !slices.Contains(binding.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Namespace: "team-a", Name: "orders-evidence-agent"}) {
			t.Fatalf("binding subjects = %v", binding.Subjects)
		}
		events := drain(s.recorder)
		if !containsEvent(events, "CertificateIssued") {
			t.Fatalf("events = %v", events)
		}
		for _, event := range events {
			if strings.Contains(event, "synthetic-canary") || strings.Contains(event, "PRIVATE KEY") {
				t.Fatal("an Event carries secret material")
			}
		}
	})

	t.Run("a status endpoint rewritten by another manager is restored under the operator's", func(t *testing.T) {
		agent := s.agent(ctx, t, "orders")
		want := agent.Status.Endpoint
		original := agent.DeepCopy()
		agent.Status.Endpoint = "https://attacker.team-a.svc:8443"
		must(t, s.admin.Status().Patch(ctx, agent, client.MergeFrom(original), client.FieldOwner("kubectl-edit")))
		s.reconcile(ctx, t, "orders")
		restored := s.agent(ctx, t, "orders")
		if restored.Status.Endpoint != want {
			t.Fatalf("endpoint = %q, want the operator's %q back", restored.Status.Endpoint, want)
		}
		owners := map[string]bool{}
		for _, entry := range restored.ManagedFields {
			if entry.Subresource == "status" && strings.Contains(string(entry.FieldsV1.GetRawBytes()), `"f:endpoint"`) {
				owners[entry.Manager] = true
			}
		}
		if len(owners) != 1 || !owners[reconcile.StatusFieldOwner] {
			t.Fatalf("status.endpoint owners = %v, want only %s (the console trusts no other)", owners, reconcile.StatusFieldOwner)
		}
	})

	t.Run("a completed rollout publishes the epoch and Ready", func(t *testing.T) {
		deployment := &appsv1.Deployment{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "orders-evidence-agent"}, deployment))
		deployment.Status = appsv1.DeploymentStatus{
			ObservedGeneration: deployment.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
		}
		must(t, s.admin.Status().Update(ctx, deployment))
		s.reconcile(ctx, t, "orders")
		agent := s.agent(ctx, t, "orders")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionProgressing, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonAsExpected)
		if agent.Status.ProducerEpoch != agent.Generation {
			t.Fatalf("producerEpoch = %d, want %d", agent.Status.ProducerEpoch, agent.Generation)
		}
		if !containsEvent(drain(s.recorder), "Normal AsExpected") {
			t.Fatal("no Event for the transition to Ready")
		}
	})

	t.Run("a no-op reconcile writes nothing", func(t *testing.T) {
		before := resourceVersions(ctx, t, s.admin)
		s.reconcile(ctx, t, "orders")
		if after := resourceVersions(ctx, t, s.admin); !mapsEqual(before, after) {
			t.Fatalf("a no-op reconcile changed objects:\nbefore %v\nafter  %v", before, after)
		}
		if events := drain(s.recorder); len(events) != 0 {
			t.Fatalf("a no-op reconcile emitted %v", events)
		}
	})

	t.Run("a missing Cluster stalls with ClusterNotFound", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "ghost", Namespace: "team-a"},
			Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "ghost"}},
		}))
		s.reconcile(ctx, t, "ghost")
		agent := s.agent(ctx, t, "ghost")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonClusterNotFound)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionStalled, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonClusterNotFound)
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonClusterNotFound)
		if err := s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ghost-evidence-agent"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Fatalf("a Deployment was rendered for an invalid configuration: %v", err)
		}
		drain(s.recorder)
	})

	t.Run("a foreign object under the generated name is not taken over", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "billing-evidence-agent", Namespace: "team-a"},
			Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80}}},
		}))
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "billing", Namespace: "team-a"},
			Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
		}))
		s.reconcile(ctx, t, "billing")
		agent := s.agent(ctx, t, "billing")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
		service := &corev1.Service{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "billing-evidence-agent"}, service))
		if service.Spec.Ports[0].Port != 80 || metav1.GetControllerOf(service) != nil {
			t.Fatal("the foreign Service was modified")
		}
		drain(s.recorder)
	})

	t.Run("a foreign ServiceAccount under the generated name is never bound", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "intruder-evidence-agent", Namespace: "team-a"}}))
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "intruder", Namespace: "team-a"},
			Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
		}))
		s.reconcile(ctx, t, "intruder")
		assertCondition(t, s.agent(ctx, t, "intruder"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonResourceNotOwned)
		s.reconcile(ctx, t, "orders")
		binding := &rbacv1.ClusterRoleBinding{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Name: ClusterRoleBindingName}, binding))
		for _, subject := range binding.Subjects {
			if subject.Name == "intruder-evidence-agent" {
				t.Fatal("a ServiceAccount the agent does not control was granted the reviews")
			}
		}
		drain(s.recorder)
	})

	t.Run("an image outside the allow-list stalls", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "exfil", Namespace: "team-a"},
			Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
				ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"},
				Image:      &pgtoolboxv1alpha1.ImageSpec{Repository: "attacker.example/exfil", Tag: "latest"},
			},
		}))
		s.reconcile(ctx, t, "exfil")
		assertCondition(t, s.agent(ctx, t, "exfil"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonImageNotAllowed)
		if err := s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "exfil-evidence-agent"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Fatalf("a Pod with the store credentials was rendered for a foreign image: %v", err)
		}
		drain(s.recorder)
	})

	t.Run("a deleted agent leaves the shared binding", func(t *testing.T) {
		must(t, s.admin.Delete(ctx, s.agent(ctx, t, "orders")))
		s.reconcile(ctx, t, "orders")
		binding := &rbacv1.ClusterRoleBinding{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Name: ClusterRoleBindingName}, binding))
		names := make([]string, 0, len(binding.Subjects))
		for _, subject := range binding.Subjects {
			names = append(names, subject.Namespace+"/"+subject.Name)
		}
		if slices.Contains(names, "team-a/orders-evidence-agent") || !slices.Contains(names, "team-a/billing-evidence-agent") {
			t.Fatalf("binding subjects after deletion = %v", names)
		}
	})

	t.Run("the OpenShift service CA path waits for, then publishes, the injected bundle", func(t *testing.T) {
		must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "ocp", Namespace: "team-a"},
			Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
				ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"},
				TLS:        &pgtoolboxv1alpha1.EvidenceAgentTLSSpec{Issuer: pgtoolboxv1alpha1.CertificateIssuerOpenShiftServiceCA},
			},
		}))
		if result := s.reconcile(ctx, t, "ocp"); result.RequeueAfter != 0 {
			t.Fatal("the service CA path set a renewal timer; renewal is the service CA's")
		}
		agent := s.agent(ctx, t, "ocp")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending)
		service := &corev1.Service{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ocp-evidence-agent"}, service))
		if service.Annotations[annotationServingCertSecret] != "ocp-evidence-agent-service-tls" {
			t.Fatalf("Service annotations = %v", service.Annotations)
		}
		deployment := &appsv1.Deployment{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ocp-evidence-agent"}, deployment))
		if deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != "ocp-evidence-agent-service-tls" {
			t.Fatal("the agent does not mount the service CA's Secret")
		}

		// What the service CA operator does: a serving certificate for the
		// Service, and its CA injected into the ConfigMap.
		ca := mustCA(t, time.Now())
		leaf, err := issueLeaf(ca, servingDNSNames("ocp-evidence-agent", "team-a"), time.Now())
		must(t, err)
		must(t, s.admin.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ocp-evidence-agent-service-tls", Namespace: "team-a"},
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{corev1.TLSCertKey: leaf.Cert, corev1.TLSPrivateKeyKey: leaf.Key},
		}))
		// A namespace editor's forged bundle (another CA) is not published.
		configMap := &corev1.ConfigMap{}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ocp-evidence-agent-ca"}, configMap))
		configMap.Data = map[string]string{serviceCABundleKey: string(mustCA(t, time.Now()).Cert)}
		must(t, s.admin.Update(ctx, configMap))
		s.reconcile(ctx, t, "ocp")
		agent = s.agent(ctx, t, "ocp")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonServiceCAPending)
		if len(agent.Status.CABundle) != 0 {
			t.Fatal("a bundle that does not verify the serving certificate was published")
		}
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ocp-evidence-agent-ca"}, configMap))
		configMap.Data = map[string]string{serviceCABundleKey: string(ca.Cert)}
		must(t, s.admin.Update(ctx, configMap))
		s.reconcile(ctx, t, "ocp")
		// Re-applying the ConfigMap must not take the injected data away.
		must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: "ocp-evidence-agent-ca"}, configMap))
		agent = s.agent(ctx, t, "ocp")
		assertCondition(t, agent, pgtoolboxv1alpha1.ConditionServingCertificateReady, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
		if string(agent.Status.CABundle) != string(ca.Cert) || configMap.Data[serviceCABundleKey] == "" {
			t.Fatal("the injected bundle was not published or was removed")
		}
	})
}

func assertCondition(t *testing.T, agent *pgtoolboxv1alpha1.EvidenceAgent, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) {
	t.Helper()
	got := condition(agent, conditionType)
	if got.Status != status || got.Reason != reason.String() {
		t.Fatalf("%s = %s/%s (%s), want %s/%s", conditionType, got.Status, got.Reason, got.Message, status, reason)
	}
}

func drain(recorder *events.FakeRecorder) []string {
	var collected []string
	for {
		select {
		case event := <-recorder.Events:
			collected = append(collected, event)
		default:
			return collected
		}
	}
}

func containsEvent(events []string, fragment string) bool {
	return slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, fragment) })
}

func resourceVersions(ctx context.Context, t *testing.T, c client.Client) map[string]string {
	t.Helper()
	versions := map[string]string{}
	for _, object := range []client.Object{
		&pgtoolboxv1alpha1.EvidenceAgent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence-agent"}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence-agent"}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence-agent"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence-agent-tls"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders-evidence-agent-ca"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleName}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleBindingName}},
	} {
		must(t, c.Get(ctx, client.ObjectKeyFromObject(object), object))
		versions[typeName(object)+"/"+object.GetName()] = object.GetResourceVersion()
	}
	return versions
}

func typeName(object client.Object) string {
	switch object.(type) {
	case *corev1.Secret:
		return "Secret"
	case *corev1.Service:
		return "Service"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	case *appsv1.Deployment:
		return "Deployment"
	case *rbacv1.ClusterRole:
		return "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		return "ClusterRoleBinding"
	default:
		return "EvidenceAgent"
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// cnpgCRDs is the vendored CloudNativePG CRD directory the suite runs
// against: CNPG_CRDS (one test/crds/cnpg-<minor>, as the console's envtest
// target sets it per minor) or the newest vendored minor.
func cnpgCRDs() string {
	if dir := os.Getenv("CNPG_CRDS"); dir != "" {
		return dir
	}
	return filepath.Join("..", "..", "..", "test", "crds", "cnpg-1.30")
}

// TestPgBackRestEvidenceAgentOnEnvtest: a Cluster backed up by Dalibo's
// plugin renders an agent for its Stanza's selected repository, with the
// cipher pass projected as a file, as the generated ClusterRole. A
// repository number past the Stanza's stalls.
func TestPgBackRestEvidenceAgentOnEnvtest(t *testing.T) {
	s := newEnvtestSuite(t)
	ctx := context.Background()
	must(t, s.admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: suiteNamespace}}))
	must(t, s.admin.Create(ctx, &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: suiteNamespace},
		Spec: cnpgv1.ClusterSpec{
			Instances:            1,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"},
			Plugins: []cnpgv1.PluginConfiguration{{
				Name: "pgbackrest.dalibo.com", Parameters: map[string]string{"stanzaRef": "orders-stanza"},
			}},
		},
	}))
	keys := map[string]any{
		"accessKeyId":     map[string]any{"name": "minio", "key": "ACCESS_KEY_ID"},
		"secretAccessKey": map[string]any{"name": "minio", "key": "ACCESS_SECRET_KEY"},
	}
	stanza := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"stanzaConfiguration": map[string]any{
		"name": "main",
		"s3Repositories": []any{map[string]any{
			"bucket": "bucket-01", "endpoint": "https://minio.storage.svc:9000", "region": "fr-par", "repoPath": "/repo-01",
			"uriStyle": "path", "secretRef": keys,
			"cipherConfig": map[string]any{"type": "aes-256-cbc", "encryptionPass": map[string]any{"name": "orders-cipher", "key": "pass"}},
		}},
	}}}}
	stanza.SetGroupVersionKind(discovery.StanzaGVK)
	stanza.SetNamespace(suiteNamespace)
	stanza.SetName("orders-stanza")
	must(t, s.admin.Create(ctx, stanza))
	must(t, s.admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio", Namespace: suiteNamespace},
		StringData: map[string]string{"ACCESS_KEY_ID": "synthetic", "ACCESS_SECRET_KEY": "synthetic-canary"},
	}))
	must(t, s.admin.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-cipher", Namespace: suiteNamespace},
		StringData: map[string]string{"pass": "cipher-canary"},
	}))
	must(t, s.admin.Create(ctx, &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: suiteNamespace},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef:     pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"},
			AllowedSecrets: []string{"minio", "orders-cipher"},
		},
	}))

	s.reconcile(ctx, t, "orders")
	agent := s.agent(ctx, t, "orders")
	assertCondition(t, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected)
	want := &pgtoolboxv1alpha1.EvidenceAgentPgBackRestStatus{
		Plugin: "pgbackrest.dalibo.com", Kind: "Stanza", ObjectName: "orders-stanza", Stanza: "main", Repository: 1,
	}
	if got := agent.Status.PgBackRest; got == nil || *got != *want || agent.Status.ObjectStoreName != "" || agent.Status.ServerName != "" {
		t.Fatalf("status.pgbackrest = %+v, objectStoreName = %q, serverName = %q", got, agent.Status.ObjectStoreName, agent.Status.ServerName)
	}
	deployment := &appsv1.Deployment{}
	must(t, s.admin.Get(ctx, client.ObjectKey{Namespace: suiteNamespace, Name: "orders-evidence-agent"}, deployment))
	environment := map[string]string{}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	for name, value := range map[string]string{
		"REPOSITORY_FORMAT": "pgbackrest", "PROVIDER": "s3", "DESTINATION_PATH": "s3://bucket-01/repo-01",
		"PGBACKREST_STANZA": "main", "PGBACKREST_REPOSITORY": "1", "ENDPOINT_URL": "https://minio.storage.svc:9000",
		"AWS_REGION": "fr-par", "PGBACKREST_CIPHER_PASS_FILE": "/var/run/secrets/pgtoolbox.fyannk.dev/store/cipher-pass",
	} {
		if environment[name] != value {
			t.Errorf("%s = %q, want %q", name, environment[name], value)
		}
	}
	for _, absent := range []string{"BARMAN_SERVER_NAMES", "PGBACKREST_ARCHIVE_STANZA"} {
		if _, present := environment[absent]; present {
			t.Errorf("%s is rendered for a single-stanza pgBackRest repository", absent)
		}
	}
	rendered, err := yaml.Marshal(deployment)
	must(t, err)
	for _, event := range drain(s.recorder) {
		if strings.Contains(event, "canary") {
			t.Fatal("an Event carries secret material")
		}
	}
	if strings.Contains(string(rendered), "canary") {
		t.Fatal("the Deployment carries secret material")
	}

	agent.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Repository: 2}
	must(t, s.admin.Update(ctx, agent))
	s.reconcile(ctx, t, "orders")
	assertCondition(t, s.agent(ctx, t, "orders"), pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore)
}
