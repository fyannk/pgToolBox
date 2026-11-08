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

package status

import (
	"context"
	"errors"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/evidence"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func TestRepositoryPresence(t *testing.T) {
	scanned := now.Add(-time.Hour)
	before, after := now.Add(-2*time.Hour), now.Add(-30*time.Minute)
	snap := &evidencev1alpha1.RepositoryEvidenceSnapshot{CompletedAt: &scanned}
	complete := clusterEvidence{result: evidence.Result{Snapshot: snap, BackupIDs: map[string]bool{"seen": true}, BackupsComplete: true}}
	partial := complete
	partial.result.BackupsComplete = false
	tests := []struct {
		name string
		e    clusterEvidence
		r    cnpgstatus.BackupObjectReading
		want viewmodel.RepositoryPresence
	}{
		{"listed", complete, cnpgstatus.BackupObjectReading{BackupID: "seen", StoppedAt: &before}, viewmodel.RepositoryPresent},
		{"missing from a later complete scan", complete, cnpgstatus.BackupObjectReading{BackupID: "lost", StoppedAt: &before}, viewmodel.RepositoryAbsent},
		{"newer than the scan", complete, cnpgstatus.BackupObjectReading{BackupID: "new", StoppedAt: &after}, viewmodel.RepositoryUnknown},
		{"no stop time", complete, cnpgstatus.BackupObjectReading{BackupID: "new"}, viewmodel.RepositoryUnknown},
		{"partial list", partial, cnpgstatus.BackupObjectReading{BackupID: "lost", StoppedAt: &before}, viewmodel.RepositoryUnknown},
		{"no snapshot", clusterEvidence{}, cnpgstatus.BackupObjectReading{BackupID: "x", StoppedAt: &before}, viewmodel.RepositoryUnknown},
		{"no backup id", complete, cnpgstatus.BackupObjectReading{}, viewmodel.RepositoryNotChecked},
		{"volume snapshot", complete, cnpgstatus.BackupObjectReading{BackupID: "x", Method: "volumeSnapshot"}, viewmodel.RepositoryNotChecked},
	}
	for _, tt := range tests {
		if got := tt.e.presence(tt.r); got != tt.want {
			t.Errorf("%s: %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestConditionHealth(t *testing.T) {
	for _, tt := range []struct {
		typ, status string
		want        viewmodel.Health
	}{
		{"Ready", "True", viewmodel.HealthOK},
		{"Ready", "False", viewmodel.HealthWarning},
		{"Stalled", "True", viewmodel.HealthCritical},
		{"Stalled", "False", viewmodel.HealthOK},
		{"Progressing", "True", viewmodel.HealthProgressing},
		{"Progressing", "False", viewmodel.HealthOK},
		{"Ready", "Unknown", viewmodel.HealthUnknown},
	} {
		if got := conditionHealth(metav1.Condition{Type: tt.typ, Status: metav1.ConditionStatus(tt.status)}); got != tt.want {
			t.Errorf("%s=%s: %s, want %s", tt.typ, tt.status, got, tt.want)
		}
	}
}

func TestSafeURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://pgadmin.example.com/x": "https://pgadmin.example.com/x",
		"http://pgadmin.example.com":    "",
		"javascript:alert(1)":           "",
		"https://user:pw@host":          "",
		"/relative":                     "",
		"":                              "",
	} {
		if got := safeURL(raw); got != want {
			t.Errorf("safeURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A list the cache fails is reported unavailable, never an empty panel
// on a live source.
func TestObjectsListFailureIsNotEmpty(t *testing.T) {
	scheme, err := cache.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects()...).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*cnpgv1.DatabaseList); ok {
				return errors.New("cache torn down")
			}
			return c.List(ctx, list, opts...)
		},
	})
	for _, index := range cache.Indexes() {
		builder = builder.WithIndex(index.Object, index.Field, index.Func)
	}
	svc := New(Options{Cache: &fakeCache{reader: builder.Build(), namespaces: []string{"team-a"}}, Now: func() time.Time { return now }})
	o, err := svc.Objects(context.Background(), "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range o.Sources {
		if s.Source == cache.SourceDatabases && s.State != viewmodel.SourceUnavailable {
			t.Errorf("databases source %s after a failed list", s.State)
		}
	}
}
