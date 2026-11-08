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
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestPinnedPatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{"annotation, locked", AnnotationPatch(RestartAnnotation, "t", "42"), `{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"t"},"resourceVersion":"42"}}`},
		{"annotation, unlocked", AnnotationPatch(ReloadAnnotation, "t", ""), `{"metadata":{"annotations":{"cnpg.io/reloadedAt":"t"}}}`},
		{"promote", PromotePatch("orders-2", "ts", "7"), `{"metadata":{"resourceVersion":"7"},"status":{"phase":"Switchover in progress","phaseReason":"Switching over to orders-2","targetPrimary":"orders-2","targetPrimaryTimestamp":"ts"}}`},
	}
	for _, tt := range tests {
		if string(tt.got) != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.name, tt.got, tt.want)
		}
	}
}

func TestBackupObject(t *testing.T) {
	t.Parallel()
	b := BackupObject("team-a", "orders", "ondemand-", "plugin", "barman-cloud.cloudnative-pg.io")
	if b.Namespace != "team-a" || b.GenerateName != "ondemand-" || b.Name != "" || b.Spec.Cluster.Name != "orders" ||
		b.Spec.Method != cnpgv1.BackupMethodPlugin || b.Spec.PluginConfiguration == nil || b.Spec.PluginConfiguration.Name != "barman-cloud.cloudnative-pg.io" {
		t.Errorf("plugin backup = %+v", b)
	}
	if b := BackupObject("team-a", "orders", "x-", "", ""); b.Spec.Method != "" || b.Spec.PluginConfiguration != nil {
		t.Errorf("default backup = %+v", b.Spec)
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	gr := schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "clusters"}
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{"forbidden by RBAC", apierrors.NewForbidden(gr, "orders", errors.New(`User "oidc:dba" cannot patch resource "clusters" in API group "postgresql.cnpg.io" in the namespace "team-a": secret detail`)), http.StatusForbidden},
		{"forbidden by RBAC, on a create", apierrors.NewForbidden(schema.GroupResource{Group: gr.Group, Resource: "backups"}, "", errors.New(`User "oidc:dba" cannot create resource "backups" in API group "postgresql.cnpg.io" in the namespace "team-a"`)), http.StatusForbidden},
		{"denied by a webhook", apierrors.NewForbidden(gr, "orders", errors.New(`admission webhook "policy.example" denied the request: secret detail`)), http.StatusUnprocessableEntity},
		{"over a quota", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "orders-4", errors.New("exceeded quota: secret detail")), http.StatusUnprocessableEntity},
		{"a terminating namespace", apierrors.NewForbidden(gr, "orders", errors.New("unable to create new content in namespace team-a because it is being terminated")), http.StatusUnprocessableEntity},
		{"unauthorized", apierrors.NewUnauthorized("x"), http.StatusUnauthorized},
		{"conflict", apierrors.NewConflict(gr, "orders", errors.New("x")), http.StatusConflict},
		{"not found", apierrors.NewNotFound(gr, "orders"), http.StatusNotFound},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: gr.Group, Kind: "Cluster"}, "orders", nil), http.StatusUnprocessableEntity},
		{"bad request", apierrors.NewBadRequest("x"), http.StatusUnprocessableEntity},
		{"deadline", fmt.Errorf("wrapped: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{"other", errors.New("connection reset"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		err := classify("restart", tt.err)
		var we *WriteError
		if !errors.As(err, &we) || we.HTTPStatus() != tt.status {
			t.Errorf("%s: %v (status %v), want %d", tt.name, err, we, tt.status)
			continue
		}
		if !errors.Is(err, tt.err) {
			t.Errorf("%s: the cause is not kept for in-process branching", tt.name)
		}
		if we.SafeText() == "" || containsAny(we.SafeText(), "secret detail", "connection reset") {
			t.Errorf("%s: text %q echoes the server's message", tt.name, we.SafeText())
		}
	}
	if classify("x", nil) != nil {
		t.Error("nil became an error")
	}
}

func containsAny(s string, subs ...string) bool {
	return slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(s, sub) })
}
