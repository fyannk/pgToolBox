//go:build e2e

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

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

var evidenceAgentImage = flag.String("evidence-agent-image", "", "evidence agent image built from this tree and side-loaded; the operator runs it as its default")

const (
	agentClusterName = "pg-evidence"
	agentStoreName   = "evidence-store"
	agentName        = "pg-evidence"
	consoleAccount   = "evidence-console"
	strangerAccount  = "evidence-stranger"
)

// TestEvidenceAgent is the EvidenceAgent scenario: an EvidenceAgent is published on
// a real cluster, and a console-style client — a bound ServiceAccount token
// for the agent's namespace audience, the CA from status.caBundle, the
// Service name — reads the channel; a token for the API server's audience
// is refused, a token for another namespace's or the retired cluster-wide
// audience is refused as an audience mismatch, and a valid token whose
// subject may not get the EvidenceAgent is forbidden.
func TestEvidenceAgent(t *testing.T) {
	if *evidenceAgentImage == "" {
		t.Skip("-evidence-agent-image not set")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	createStoreCredentials(t)
	createEvidenceObjectStore(t)
	createEvidenceCluster(t, c)
	// This agent deliberately leaves spec.allowedSecrets empty: that is the
	// deprecated behaviour (every Secret the plugin's ObjectStore names, and
	// a plain-http endpoint accepted), still supported for an ObjectStore, so
	// the happy path keeps covering it. TestEvidenceAgentPolicy covers the
	// agent author's allow-list.
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef:      pgtoolboxv1alpha1.LocalObjectReference{Name: agentClusterName},
			RefreshInterval: &metav1.Duration{Duration: 30 * time.Second},
		},
	}
	if err := c.Create(ctx(), agent); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create EvidenceAgent: %v", err)
	}

	eventually(t, readyTimeout, "EvidenceAgent Ready", func() error {
		if err := c.Get(ctx(), client.ObjectKeyFromObject(agent), agent); err != nil {
			return err
		}
		ready := meta.FindStatusCondition(agent.Status.Conditions, pgtoolboxv1alpha1.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue {
			return fmt.Errorf("Ready = %+v", ready)
		}
		return nil
	})
	if agent.Status.Endpoint != evidencev1alpha1.Endpoint(agentName+"-evidence-agent", testNamespace) ||
		agent.Status.Image != *evidenceAgentImage || agent.Status.ProducerEpoch != agent.Generation || len(agent.Status.CABundle) == 0 || agent.Status.Fingerprint == "" {
		t.Fatalf("status = %+v", agent.Status)
	}

	// The console side: a ServiceAccount allowed to get the EvidenceAgent,
	// and one that is not.
	grantConsole(t, c)
	// The agents of testNamespace accept only this namespace's audience, so a
	// token for one namespace's agents opens no other's.
	audience, err := evidencev1alpha1.TokenAudience(testNamespace)
	if err != nil {
		t.Fatal(err)
	}
	otherAudience, err := evidencev1alpha1.TokenAudience("another-team")
	if err != nil {
		t.Fatal(err)
	}
	consoleToken := boundToken(t, consoleAccount, audience)
	apiServerToken := boundToken(t, consoleAccount, "")
	strangerToken := boundToken(t, strangerAccount, audience)
	otherNamespaceToken := boundToken(t, consoleAccount, otherAudience)
	clusterWideToken := boundToken(t, consoleAccount, evidencev1alpha1.ClusterWideTokenAudience)

	address, stop := forwardToAgent(t)
	defer stop()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(agent.Status.CABundle) {
		t.Fatal("status.caBundle holds no certificate")
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, ServerName: evidencev1alpha1.ServerName(agentName+"-evidence-agent", testNamespace), MinVersion: tls.VersionTLS13,
	}}}
	get := func(path, token string) (int, []byte, error) {
		request, err := http.NewRequestWithContext(ctx(), http.MethodGet, "https://"+address+path, nil)
		if err != nil {
			return 0, nil, err
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, evidencev1alpha1.MaximumSnapshotBytes+1))
		return response.StatusCode, body, err
	}

	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	eventually(t, 2*time.Minute, "an authorized snapshot", func() error {
		status, body, err := get(evidencev1alpha1.SnapshotPath, consoleToken)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("snapshot = %d %s", status, body)
		}
		if err := json.Unmarshal(body, &snapshot); err != nil {
			return err
		}
		return snapshot.Validate()
	})
	if snapshot.ProducerEpoch != uint64(agent.Status.ProducerEpoch) ||
		snapshot.Identity.Repository.DestinationFingerprint != agent.Status.Fingerprint ||
		snapshot.Identity.Cluster.Namespace != testNamespace {
		t.Fatalf("snapshot does not match the EvidenceAgent status: epoch %d/%d fingerprint %s/%s",
			snapshot.ProducerEpoch, agent.Status.ProducerEpoch, snapshot.Identity.Repository.DestinationFingerprint, agent.Status.Fingerprint)
	}

	for _, refusal := range []struct {
		name, token string
		status      int
		code        evidencev1alpha1.ErrorCode
	}{
		{"no token", "", http.StatusUnauthorized, evidencev1alpha1.ErrorUnauthenticated},
		{"API-server audience", apiServerToken, http.StatusUnauthorized, evidencev1alpha1.ErrorUnauthenticated},
		{"subject without get evidenceagents", strangerToken, http.StatusForbidden, evidencev1alpha1.ErrorForbidden},
		{"another namespace's audience", otherNamespaceToken, http.StatusUnauthorized, evidencev1alpha1.ErrorAudienceMismatch},
		{"the bare cluster-wide audience", clusterWideToken, http.StatusUnauthorized, evidencev1alpha1.ErrorAudienceMismatch},
	} {
		status, body, err := get(evidencev1alpha1.SnapshotPath, refusal.token)
		if err != nil {
			t.Fatalf("%s: %v", refusal.name, err)
		}
		var apiError evidencev1alpha1.EvidenceAPIError
		if status != refusal.status || json.Unmarshal(body, &apiError) != nil || apiError.Code != refusal.code {
			t.Fatalf("%s: %d %s", refusal.name, status, body)
		}
	}
}

const (
	policyStoreName     = "evidence-policy-store"
	policyAgentName     = "pg-policy"
	otherCredentials    = "other-credentials"
	otherSecretSentinel = "sentinel-value-that-must-never-be-published"
	inTreeClusterName   = "pg-intree"
	inTreeAgentName     = "pg-intree-agent"
)

// TestEvidenceAgentPolicy is what the agent's author allows (spec.allowedSecrets,
// spec.allowInsecureEndpoint), enforced against the live Barman configuration
// that others edit: a configuration outside the allow-list is refused with
// ConfigurationValid=False / UnsupportedObjectStore, the message names the
// Secret and no value, and no agent Deployment ever mounts the unlisted Secret.
func TestEvidenceAgentPolicy(t *testing.T) {
	if *evidenceAgentImage == "" {
		t.Skip("-evidence-agent-image not set")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, cnpgv1.AddToScheme, pgtoolboxv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	createStoreCredentials(t)
	createEvidenceObjectStore(t)
	createEvidenceCluster(t, c)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: otherCredentials, Namespace: testNamespace},
		StringData: map[string]string{"ACCESS_KEY_ID": otherSecretSentinel, "ACCESS_SECRET_KEY": otherSecretSentinel},
	}
	if err := c.Create(ctx(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s: %v", otherCredentials, err)
	}

	// A second ObjectStore the agent is pointed to, edited later by "someone
	// else" (whoever may write ObjectStores).
	policyStore := newEvidenceObjectStore(policyStoreName, "store-credentials")
	if err := k8s.Create(ctx(), policyStore); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ObjectStore: %v", err)
	}
	agent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: policyAgentName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef:            pgtoolboxv1alpha1.LocalObjectReference{Name: agentClusterName},
			ObjectStoreRef:        &pgtoolboxv1alpha1.LocalObjectReference{Name: policyStoreName},
			RefreshInterval:       &metav1.Duration{Duration: 30 * time.Second},
			AllowedSecrets:        []string{"store-credentials"},
			AllowInsecureEndpoint: true, // the test MinIO speaks http
		},
	}
	if err := c.Create(ctx(), agent); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create EvidenceAgent: %v", err)
	}
	waitCondition(t, c, agent, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionTrue, "")

	// (a) The ObjectStore now names a Secret the author did not list.
	updateStoreSecret(t, policyStoreName, otherCredentials)
	refused := waitCondition(t, c, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore)
	requireNamesSecretOnly(t, refused.Message, otherCredentials)
	requireNoDeploymentMounts(t, c, otherCredentials)

	// Back to the listed Secret: the agent recovers by itself.
	updateStoreSecret(t, policyStoreName, "store-credentials")
	waitCondition(t, c, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionTrue, "")
	waitCondition(t, c, agent, pgtoolboxv1alpha1.ConditionReady, metav1.ConditionTrue, "")

	// (b) The same agent without allowInsecureEndpoint: the store's endpoint
	// is http.
	if err := c.Get(ctx(), client.ObjectKeyFromObject(agent), agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.AllowInsecureEndpoint = false
	if err := c.Update(ctx(), agent); err != nil {
		t.Fatalf("update EvidenceAgent: %v", err)
	}
	refused = waitCondition(t, c, agent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore)
	requireNamesSecretOnly(t, refused.Message, "")
	if !strings.Contains(refused.Message, "plain-http") {
		t.Fatalf("the refusal does not name the http endpoint: %q", refused.Message)
	}
	requireNoDeploymentMounts(t, c, otherCredentials)

	// (c) A Cluster with the in-tree spec.backup.barmanObjectStore is never
	// read by an agent without an allow-list.
	inTree := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: inTreeClusterName, Namespace: testNamespace},
		Spec: cnpgv1.ClusterSpec{
			Instances:            1,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "256Mi"},
			Backup: &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{
				DestinationPath: "s3://e2e-backups/" + inTreeClusterName,
				EndpointURL:     "http://minio.minio.svc:9000",
				BarmanCredentials: cnpgv1.BarmanCredentials{AWS: &cnpgv1.S3Credentials{
					AccessKeyIDReference:     &cnpgv1.SecretKeySelector{LocalObjectReference: cnpgv1.LocalObjectReference{Name: "store-credentials"}, Key: "ACCESS_KEY_ID"},
					SecretAccessKeyReference: &cnpgv1.SecretKeySelector{LocalObjectReference: cnpgv1.LocalObjectReference{Name: "store-credentials"}, Key: "ACCESS_SECRET_KEY"},
				}},
			}},
		},
	}
	if err := c.Create(ctx(), inTree); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create in-tree Cluster: %v", err)
	}
	inTreeAgent := &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: inTreeAgentName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef:      pgtoolboxv1alpha1.LocalObjectReference{Name: inTreeClusterName},
			RefreshInterval: &metav1.Duration{Duration: 30 * time.Second},
			// No allowedSecrets, and allowInsecureEndpoint changes nothing.
			AllowInsecureEndpoint: true,
		},
	}
	if err := c.Create(ctx(), inTreeAgent); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create in-tree EvidenceAgent: %v", err)
	}
	refused = waitCondition(t, c, inTreeAgent, pgtoolboxv1alpha1.ConditionConfigurationValid, metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonUnsupportedObjectStore)
	requireNamesSecretOnly(t, refused.Message, "store-credentials")
	requireNoDeploymentMounts(t, c, otherCredentials)
	requireNoAgentDeployment(t, c, inTreeAgentName)
}

// waitCondition waits until the agent's condition has the status (and the
// reason, when given) and returns it. It reads the agent afresh each time.
func waitCondition(t *testing.T, c client.Client, agent *pgtoolboxv1alpha1.EvidenceAgent, conditionType string, status metav1.ConditionStatus, reason pgtoolboxv1alpha1.Reason) metav1.Condition {
	t.Helper()
	var found metav1.Condition
	eventually(t, readyTimeout, fmt.Sprintf("%s %s=%s %s", agent.Name, conditionType, status, reason), func() error {
		if err := c.Get(ctx(), client.ObjectKeyFromObject(agent), agent); err != nil {
			return err
		}
		condition := meta.FindStatusCondition(agent.Status.Conditions, conditionType)
		if condition == nil || condition.Status != status || (reason != "" && condition.Reason != string(reason)) ||
			condition.ObservedGeneration != agent.Generation {
			return fmt.Errorf("%s = %+v (generation %d)", conditionType, condition, agent.Generation)
		}
		found = *condition
		return nil
	})
	return found
}

// requireNamesSecretOnly: the refusal message names the Secret (when given)
// and carries no credential value, neither the listed Secret's nor the
// unlisted one's.
func requireNamesSecretOnly(t *testing.T, message, secret string) {
	t.Helper()
	if secret != "" && !strings.Contains(message, secret) {
		t.Fatalf("the refusal does not name Secret %s: %q", secret, message)
	}
	for _, value := range []string{otherSecretSentinel, "e2eaccesskey", "e2esecretkey"} {
		if strings.Contains(message, value) {
			t.Fatalf("the refusal publishes a Secret value %q: %q", value, message)
		}
	}
}

// requireNoDeploymentMounts: no Deployment of the namespace reads the Secret
// through a volume, a projected volume, an environment variable or envFrom.
func requireNoDeploymentMounts(t *testing.T, c client.Client, secret string) {
	t.Helper()
	var deployments appsv1.DeploymentList
	if err := c.List(ctx(), &deployments, client.InNamespace(testNamespace)); err != nil {
		t.Fatal(err)
	}
	for i := range deployments.Items {
		spec := deployments.Items[i].Spec.Template.Spec
		for _, volume := range spec.Volumes {
			if volume.Secret != nil && volume.Secret.SecretName == secret {
				t.Fatalf("Deployment %s mounts Secret %s", deployments.Items[i].Name, secret)
			}
			if volume.Projected != nil {
				for _, source := range volume.Projected.Sources {
					if source.Secret != nil && source.Secret.Name == secret {
						t.Fatalf("Deployment %s projects Secret %s", deployments.Items[i].Name, secret)
					}
				}
			}
		}
		for _, container := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			for _, env := range container.Env {
				if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == secret {
					t.Fatalf("Deployment %s reads Secret %s in %s", deployments.Items[i].Name, secret, env.Name)
				}
			}
			for _, from := range container.EnvFrom {
				if from.SecretRef != nil && from.SecretRef.Name == secret {
					t.Fatalf("Deployment %s reads Secret %s through envFrom", deployments.Items[i].Name, secret)
				}
			}
		}
	}
}

// requireNoAgentDeployment: a refused agent that never resolved has no workload.
func requireNoAgentDeployment(t *testing.T, c client.Client, agentName string) {
	t.Helper()
	var deployments appsv1.DeploymentList
	if err := c.List(ctx(), &deployments, client.InNamespace(testNamespace),
		client.MatchingLabels{"app.kubernetes.io/instance": agentName}); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 0 {
		t.Fatalf("the refused agent %s has %d Deployment(s)", agentName, len(deployments.Items))
	}
}

// updateStoreSecret points the ObjectStore's credentials at another Secret.
func updateStoreSecret(t *testing.T, store, secret string) {
	t.Helper()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(objectStoreGVK)
	if err := k8s.Get(ctx(), client.ObjectKey{Namespace: testNamespace, Name: store}, live); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"accessKeyId", "secretAccessKey"} {
		if err := unstructured.SetNestedField(live.Object, secret, "spec", "configuration", "s3Credentials", field, "name"); err != nil {
			t.Fatal(err)
		}
	}
	if err := k8s.Update(ctx(), live); err != nil {
		t.Fatalf("update ObjectStore %s: %v", store, err)
	}
}

func createEvidenceObjectStore(t *testing.T) {
	t.Helper()
	if err := k8s.Create(ctx(), newEvidenceObjectStore(agentStoreName, "store-credentials")); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ObjectStore: %v", err)
	}
}

// newEvidenceObjectStore is a barman-cloud ObjectStore on the test MinIO
// (plain http), reading its keys from the named Secret.
func newEvidenceObjectStore(name, credentials string) *unstructured.Unstructured {
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(objectStoreGVK)
	store.SetName(name)
	store.SetNamespace(testNamespace)
	store.Object["spec"] = map[string]any{"configuration": map[string]any{
		"destinationPath": "s3://e2e-backups/" + agentClusterName,
		"endpointURL":     "http://minio.minio.svc:9000",
		"s3Credentials": map[string]any{
			"accessKeyId":     map[string]any{"name": credentials, "key": "ACCESS_KEY_ID"},
			"secretAccessKey": map[string]any{"name": credentials, "key": "ACCESS_SECRET_KEY"},
		},
	}}
	return store
}

func createEvidenceCluster(t *testing.T, c client.Client) {
	t.Helper()
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: agentClusterName, Namespace: testNamespace},
		Spec: cnpgv1.ClusterSpec{
			Instances:            1,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "256Mi"},
			Plugins: []cnpgv1.PluginConfiguration{{
				Name: "barman-cloud.cloudnative-pg.io", Enabled: new(true),
				Parameters: map[string]string{"barmanObjectName": agentStoreName},
			}},
		},
	}
	if err := c.Create(ctx(), cluster); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create Cluster: %v", err)
	}
}

// grantConsole creates the two ServiceAccounts and lets only the console one
// get EvidenceAgents, as the console chart's Role does.
func grantConsole(t *testing.T, c client.Client) {
	t.Helper()
	for _, object := range []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: consoleAccount, Namespace: testNamespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: strangerAccount, Namespace: testNamespace}},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: consoleAccount, Namespace: testNamespace},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{evidencev1alpha1.AuthorizationGroup}, Resources: []string{evidencev1alpha1.AuthorizationResource},
				Verbs: []string{"get", "list", "watch"},
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: consoleAccount, Namespace: testNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: consoleAccount},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: consoleAccount, Namespace: testNamespace}},
		},
	} {
		if err := c.Create(ctx(), object); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %T: %v", object, err)
		}
	}
}

// boundToken mints what the kubelet projects into a console Pod: a bound
// ServiceAccount token, for audience (empty: the API server's default).
func boundToken(t *testing.T, account, audience string) string {
	t.Helper()
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: new(int64(600))}}
	if audience != "" {
		request.Spec.Audiences = []string{audience}
	}
	response, err := clientset.CoreV1().ServiceAccounts(testNamespace).CreateToken(ctx(), account, request, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token for %s: %v", account, err)
	}
	return response.Status.Token
}

// forwardToAgent port-forwards a local port to the agent Pod's HTTPS port.
// TLS still terminates in the agent and is verified with the Service name.
func forwardToAgent(t *testing.T) (string, func()) {
	t.Helper()
	pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=evidence-agent,app.kubernetes.io/instance=" + agentName,
	})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("agent Pod: %v (%d found)", err, len(pods.Items))
	}
	pod := pods.Items[0]
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward").URL().String())
	if err != nil {
		t.Fatal(err)
	}
	stop, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target),
		[]string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", evidencev1alpha1.AgentPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		t.Fatalf("port-forward: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal(errors.New("port-forward did not become ready"))
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("forwarded ports: %v", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), func() { close(stop) }
}

// createStoreCredentials creates the Secret the ObjectStore names for MinIO's
// access keys (test/e2e/testdata/minio.yaml).
func createStoreCredentials(t *testing.T) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "store-credentials", Namespace: testNamespace},
		StringData: map[string]string{
			"ACCESS_KEY_ID":     "e2eaccesskey",
			"ACCESS_SECRET_KEY": "e2esecretkey",
		},
	}
	if err := k8s.Create(ctx(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create store credentials: %v", err)
	}
}

// objectStoreGVK is the Barman Cloud Plugin's ObjectStore, read unstructured:
// the plugin's types are not a dependency of this repository.
var objectStoreGVK = schema.GroupVersionKind{
	Group:   "barmancloud.cnpg.io",
	Version: "v1",
	Kind:    "ObjectStore",
}
