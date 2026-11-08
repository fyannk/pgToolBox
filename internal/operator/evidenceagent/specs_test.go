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
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/backupresolve"
)

var updateGolden = flag.Bool("update", false, "rewrite the rendered-object golden files")

func testAgent() *pgtoolboxv1alpha1.EvidenceAgent {
	return &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a", UID: "4d7c6c1e-8a51-4c43-9a0e-2b1d5f6e7a80", Generation: 3},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{
			ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"},
		},
	}
}

func testComposition() composition {
	return composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191",
		ObjectStoreName: "orders-store", ServerName: "orders",
		Provider: "s3", DestinationPath: "s3://backups/team-a", AgentDestination: "s3://backups/team-a",
		EndpointURL:     "https://minio.storage.svc:9000",
		EndpointCA:      &secretKeyRef{Name: "minio-ca", Key: "ca.crt"},
		Credential:      backupresolve.CredentialStaticKeys,
		AccessKeyID:     &secretKeyRef{Name: "minio-credentials", Key: "ACCESS_KEY_ID"},
		SecretAccessKey: &secretKeyRef{Name: "minio-credentials", Key: "ACCESS_SECRET_KEY"},
		Region:          regionSource{SecretRef: &secretKeyRef{Name: "minio-region", Key: "REGION"}},
		Fingerprint:     "sha256:" + strings.Repeat("ab", 32),
	}
}

func testInputs(agent *pgtoolboxv1alpha1.EvidenceAgent) renderInputs {
	return renderInputs{
		Agent:             agent,
		Image:             resolveImage(nil, "registry.example/pgtoolbox-evidence-agent@sha256:"+strings.Repeat("0", 64)),
		Composition:       testComposition(),
		ServingSecretName: agent.EffectiveServingSecretName(),
	}
}

func assertGolden(t *testing.T, name string, object any) {
	t.Helper()
	rendered, err := yaml.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name+".golden.yaml")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, rendered, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatalf("%v (regenerate with go test ./internal/operator/evidenceagent -run TestRenderedObjectsMatchGoldens -update)", err)
	}
	if !bytes.Equal(want, rendered) {
		t.Fatalf("%s changed; review and regenerate with -update\n--- want\n%s\n--- got\n%s", name, want, rendered)
	}
}

func TestRenderedObjectsMatchGoldens(t *testing.T) {
	agent := testAgent()
	assertGolden(t, "deployment", buildDeployment(testInputs(agent)))
	assertGolden(t, "service", buildService(agent, agent.EffectiveServingSecretName()))
	assertGolden(t, "serviceaccount", buildServiceAccount(agent))
	assertGolden(t, "clusterrole", buildClusterRole())
	assertGolden(t, "clusterrolebinding", buildClusterRoleBinding([]agentSubject{
		{Namespace: "team-b", Name: "billing-evidence-agent"},
		{Namespace: "team-a", Name: "orders-evidence-agent"},
		{Namespace: "team-a", Name: "orders-evidence-agent"},
	}))

	serviceCA := testAgent()
	serviceCA.Spec.TLS = &pgtoolboxv1alpha1.EvidenceAgentTLSSpec{Issuer: pgtoolboxv1alpha1.CertificateIssuerOpenShiftServiceCA}
	assertGolden(t, "service-openshift-service-ca", buildService(serviceCA, serviceCA.EffectiveServingSecretName()))

	azure := testInputs(agent)
	azure.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders",
		Provider: "azure", DestinationPath: "http://azurite.storage.svc:10000/devstoreaccount1/backups/team-a",
		AgentDestination: "azure://backups/team-a", EndpointURL: "http://azurite.storage.svc:10000", AzureAccount: "devstoreaccount1",
		Credential: backupresolve.CredentialConnectionString, AzureSecret: &secretKeyRef{Name: "azurite", Key: "AZURE_CONNECTION_STRING"},
		Fingerprint: "sha256:" + strings.Repeat("cd", 32),
	}
	assertGolden(t, "deployment-azure", buildDeployment(azure))
	// Azurite over TLS with a private CA: endpointCA mounts as for S3.
	azureCA := testInputs(agent)
	azureCA.Composition = azure.Composition
	azureCA.Composition.DestinationPath = "https://azurite.storage.svc:10000/devstoreaccount1/backups/team-a"
	azureCA.Composition.EndpointURL = "https://azurite.storage.svc:10000"
	azureCA.Composition.EndpointCA = &secretKeyRef{Name: "azurite-ca", Key: "ca.crt"}
	assertGolden(t, "deployment-azure-endpoint-ca", buildDeployment(azureCA))
	gcs := testInputs(agent)
	gcs.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders",
		Provider: "gcs", DestinationPath: "gs://backups/team-a", AgentDestination: "gs://backups/team-a",
		Credential: backupresolve.CredentialJSONKey, GoogleCredentials: &secretKeyRef{Name: "gcs-key", Key: "gcsCredentials"},
		Fingerprint: "sha256:" + strings.Repeat("ef", 32),
	}
	assertGolden(t, "deployment-gcs", buildDeployment(gcs))

	// Workload identity: no Secret, the identity on the ServiceAccount and
	// the Pod.
	identity := testAgent()
	identity.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{
		ServiceAccountAnnotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/orders-backups"},
		PodLabels:                 map[string]pgtoolboxv1alpha1.LabelValue{"azure.workload.identity/use": "true", "app.kubernetes.io/name": "ignored"},
	}
	ambient := testInputs(identity)
	ambient.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders",
		Provider: "s3", DestinationPath: "s3://backups/team-a", AgentDestination: "s3://backups/team-a",
		Credential: backupresolve.CredentialAmbient, Region: regionSource{Value: "eu-west-3"},
		Fingerprint: "sha256:" + strings.Repeat("01", 32),
	}
	assertGolden(t, "deployment-workload-identity", buildDeployment(ambient))
	assertGolden(t, "serviceaccount-workload-identity", buildServiceAccount(identity))
	// SSE-C with workload identity: the key is the only file the store
	// volume projects (the credential needs no Secret).
	sseC := testInputs(identity)
	sseC.Composition = ambient.Composition
	sseC.Composition.SSECustomerKey = &secretKeyRef{Name: "orders-sse-c", Key: "key.b64"}
	assertGolden(t, "deployment-sse-c", buildDeployment(sseC))
	assertGolden(t, "configmap-openshift-service-ca", buildCABundleConfigMap(serviceCA))

	// pgBackRest: Dalibo's S3 repository with a cipher pass,
	// Dalibo's Azure repository, and Opera's split stanza with the CA the
	// Archive names.
	daliboS3 := testInputs(agent)
	daliboS3.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", Format: backupresolve.FormatPgBackRest,
		Provider: "s3", DestinationPath: "s3://bucket-01/repo-01", AgentDestination: "s3://bucket-01/repo-01",
		EndpointURL: "https://minio.storage.svc:9000", Credential: backupresolve.CredentialStaticKeys,
		AccessKeyID:      &secretKeyRef{Name: "minio", Key: "ACCESS_KEY_ID"},
		SecretAccessKey:  &secretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"},
		Region:           regionSource{Value: "fr-par"},
		PgBackRestPlugin: "pgbackrest.dalibo.com", PgBackRestKind: backupresolve.PgBackRestKindStanza, PgBackRestObject: "orders-stanza",
		Stanza: "main", Repository: 1, URIStyle: "path",
		CipherPass: &secretKeyRef{Name: "orders-cipher", Key: "pass"},
		// The example Stanza's retention.
		Retention: &backupresolve.PgBackRestRetention{Full: 7, FullType: "count", Diff: 14, Archive: 2, ArchiveType: "full"},
	}
	assertGolden(t, "deployment-pgbackrest-dalibo-s3", buildDeployment(daliboS3))
	daliboAzure := testInputs(agent)
	daliboAzure.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", Format: backupresolve.FormatPgBackRest,
		Provider: "azure", DestinationPath: "azure://backups/cluster-sample", AgentDestination: "azure://backups/cluster-sample",
		AzureAccount: "stsample", Credential: backupresolve.CredentialStorageKey,
		AzureSecret:      &secretKeyRef{Name: "azure", Key: "KEY"},
		PgBackRestPlugin: "pgbackrest.dalibo.com", PgBackRestKind: backupresolve.PgBackRestKindStanza, PgBackRestObject: "orders-stanza",
		Stanza: "main", Repository: 2,
		// No retentionPolicy: repo-retention-full unset, the types rendered.
		Retention: &backupresolve.PgBackRestRetention{FullType: "count", ArchiveType: "full"},
	}
	assertGolden(t, "deployment-pgbackrest-dalibo-azure", buildDeployment(daliboAzure))
	opera := testInputs(agent)
	opera.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", Format: backupresolve.FormatPgBackRest,
		Provider: "s3", DestinationPath: "s3://backups/orders", AgentDestination: "s3://backups/orders",
		EndpointURL: "https://minio.storage.svc:9000", EndpointCA: &secretKeyRef{Name: "minio-ca", Key: "ca.crt"},
		Credential:       backupresolve.CredentialStaticKeys,
		AccessKeyID:      &secretKeyRef{Name: "minio", Key: "ACCESS_KEY_ID"},
		SecretAccessKey:  &secretKeyRef{Name: "minio", Key: "ACCESS_SECRET_KEY"},
		Region:           regionSource{Value: "us-east-1"},
		PgBackRestPlugin: "pgbackrest.cnpg.opera.com", PgBackRestKind: backupresolve.PgBackRestKindArchive, PgBackRestObject: "orders-archive",
		Stanza: "orders", ArchiveStanza: "orders-wal", Repository: 1,
		CipherPass: &secretKeyRef{Name: "minio", Key: "ENCRYPTION_KEY"},
		// The example Archive's time-based retention.
		Retention: &backupresolve.PgBackRestRetention{Full: 7, FullType: "time", ArchiveType: "full"},
	}
	assertGolden(t, "deployment-pgbackrest-opera-split", buildDeployment(opera))
}

// TestRenderedDeploymentIsRestrictedAndCredentialFree checks the properties
// a reviewer must not have to find in the golden file.
func TestRenderedDeploymentIsRestrictedAndCredentialFree(t *testing.T) {
	agent := testAgent()
	deployment := buildDeployment(testInputs(agent))
	pod := deployment.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != "orders-evidence-agent" {
		t.Fatal("the Pod must use its own ServiceAccount with an explicit projected token only")
	}
	if pod.SecurityContext.RunAsUser != nil || pod.SecurityContext.FSGroup != nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod security context = %+v", pod.SecurityContext)
	}
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		t.Fatal("the agent Pod has exactly one container")
	}
	container := pod.Containers[0]
	sc := container.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.RunAsUser != nil || sc.Privileged != nil {
		t.Fatalf("container security context = %+v", sc)
	}
	env := map[string]corev1.EnvVar{}
	for _, variable := range container.Env {
		env[variable.Name] = variable
		if strings.HasPrefix(variable.Name, "AWS_ACCESS_KEY_ID") && variable.Name != "AWS_ACCESS_KEY_ID_FILE" {
			t.Fatal("a credential is passed by value")
		}
	}
	for name, want := range map[string]string{
		"RUNTIME_MODE": "agent", "AGENT_NAME": "orders", "AGENT_NAMESPACE": "team-a", "CNPG_CLUSTER_NAMESPACE": "team-a",
		"PRODUCER_EPOCH": "3", "BARMAN_SERVER_NAMES": "orders", "LISTEN_ADDR": ":8443", "CATALOG_REFRESH_INTERVAL": "5m0s",
	} {
		if env[name].Value != want {
			t.Errorf("%s = %q, want %q", name, env[name].Value, want)
		}
	}
	if env["AWS_REGION"].ValueFrom == nil || env["AWS_REGION"].ValueFrom.SecretKeyRef.Name != "minio-region" {
		t.Error("the region Secret must pass through valueFrom, unread by the operator")
	}
	if deployment.Spec.Template.Annotations[AnnotationProducerEpoch] != "3" {
		t.Error("the Pod template does not record its epoch")
	}
	if _, hashed := deployment.Annotations[pgtoolboxv1alpha1.AnnotationSpecHash]; !hashed {
		t.Error("no spec hash")
	}
	for _, volume := range pod.Volumes {
		if volume.Name == volumeServing {
			for _, item := range volume.Secret.Items {
				if item.Key == caKeyKey {
					t.Fatal("the CA key reaches the agent Pod")
				}
			}
		}
	}
}

func TestSpecHashTracksContentDeterministically(t *testing.T) {
	agent := testAgent()
	first := buildDeployment(testInputs(agent)).Annotations[pgtoolboxv1alpha1.AnnotationSpecHash]
	second := buildDeployment(testInputs(testAgent())).Annotations[pgtoolboxv1alpha1.AnnotationSpecHash]
	if first == "" || first != second {
		t.Fatal("rendering is not deterministic")
	}
	agent.Spec.RefreshInterval = &metav1.Duration{Duration: time.Minute}
	if buildDeployment(testInputs(agent)).Annotations[pgtoolboxv1alpha1.AnnotationSpecHash] == first {
		t.Fatal("a spec change kept the hash")
	}
	agent = testAgent()
	limit := resource.MustParse("2Gi")
	agent.Spec.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: limit}}
	rendered := buildDeployment(testInputs(agent))
	if got := rendered.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; got.Cmp(limit) != 0 {
		t.Fatal("spec.resources is ignored")
	}
	agent.Spec.Resources.Limits[corev1.ResourceMemory] = resource.MustParse("3Gi")
	if got := rendered.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; got.Cmp(limit) != 0 {
		t.Fatal("the builder shares memory with the CR")
	}
}

func TestResolveImage(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	cases := []struct {
		name   string
		spec   *pgtoolboxv1alpha1.ImageSpec
		want   string
		policy corev1.PullPolicy
	}{
		{"default", nil, "default@sha256:x", corev1.PullIfNotPresent},
		{"digest", &pgtoolboxv1alpha1.ImageSpec{Repository: "r/a", Digest: digest}, "r/a@" + digest, corev1.PullIfNotPresent},
		{"tag and digest", &pgtoolboxv1alpha1.ImageSpec{Repository: "r/a", Tag: "1.0", Digest: digest}, "r/a:1.0@" + digest, corev1.PullIfNotPresent},
		{"tag only", &pgtoolboxv1alpha1.ImageSpec{Repository: "r/a", Tag: "1.0"}, "r/a:1.0", corev1.PullAlways},
		{"explicit policy", &pgtoolboxv1alpha1.ImageSpec{Repository: "r/a", Tag: "1.0", PullPolicy: corev1.PullNever}, "r/a:1.0", corev1.PullNever},
	}
	for _, tc := range cases {
		got := resolveImage(tc.spec, "default@sha256:x")
		if got.Reference != tc.want || got.PullPolicy != tc.policy {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
	if resolveImage(nil, "").Reference != "" {
		t.Error("no default must resolve to an empty reference (ImageRequired)")
	}
}

// TestSamplesDecodeStrictly keeps config/samples/evidenceagent*.yaml in step
// with the API: an unknown or misspelt field fails here.
func TestSamplesDecodeStrictly(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "config", "samples", "evidenceagent*.yaml"))
	if err != nil || len(paths) < 2 {
		t.Fatalf("samples = %v, %v", paths, err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path) // #nosec G304 -- repository sample
		if err != nil {
			t.Fatal(err)
		}
		var agent pgtoolboxv1alpha1.EvidenceAgent
		if err := yaml.UnmarshalStrict(raw, &agent); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if agent.Kind != "EvidenceAgent" || agent.Spec.ClusterRef.Name == "" {
			t.Fatalf("%s: %+v", path, agent)
		}
	}
}
