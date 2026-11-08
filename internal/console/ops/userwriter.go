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

// Package ops is the sole origin of the console's day-2 writes: the
// enumerated operations (backup, reload, restart, promote), each one typed,
// minimal and version-pinned, sent to the API server as the signed-in user.
// There is no generic apply, no spec editing and no free-form patch. The
// writer is constructed only when operations are enabled, so a read-only
// assembly graph contains no mutation path at all.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The console's writer: every call goes to
// the API server as the signed-in user through a client whose transport
// carries the token of the request context (auth.UserConfig); there is
// no other credential behind it and no impersonation. The API server
// authorizes, admits and audits each write under the user's name. The
// mutation surface is exactly the four operations of the catalog, each a
// pinned object or patch built below; reads are live Gets/Lists, never
// the informer cache, because a precondition must not be judged on a
// snapshot seconds old.

// Pinned CloudNativePG interactions (the kubectl cnpg plugin's, verified
// at release-1.30; cnpgstatus/operations.go cites the operator side).
const (
	ReloadAnnotation  = "cnpg.io/reloadedAt"
	RestartAnnotation = "kubectl.kubernetes.io/restartedAt"
	SwitchoverPhase   = "Switchover in progress"
	// CNPGTimestamp is the microsecond RFC 3339 format the plugin's reload
	// and promote write; restart writes plain RFC 3339.
	CNPGTimestamp = "2006-01-02T15:04:05.000000Z07:00"
)

// AnnotationPatch is the minimal merge patch adding one annotation,
// locked to resourceVersion when one is given: the preconditions were
// judged on that version, so a switchover that started since makes the
// write a 409 instead of a restart in the middle of it.
func AnnotationPatch(key, value, resourceVersion string) []byte {
	meta := map[string]any{"annotations": map[string]any{key: value}}
	if resourceVersion != "" {
		meta["resourceVersion"] = resourceVersion
	}
	out, _ := json.Marshal(map[string]any{"metadata": meta})
	return out
}

// PromotePatch is the status merge patch of a switchover, locked to
// resourceVersion (the API server answers 409 when the Cluster changed
// since it was read).
func PromotePatch(instance, timestamp, resourceVersion string) []byte {
	out, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
		"status": map[string]any{
			"targetPrimary": instance, "targetPrimaryTimestamp": timestamp,
			"phase": SwitchoverPhase, "phaseReason": "Switching over to " + instance,
		},
	})
	return out
}

// BackupObject is the on-demand Backup: a server-generated name, the
// Cluster, and the method the Cluster can actually take (an empty method
// is the CRD default barmanObjectStore, which fails on a plugin-only
// Cluster).
func BackupObject(namespace, cluster, generateName, method, plugin string) *cnpgv1.Backup {
	b := &cnpgv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, GenerateName: generateName},
		Spec:       cnpgv1.BackupSpec{Cluster: cnpgv1.LocalObjectReference{Name: cluster}, Method: cnpgv1.BackupMethod(method)},
	}
	if plugin != "" {
		b.Spec.PluginConfiguration = &cnpgv1.BackupPluginConfiguration{Name: plugin}
	}
	return b
}

// WriteError is a write the API server refused or could not take,
// classified for the caller: Status is the HTTP status to answer,
// Text is display-safe (never the server's raw message, which can echo
// object content).
type WriteError struct {
	Status int
	Text   string
	cause  error
}

// Error is the display-safe text.
func (e *WriteError) Error() string { return e.Text }

// Unwrap exposes the API error for in-process branching only.
func (e *WriteError) Unwrap() error { return e.cause }

// HTTPStatus is the status the API answers with.
func (e *WriteError) HTTPStatus() int { return e.Status }

// SafeText is the display-safe text.
func (e *WriteError) SafeText() string { return e.Text }

// OutcomeConflict is the text of a write that lost its optimistic lock.
const OutcomeConflict = "conflict: the cluster changed since it was checked; reload the page and retry"

// classify maps an API error onto a WriteError.
func classify(op string, err error) error {
	if err == nil {
		return nil
	}
	w := &WriteError{Status: 503, Text: op + ": the Kubernetes API server did not take the request", cause: err}
	switch {
	case apierrors.IsForbidden(err) && rbacRefusal(err):
		w.Status, w.Text = 403, op+": the Kubernetes API server refused it for your account"
	case apierrors.IsForbidden(err):
		// Forbidden is also the status of an admission webhook's or
		// policy's denial, an exceeded ResourceQuota and a terminating
		// namespace: not a matter of the person's rights, and not to be
		// read as one.
		w.Status, w.Text = 422, op+": an admission check, a quota or the namespace's state refused the request (not your account's rights)"
	case apierrors.IsUnauthorized(err):
		w.Status, w.Text = 401, op+": your session's token is no longer accepted; sign in again"
	case apierrors.IsConflict(err):
		w.Status, w.Text = 409, op+": "+OutcomeConflict
	case apierrors.IsNotFound(err):
		w.Status, w.Text = 404, op+": the object no longer exists"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		w.Status, w.Text = 422, op+": the API server or an admission check rejected the request"
	case errors.Is(err, context.DeadlineExceeded):
		w.Status, w.Text = 504, op+": the Kubernetes API server did not answer in time"
	}
	return w
}

// rbacRefusal reports whether a Forbidden error is the authorizer's: the
// API server words it `<resource> "<name>" is forbidden: User "<user>"
// cannot <verb> resource ...` (k8s.io/apiserver
// pkg/endpoints/handlers/responsewriters/errors.go forbiddenMessage).
func rbacRefusal(err error) bool {
	var s apierrors.APIStatus
	if !errors.As(err, &s) {
		return false
	}
	msg := s.Status().Message
	return strings.Contains(msg, "is forbidden: User \"") && strings.Contains(msg, "\" cannot ")
}

// UserWriter performs the operations as the user the context carries.
type UserWriter struct {
	c       client.Client
	timeout time.Duration
}

// NewUserWriter wraps a client built over auth.UserConfig: its transport
// sets the user's token from each request's context and refuses a
// context without one.
func NewUserWriter(c client.Client, timeout time.Duration) *UserWriter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &UserWriter{c: c, timeout: timeout}
}

// ReadCluster reads the Cluster live as the user.
func (w *UserWriter) ReadCluster(ctx context.Context, namespace, name string) (*cnpgv1.Cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	c := &cnpgv1.Cluster{}
	if err := w.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, c); err != nil {
		return nil, classify("reading the Cluster", err)
	}
	return c, nil
}

// ListPods lists the Cluster's pods live as the user.
func (w *UserWriter) ListPods(ctx context.Context, namespace, cluster string) ([]corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	list := &corev1.PodList{}
	if err := w.c.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{"cnpg.io/cluster": cluster}); err != nil {
		return nil, classify("listing the Cluster's pods", err)
	}
	return list.Items, nil
}

// CreateBackup creates the on-demand Backup and returns its name.
func (w *UserWriter) CreateBackup(ctx context.Context, b *cnpgv1.Backup) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.c.Create(ctx, b); err != nil {
		return "", 0, classify("creating the Backup", err)
	}
	return b.Name, b.Generation, nil
}

// AnnotateCluster merge-patches one annotation of the Cluster (restart,
// reload) and returns the Cluster's generation after the write.
func (w *UserWriter) AnnotateCluster(ctx context.Context, namespace, name, key, value, resourceVersion string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	c := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := w.c.Patch(ctx, c, client.RawPatch(types.MergePatchType, AnnotationPatch(key, value, resourceVersion))); err != nil {
		return 0, classify("annotating the Cluster", err)
	}
	return c.Generation, nil
}

// Promote writes the switchover request to the status subresource,
// locked to resourceVersion.
func (w *UserWriter) Promote(ctx context.Context, namespace, name, instance, timestamp, resourceVersion string) (int64, error) {
	if resourceVersion == "" {
		return 0, &WriteError{Status: 500, Text: "promote without a resourceVersion lock", cause: errors.New("no lock")}
	}
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	c := &cnpgv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := w.c.Status().Patch(ctx, c, client.RawPatch(types.MergePatchType, PromotePatch(instance, timestamp, resourceVersion))); err != nil {
		return 0, classify("requesting the switchover", err)
	}
	return c.Generation, nil
}

// NewUserClient builds the user client over a configuration from
// auth.UserConfig. Its REST mapping is static (Pods, Clusters, Backups,
// DatabaseRoles):
// a discovery call would carry no user token and be refused by the
// transport, and the console must not fall back to any other identity.
func NewUserClient(userConfig *rest.Config) (client.Client, error) {
	scheme, err := NewUserScheme()
	if err != nil {
		return nil, err
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	mapper.Add(cnpgv1.SchemeGroupVersion.WithKind("Cluster"), meta.RESTScopeNamespace)
	mapper.Add(cnpgv1.SchemeGroupVersion.WithKind("Backup"), meta.RESTScopeNamespace)
	// The per-person roles (pgadmin_databaserole.go).
	mapper.Add(cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole"), meta.RESTScopeNamespace)
	c, err := client.New(userConfig, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, fmt.Errorf("ops: user client: %w", err)
	}
	return c, nil
}

// NewUserScheme is the scheme of the user client: core and CNPG.
func NewUserScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("ops: scheme: %w", err)
	}
	if err := cnpgv1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("ops: scheme: %w", err)
	}
	return s, nil
}
