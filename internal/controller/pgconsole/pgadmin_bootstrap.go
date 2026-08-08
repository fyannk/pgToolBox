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

package pgconsole

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// pgAdmin refuses to start without an initial administrator account, and
// initializing its settings database is what makes `setup.py` usable at all —
// which is how the admin-sync sidecar adds every real user. So this account
// is not a convenience: without it the container crash-loops on
//
//	You need to define the PGADMIN_DEFAULT_EMAIL and PGADMIN_DEFAULT_PASSWORD
//	or PGADMIN_DEFAULT_PASSWORD_FILE environment variables.
//
// and no user is ever provisioned.
//
// Nobody signs in with it. Users reach pgAdmin through the proxy, which is
// the only authentication boundary, and their accounts arrive over the
// admin-sync API. The credential therefore exists only to unlock the
// settings database: the operator generates it once per console, never
// reports it, and no human is ever expected to read it.
const (
	// pgAdminBootstrapSuffix names the per-console bootstrap Secret.
	pgAdminBootstrapSuffix = "-pgadmin-bootstrap"

	// pgAdminBootstrapEmail is the account's login. It is a constant
	// because it is not a contact address and no mail is ever sent to it.
	// pgAdmin still validates it, and rejects the reserved domains
	// (.invalid, .localhost, .test, .example) that would otherwise be the
	// honest choice for an address nobody can write to — so it uses the
	// project's own domain, the one the API group already carries.
	pgAdminBootstrapEmail = "pgtoolbox@pgtoolbox.fyannk.dev"

	// pgAdminBootstrapPasswordKey is the Secret key holding the generated
	// password, and the file name it is mounted under.
	pgAdminBootstrapPasswordKey = "password"

	// pgAdminBootstrapMountPath is where the password file is mounted. It
	// is passed to pgAdmin as a file rather than a value so the password
	// never appears in the Pod spec, an env dump, or a crash log.
	pgAdminBootstrapMountPath = "/run/secrets/pgadmin-bootstrap" // #nosec G101 -- mount path, not a credential.

	// pgAdminBootstrapVolume is the volume carrying that file.
	pgAdminBootstrapVolume = "pgadmin-bootstrap"
)

// pgAdminBootstrapSecretName is the name of a console's bootstrap Secret.
func pgAdminBootstrapSecretName(consoleName string) string {
	return application.ResourceName(consoleName, pgAdminBootstrapSuffix)
}

// reconcilePgAdminBootstrapSecret ensures the bootstrap credential exists,
// generating it exactly once. It is never rotated on reconcile: the password
// is baked into pgAdmin's settings database at first start, so regenerating
// it would leave the Secret and the database disagreeing without unlocking
// anything — the same stability the console's session key has.
func (r *Reconciler) reconcilePgAdminBootstrapSecret(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) error {
	key := client.ObjectKey{
		Namespace: console.Namespace,
		Name:      pgAdminBootstrapSecretName(console.Name),
	}

	// APIReader, not the cache: Secret contents are deliberately kept out of
	// the informer cache, so the live object is the only source.
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, key, &existing)
	if err == nil {
		if len(existing.Data[pgAdminBootstrapPasswordKey]) > 0 {
			return nil
		}
		return fmt.Errorf(
			"pgAdmin bootstrap secret %s/%s has no %s; delete it to regenerate",
			key.Namespace, key.Name, pgAdminBootstrapPasswordKey)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	password, err := generatePgAdminBootstrapPassword()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Data: map[string][]byte{pgAdminBootstrapPasswordKey: password},
	}
	if err := controllerutil.SetControllerReference(console, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pgAdmin bootstrap secret: %w", err)
	}
	return nil
}

// generatePgAdminBootstrapPassword returns 32 bytes of entropy in a form
// pgAdmin's password policy accepts.
func generatePgAdminBootstrapPassword() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate pgAdmin bootstrap password: %w", err)
	}
	return []byte(base64.RawURLEncoding.EncodeToString(raw)), nil
}
