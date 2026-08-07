/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package adminsync

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDisabledSync(t *testing.T) {
	if err := (Disabled{}).Sync(context.Background(), Request{}); err != nil {
		t.Errorf("Disabled.Sync() error = %v, want nil", err)
	}
}

func TestSelectReadyPod(t *testing.T) {
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "beta", Annotations: map[string]string{pgtoolboxv1alpha1.ConfigChecksumAnnotation: "old"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha", Annotations: map[string]string{pgtoolboxv1alpha1.ConfigChecksumAnnotation: "new"}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
		},
	}
	got, err := selectReadyPod(pods, "new")
	if err != nil {
		t.Fatalf("selectReadyPod() error = %v", err)
	}
	if got.Name != "alpha" {
		t.Errorf("selectReadyPod() = %q, want alpha", got.Name)
	}

	if _, err := selectReadyPod(pods, "missing"); err == nil {
		t.Error("selectReadyPod() = nil error for missing checksum, want error")
	}
}

func TestPodReady(t *testing.T) {
	cases := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "running and ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "not running",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "running not ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := podReady(&tc.pod); got != tc.want {
				t.Errorf("podReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPgpassLine(t *testing.T) {
	got := pgpassLine("h:ost", 5432, "db:1", "user", "p:ass")
	want := `h\:ost:5432:db\:1:user:p\:ass`
	if got != want {
		t.Errorf("pgpassLine() = %q, want %q", got, want)
	}
}

func TestServerDocument(t *testing.T) {
	server := Server{
		Name:          "cluster",
		Group:         "PgToolBox",
		Host:          "cluster-rw.ns.svc",
		Port:          5432,
		MaintenanceDB: "postgres",
		Username:      "app",
		PassFile:      DefaultPassFilePath,
		SSLMode:       "prefer",
	}
	doc := serverDocument{
		Servers: map[string]serverEntry{
			"1": {
				Name:          server.Name,
				Group:         server.Group,
				Host:          server.Host,
				Port:          server.Port,
				MaintenanceDB: server.MaintenanceDB,
				Username:      server.Username,
				ConnectionParameters: map[string]string{
					"sslmode":  server.SSLMode,
					"passfile": server.PassFile,
				},
			},
		},
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal server document: %v", err)
	}
	var parsed serverDocument
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("unmarshal server document: %v", err)
	}
	entry := parsed.Servers["1"]
	if entry.Name != server.Name || entry.Username != server.Username {
		t.Errorf("server entry = %+v, want name=%s username=%s", entry, server.Name, server.Username)
	}
	if !reflect.DeepEqual(entry.ConnectionParameters, map[string]string{"sslmode": "prefer", "passfile": DefaultPassFilePath}) {
		t.Errorf("connection parameters = %v", entry.ConnectionParameters)
	}
}
