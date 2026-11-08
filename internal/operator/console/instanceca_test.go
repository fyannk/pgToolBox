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

package console

import (
	"bytes"
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// testCA reads a test CA certificate (testdata/instance-ca; their keys were
// discarded when they were made). alpha-renewed is alpha re-issued on the
// same key, the way CloudNativePG renews its CA.
func testCA(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "instance-ca", name+".crt")) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeKeyBlock is a PEM block shaped like a private key; the bytes are not
// a key, the type is what the filter must refuse.
var fakeKeyBlock = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not-a-key-but-must-never-be-copied")})

func mirrorConsole() *pgtoolboxv1alpha1.PgToolBoxConsole {
	return &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
}

func TestServerCASecretName(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		cluster cnpgv1.Cluster
		want    string
	}{
		"default": {cluster: cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "pg"}}, want: "pg-ca"},
		"spec": {cluster: cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pg"},
			Spec:       cnpgv1.ClusterSpec{Certificates: &cnpgv1.CertificatesConfiguration{ServerCASecret: "shared-ca"}},
		}, want: "shared-ca"},
		"status wins over spec": {cluster: cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pg"},
			Spec:       cnpgv1.ClusterSpec{Certificates: &cnpgv1.CertificatesConfiguration{ServerCASecret: "spec-ca"}},
			Status:     cnpgv1.ClusterStatus{Certificates: cnpgv1.CertificatesStatus{CertificatesConfiguration: cnpgv1.CertificatesConfiguration{ServerCASecret: "status-ca"}}},
		}, want: "status-ca"},
		"empty spec certificates": {cluster: cnpgv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "pg"},
			Spec:       cnpgv1.ClusterSpec{Certificates: &cnpgv1.CertificatesConfiguration{}},
		}, want: "pg-ca"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := serverCASecretName(&tc.cluster); got != tc.want {
				t.Fatalf("serverCASecretName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildInstanceCABundle(t *testing.T) {
	t.Parallel()
	alpha, beta := testCA(t, "alpha"), testCA(t, "beta")
	cases := map[string]struct {
		cas      []clusterCA
		want     [][]byte // the bundle's certificates, in order
		clusters int
		missing  []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing
	}{
		"two Clusters, sorted by name whatever the input order": {
			cas:      []clusterCA{{Cluster: "b", Secret: "b-ca", CACert: beta}, {Cluster: "a", Secret: "a-ca", CACert: alpha}},
			want:     [][]byte{alpha, beta},
			clusters: 2,
		},
		"a shared serverCASecret appears once": {
			cas: []clusterCA{
				{Cluster: "a", Secret: "shared", CACert: alpha},
				{Cluster: "b", Secret: "shared", CACert: alpha},
				{Cluster: "c", Secret: "c-ca", CACert: beta},
			},
			want:     [][]byte{alpha, beta},
			clusters: 3,
		},
		"a private key and comments next to the certificate are dropped": {
			cas:      []clusterCA{{Cluster: "a", Secret: "a-ca", CACert: append(append([]byte("# a comment\n"), fakeKeyBlock...), alpha...)}},
			want:     [][]byte{alpha},
			clusters: 1,
		},
		"missing Secret, missing key and no certificate are reported": {
			cas: []clusterCA{
				{Cluster: "a", Secret: "a-ca", Missing: pgtoolboxv1alpha1.ReasonSecretNotFound},
				{Cluster: "b", Secret: "b-ca", Missing: pgtoolboxv1alpha1.ReasonSecretKeyMissing},
				{Cluster: "c", Secret: "c-ca", CACert: fakeKeyBlock},
				{Cluster: "d", Secret: "d-ca", CACert: []byte("garbage")},
				{Cluster: "e", Secret: "e-ca", CACert: beta},
			},
			want:     [][]byte{beta},
			clusters: 1,
			missing: []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{
				{Cluster: "a", Secret: "a-ca", Reason: pgtoolboxv1alpha1.ReasonSecretNotFound},
				{Cluster: "b", Secret: "b-ca", Reason: pgtoolboxv1alpha1.ReasonSecretKeyMissing},
				{Cluster: "c", Secret: "c-ca", Reason: pgtoolboxv1alpha1.ReasonInstanceCAInvalid},
				{Cluster: "d", Secret: "d-ca", Reason: pgtoolboxv1alpha1.ReasonInstanceCAInvalid},
			},
		},
		"no Cluster: an empty bundle": {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := buildInstanceCABundle(mirrorConsole(), tc.cas)
			if got.ConfigMap.Name != "console-instance-ca" || got.ConfigMap.Namespace != "team-a" {
				t.Fatalf("ConfigMap %s/%s", got.ConfigMap.Namespace, got.ConfigMap.Name)
			}
			if len(got.ConfigMap.Data) != 1 || len(got.ConfigMap.BinaryData) != 0 {
				t.Fatalf("data keys = %v, want ca.crt only", got.ConfigMap.Data)
			}
			if want := string(bytes.Join(tc.want, nil)); got.ConfigMap.Data[caKey] != want {
				t.Fatalf("ca.crt =\n%s\nwant\n%s", got.ConfigMap.Data[caKey], want)
			}
			if strings.Contains(got.ConfigMap.Data[caKey], "PRIVATE KEY") || strings.Contains(got.ConfigMap.Data[caKey], "#") {
				t.Fatal("something other than a certificate reached the bundle")
			}
			if got.Clusters != tc.clusters || !reflect.DeepEqual(got.Missing, tc.missing) {
				t.Fatalf("clusters %d missing %+v, want %d %+v", got.Clusters, got.Missing, tc.clusters, tc.missing)
			}
			if again := buildInstanceCABundle(mirrorConsole(), tc.cas); !reflect.DeepEqual(again.ConfigMap, got.ConfigMap) {
				t.Fatal("the bundle is not byte-stable")
			}
		})
	}
}

func TestBuildInstanceCABundleFull(t *testing.T) {
	t.Parallel()
	alpha, beta := testCA(t, "alpha"), testCA(t, "beta")
	cas := []clusterCA{
		{Cluster: "a", Secret: "a-ca", CACert: alpha},
		{Cluster: "b", Secret: "b-ca", CACert: beta},
		// Already in the bundle: costs nothing, so it still fits.
		{Cluster: "c", Secret: "c-ca", CACert: alpha},
	}
	got := buildBoundedInstanceCABundle(mirrorConsole(), cas, len(alpha)+1)
	if got.ConfigMap.Data[caKey] != string(alpha) || got.Clusters != 2 || got.Distinct != 1 {
		t.Fatalf("ca.crt %q, clusters %d, distinct %d", got.ConfigMap.Data[caKey], got.Clusters, got.Distinct)
	}
	want := []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{{Cluster: "b", Secret: "b-ca", Reason: pgtoolboxv1alpha1.ReasonInstanceCABundleFull}}
	if !reflect.DeepEqual(got.Missing, want) {
		t.Fatalf("missing = %+v, want %+v", got.Missing, want)
	}
}

func TestInstanceCACondition(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-instance-ca"}}
	cases := map[string]struct {
		bundle instanceCABundle
		status metav1.ConditionStatus
		reason pgtoolboxv1alpha1.Reason
		text   string
	}{
		"empty":    {bundle: instanceCABundle{ConfigMap: cm}, status: metav1.ConditionTrue, reason: pgtoolboxv1alpha1.ReasonAsExpected, text: "no Cluster selected"},
		"complete": {bundle: instanceCABundle{ConfigMap: cm, Clusters: 2, Distinct: 1}, status: metav1.ConditionTrue, reason: pgtoolboxv1alpha1.ReasonAsExpected, text: "all 2 selected Clusters"},
		"missing": {
			bundle: instanceCABundle{ConfigMap: cm, Clusters: 1, Missing: []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{{Cluster: "b", Secret: "b-ca", Reason: pgtoolboxv1alpha1.ReasonSecretNotFound}}},
			status: metav1.ConditionFalse, reason: pgtoolboxv1alpha1.ReasonInstanceCAMissing, text: "1 of 2 selected Clusters have no CA in ConfigMap console-instance-ca and read as not verifiable: b (SecretNotFound, Secret b-ca)",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			status, reason, message := instanceCACondition(tc.bundle)
			if status != tc.status || reason != tc.reason || !strings.Contains(message, tc.text) {
				t.Fatalf("= %s/%s %q, want %s/%s containing %q", status, reason, message, tc.status, tc.reason, tc.text)
			}
		})
	}
}

func cnpgScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, pgtoolboxv1alpha1.AddToScheme, cnpgv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func testCluster(name string, labels map[string]string, caSecret string) *cnpgv1.Cluster {
	cluster := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", Labels: labels}}
	if caSecret != "" {
		cluster.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: caSecret}
	}
	return cluster
}

func caSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a"}, Data: data}
}

func TestCollectInstanceCAs(t *testing.T) {
	t.Parallel()
	alpha, beta := testCA(t, "alpha"), testCA(t, "beta")
	objects := []client.Object{
		testCluster("pg-a", map[string]string{"team": "a"}, ""),
		testCluster("pg-b", map[string]string{"team": "a"}, "shared-ca"),
		testCluster("pg-c", map[string]string{"team": "a"}, "shared-ca"),
		testCluster("pg-nokey", map[string]string{"team": "a"}, ""),
		testCluster("pg-gone", map[string]string{"team": "a"}, ""),
		testCluster("pg-other", map[string]string{"team": "b"}, ""),
		caSecret("pg-a-ca", map[string][]byte{"ca.crt": alpha, "ca.key": []byte("SIGNING KEY")}),
		caSecret("shared-ca", map[string][]byte{"ca.crt": beta}),
		caSecret("pg-nokey-ca", map[string][]byte{"tls.crt": beta}),
		caSecret("pg-other-ca", map[string][]byte{"ca.crt": beta}),
		&cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "team-b"}},
	}
	c := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).WithObjects(objects...).Build()
	console := mirrorConsole()
	console.Spec.ClusterSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}

	cas, err := collectInstanceCAs(context.Background(), c, c, console)
	if err != nil {
		t.Fatal(err)
	}
	bundle := buildInstanceCABundle(console, cas)
	if want := string(alpha) + string(beta); bundle.ConfigMap.Data[caKey] != want {
		t.Fatalf("ca.crt = %q", bundle.ConfigMap.Data[caKey])
	}
	if strings.Contains(bundle.ConfigMap.Data[caKey], "SIGNING KEY") {
		t.Fatal("ca.key reached the bundle")
	}
	want := []pgtoolboxv1alpha1.PgToolBoxConsoleInstanceCAMissing{
		{Cluster: "pg-gone", Secret: "pg-gone-ca", Reason: pgtoolboxv1alpha1.ReasonSecretNotFound},
		{Cluster: "pg-nokey", Secret: "pg-nokey-ca", Reason: pgtoolboxv1alpha1.ReasonSecretKeyMissing},
	}
	if bundle.Clusters != 3 || bundle.Distinct != 2 || !reflect.DeepEqual(bundle.Missing, want) {
		t.Fatalf("clusters %d distinct %d missing %+v", bundle.Clusters, bundle.Distinct, bundle.Missing)
	}
}

// TestInstanceCABundleGolden pins the rendered mirror (metadata, owner,
// hash, data) for two Clusters with distinct CAs. Rewrite with -update.
func TestInstanceCABundleGolden(t *testing.T) {
	t.Parallel()
	got, err := yaml.Marshal(buildInstanceCABundle(mirrorConsole(), []clusterCA{
		{Cluster: "pg-b", Secret: "pg-b-ca", CACert: testCA(t, "beta")},
		{Cluster: "pg-a", Secret: "pg-a-ca", CACert: testCA(t, "alpha")},
	}).ConfigMap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "instance-ca.golden.yaml")
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update to accept)", path)
	}
}

func TestClusterCAChanged(t *testing.T) {
	t.Parallel()
	base := testCluster("pg", map[string]string{"team": "a"}, "")
	relabelled := base.DeepCopy()
	relabelled.Labels["team"] = "b"
	newCA := base.DeepCopy()
	newCA.Status.Certificates.ServerCASecret = "other-ca"
	statusOnly := base.DeepCopy()
	statusOnly.Status.Phase = "Cluster in healthy state"
	cases := map[string]struct {
		after *cnpgv1.Cluster
		want  bool
	}{
		"labels":         {after: relabelled, want: true},
		"server CA":      {after: newCA, want: true},
		"another status": {after: statusOnly, want: false},
	}
	predicate := clusterCAChanged()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.after}); got != tc.want {
				t.Fatalf("Update = %t, want %t", got, tc.want)
			}
		})
	}
	if !predicate.Create(event.CreateEvent{Object: base}) || !predicate.Delete(event.DeleteEvent{Object: base}) {
		t.Fatal("a Cluster created or deleted must reach the mirror")
	}
}

func TestForCASecret(t *testing.T) {
	t.Parallel()
	referenced := &pgtoolboxv1alpha1.PgToolBoxConsole{
		ObjectMeta: metav1.ObjectMeta{Name: "own-bundle", Namespace: "team-a"},
		Spec:       pgtoolboxv1alpha1.PgToolBoxConsoleSpec{InstanceCABundleRef: &pgtoolboxv1alpha1.LocalObjectReference{Name: "mine"}},
	}
	c := fake.NewClientBuilder().WithScheme(cnpgScheme(t)).
		WithObjects(testCluster("pg-a", nil, ""), testCluster("pg-b", nil, "shared-ca"), mirrorConsole(), referenced,
			&pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "team-b"}}).
		WithIndex(&cnpgv1.Cluster{}, indexServerCASecret, func(object client.Object) []string {
			return []string{serverCASecretName(object.(*cnpgv1.Cluster))}
		}).Build()
	r := &Reconciler{Client: c}
	ctx := context.Background()
	cases := map[string]int{"pg-a-ca": 1, "shared-ca": 1, "pg-b-ca": 0, "app-credentials": 0}
	for name, want := range cases {
		requests := r.forCASecret(ctx, caSecret(name, nil))
		if len(requests) != want {
			t.Fatalf("Secret %s: %d requests %v, want %d", name, len(requests), requests, want)
		}
		if want == 1 && requests[0].Name != "console" {
			t.Fatalf("Secret %s enqueued %v, want the mirroring console only", name, requests)
		}
	}
	if requests := r.forCluster(ctx, testCluster("pg-z", nil, "")); len(requests) != 1 || requests[0].Name != "console" {
		t.Fatalf("forCluster = %v, want the mirroring console", requests)
	}
}
