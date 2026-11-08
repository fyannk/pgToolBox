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

package v1alpha1_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestEvidenceAgentCEL applies EvidenceAgents to a real API server and checks
// the CRD's admission rules (and that their cost fits the budget: a rule over
// budget fails the CRD install in apiServer).
func TestEvidenceAgentCEL(t *testing.T) {
	c := apiServer(t)
	valid := func() *pgtoolboxv1alpha1.EvidenceAgent {
		return &pgtoolboxv1alpha1.EvidenceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "default"},
			Spec:       pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*pgtoolboxv1alpha1.EvidenceAgent)
		wantErr string
	}{
		{name: "minimal"},
		{name: "every optional field", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.ObjectStoreRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "store"}
			a.Spec.ServerName = "orders-2026"
			a.Spec.RefreshInterval = &metav1.Duration{Duration: time.Minute}
			a.Spec.Exposure = &pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeClusterIP}
			a.Spec.TLS = &pgtoolboxv1alpha1.EvidenceAgentTLSSpec{Issuer: pgtoolboxv1alpha1.CertificateIssuerOpenShiftServiceCA}
			a.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{Repository: "registry.example/pgtoolbox-evidence-agent", Digest: "sha256:" + strings.Repeat("a", 64)}
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{
				ServiceAccountAnnotations: map[string]string{"azure.workload.identity/client-id": "00000000-0000-0000-0000-000000000000"},
				PodLabels:                 map[string]pgtoolboxv1alpha1.LabelValue{"azure.workload.identity/use": "true"},
			}
		}},
		{name: "every workload identity annotation", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{ServiceAccountAnnotations: map[string]string{
				"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/r", "eks.amazonaws.com/sts-regional-endpoints": "true",
				"eks.amazonaws.com/token-expiration": "86400", "azure.workload.identity/client-id": "c",
				"azure.workload.identity/tenant-id": "t", "iam.gke.io/gcp-service-account": "sa@p.iam.gserviceaccount.com",
			}}
		}},
		{name: "allowed Secrets and an insecure endpoint", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.AllowedSecrets, a.Spec.AllowInsecureEndpoint = []string{"minio", "minio-ca.v2"}, true
		}},
		{name: "an empty workload identity is the opt-in", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{}
		}},
		{name: "workload identity annotation in the operator's prefix", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{ServiceAccountAnnotations: map[string]string{"pgtoolbox.fyannk.dev/spec-hash": "x"}}
		}, wantErr: "serviceAccountAnnotations accepts only"},
		{name: "OpenShift OAuth redirect annotation", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{ServiceAccountAnnotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirecturi.first": "https://attacker.example",
			}}
		}, wantErr: "serviceAccountAnnotations accepts only"},
		{name: "CloudNativePG labels on the agent", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{PodLabels: map[string]pgtoolboxv1alpha1.LabelValue{"cnpg.io/instanceRole": "primary"}}
		}, wantErr: "podLabels accepts only azure.workload.identity/use"},
		{name: "workload identity pod label value with a space", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.WorkloadIdentity = &pgtoolboxv1alpha1.WorkloadIdentitySpec{PodLabels: map[string]pgtoolboxv1alpha1.LabelValue{"azure.workload.identity/use": "yes please"}}
		}, wantErr: "podLabels"},
		{name: "allowed Secret not a name", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.AllowedSecrets = []string{"Minio_Creds"}
		}, wantErr: "allowedSecrets"},
		{name: "too many allowed Secrets", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			for i := range 17 {
				a.Spec.AllowedSecrets = append(a.Spec.AllowedSecrets, fmt.Sprintf("s%d", i))
			}
		}, wantErr: "allowedSecrets"},
		{name: "name too long", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Name = "a" + strings.Repeat("b", 40) }, wantErr: "DNS-1035 label of at most 40"},
		{name: "external exposure", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.Exposure = &pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "agent.example"}
		}, wantErr: "in-cluster only"},
		{name: "server name with a slash", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Spec.ServerName = "a/b" }, wantErr: "serverName must not contain"},
		{name: "server name dot-dot", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) { a.Spec.ServerName = ".." }, wantErr: "serverName must not contain"},
		{name: "refresh too fast", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.RefreshInterval = &metav1.Duration{Duration: 10 * time.Second}
		}, wantErr: "between 30s and 24h"},
		{name: "cross-namespace looking cluster ref", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.ClusterRef.Name = "other/orders"
		}, wantErr: "spec.clusterRef.name"},
		{name: "pgBackRest repository and CA", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.AllowedSecrets = []string{"minio", "minio-ca"}
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{
				Repository: 2, EndpointCA: &pgtoolboxv1alpha1.SecretKeyReference{Name: "minio-ca", Key: "ca.crt"},
			}
		}},
		{name: "pgBackRest with defaults", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{}
		}},
		{name: "pgBackRest repository beyond 256", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Repository: 257}
		}, wantErr: "spec.pgbackrest.repository"},
		{name: "pgBackRest with an ObjectStore", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.ObjectStoreRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "store"}
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{}
		}, wantErr: "objectStoreRef and serverName are barman-cloud settings"},
		{name: "pgBackRest with a server name", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.ServerName = "orders"
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Repository: 1}
		}, wantErr: "objectStoreRef and serverName are barman-cloud settings"},
		{name: "pgBackRest endpoint pin", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Endpoint: "minio.storage.svc:9000"}
		}},
		{name: "pgBackRest endpoint pin, IPv6", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Endpoint: "[fd00::1]:9000"}
		}},
		{name: "pgBackRest endpoint pin with a scheme", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Endpoint: "https://minio:9000"}
		}, wantErr: "spec.pgbackrest.endpoint"},
		{name: "pgBackRest endpoint pin with a path", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{Endpoint: "minio:9000/repo"}
		}, wantErr: "spec.pgbackrest.endpoint"},
		{name: "pgBackRest CA without a key", mutate: func(a *pgtoolboxv1alpha1.EvidenceAgent) {
			a.Spec.PgBackRest = &pgtoolboxv1alpha1.EvidenceAgentPgBackRestSpec{EndpointCA: &pgtoolboxv1alpha1.SecretKeyReference{Name: "minio-ca"}}
		}, wantErr: "spec.pgbackrest.endpointCA.key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			if tc.mutate != nil {
				tc.mutate(obj)
			}
			err := c.Create(context.Background(), obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				_ = c.Delete(context.Background(), obj)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				if err == nil {
					_ = c.Delete(context.Background(), obj)
				}
				t.Fatalf("create error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}

	// clusterRef is required and the issuer is a closed enum: send raw
	// objects the typed client cannot express.
	for _, raw := range []map[string]any{
		{"spec": map[string]any{}},
		{"spec": map[string]any{"clusterRef": map[string]any{"name": "orders"}, "tls": map[string]any{"issuer": "SelfSigned"}}},
	} {
		obj := &unstructured.Unstructured{Object: raw}
		obj.SetAPIVersion(pgtoolboxv1alpha1.GroupVersion.String())
		obj.SetKind("EvidenceAgent")
		obj.SetNamespace("default")
		obj.SetName("raw")
		if err := c.Create(context.Background(), obj); err == nil {
			_ = c.Delete(context.Background(), obj)
			t.Fatalf("accepted %v", raw)
		}
	}
}
