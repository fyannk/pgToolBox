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

package operations

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

var now = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

var errNotWatched = errors.New("not watched")

func cluster() *cnpgv1.Cluster {
	t := true
	return &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "orders", UID: "u1", ResourceVersion: "41"},
		Spec: cnpgv1.ClusterSpec{Instances: 3, Plugins: []cnpgv1.PluginConfiguration{{
			Name: "barman-cloud.cloudnative-pg.io", IsWALArchiver: &t, Parameters: map[string]string{"barmanObjectName": "store"},
		}}},
		Status: cnpgv1.ClusterStatus{
			Phase: "Cluster in healthy state", CurrentPrimary: "orders-1", TargetPrimary: "orders-1",
			InstanceNames: []string{"orders-1", "orders-2", "orders-3"},
		},
	}
}

func instancePod(name, role string) corev1.Pod {
	t := true
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: name,
		Labels:          map[string]string{"cnpg.io/cluster": "orders", "cnpg.io/podRole": "instance", "cnpg.io/instanceRole": role},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: "orders", UID: "u1", Controller: &t}},
	}}
}

type fakeState struct{ st ClusterState }

func (f *fakeState) OperationState(_ context.Context, ns, name string) (ClusterState, error) {
	if ns != "team-a" || name != "orders" {
		return ClusterState{}, errNotWatched
	}
	return f.st, nil
}

type fakeWriter struct {
	c       *cnpgv1.Cluster
	pods    []corev1.Pod
	err     error
	backups []*cnpgv1.Backup
	patches []string
	drills  []*cnpgv1.Cluster
}

func (f *fakeWriter) CreateDrillCluster(_ context.Context, c *cnpgv1.Cluster) (types.UID, error) {
	if f.err != nil {
		return "", f.err
	}
	f.drills = append(f.drills, c)
	return "drill-uid", nil
}

func (f *fakeWriter) ReadCluster(context.Context, string, string) (*cnpgv1.Cluster, error) {
	return f.c.DeepCopy(), nil
}

func (f *fakeWriter) ListPods(context.Context, string, string) ([]corev1.Pod, error) {
	return f.pods, nil
}

func (f *fakeWriter) CreateBackup(_ context.Context, b *cnpgv1.Backup) (string, int64, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	f.backups = append(f.backups, b)
	return b.GenerateName + "abcde", 1, nil
}

func (f *fakeWriter) AnnotateCluster(_ context.Context, _, _, key, value, _ string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.patches = append(f.patches, key+"="+value)
	return 7, nil
}

func (f *fakeWriter) Promote(_ context.Context, _, _, instance, stamp, rv string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.patches = append(f.patches, "promote="+instance+"@"+rv)
	return 7, nil
}

func service(st ClusterState, w *fakeWriter, log *bytes.Buffer) *Service {
	return New(Options{
		State: &fakeState{st: st}, Writer: w, Now: func() time.Time { return now },
		IsNotFound: func(err error) bool { return errors.Is(err, errNotWatched) },
		Logger:     slog.New(slog.NewTextHandler(log, nil)),
	})
}

func principal() *auth.Principal {
	return &auth.Principal{User: authz.User{Name: "jane"}, SessionID: "s1", Token: "secret-token-value"}
}

func pods() []corev1.Pod {
	return []corev1.Pod{instancePod("orders-1", "primary"), instancePod("orders-2", "replica"), instancePod("orders-3", "replica")}
}

func TestRequest(t *testing.T) {
	ctx := context.Background()
	fenced := cluster()
	fenced.Annotations = map[string]string{"cnpg.io/fencedInstances": `["orders-3"]`}
	hibernated := cluster()
	hibernated.Annotations = map[string]string{"cnpg.io/hibernation": "on"}
	switching := cluster()
	switching.Status.TargetPrimary = "orders-2"

	tests := []struct {
		name    string
		c       *cnpgv1.Cluster
		req     viewmodel.OperationRequest
		status  int // 0: accepted
		patches string
	}{
		{"backup with the plugin", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationBackup, Confirmation: "orders"}, 0, ""},
		{"backup of a hibernated cluster", hibernated, viewmodel.OperationRequest{Kind: viewmodel.OperationBackup, Confirmation: "orders"}, http.StatusConflict, ""},
		{"restart", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationRestart, Confirmation: "orders"}, 0, "kubectl.kubernetes.io/restartedAt=2026-10-29T12:00:00Z"},
		{"restart during a switchover", switching, viewmodel.OperationRequest{Kind: viewmodel.OperationRestart, Confirmation: "orders"}, http.StatusConflict, ""},
		{"reload during a switchover", switching, viewmodel.OperationRequest{Kind: viewmodel.OperationReload, Confirmation: "orders"}, 0, "cnpg.io/reloadedAt=2026-10-29T12:00:00.000000Z"},
		{"promote a replica", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-2", Confirmation: "orders-2"}, 0, "promote=orders-2@41"},
		{"promote a fenced replica", fenced, viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-3", Confirmation: "orders-3"}, http.StatusConflict, ""},
		{"promote the primary", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "orders-1", Confirmation: "orders-1"}, http.StatusConflict, ""},
		{"promote a stranger", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationPromote, Target: "other-1", Confirmation: "other-1"}, http.StatusConflict, ""},
		{"mistyped confirmation", cluster(), viewmodel.OperationRequest{Kind: viewmodel.OperationRestart, Confirmation: "order"}, http.StatusUnprocessableEntity, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			w := &fakeWriter{c: tt.c, pods: pods()}
			svc := service(ClusterState{Cluster: tt.c}, w, &log)
			acc, err := svc.Request(ctx, principal(), "team-a", "orders", tt.req)
			if tt.status != 0 {
				var r interface{ HTTPStatus() int }
				if !errors.As(err, &r) || r.HTTPStatus() != tt.status {
					t.Fatalf("err %v, want status %d", err, tt.status)
				}
				if len(w.patches)+len(w.backups) != 0 {
					t.Errorf("a refused request wrote: %v %v", w.patches, w.backups)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.patches != "" && (len(w.patches) != 1 || w.patches[0] != tt.patches) {
				t.Errorf("patches %v, want %s", w.patches, tt.patches)
			}
			id, err := ParseID(acc.OperationID)
			if err != nil || id.Kind != tt.req.Kind || id.Cluster != "orders" {
				t.Errorf("handle %q: %+v %v", acc.OperationID, id, err)
			}
			if tt.req.Kind == viewmodel.OperationBackup {
				b := w.backups[0]
				if b.Spec.Method != "plugin" || b.Spec.PluginConfiguration == nil || b.Spec.PluginConfiguration.Name != "barman-cloud.cloudnative-pg.io" || b.Spec.Cluster.Name != "orders" {
					t.Errorf("backup %+v", b.Spec)
				}
				if acc.Created == "" || id.Created != acc.Created {
					t.Errorf("created %q / %q", acc.Created, id.Created)
				}
			}
			if !strings.Contains(log.String(), "user=jane") || strings.Contains(log.String(), "secret-token-value") {
				t.Errorf("audit line: %s", log.String())
			}
		})
	}
}

func TestRequestUnknownCluster(t *testing.T) {
	var log bytes.Buffer
	svc := service(ClusterState{Cluster: cluster()}, &fakeWriter{c: cluster()}, &log)
	_, err := svc.Request(context.Background(), principal(), "team-a", "gone", viewmodel.OperationRequest{Kind: viewmodel.OperationBackup, Confirmation: "gone"})
	var r *Rejected
	if !errors.As(err, &r) || r.Status != http.StatusNotFound {
		t.Errorf("err %v", err)
	}
}

func TestWriteErrorsPassThrough(t *testing.T) {
	var log bytes.Buffer
	w := &fakeWriter{c: cluster(), err: &ops.WriteError{Status: http.StatusForbidden, Text: "refused for your account"}}
	svc := service(ClusterState{Cluster: cluster()}, w, &log)
	_, err := svc.Request(context.Background(), principal(), "team-a", "orders", viewmodel.OperationRequest{Kind: viewmodel.OperationReload, Confirmation: "orders"})
	var r interface{ HTTPStatus() int }
	if !errors.As(err, &r) || r.HTTPStatus() != http.StatusForbidden {
		t.Errorf("err %v", err)
	}
	if !strings.Contains(log.String(), "outcome=\"refused Forbidden\"") {
		t.Errorf("audit: %s", log.String())
	}
}

func TestOffers(t *testing.T) {
	c := cluster()
	c.Annotations = map[string]string{"cnpg.io/fencedInstances": `["orders-3"]`}
	st := ClusterState{Cluster: c, Pods: pods(), Instances: []InstanceState{
		{Name: "orders-1", Health: viewmodel.HealthOK}, {Name: "orders-2", Health: viewmodel.HealthOK}, {Name: "orders-3", Health: viewmodel.HealthInactive},
	}}
	var log bytes.Buffer
	svc := service(st, &fakeWriter{c: c}, &log)
	caps := authz.Capabilities{View: true, Reload: true, Restart: true, Undecided: []string{"promote"}}
	v, err := svc.Offers(context.Background(), principal(), caps, "team-a", "orders")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range v.Offers {
		switch o.Kind {
		case viewmodel.OperationBackup:
			if o.Allowed {
				t.Error("backup allowed without create backups")
			}
		case viewmodel.OperationReload:
			if !o.Allowed || o.Observable {
				t.Errorf("reload %+v", o)
			}
		case viewmodel.OperationPromote:
			if o.Allowed || !o.Undecided || len(o.Targets) != 2 {
				t.Fatalf("promote %+v", o)
			}
			if !o.Targets[0].Eligible || o.Targets[1].Eligible {
				t.Errorf("targets %+v", o.Targets)
			}
		case viewmodel.OperationRestart, viewmodel.OperationRestoreDrill:
			// Not what this test is about.
		}
	}
}

func TestLookup(t *testing.T) {
	c := cluster()
	accepted := now.Add(-time.Minute)
	backup := &cnpgv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "ondemand-1"}, Status: cnpgv1.BackupStatus{Phase: cnpgv1.BackupPhaseCompleted}}
	tests := []struct {
		name  string
		id    ID
		st    ClusterState
		at    time.Time
		phase viewmodel.OperationPhase
	}{
		{"backup completed", ID{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: "ondemand-1", AcceptedAt: accepted.Unix()}, ClusterState{Cluster: c, Backups: []*cnpgv1.Backup{backup}}, now, viewmodel.OpPhaseSettled},
		{"backup not seen yet", ID{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: "ondemand-2", AcceptedAt: accepted.Unix()}, ClusterState{Cluster: c}, now, viewmodel.OpPhaseAccepted},
		{"promote settled", ID{Kind: viewmodel.OperationPromote, Namespace: "team-a", Cluster: "orders", Instance: "orders-1", Marker: "2026-10-29T11:59:00.000000Z", AcceptedAt: accepted.Unix()}, ClusterState{Cluster: c}, now, viewmodel.OpPhaseSettled},
		{"reload not observable", ID{Kind: viewmodel.OperationReload, Namespace: "team-a", Cluster: "orders", Marker: "2026-10-29T11:59:00.000000Z", AcceptedAt: accepted.Unix()}, ClusterState{Cluster: withAnnotation(c, "cnpg.io/reloadedAt", "2026-10-29T11:59:00.000000Z")}, now, viewmodel.OpPhaseNotObservable},
		{"backup timed out", ID{Kind: viewmodel.OperationBackup, Namespace: "team-a", Cluster: "orders", Created: "ondemand-3", AcceptedAt: now.Add(-2 * time.Hour).Unix()}, ClusterState{Cluster: c}, now, viewmodel.OpPhaseTimedOut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			svc := service(tt.st, &fakeWriter{c: c}, &log)
			op, err := svc.Lookup(context.Background(), tt.id)
			if err != nil {
				t.Fatal(err)
			}
			if op.Phase != tt.phase {
				t.Errorf("phase %s, want %s (steps %+v)", op.Phase, tt.phase, op.Steps)
			}
			if want := viewmodel.OperationHealth(tt.phase, op.Blocked); op.Health != want {
				t.Errorf("health %s, want %s for phase %s", op.Health, want, tt.phase)
			}
			if op.Steps[0].Phase != viewmodel.OpPhaseRequested || op.Steps[len(op.Steps)-1].Phase != tt.phase {
				t.Errorf("steps %+v", op.Steps)
			}
		})
	}
}

func withAnnotation(c *cnpgv1.Cluster, k, v string) *cnpgv1.Cluster {
	c = c.DeepCopy()
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[k] = v
	return c
}

// TestAuditOrigin: the audit line says which face asked; an MCP request
// names its client and grant, a request of the console's own screens
// says via=api and nothing more.
func TestAuditOrigin(t *testing.T) {
	req := viewmodel.OperationRequest{Kind: viewmodel.OperationReload, Confirmation: "orders"}
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		want     []string
		wantNone []string
	}{
		{"api", context.Background(), []string{"via=api"}, []string{"assistant=", "grant="}},
		{
			"mcp", WithOrigin(context.Background(), Origin{Via: ViaMCP, Assistant: "Claude Code", Grant: "grant-0123"}),
			[]string{"via=mcp", `assistant="Claude Code"`, "grant=grant-0123"},
			nil,
		},
		{
			"mcp, a long name", WithOrigin(context.Background(), Origin{Via: ViaMCP, Assistant: strings.Repeat("é", 100)}),
			[]string{"via=mcp", "assistant=" + strings.Repeat("é", 64) + " "},
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			svc := service(ClusterState{Cluster: cluster()}, &fakeWriter{c: cluster()}, &log)
			if _, err := svc.Request(tc.ctx, principal(), "team-a", "orders", req); err != nil {
				t.Fatal(err)
			}
			for _, s := range tc.want {
				if !strings.Contains(log.String(), s) {
					t.Errorf("audit line lacks %q: %s", s, log.String())
				}
			}
			for _, s := range tc.wantNone {
				if strings.Contains(log.String(), s) {
					t.Errorf("audit line has %q: %s", s, log.String())
				}
			}
		})
	}
}
