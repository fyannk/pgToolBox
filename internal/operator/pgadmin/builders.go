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

package pgadmin

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// Pure builders: each takes a deep copy of the PgAdmin and the resolved
// inputs, reads nothing from the cluster, and returns the complete desired
// object with its TypeMeta, labels, owner reference and spec-hash
// annotation, ready for server-side apply.

// buildInputs is what the builders need beyond the PgAdmin itself.
type buildInputs struct {
	// ConfigDatabase is the settings database state with
	// spec.configDatabase (zero without).
	ConfigDatabase configDatabaseState
	// Replicas is the Deployment's replica count this loop: 0 while the
	// settings database is not ready, 1 until pgAdmin runs the desired
	// image and registered once (a single Pod creates or migrates the
	// settings schema), spec.replicas after.
	Replicas int32
	// Image is the pgAdmin image the Deployment runs.
	Image specs.ResolvedImage
	// ProxyImage is the OpenShift oauth-proxy image.
	ProxyImage specs.ResolvedImage
	// OperatorImage is copied into the Pod for passfile-sync.
	OperatorImage string
	// PlatformAssignsGroups is true on OpenShift (SCCs present): fsGroup
	// is omitted.
	PlatformAssignsGroups bool
	// Offer is the encoded offered set and OfferHash its hash.
	Offer     []byte
	OfferHash string
	// Owners are the pgAdmin users of the offer's personal servers.
	Owners []string
	// OfferSecrets are the Secrets the offer names (passwords and CAs).
	OfferSecrets []string
	// ClientSecretVersion is the resourceVersion of the OAuth2 client
	// Secret when it carries the reload label (a change rolls the Pod).
	ClientSecretVersion string
	// APIServer are the API server endpoints for the NetworkPolicy.
	APIServer apiServerEndpoints
	// Clusters are the registered Clusters' names (NetworkPolicy).
	Clusters []string
	// RemoteClusters are the granted Clusters of other namespaces, by
	// namespace (NetworkPolicy).
	RemoteClusters map[string][]string
	// Poolers are the poolers the endpoints use (NetworkPolicy).
	Poolers []string
}

func ownerRef(p *pgtoolboxv1alpha1.PgAdmin) metav1.OwnerReference {
	return *metav1.NewControllerRef(p, pgtoolboxv1alpha1.GroupVersion.WithKind("PgAdmin"))
}

// objectMeta returns the metadata of a generated object of component and
// propagation target, owned by p.
func objectMeta(p *pgtoolboxv1alpha1.PgAdmin, name, component string, target pgtoolboxv1alpha1.PropagationTarget) metav1.ObjectMeta {
	labels, annotations := metadata.Propagate(target, p.Spec.MetadataPropagation, identity(p).Labels(component), nil)
	return metav1.ObjectMeta{
		Name:            name,
		Namespace:       p.Namespace,
		Labels:          labels,
		Annotations:     annotations,
		OwnerReferences: []metav1.OwnerReference{ownerRef(p)},
	}
}

func buildServiceAccount(p *pgtoolboxv1alpha1.PgAdmin) *corev1.ServiceAccount {
	n := namesOf(p)
	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: objectMeta(p, n.base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll),
		// The token is never ambient: only the passfile-sync container
		// (and the oauth-proxy) mount one, through a projected volume.
		AutomountServiceAccountToken: new(false),
	}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		if sa.Annotations == nil {
			sa.Annotations = map[string]string{}
		}
		// The ServiceAccount is the oauth-proxy's OAuth client; the
		// redirect goes back through the Route of the same name.
		sa.Annotations["serviceaccounts.openshift.io/oauth-redirectreference.primary"] = fmt.Sprintf(`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":%q}}`, n.base)
	}
	specs.SetHash(sa, specs.Hash(sa))
	return sa
}

// buildCredentialsRole is the pgAdmin Pod's name-scoped Role (CNPG's
// instance-Role pattern): get and watch on exactly the offer
// Secret and the Secrets the offer names. resourceNames cannot express
// "list", so the sidecar gets and watches each object by name.
func buildCredentialsRole(p *pgtoolboxv1alpha1.PgAdmin, secretNames []string) *rbacv1.Role {
	n := namesOf(p)
	names := append([]string{n.offer}, secretNames...)
	slices.Sort(names)
	rules := []rbacv1.PolicyRule{{
		APIGroups:     []string{""},
		Resources:     []string{"secrets"},
		ResourceNames: slices.Compact(names),
		Verbs:         []string{"get", "watch"},
	}}
	role := &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: objectMeta(p, n.credentials, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll),
		Rules:      rules,
	}
	specs.SetHash(role, specs.Hash(role.Rules))
	return role
}

func buildCredentialsRoleBinding(p *pgtoolboxv1alpha1.PgAdmin) *rbacv1.RoleBinding {
	n := namesOf(p)
	binding := &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: objectMeta(p, n.credentials, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll),
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: n.credentials},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: n.base, Namespace: p.Namespace}},
	}
	specs.SetHash(binding, specs.Hash(binding.Subjects))
	return binding
}

// buildOfferSecret carries the offered set: names of Secrets and roles,
// never a password. It is a Secret rather than a ConfigMap only so that the
// people who may steer it are those who may update Secrets in the
// namespace, who can read the offered credentials anyway.
func buildOfferSecret(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) *corev1.Secret {
	meta := objectMeta(p, namesOf(p).offer, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll)
	meta.Labels[pgtoolboxv1alpha1.LabelReload] = pgtoolboxv1alpha1.ReloadLabelValue
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: meta,
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{offer.SecretKey: in.Offer},
	}
	specs.SetHash(secret, in.OfferHash)
	return secret
}

// buildSettingsPVC is the settings volume. Under Retain it carries no owner
// reference, so deleting the PgAdmin leaves it; a PgAdmin of the same name
// adopts it again by its labels.
func buildSettingsPVC(p *pgtoolboxv1alpha1.PgAdmin) *corev1.PersistentVolumeClaim {
	meta := objectMeta(p, namesOf(p).base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetStorage)
	if p.Spec.Storage.RetainPolicyOrDefault() == pgtoolboxv1alpha1.PgAdminRetainPolicyRetain {
		meta.OwnerReferences = nil
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: meta,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: p.Spec.Storage.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: p.Spec.Storage.SizeOrDefault(),
			}},
		},
	}
	specs.SetHash(pvc, specs.Hash(pvc.Spec))
	return pvc
}

func buildService(p *pgtoolboxv1alpha1.PgAdmin) *corev1.Service {
	n := namesOf(p)
	meta := objectMeta(p, n.base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetService)
	port := corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		port = corev1.ServicePort{Name: "https", Port: 443, TargetPort: intstr.FromString("https"), Protocol: corev1.ProtocolTCP}
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		// The OpenShift service CA issues the proxy's serving certificate.
		meta.Annotations["service.beta.openshift.io/serving-cert-secret-name"] = n.proxyTLS
	}
	svc := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: meta,
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: identity(p).SelectorLabels(componentPgAdmin),
			Ports:    []corev1.ServicePort{port},
		},
	}
	if p.Spec.ReplicasOrDefault() > 1 {
		// pgAdmin keeps a user's open connections and session in one
		// process: a client stays on one Pod (the exposure in front must be
		// sticky as well).
		svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
	}
	specs.SetHash(svc, specs.Hash(svc.Spec))
	return svc
}

// generatedSecret is a create-once Secret (Get before Create, immutable);
// the reconciler never updates one and never reads its content back.
func generatedSecret(p *pgtoolboxv1alpha1.PgAdmin, name, key string, value []byte) *corev1.Secret {
	meta := objectMeta(p, name, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll)
	// The operator's Secret informer is metadata-only and selects on the
	// reload label, so its own Secrets carry it too (a deleted one is
	// noticed and recreated).
	meta.Labels[pgtoolboxv1alpha1.LabelReload] = pgtoolboxv1alpha1.ReloadLabelValue
	return &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: meta,
		Immutable:  new(true),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{key: value},
	}
}
