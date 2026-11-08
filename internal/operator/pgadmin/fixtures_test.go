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

package pgadmin

import (
	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

const (
	testNamespace     = "team-a"
	testPgAdminImage  = "ghcr.io/fyannk/pgadmin:9.18-hardened@sha256:96576df8aba0d0057248644d6d5ad398869e42bcf3fcfba2c0f7c13d151ea17c"
	testOperatorImage = "ghcr.io/fyannk/pgtoolbox-operator:2.0.0@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testProxyImage    = "quay.io/openshift/origin-oauth-proxy:4.18"
)

// oauth2PgAdmin is the Kubernetes shape: native OAuth2, clusterIP.
func oauth2PgAdmin() *pgtoolboxv1alpha1.PgAdmin {
	return &pgtoolboxv1alpha1.PgAdmin{
		TypeMeta:   metav1.TypeMeta{APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "PgAdmin"},
		ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: testNamespace, UID: "pga-uid", Generation: 3},
		Spec: pgtoolboxv1alpha1.PgAdminSpec{
			ClusterRefs: []pgtoolboxv1alpha1.LocalObjectReference{{Name: "orders"}, {Name: "billing"}},
			Auth: pgtoolboxv1alpha1.PgAdminAuthSpec{OAuth2: &pgtoolboxv1alpha1.PgAdminOAuth2Spec{
				IssuerURL:       "https://idp.example.com/realms/corp/",
				ClientID:        "pgadmin",
				ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgadmin-oidc"},
				RequiredClaims:  map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba", "it's"}},
			}},
			Admins: []string{"alice@example.com"},
		},
	}
}

// openshiftPgAdmin is the OpenShift shape: oauth-proxy, Route, the
// NetworkPolicy, a selector, ro and pooler endpoints, superuser opt-in,
// metadata propagation.
func openshiftPgAdmin() *pgtoolboxv1alpha1.PgAdmin {
	p := oauth2PgAdmin()
	p.Name = "ops"
	p.Spec.ClusterRefs = nil
	p.Spec.ClusterSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
	p.Spec.Endpoints = []pgtoolboxv1alpha1.Endpoint{"rw", "ro", "pooler:orders-pooler"}
	p.Spec.Credentials.Superuser = new(true)
	p.Spec.Auth = pgtoolboxv1alpha1.PgAdminAuthSpec{OpenShiftOAuthProxy: &pgtoolboxv1alpha1.PgAdminOpenShiftOAuthProxySpec{}}
	p.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "pgadmin.apps.example.com",
		Labels: map[string]string{"router": "internal"},
	}
	p.Spec.NetworkPolicy = pgtoolboxv1alpha1.PgAdminNetworkPolicySpec{
		Enabled:     new(true),
		ExtraEgress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}}},
	}
	p.Spec.Storage = pgtoolboxv1alpha1.PgAdminStorageSpec{Size: new(resource.MustParse("5Gi")), RetainPolicy: pgtoolboxv1alpha1.PgAdminRetainPolicyDelete}
	p.Spec.MetadataPropagation = []pgtoolboxv1alpha1.MetadataPropagation{
		{Target: pgtoolboxv1alpha1.PropagationTargetPod, Annotations: map[string]string{"sidecar.istio.io/inject": "false"}},
		{Target: pgtoolboxv1alpha1.PropagationTargetAll, Labels: map[string]string{"cost-center": "db"}},
	}
	return p
}

// ingressPgAdmin: Ingress with TLS, a tag-only image, resources.
func ingressPgAdmin() *pgtoolboxv1alpha1.PgAdmin {
	p := oauth2PgAdmin()
	p.Name = "web"
	p.Spec.Image = &pgtoolboxv1alpha1.ImageSpec{
		Repository: "registry.example.com/pgadmin", Tag: "9.18",
		PullSecrets: []pgtoolboxv1alpha1.LocalObjectReference{{Name: "pull"}},
	}
	p.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "pgadmin.example.com",
		IngressClassName: "nginx", TLS: &pgtoolboxv1alpha1.ExposureTLSSpec{CertificateSecretRef: &pgtoolboxv1alpha1.LocalObjectReference{Name: "pgadmin-tls"}},
		Annotations: map[string]string{"nginx.ingress.kubernetes.io/proxy-body-size": "64m"},
	}
	p.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}
	return p
}

func testCluster2(name string, mutate func(*cnpgv1.Cluster)) cnpgv1.Cluster {
	c := cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: map[string]string{"team": "a"}}}
	// Two instances: a replica behind the ro Service (cnpgcred.EndpointNotServed).
	c.Spec.Instances = 2
	c.Status.WriteService = name + "-rw"
	c.Status.ReadService = name + "-r"
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func testClusters() []cnpgv1.Cluster {
	return []cnpgv1.Cluster{
		testCluster2("orders", func(c *cnpgv1.Cluster) {
			c.Spec.EnableSuperuserAccess = new(true)
			c.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{Database: "orders", Owner: "orders_owner"}}
		}),
		testCluster2("billing", nil),
	}
}

func testRoles() []cnpgv1.DatabaseRole {
	reader := databaseRole("reporting", "orders", "reporting-pw")
	reader.Namespace = testNamespace
	admin := databaseRole("dbadmin", "orders", "dbadmin-pw")
	admin.Namespace = testNamespace
	admin.Spec.CreateRole = true
	pending := databaseRole("pending", "orders", "pending-pw")
	pending.Namespace = testNamespace
	pending.Status.Applied = nil
	return []cnpgv1.DatabaseRole{*reader, *admin, *pending}
}

func testPoolers() []cnpgv1.Pooler {
	return []cnpgv1.Pooler{{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-pooler", Namespace: testNamespace},
		Spec:       cnpgv1.PoolerSpec{Cluster: cnpgv1.LocalObjectReference{Name: "orders"}},
	}}
}

// testInputs resolves the build inputs the reconciler would.
func testInputs(p *pgtoolboxv1alpha1.PgAdmin, openshift bool) buildInputs {
	clusters, _, _ := selectClusters(p, testClusters())
	result := resolveOffer(offerInputs{
		PgAdmin: p, Clusters: clusters, Roles: testRoles(), RolesAvailable: true,
		Poolers: testPoolers(), PoolersAvailable: true,
	})
	in := buildInputs{
		Replicas:              1,
		Image:                 specs.ResolveImage(p.Spec.Image, testPgAdminImage),
		OperatorImage:         testOperatorImage,
		PlatformAssignsGroups: openshift,
		APIServer:             apiServerEndpoints{{IP: "172.18.0.2", Ports: []int32{6443}}},
	}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		in.ProxyImage = specs.ResolveImage(nil, testProxyImage)
	}
	for _, c := range clusters {
		in.Clusters = append(in.Clusters, c.Name)
	}
	for _, e := range p.Spec.EndpointsOrDefault() {
		if n, ok := e.PoolerName(); ok {
			in.Poolers = append(in.Poolers, n)
		}
	}
	raw, err := encodeOffer(result)
	if err != nil {
		panic(err)
	}
	in.Offer, in.OfferHash, in.OfferSecrets = raw, specs.Hash(string(raw)), result.Document.SecretNames("")
	return in
}

type pgadminT = pgtoolboxv1alpha1.PgAdmin

func encodeOffer(result offerResult) ([]byte, error) { return offerEncode(result.Document) }
