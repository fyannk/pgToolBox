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

package ops

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files")

var (
	drillKey  = bytes.Repeat([]byte{7}, 32)
	otherKey  = bytes.Repeat([]byte{9}, 32)
	drillTime = time.Date(2026, 11, 3, 14, 15, 0, 0, time.UTC)
)

func pluginSource() *cnpgv1.Cluster {
	sc := "standard"
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "src-uid", Labels: map[string]string{"team": "a"}},
		Spec: cnpgv1.ClusterSpec{
			Instances: 3,
			Plugins: []cnpgv1.PluginConfiguration{{
				Name: BarmanCloudPlugin, IsWALArchiver: new(true),
				Parameters: map[string]string{"barmanObjectName": "orders-store", "serverName": "orders-v2"},
			}},
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "10Gi", StorageClass: &sc},
			WalStorage:           &cnpgv1.StorageConfiguration{Size: "2Gi"},
			PostgresConfiguration: cnpgv1.PostgresConfiguration{
				Parameters:  map[string]string{"max_connections": "300"},
				Synchronous: &cnpgv1.SynchronousReplicaConfiguration{Method: cnpgv1.SynchronousReplicaConfigurationMethodAny, Number: 1},
			},
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}},
		},
		Status: cnpgv1.ClusterStatus{Image: "ghcr.io/cloudnative-pg/postgresql:17.6"},
	}
}

func inTreeSource() *cnpgv1.Cluster {
	c := pluginSource()
	c.Spec.Plugins = nil
	c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{DestinationPath: "s3://bucket/x"}}
	return c
}

func drillRequest(t *testing.T, src *cnpgv1.Cluster) DrillRequest {
	t.Helper()
	rs, err := DrillRecoverySource(src)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(1 << 30)
	return DrillRequest{
		Source: src, Recovery: rs, Namespace: "team-a", Name: DrillName("drill-", src.Name, drillTime),
		Meta:        DrillMeta{Backup: "orders-20261103", StartedAt: drillTime, Deadline: drillTime.Add(2 * time.Hour), SourceBytes: &size, Owner: "team-a"},
		Resources:   DrillResources{MemoryRequest: "1Gi", StorageClass: "drills"},
		ExtraLabels: map[string]string{"team": "a", "cnpg.io/cluster": "x", LabelDrillOwner: "spoof"},
	}
}

func TestDrillClusterGolden(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]*cnpgv1.Cluster{"plugin": pluginSource(), "intree": inTreeSource(), "source-ca": pluginSource()} {
		r := drillRequest(t, src)
		if name == "source-ca" {
			r.ServerCA = DrillServerCA{Mode: DrillServerCASource}
		}
		c, err := DrillCluster(r, [][]byte{drillKey})
		if err != nil {
			t.Fatal(name, err)
		}
		got, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "restoredrill-"+name+".golden.yaml")
		if *updateGolden {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drill differs from %s (go test -update):\n%s", name, path, got)
		}
	}
}

// The drill can never write into a repository: whatever the source
// carries, the built object has no backup section, no plugin and one
// instance, and an object that has any of them is refused.
func TestDrillClusterNeverWritesToTheRepository(t *testing.T) {
	t.Parallel()
	for _, src := range []*cnpgv1.Cluster{pluginSource(), inTreeSource()} {
		c, err := DrillCluster(drillRequest(t, src), [][]byte{drillKey})
		if err != nil {
			t.Fatal(err)
		}
		if c.Spec.Backup != nil || len(c.Spec.Plugins) != 0 || c.Spec.Instances != 1 || c.Spec.ReplicaCluster != nil ||
			c.Spec.PostgresConfiguration.Synchronous != nil {
			t.Errorf("drill spec = %+v", c.Spec)
		}
		if c.Labels[LabelDrillOwner] != "team-a" || c.Labels["cnpg.io/cluster"] != "" || c.Labels["team"] != "a" {
			t.Errorf("labels = %v", c.Labels)
		}
		for k, v := range DrillParameters {
			if c.Spec.PostgresConfiguration.Parameters[k] != v {
				t.Errorf("parameter %s = %q, want %q", k, c.Spec.PostgresConfiguration.Parameters[k], v)
			}
		}
		if c.Spec.InheritedMetadata == nil || c.Spec.InheritedMetadata.Labels[LabelDrillOwner] != "team-a" ||
			c.Spec.InheritedMetadata.Labels[LabelRestoreDrillOf] != "orders" {
			t.Errorf("inherited metadata = %+v", c.Spec.InheritedMetadata)
		}
	}
	mutations := map[string]func(*cnpgv1.Cluster){
		"backup": func(c *cnpgv1.Cluster) {
			c.Spec.Backup = &cnpgv1.BackupConfiguration{BarmanObjectStore: &cnpgv1.BarmanObjectStoreConfiguration{DestinationPath: "s3://x"}}
		},
		"archiver plugin": func(c *cnpgv1.Cluster) {
			c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: BarmanCloudPlugin, IsWALArchiver: new(true)}}
		},
		"any plugin": func(c *cnpgv1.Cluster) { c.Spec.Plugins = []cnpgv1.PluginConfiguration{{Name: "other"}} },
		"logical replication workers": func(c *cnpgv1.Cluster) {
			c.Spec.PostgresConfiguration.Parameters["max_logical_replication_workers"] = "4"
		},
		"pg_cron jobs": func(c *cnpgv1.Cluster) { delete(c.Spec.PostgresConfiguration.Parameters, "cron.launch_active_jobs") },
		"timescaledb jobs": func(c *cnpgv1.Cluster) {
			c.Spec.PostgresConfiguration.Parameters["timescaledb.max_background_workers"] = "8"
		},
		"service account template": func(c *cnpgv1.Cluster) {
			c.Spec.ServiceAccountTemplate = &cnpgv1.ServiceAccountTemplate{}
		},
		"labels not inherited": func(c *cnpgv1.Cluster) { c.Spec.InheritedMetadata = nil },
		"two instances":        func(c *cnpgv1.Cluster) { c.Spec.Instances = 2 },
		"replica": func(c *cnpgv1.Cluster) {
			c.Spec.ReplicaCluster = &cnpgv1.ReplicaClusterConfiguration{Enabled: new(true)}
		},
		"initdb": func(c *cnpgv1.Cluster) {
			c.Spec.Bootstrap = &cnpgv1.BootstrapConfiguration{InitDB: &cnpgv1.BootstrapInitDB{}}
		},
		"its own server certificate": func(c *cnpgv1.Cluster) {
			c.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ServerCASecret: "orders-ca", ServerTLSSecret: "orders-server"}
		},
		"the source's client CA": func(c *cnpgv1.Cluster) {
			c.Spec.Certificates = &cnpgv1.CertificatesConfiguration{ClientCASecret: "orders-ca"}
		},
		"external archive": func(c *cnpgv1.Cluster) { c.Spec.ExternalClusters[0].PluginConfiguration.IsWALArchiver = new(true) },
		"second external": func(c *cnpgv1.Cluster) {
			c.Spec.ExternalClusters = append(c.Spec.ExternalClusters, cnpgv1.ExternalCluster{Name: "x"})
		},
	}
	for name, mutate := range mutations {
		c, err := DrillCluster(drillRequest(t, pluginSource()), [][]byte{drillKey})
		if err != nil {
			t.Fatal(err)
		}
		mutate(c)
		if err := CheckDrillCluster(c); !errors.Is(err, ErrDrillWrites) {
			t.Errorf("%s: CheckDrillCluster = %v", name, err)
		}
		scheme, _ := NewUserScheme()
		w := NewUserWriter(fake.NewClientBuilder().WithScheme(scheme).Build(), time.Second)
		if _, err := w.CreateDrillCluster(context.Background(), c); err == nil {
			t.Errorf("%s: created", name)
		}
	}
}

func TestDrillRecoverySource(t *testing.T) {
	t.Parallel()
	rs, err := DrillRecoverySource(pluginSource())
	if err != nil || !rs.Plugin || rs.ObjectStore != "orders-store" || rs.ServerName != "orders-v2" {
		t.Errorf("plugin = %+v, %v", rs, err)
	}
	src := pluginSource()
	delete(src.Spec.Plugins[0].Parameters, "serverName")
	if rs, _ := DrillRecoverySource(src); rs.ServerName != "orders" {
		t.Errorf("default server name = %q", rs.ServerName)
	}
	src.Spec.Plugins[0].Enabled = new(false)
	if _, err := DrillRecoverySource(src); !errors.Is(err, ErrNoRecoverySource) {
		t.Errorf("disabled plugin = %v", err)
	}
	if rs, err := DrillRecoverySource(inTreeSource()); err != nil || rs.Plugin || rs.InTree == nil || rs.ServerName != "orders" {
		t.Errorf("in-tree = %+v, %v", rs, err)
	}
	snap := pluginSource()
	snap.Spec.Plugins = nil
	snap.Spec.Backup = &cnpgv1.BackupConfiguration{VolumeSnapshot: &cnpgv1.VolumeSnapshotConfiguration{}}
	if _, err := DrillRecoverySource(snap); !errors.Is(err, ErrNoRecoverySource) {
		t.Errorf("volume snapshots = %v", err)
	}
}

func TestDrillName(t *testing.T) {
	t.Parallel()
	if got := DrillName("drill-", "orders", drillTime); got != "drill-orders-2611031415" {
		t.Errorf("name = %q", got)
	}
	long := DrillName("drill-", strings.Repeat("a", 40)+"-"+strings.Repeat("b", 20), drillTime)
	if len(long) > MaxClusterName || !strings.HasPrefix(long, "drill-aaaa") || !strings.HasSuffix(long, "-2611031415") {
		t.Errorf("long name = %q (%d)", long, len(long))
	}
	cut := DrillName("drill-", strings.Repeat("a", 33)+"-x", drillTime)
	if strings.Contains(cut, "--") || len(cut) > MaxClusterName {
		t.Errorf("cut name = %q", cut)
	}
}

func TestVerifyDrill(t *testing.T) {
	t.Parallel()
	good, err := DrillCluster(drillRequest(t, pluginSource()), [][]byte{drillKey})
	if err != nil {
		t.Fatal(err)
	}
	good.CreationTimestamp = metav1.NewTime(drillTime.Add(3 * time.Second))
	if m, err := VerifyDrill(good, "team-a", [][]byte{otherKey, drillKey}); err != nil || m.Source != "orders" || m.Backup != "orders-20261103" {
		t.Errorf("rotated keys: %+v, %v", m, err)
	}
	cases := map[string]func(*cnpgv1.Cluster) (string, [][]byte){
		"wrong key":     func(*cnpgv1.Cluster) (string, [][]byte) { return "team-a", [][]byte{otherKey} },
		"other console": func(*cnpgv1.Cluster) (string, [][]byte) { return "team-b", [][]byte{drillKey} },
		"moved to another name": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.Name = "victim"
			return "team-a", [][]byte{drillKey}
		},
		"moved to another namespace": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.Namespace = "team-b"
			return "team-a", [][]byte{drillKey}
		},
		"record edited": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.Annotations[AnnotationRestoreDrill] = strings.Replace(c.Annotations[AnnotationRestoreDrill], "orders", "billing", 1)
			return "team-a", [][]byte{drillKey}
		},
		"source label changed": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.Labels[LabelRestoreDrillOf] = "billing"
			return "team-a", [][]byte{drillKey}
		},
		"labels only": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.Annotations = nil
			return "team-a", [][]byte{drillKey}
		},
		"no keys": func(*cnpgv1.Cluster) (string, [][]byte) { return "team-a", nil },
		// The record copied onto a Cluster created long after the drill
		// started (the drill deleted, its name reused).
		"created later": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.CreationTimestamp = metav1.NewTime(drillTime.Add(DrillCreationSkewAfter + time.Second))
			return "team-a", [][]byte{drillKey}
		},
		"created before the drill": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.CreationTimestamp = metav1.NewTime(drillTime.Add(-DrillCreationSkewBefore - time.Second))
			return "team-a", [][]byte{drillKey}
		},
		"no creation time": func(c *cnpgv1.Cluster) (string, [][]byte) {
			c.CreationTimestamp = metav1.Time{}
			return "team-a", [][]byte{drillKey}
		},
	}
	for name, mutate := range cases {
		c := good.DeepCopy()
		owner, keys := mutate(c)
		if _, err := VerifyDrill(c, owner, keys); !errors.Is(err, ErrNotOurDrill) {
			t.Errorf("%s: VerifyDrill = %v", name, err)
		}
	}
}

func TestDrillReaperDeletesOnlySealedDrills(t *testing.T) {
	t.Parallel()
	drill, err := DrillCluster(drillRequest(t, pluginSource()), [][]byte{drillKey})
	if err != nil {
		t.Fatal(err)
	}
	drill.UID = "drill-uid"
	drill.CreationTimestamp = metav1.NewTime(drillTime.Add(time.Second))
	spoof := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "src-uid", Labels: maps2(drill.Labels)}}
	var deleted []string
	var preconditionUID string
	scheme, err := NewUserScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(drill.DeepCopy(), spoof.DeepCopy()).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := &client.DeleteOptions{}
			o.ApplyOptions(opts)
			if o.Preconditions != nil && o.Preconditions.UID != nil {
				preconditionUID = string(*o.Preconditions.UID)
			}
			deleted = append(deleted, obj.GetName())
			return cl.Delete(ctx, obj, opts...)
		},
	}).Build()
	r := NewDrillReaper(c, "team-a", [][]byte{drillKey}, time.Second)
	if err := r.Teardown(context.Background(), spoof); !errors.Is(err, ErrNotOurDrill) {
		t.Errorf("spoofed Cluster: %v", err)
	}
	if err := r.Teardown(context.Background(), drill); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != drill.Name || preconditionUID != "drill-uid" {
		t.Errorf("deleted = %v, precondition UID = %q", deleted, preconditionUID)
	}
	if err := r.Teardown(context.Background(), drill); err != nil {
		t.Errorf("second teardown of a gone drill: %v", err)
	}
	noUID := drill.DeepCopy()
	noUID.UID = ""
	if err := r.Teardown(context.Background(), noUID); !errors.Is(err, ErrNotOurDrill) {
		t.Errorf("no UID: %v", err)
	}
}

func maps2(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// A source that sets the neutralised workers itself does not carry them
// into the drill.
func TestDrillClusterOverridesTheSourcesWorkers(t *testing.T) {
	t.Parallel()
	src := pluginSource()
	src.Spec.PostgresConfiguration.Parameters["max_logical_replication_workers"] = "10"
	src.Spec.PostgresConfiguration.Parameters["cron.launch_active_jobs"] = "on"
	src.Spec.PostgresConfiguration.AdditionalLibraries = []string{"pg_cron", "timescaledb"}
	src.Spec.ServiceAccountTemplate = &cnpgv1.ServiceAccountTemplate{}
	c, err := DrillCluster(drillRequest(t, src), [][]byte{drillKey})
	if err != nil {
		t.Fatal(err)
	}
	p := c.Spec.PostgresConfiguration.Parameters
	if p["max_logical_replication_workers"] != "0" || p["cron.launch_active_jobs"] != "off" || p["timescaledb.max_background_workers"] != "0" ||
		p["max_connections"] != "300" {
		t.Errorf("parameters = %v", p)
	}
	if c.Spec.ServiceAccountTemplate != nil {
		t.Error("serviceAccountTemplate copied")
	}
}

func TestDrillStorageDropsTheSourcesVolumeBinding(t *testing.T) {
	t.Parallel()
	src := cnpgv1.StorageConfiguration{Size: "1Gi", PersistentVolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
		Selector:   &metav1.LabelSelector{MatchLabels: map[string]string{"pv": "prod"}},
		VolumeName: "pv-prod",
		DataSource: &corev1.TypedLocalObjectReference{Kind: "VolumeSnapshot", Name: "snap"},
	}}
	out := drillStorage(src, "", "")
	if t2 := out.PersistentVolumeClaimTemplate; t2.Selector != nil || t2.VolumeName != "" || t2.DataSource != nil {
		t.Errorf("template = %+v", t2)
	}
}

// Only the barman-cloud plugin, by name, is a recovery source: another
// plugin flagged as WAL archiver with a barmanObjectName parameter is not.
func TestDrillRecoverySourceIsTheBarmanCloudPluginOnly(t *testing.T) {
	t.Parallel()
	src := pluginSource()
	src.Spec.Plugins[0].Name = "lookalike.example.com"
	if _, err := DrillRecoverySource(src); !errors.Is(err, ErrNoRecoverySource) {
		t.Errorf("another archiver plugin = %v", err)
	}
}

func TestStoreFacts(t *testing.T) {
	t.Parallel()
	store := map[string]any{"spec": map[string]any{"configuration": map[string]any{
		"destinationPath": "s3://bucket/path/", "endpointURL": "https://minio:9000/",
		"s3Credentials": map[string]any{"inheritFromIAMRole": true},
	}}}
	f, err := ObjectStoreFacts(store)
	if err != nil || f.DestinationPath != "s3://bucket/path" || f.EndpointURL != "https://minio:9000" || f.WorkloadIdentity != "inheritFromIAMRole" {
		t.Errorf("facts = %+v, %v", f, err)
	}
	if _, err := ObjectStoreFacts(map[string]any{"spec": map[string]any{}}); err == nil {
		t.Error("no configuration accepted")
	}
	keys := FactsOf(&cnpgv1.BarmanObjectStoreConfiguration{DestinationPath: "gs://b", BarmanCredentials: cnpgv1.BarmanCredentials{
		Google: &cnpgv1.GoogleCredentials{GKEEnvironment: true},
	}})
	if keys.WorkloadIdentity != "gkeEnvironment" {
		t.Errorf("gke = %+v", keys)
	}
}

// The drill's server CA: what each mode names, and the cases where the
// source's CA cannot sign a drill (the console could not verify it).
func TestDrillServerCASecretFor(t *testing.T) {
	t.Parallel()
	withCerts := func(spec *cnpgv1.CertificatesConfiguration, status string) *cnpgv1.Cluster {
		c := pluginSource()
		c.Spec.Certificates = spec
		c.Status.Certificates.ServerCASecret = status
		return c
	}
	tests := []struct {
		name      string
		source    *cnpgv1.Cluster
		namespace string
		ca        DrillServerCA
		secret    string
		refused   bool
	}{
		{name: "own: CloudNativePG's default", source: pluginSource(), namespace: "team-a"},
		{name: "zero value is own", source: pluginSource(), namespace: "team-a", ca: DrillServerCA{}},
		{name: "source: the default name", source: pluginSource(), namespace: "team-a", ca: DrillServerCA{Mode: DrillServerCASource}, secret: "orders-ca"},
		{
			name: "source: what CloudNativePG resolved", source: withCerts(nil, "orders-ca-resolved"), namespace: "team-a",
			ca: DrillServerCA{Mode: DrillServerCASource}, secret: "orders-ca-resolved",
		},
		{
			name: "source: a user-managed CA with its key", source: withCerts(&cnpgv1.CertificatesConfiguration{ServerCASecret: "team-ca"}, ""),
			namespace: "team-a", ca: DrillServerCA{Mode: DrillServerCASource}, secret: "team-ca",
		},
		{
			name: "source: a user-provided server certificate", source: withCerts(&cnpgv1.CertificatesConfiguration{ServerCASecret: "issuer", ServerTLSSecret: "orders-tls"}, "issuer"),
			namespace: "team-a", ca: DrillServerCA{Mode: DrillServerCASource}, refused: true,
		},
		{name: "source: another namespace", source: pluginSource(), namespace: "drills", ca: DrillServerCA{Mode: DrillServerCASource}, refused: true},
		{name: "secret: in any namespace", source: pluginSource(), namespace: "drills", ca: DrillServerCA{Mode: DrillServerCASecret, Secret: "drill-ca"}, secret: "drill-ca"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			secret, why := DrillServerCASecretFor(tt.source, tt.namespace, tt.ca)
			if secret != tt.secret || (why != "") != tt.refused {
				t.Fatalf("got %q, why %q; want %q, refused %v", secret, why, tt.secret, tt.refused)
			}
			r := drillRequest(t, tt.source)
			r.Namespace, r.ServerCA = tt.namespace, tt.ca
			c, err := DrillCluster(r, [][]byte{drillKey})
			if tt.refused {
				if !errors.Is(err, ErrDrillServerCA) {
					t.Fatalf("DrillCluster = %v, want ErrDrillServerCA", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.secret == "" && c.Spec.Certificates != nil:
				t.Errorf("certificates = %+v, want none", c.Spec.Certificates)
			case tt.secret != "" && !reflect.DeepEqual(c.Spec.Certificates, &cnpgv1.CertificatesConfiguration{ServerCASecret: tt.secret}):
				t.Errorf("certificates = %+v, want serverCASecret %s only", c.Spec.Certificates, tt.secret)
			}
		})
	}
}
