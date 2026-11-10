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

package console

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// indexReferencedSecret holds the names of the user Secrets a PgToolBoxConsole
// mounts (referencedSecrets).
const indexReferencedSecret = "pgtoolbox-console.referencedSecrets" //nolint:gosec // a field index name, not a credential

// configMapMissingRetry is how often a console stalled on a missing
// referenced ConfigMap looks for it again. Unlike the Secret informer, the
// operator's ConfigMap informer is label-filtered (managed-by=pgtoolbox),
// so it never sees a user's ConfigMap appear: there is no event to wait
// for, and unfiltering it would cache the metadata of every ConfigMap in
// the cluster.
const configMapMissingRetry = 2 * time.Minute

// objectReference is a user object the console's Pod mounts: the spec
// field that names it, for the message, and the object's name.
type objectReference struct {
	Field, Name string
}

// references builds a reference table: one line per field.
type references []objectReference

func (r *references) add(field, name string) {
	if name != "" {
		*r = append(*r, objectReference{Field: field, Name: name})
	}
}

func (r *references) keyRef(field string, ref *pgtoolboxv1alpha1.SecretKeyReference) {
	if ref != nil {
		r.add(field, ref.Name)
	}
}

func (r *references) localRef(field string, ref *pgtoolboxv1alpha1.LocalObjectReference) {
	if ref != nil {
		r.add(field, ref.Name)
	}
}

// referencedSecrets are the user Secrets consoleVolumes mounts, under the
// same conditions, in the field order the message reports them. A mounted
// Secret that does not exist holds the Pod in ContainerCreating; the
// reconciler reports it as ConfigurationValid=False instead. A new Secret
// reference is one line here (TestReferencesAreTheMountedOnes holds the
// table to the builder). Not listed: the session Secret (the operator's
// own), image pull Secrets (the kubelet goes on without a missing one) and
// the Ingress certificate (the ingress controller's to read).
func referencedSecrets(c *pgtoolboxv1alpha1.PgToolBoxConsole) []objectReference {
	var refs references
	provider := c.AuthProvider()
	if oidc := c.Spec.Auth.OIDC; provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC && oidc != nil {
		refs.add("spec.auth.oidc.clientSecretRef", oidc.ClientSecretRef.Name)
		if g := oidc.GrantClient; g != nil {
			refs.add("spec.auth.oidc.grantClient.clientSecretRef", g.ClientSecretRef.Name)
		}
	}
	if openshift := c.Spec.Auth.OpenShift; provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift && openshift != nil {
		refs.keyRef("spec.auth.openshift.clientSecretRef", openshift.ClientSecretRef)
	}
	if prom := c.Spec.Prometheus; prom != nil {
		refs.keyRef("spec.prometheus.tokenSecretRef", prom.TokenSecretRef)
	}
	if d := c.Spec.Digest.Delivery; d != nil {
		refs.keyRef("spec.digest.delivery.chatWebhookSecretRef", d.ChatWebhookSecretRef)
		refs.keyRef("spec.digest.delivery.webhookSecretRef", d.WebhookSecretRef)
		if m := d.SMTP; m != nil && m.Username != "" {
			refs.keyRef("spec.digest.delivery.smtp.passwordSecretRef", m.PasswordSecretRef)
		}
	}
	if c.TracingEnabled() {
		refs.keyRef("spec.tracing.headersSecretRef", c.Spec.Tracing.HeadersSecretRef)
	}
	refs.keyRef("spec.restoreDrills.sealKeySecretRef", drillSealKeyRef(c))
	return refs
}

// referencedConfigMaps are the user ConfigMaps consoleVolumes mounts, as
// referencedSecrets. The CA ConfigMaps are mounted without optional, so a
// missing one holds the Pod in ContainerCreating. instanceCABundleRef and
// lifecycleConfigMapRef are mounted optional (the operator's own
// <name>-instance-ca and <name>-lifecycle may not exist yet), so a missing
// one would start a console that silently reads every Cluster as "not
// verifiable" or has no end-of-life table: named by the user, its absence
// is a configuration error all the same. Not listed: kube-root-ca.crt and
// openshift-service-ca.crt (the platform's, in every namespace) and the
// ConfigMaps the operator keeps itself.
func referencedConfigMaps(c *pgtoolboxv1alpha1.PgToolBoxConsole) []objectReference {
	var refs references
	provider := c.AuthProvider()
	if oidc := c.Spec.Auth.OIDC; provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC && oidc != nil {
		refs.localRef("spec.auth.oidc.caConfigMapRef", oidc.CAConfigMapRef)
	}
	if prom := c.Spec.Prometheus; prom != nil {
		refs.localRef("spec.prometheus.caConfigMapRef", prom.CAConfigMapRef)
	}
	if openshift := c.Spec.Auth.OpenShift; provider == pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift && openshift != nil {
		refs.localRef("spec.auth.openshift.caConfigMapRef", openshift.CAConfigMapRef)
	}
	refs.localRef("spec.instanceCABundleRef", c.Spec.InstanceCABundleRef)
	refs.localRef("spec.lifecycleConfigMapRef", c.Spec.LifecycleConfigMapRef)
	return refs
}

// referencedSecretNames is the field index of referencedSecrets.
func referencedSecretNames(object client.Object) []string {
	refs := referencedSecrets(object.(*pgtoolboxv1alpha1.PgToolBoxConsole))
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

// checkReferences stalls with ConfigurationValid=False when an object the
// Pod would mount does not exist: SecretNotFound first, then
// ConfigMapNotFound, each message naming every missing object and its
// field. It reads metadata past the cache, never content. The Secret watch
// (forSecret) brings the console back when a Secret is created; a missing
// ConfigMap is looked for again after configMapMissingRetry. A Secret or
// ConfigMap without the referenced key is the Pod's to report
// (SecretKeyMissing on WorkloadReady): seeing the key would mean reading
// the data.
func (r *Reconciler) checkReferences(ctx context.Context, console *pgtoolboxv1alpha1.PgToolBoxConsole) error {
	for _, check := range []struct {
		kind   string
		refs   []objectReference
		reason pgtoolboxv1alpha1.Reason
	}{
		{"Secret", referencedSecrets(console), pgtoolboxv1alpha1.ReasonSecretNotFound},
		{"ConfigMap", referencedConfigMaps(console), pgtoolboxv1alpha1.ReasonConfigMapNotFound},
	} {
		missing, err := r.missingReferences(ctx, console.Namespace, check.kind, check.refs)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return r.invalid(console, check.reason, fmt.Sprintf(
				"%s does not exist in namespace %s; create it, or change the reference: the console needs it",
				strings.Join(missing, ", "), console.Namespace))
		}
	}
	return nil
}

// missingReferences lists, as "<field>: <kind> <name>", the references to
// objects of kind that do not exist in namespace.
func (r *Reconciler) missingReferences(ctx context.Context, namespace, kind string, refs []objectReference) ([]string, error) {
	var missing []string
	for _, ref := range refs {
		object := &metav1.PartialObjectMetadata{}
		object.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind))
		err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, object)
		switch {
		case apierrors.IsNotFound(err):
			missing = append(missing, fmt.Sprintf("%s: %s %s", ref.Field, kind, ref.Name))
		case err != nil:
			return nil, fmt.Errorf("check %s %s (%s): %w", kind, ref.Name, ref.Field, err)
		}
	}
	return missing, nil
}

// forSecret maps a Secret event to the PgToolBoxConsoles that need it: those
// that mount it, and the mirroring ones when a Cluster uses it as its
// server CA Secret. One mapping for the one metadata-only Secret informer.
func (r *Reconciler) forSecret(ctx context.Context, object client.Object) []ctrlreconcile.Request {
	requests := r.forCASecret(ctx, object)
	var consoles pgtoolboxv1alpha1.PgToolBoxConsoleList
	if err := r.Client.List(ctx, &consoles, client.InNamespace(object.GetNamespace()),
		client.MatchingFields{indexReferencedSecret: object.GetName()}); err != nil {
		return requests
	}
	for i := range consoles.Items {
		request := ctrlreconcile.Request{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: consoles.Items[i].Name}}
		if !slices.Contains(requests, request) {
			requests = append(requests, request)
		}
	}
	return requests
}
