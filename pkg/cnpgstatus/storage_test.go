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

package cnpgstatus

import (
	"reflect"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPVCGroup(t *testing.T) {
	c := &cnpgv1.Cluster{Status: cnpgv1.ClusterStatus{
		HealthyPVC:      []string{"pg-1", "pg-1-wal"},
		ResizingPVC:     []string{"pg-2"},
		InitializingPVC: []string{"pg-3"},
		DanglingPVC:     []string{"pg-4"},
		UnusablePVC:     []string{"pg-5-wal"},
	}}
	tests := []struct {
		pvc   string
		group string
		class Class
	}{
		{"pg-1", PVCHealthy, ClassOK},
		{"pg-1-wal", PVCHealthy, ClassOK},
		{"pg-2", PVCResizing, ClassProgressing},
		{"pg-3", PVCInitializing, ClassProgressing},
		{"pg-4", PVCDangling, ClassWarning},
		{"pg-5-wal", PVCUnusable, ClassCritical},
		{"pg-6", PVCUnlisted, ClassUnknown},
		{"", PVCUnlisted, ClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.pvc, func(t *testing.T) {
			got := PVCGroup(Minor130, c, tt.pvc)
			if got.Group != tt.group || got.Class != tt.class || got.Text == "" {
				t.Errorf("PVCGroup(%q) = %+v, want %s/%s", tt.pvc, got, tt.group, tt.class)
			}
		})
	}
	if got := PVCGroup(Minor130, nil, "pg-1"); got.Class != ClassUnknown {
		t.Errorf("nil cluster: %+v", got)
	}
	if a, b := PVCGroup(Minor130, c, "pg-3").Text, PVCGroup(Minor131, c, "pg-3").Text; a == b || !strings.Contains(b, "init container") {
		t.Errorf("1.31 initializing text must mention the bootstrap init container: %q vs %q", a, b)
	}
}

func TestPVCRole(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   PVCRoleReading
	}{
		{
			"data",
			map[string]string{LabelPVCRole: PVCRolePgData, LabelInstanceName: "pg-1"},
			PVCRoleReading{Role: PVCRolePgData, Known: true, Instance: "pg-1", VolumeUsageName: "pgdata"},
		},
		{
			"wal",
			map[string]string{LabelPVCRole: PVCRolePgWal, LabelInstanceName: "pg-1"},
			PVCRoleReading{Role: PVCRolePgWal, Known: true, Instance: "pg-1", VolumeUsageName: "wal"},
		},
		{
			"tablespace",
			map[string]string{LabelPVCRole: PVCRolePgTablespace, LabelInstanceName: "pg-1", LabelTablespaceName: "idx"},
			PVCRoleReading{Role: PVCRolePgTablespace, Known: true, Instance: "pg-1", Tablespace: "idx", VolumeUsageName: "tbs-idx"},
		},
		{
			"tablespace without name",
			map[string]string{LabelPVCRole: PVCRolePgTablespace},
			PVCRoleReading{Role: PVCRolePgTablespace, Known: true},
		},
		{"foreign role", map[string]string{LabelPVCRole: "PG_SOMETHING"}, PVCRoleReading{Role: "PG_SOMETHING"}},
		{"no labels", nil, PVCRoleReading{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PVCRole(tt.labels); got != tt.want {
				t.Errorf("PVCRole = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStorageRequests(t *testing.T) {
	if StorageRequests(nil) != nil {
		t.Error("nil cluster has requests")
	}
	c := &cnpgv1.Cluster{Spec: cnpgv1.ClusterSpec{
		StorageConfiguration: cnpgv1.StorageConfiguration{Size: "10Gi", StorageClass: new("fast"), ResizeInUseVolumes: new(false)},
		WalStorage: &cnpgv1.StorageConfiguration{PersistentVolumeClaimTemplate: &corev1.PersistentVolumeClaimSpec{
			StorageClassName: new("wal-class"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("2Gi"),
			}},
		}},
		Tablespaces: []cnpgv1.TablespaceConfiguration{
			{Name: "tmp", Temporary: true, Storage: cnpgv1.StorageConfiguration{Size: "1Gi"}},
		},
	}}
	want := []VolumeRequest{
		{Role: PVCRolePgData, Size: "10Gi", StorageClass: "fast", ResizeInUseVolumes: new(false)},
		{Role: PVCRolePgWal, Size: "2Gi", StorageClass: "wal-class"},
		{Role: PVCRolePgTablespace, Tablespace: "tmp", Size: "1Gi", Temporary: true},
	}
	if got := StorageRequests(c); !reflect.DeepEqual(got, want) {
		t.Errorf("StorageRequests = %+v\nwant %+v", got, want)
	}
}

func TestForkAutoGrow(t *testing.T) {
	t.Parallel()
	object := map[string]any{"spec": map[string]any{
		"storage":    map[string]any{"size": "10Gi", "autoGrow": map[string]any{"maxSize": "100Gi", "threshold": int64(85), "step": "20%"}},
		"walStorage": map[string]any{"size": "5Gi", "autoGrow": map[string]any{"maxSize": "not a size"}},
		"tablespaces": []any{
			map[string]any{"name": "hist", "storage": map[string]any{"autoGrow": map[string]any{"maxSize": "1Ti", "threshold": float64(90), "step": "5Gi"}}},
			map[string]any{"name": "tmp", "storage": map[string]any{"size": "1Gi"}},
			map[string]any{"storage": map[string]any{"autoGrow": map[string]any{"maxSize": "1Gi"}}},
			"garbage",
		},
	}}
	got := ForkAutoGrow(object)
	if len(got) != 3 {
		t.Fatalf("autoGrow = %+v", got)
	}
	if g := got["pgdata"]; g.MaxBytes == nil || *g.MaxBytes != 100<<30 || g.Threshold != 85 || g.Step != "20%" {
		t.Errorf("pgdata = %+v", g)
	}
	if g := got["wal"]; g.MaxSize != "not a size" || g.MaxBytes != nil {
		t.Errorf("an unparsable maxSize: %+v", g)
	}
	if g := got["tbs-hist"]; g.MaxBytes == nil || *g.MaxBytes != 1<<40 || g.Threshold != 90 {
		t.Errorf("tbs-hist = %+v", g)
	}
	if len(ForkAutoGrow(map[string]any{"spec": map[string]any{"storage": map[string]any{"size": "1Gi"}}})) != 0 {
		t.Error("an upstream Cluster has autoGrow")
	}
	for _, r := range []struct {
		req  VolumeRequest
		want string
	}{
		{VolumeRequest{Role: PVCRolePgData}, "pgdata"},
		{VolumeRequest{Role: PVCRolePgWal}, "wal"},
		{VolumeRequest{Role: PVCRolePgTablespace, Tablespace: "hist"}, "tbs-hist"},
		{VolumeRequest{Role: PVCRolePgTablespace}, ""},
	} {
		if got := r.req.VolumeName(); got != r.want {
			t.Errorf("VolumeName(%+v) = %q, want %q", r.req, got, r.want)
		}
	}
}
