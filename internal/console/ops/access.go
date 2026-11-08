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
	"fmt"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// AccessWriter is the access-request half of the console's mutation
// surface: every call goes to the API server as the
// signed-in user through a client over auth.UserConfig, exactly like
// UserWriter. Its writes are three, each pinned below: create a
// PgAccessRequest, create the RoleBinding an approval resolves to (built
// by the API package's one builder, which the operator also checks
// against), and update a request's status with the decision, locked to
// the resourceVersion the decision was judged on. The API server's RBAC,
// its RoleBinding escalation check and the CRD's CEL rules decide; the
// console decides nothing.
type AccessWriter struct {
	c       client.Client
	timeout time.Duration
}

// NewAccessWriter wraps a client from NewAccessUserClient.
func NewAccessWriter(c client.Client, timeout time.Duration) *AccessWriter {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &AccessWriter{c: c, timeout: timeout}
}

// ReadAccessRequest reads one request live as the user: a decision is
// judged on the object as it is, never on the cache.
func (w *AccessWriter) ReadAccessRequest(ctx context.Context, namespace, name string) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	r := &pgtoolboxv1alpha1.PgAccessRequest{}
	if err := w.c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, r); err != nil {
		return nil, classify("reading the access request", err)
	}
	return r, nil
}

// CreateAccessRequest creates r (GenerateName set by the caller) and
// returns the stored object.
func (w *AccessWriter) CreateAccessRequest(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) (*pgtoolboxv1alpha1.PgAccessRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.c.Create(ctx, r); err != nil {
		return nil, classify("creating the access request", err)
	}
	return r, nil
}

// CreateGrant creates the RoleBinding r resolves to; an existing binding
// is never overwritten. One of that name is the approval's own when it is
// retried after its status write failed: success when the caller may read
// it and it grants exactly r, a conflict when it grants anything else.
// pgtoolbox-console-operator holds create, not get, on RoleBindings, so most
// approvers cannot look: their retry proceeds, and the operator, which
// trusts only a binding the request controls and that matches it,
// reports any other as RoleBindingMissing (and deletes a controlled one
// that drifted).
func (w *AccessWriter) CreateGrant(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	binding := r.DesiredRoleBinding()
	err := w.c.Create(ctx, binding)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return classify("creating the RoleBinding", err)
	}
	existing := &rbacv1.RoleBinding{}
	switch getErr := w.c.Get(ctx, client.ObjectKeyFromObject(binding), existing); {
	case apierrors.IsForbidden(getErr):
		return nil
	case getErr == nil && r.GrantMatches(existing):
		return nil
	}
	return &WriteError{Status: 409, Text: "creating the RoleBinding: " + binding.Name + " already exists and is not this request's grant", cause: err}
}

// RecordDecision writes r's status as the user with an update: the
// resourceVersion r was read at is the lock, so a decision someone else
// recorded in between makes this a 409, never an overwrite.
func (w *AccessWriter) RecordDecision(ctx context.Context, r *pgtoolboxv1alpha1.PgAccessRequest) error {
	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	if err := w.c.Status().Update(ctx, r); err != nil {
		return classify("recording the decision", err)
	}
	return nil
}

// NewAccessUserClient builds the access writer's client over a
// configuration from auth.UserConfig, with a static REST mapping
// (PgAccessRequests, RoleBindings): a discovery call would carry no user
// token, and the console must not fall back to any other identity.
func NewAccessUserClient(userConfig *rest.Config) (client.Client, error) {
	scheme := runtime.NewScheme()
	if err := rbacv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("ops: access scheme: %w", err)
	}
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("ops: access scheme: %w", err)
	}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(rbacv1.SchemeGroupVersion.WithKind("RoleBinding"), meta.RESTScopeNamespace)
	mapper.Add(pgtoolboxv1alpha1.GroupVersion.WithKind("PgAccessRequest"), meta.RESTScopeNamespace)
	c, err := client.New(userConfig, client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		return nil, fmt.Errorf("ops: access user client: %w", err)
	}
	return c, nil
}
