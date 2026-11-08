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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// Label and annotation keys shared by every v1alpha1 kind. They are API
// surface (selectors, alerting and the operator's own label-filtered caches
// key on them): extend, never rename.
const (
	// ReservedKeyPrefix starts every label and annotation key the operator
	// owns; user-supplied metadata under it is dropped, never applied.
	ReservedKeyPrefix = "pgtoolbox.fyannk.dev/"

	// LabelManagedBy and ManagedByValue mark every object the operator
	// creates.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedByValue = "pgtoolbox"

	// LabelReconcile=ReconcileLabelValue marks every object the operator's
	// controllers create and own, so they are told apart from other objects
	// that carry managed-by=pgtoolbox (the label the owned-kind informers
	// select on), such as those the pgtools chart renders.
	LabelReconcile      = "pgtoolbox.fyannk.dev/reconcile"
	ReconcileLabelValue = "true"

	// LabelReload=ReloadLabelValue on a user Secret a CR references makes
	// the operator watch its metadata and roll the workload when it
	// changes.
	LabelReload      = "pgtoolbox.fyannk.dev/reload"
	ReloadLabelValue = "true"

	// AnnotationSpecHash carries the content hash of a child object's
	// desired state; a change of hash is a rollout, and the Event says so.
	AnnotationSpecHash = "pgtoolbox.fyannk.dev/spec-hash"

	// AnnotationReconcile=ReconcileSkip on a CR pauses its reconciliation;
	// the controller still publishes status (ReconciliationPaused).
	AnnotationReconcile = "pgtoolbox.fyannk.dev/reconcile"
	ReconcileSkip       = "skip"
)

// LocalObjectReference names an object in the referencing object's own
// namespace. Cross-namespace references do not exist in v1alpha1; a future
// one goes through an explicit grant object in the target
// namespace, never through a selector.
type LocalObjectReference struct {
	// The name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`
}

// ImageSpec selects a container image. Omitted, the operator's default
// image applies — a digest-pinned reference built into the operator and
// echoed into the kind's status. A tag alone is accepted but the resolved
// reference in status says it is not pinned; production use names a digest.
// +kubebuilder:validation:XValidation:rule="has(self.tag) || has(self.digest)",message="an image needs a tag, a digest or both",reason=FieldValueRequired
type ImageSpec struct {
	// Image repository, without tag or digest (registry/path/name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(:[0-9]{1,5})?(/[a-z0-9]+([._-][a-z0-9]+)*)*$`
	Repository string `json:"repository"`

	// Image tag; informative when a digest is set.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`
	// +optional
	Tag string `json:"tag,omitempty"`

	// Image digest; when set it is what the Pod runs.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`

	// Image pull policy. Defaults to IfNotPresent for a digest and Always
	// for a tag alone.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`

	// Pull secrets for the image registry, in the CR's namespace.
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	// +optional
	PullSecrets []LocalObjectReference `json:"pullSecrets,omitempty"`
}

// ExposureType selects how a workload is published.
// +kubebuilder:validation:Enum=clusterIP;route;ingress;gateway
type ExposureType string

const (
	// ExposureTypeClusterIP exposes the workload only inside the cluster.
	ExposureTypeClusterIP ExposureType = "clusterIP"
	// ExposureTypeRoute publishes an OpenShift Route (edge TLS).
	ExposureTypeRoute ExposureType = "route"
	// ExposureTypeIngress publishes a Kubernetes Ingress.
	ExposureTypeIngress ExposureType = "ingress"
	// ExposureTypeGateway publishes a Gateway API HTTPRoute.
	ExposureTypeGateway ExposureType = "gateway"
)

// ExposureSpec configures access to a workload from outside its Service.
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type == 'clusterIP' || (has(self.hostname) && size(self.hostname) > 0)",message="hostname is required for route, ingress and gateway exposure",fieldPath=".hostname",reason=FieldValueRequired
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'gateway') == has(self.gateway)",message="gateway is required exactly when type is gateway",fieldPath=".gateway",reason=FieldValueInvalid
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'ingress') || !has(self.ingressClassName)",message="ingressClassName applies to ingress exposure only",fieldPath=".ingressClassName",reason=FieldValueForbidden
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'ingress') || !has(self.tls) || !has(self.tls.certificateSecretRef)",message="tls.certificateSecretRef applies to ingress exposure only",fieldPath=".tls.certificateSecretRef",reason=FieldValueForbidden
type ExposureSpec struct {
	// The exposure mechanism.
	// +kubebuilder:default=clusterIP
	// +optional
	Type ExposureType `json:"type,omitempty"`

	// The external hostname. Required for route, ingress and gateway.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// TLS for route and ingress exposure. A Route always terminates TLS at
	// the edge; an Ingress uses certificateSecretRef, or the controller's
	// default certificate when it is omitted. For gateway exposure TLS
	// terminates at the Gateway listener.
	// +optional
	TLS *ExposureTLSSpec `json:"tls,omitempty"`

	// IngressClass name, for ingress exposure only.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	IngressClassName string `json:"ingressClassName,omitempty"`

	// Gateway API attachment, for gateway exposure only.
	// +optional
	Gateway *GatewayExposureSpec `json:"gateway,omitempty"`

	// Annotations added to the exposure object; keys under
	// pgtoolbox.fyannk.dev/ are rejected.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('pgtoolbox.fyannk.dev/'))",message="annotation keys under pgtoolbox.fyannk.dev/ are reserved",reason=FieldValueForbidden
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Labels added to the exposure object under the operator's own
	// identity labels, which always win. Ingress controllers and OpenShift
	// router shards select by label; keys under pgtoolbox.fyannk.dev/ are
	// rejected.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('pgtoolbox.fyannk.dev/'))",message="label keys under pgtoolbox.fyannk.dev/ are reserved",reason=FieldValueForbidden
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// ExposureTLSSpec configures TLS for route and ingress exposure.
type ExposureTLSSpec struct {
	// TLS certificate Secret (kubernetes.io/tls) in the CR's namespace,
	// for ingress exposure.
	// +optional
	CertificateSecretRef *LocalObjectReference `json:"certificateSecretRef,omitempty"`
}

// GatewayExposureSpec configures the HTTPRoute parent reference.
type GatewayExposureSpec struct {
	// The Gateway listener to attach to.
	ParentRef GatewayParentRef `json:"parentRef"`
}

// GatewayParentRef identifies a Gateway listener. A Gateway in another
// namespace admits the route only through its own listener's allowedRoutes
// (and a ReferenceGrant where its implementation needs one): the grant is
// the Gateway owner's, never this object's.
type GatewayParentRef struct {
	// Gateway name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Gateway namespace; defaults to the CR's namespace.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Listener (section) name on the Gateway.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SectionName string `json:"sectionName,omitempty"`
}

// PropagationTarget selects which generated objects receive propagated
// metadata.
// +kubebuilder:validation:Enum=All;Workload;Pod;Service;Exposure;Storage
type PropagationTarget string

const (
	// PropagationTargetAll is every generated object.
	PropagationTargetAll PropagationTarget = "All"
	// PropagationTargetWorkload is Deployments, StatefulSets and Jobs.
	PropagationTargetWorkload PropagationTarget = "Workload"
	// PropagationTargetPod is Pod templates.
	PropagationTargetPod PropagationTarget = "Pod"
	// PropagationTargetService is Services.
	PropagationTargetService PropagationTarget = "Service"
	// PropagationTargetExposure is Routes, Ingresses and HTTPRoutes.
	PropagationTargetExposure PropagationTarget = "Exposure"
	// PropagationTargetStorage is PersistentVolumeClaims.
	PropagationTargetStorage PropagationTarget = "Storage"
)

// MetadataPropagation copies labels and annotations onto generated
// objects (cost-centre labels, backup-tool annotations, mesh opt-outs). The
// operator's own keys always win, keys under pgtoolbox.fyannk.dev/ are
// rejected, and a change rolls only the objects it targets.
type MetadataPropagation struct {
	// Which generated objects receive the metadata.
	Target PropagationTarget `json:"target"`

	// Labels to add.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('pgtoolbox.fyannk.dev/') && !k.startsWith('app.kubernetes.io/'))",message="label keys under pgtoolbox.fyannk.dev/ and app.kubernetes.io/ are reserved",reason=FieldValueForbidden
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations to add.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, !k.startsWith('pgtoolbox.fyannk.dev/'))",message="annotation keys under pgtoolbox.fyannk.dev/ are reserved",reason=FieldValueForbidden
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}
