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

package server

import (
	"context"
	"fmt"
	"net/http"
	"unicode/utf8"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgtoolbox/internal/proxy/pages"
)

// maxMessageRunes mirrors the PgToolBoxAccessRequest message MaxLength.
const maxMessageRunes = 1024

// AccessRequestCreator creates a PgToolBoxAccessRequest for subject. It
// is the only Kubernetes API use of the proxy.
type AccessRequestCreator interface {
	CreateAccessRequest(ctx context.Context, consoleName, namespace, subject, message string) error
}

// kubeAccessRequestCreator is the production creator, backed by a
// controller-runtime client from the in-cluster configuration. The proxy
// service account holds only create permission on accessrequests.
type kubeAccessRequestCreator struct {
	client ctrlclient.Client
}

// NewInClusterAccessRequestCreator builds a creator from the in-cluster
// REST config; it errors when the proxy is not running in a cluster.
func NewInClusterAccessRequestCreator() (AccessRequestCreator, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering pgtoolbox API: %w", err)
	}
	c, err := ctrlclient.New(restCfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}
	return &kubeAccessRequestCreator{client: c}, nil
}

// CreateAccessRequest implements AccessRequestCreator.
func (k *kubeAccessRequestCreator) CreateAccessRequest(ctx context.Context, consoleName, namespace, subject, message string) error {
	req := &pgtoolboxv1alpha1.PgToolBoxAccessRequest{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pgreq-",
			Namespace:    namespace,
		},
		Spec: pgtoolboxv1alpha1.PgToolBoxAccessRequestSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: consoleName},
			Subject:      subject,
			Message:      message,
		},
	}
	return k.client.Create(ctx, req)
}

// handleAccessRequest processes the request-access form POST. The form
// carries an HMAC CSRF token bound to the caller's session; it is
// validated before any Kubernetes call.
func (e *Env) handleAccessRequest(w http.ResponseWriter, r *http.Request) {
	rt := e.Runtime()
	arc := rt.Config.AccessRequest
	sess, err := rt.Codec.ReadCookie(r, rt.Config.Session.CookieName)
	if err != nil {
		pages.Error(w, http.StatusUnauthorized, "Authentication is required.")
		return
	}
	if !arc.Enabled || e.AccessRequests == nil {
		pages.Error(w, http.StatusForbidden, "Access requests are not enabled on this console.")
		return
	}
	if err := r.ParseForm(); err != nil {
		pages.Error(w, http.StatusBadRequest, "The form could not be parsed.")
		return
	}
	if !rt.Codec.ValidCSRF(sess, r.PostFormValue("csrf")) {
		pages.Error(w, http.StatusForbidden, "The form token is invalid; reload the page and try again.")
		return
	}
	message := truncateRunes(r.PostFormValue("message"), maxMessageRunes)
	err = e.AccessRequests.CreateAccessRequest(r.Context(), arc.ConsoleName, arc.Namespace, sess.Subject, message)
	switch {
	case err == nil:
		// created
	case apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err):
		// A pending request for this identity already exists; do not
		// leak its state, just confirm as if newly created.
		e.Logger.Info("access request already pending", "subject", sess.Subject)
	default:
		e.Logger.Error("creating access request failed", "subject", sess.Subject, "error", err)
		pages.Error(w, http.StatusInternalServerError, "Your request could not be recorded; please contact your administrator.")
		return
	}
	pages.Confirmation(w, pages.ConfirmationData{Subject: sess.Subject})
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}
