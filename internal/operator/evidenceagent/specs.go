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

package evidenceagent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/backupresolve"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Pure builders of every object the controller applies. They take a deep
// copy of the EvidenceAgent and the resolved inputs and return typed objects
// carrying the spec-hash annotation; the reconciler only applies them (SSA,
// ForceOwnership) — there is no hand-written drift comparison anywhere.

const (
	// ClusterRoleName is the one ClusterRole every agent ServiceAccount is
	// bound to: create tokenreviews and subjectaccessreviews, nothing else.
	ClusterRoleName = "pgtoolbox-evidence-agent"
	// ClusterRoleBindingName binds every agent ServiceAccount to it.
	ClusterRoleBindingName = "pgtoolbox-evidence-agent"

	containerName = "evidence-agent"
	componentName = "evidence-agent"

	// AnnotationProducerEpoch on the Pod template records the epoch the
	// Deployment was rendered with; status.producerEpoch is read back from
	// the live Deployment once its rollout completed.
	AnnotationProducerEpoch = "pgtoolbox.fyannk.dev/producer-epoch"

	// The OpenShift service CA's annotations.
	// #nosec G101 -- annotation keys, not credentials.
	annotationServingCertSecret = "service.beta.openshift.io/serving-cert-secret-name"
	annotationInjectCABundle    = "service.beta.openshift.io/inject-cabundle"
	serviceCABundleKey          = "service-ca.crt"

	servingDirectory = "/var/run/secrets/pgtoolbox.fyannk.dev/serving"
	// #nosec G101 -- mount paths, not credentials.
	storeCredentialDirectory = "/var/run/secrets/pgtoolbox.fyannk.dev/store"
	endpointCADirectory      = "/var/run/secrets/pgtoolbox.fyannk.dev/endpoint-ca"
	kubeAPIDirectory         = "/var/run/secrets/kubernetes.io/serviceaccount"

	storeAccessKeyFile        = "access-key-id"
	storeSecretKeyFile        = "secret-access-key"
	storeSessionTokenFile     = "session-token"
	storeSSECustomerKeyFile   = "sse-customer-key"
	storeConnectionStringFile = "connection-string"
	storeAccountKeyFile       = "account-key"
	storeSASTokenFile         = "sas-token"
	storeGoogleCredentialFile = "credentials.json"
	storeCipherPassFile       = "cipher-pass"
	endpointCAFile            = "ca.crt"

	volumeServing    = "serving-certificate"
	volumeStore      = "store-credentials"
	volumeEndpointCA = "endpoint-ca"
	volumeTmp        = "tmp"
	volumeKubeAPI    = "kube-api-access"

	// kubeAPITokenSeconds is the lifetime of the agent's own projected
	// token, the one it presents to the API server for its two reviews.
	kubeAPITokenSeconds = 3600
)

// renderInputs is the complete input of the builders.
type renderInputs struct {
	Agent       *pgtoolboxv1alpha1.EvidenceAgent
	Image       resolvedImage
	Composition composition
	// ServingSecretName is the Secret the agent mounts tls.crt/tls.key from.
	ServingSecretName string
}

// resolvedImage is the effective image reference and pull settings.
type resolvedImage struct {
	Reference   string
	PullPolicy  corev1.PullPolicy
	PullSecrets []corev1.LocalObjectReference
}

// resolveImage turns spec.image (or the operator default) into a reference.
func resolveImage(spec *pgtoolboxv1alpha1.ImageSpec, defaultImage string) resolvedImage {
	if spec == nil {
		return resolvedImage{Reference: defaultImage, PullPolicy: corev1.PullIfNotPresent}
	}
	reference := spec.Repository
	if spec.Tag != "" {
		reference += ":" + spec.Tag
	}
	if spec.Digest != "" {
		reference += "@" + spec.Digest
	}
	policy := spec.PullPolicy
	if policy == "" {
		policy = corev1.PullIfNotPresent
		if spec.Digest == "" {
			policy = corev1.PullAlways
		}
	}
	image := resolvedImage{Reference: reference, PullPolicy: policy}
	for _, secret := range spec.PullSecrets {
		image.PullSecrets = append(image.PullSecrets, corev1.LocalObjectReference{Name: secret.Name})
	}
	return image
}

// commonLabels are stamped on every object generated for agent; the
// manager's caches select on managed-by and reconcile.
func commonLabels(agent *pgtoolboxv1alpha1.EvidenceAgent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":             componentName,
		"app.kubernetes.io/instance":         agent.Name,
		"app.kubernetes.io/component":        componentName,
		"app.kubernetes.io/part-of":          "pgtoolbox",
		pgtoolboxv1alpha1.LabelManagedBy:     pgtoolboxv1alpha1.ManagedByValue,
		pgtoolboxv1alpha1.LabelReconcile:     pgtoolboxv1alpha1.ReconcileLabelValue,
		pgtoolboxv1alpha1.LabelEvidenceAgent: agent.Name,
	}
}

// selectorLabels is the immutable Deployment/Service selector: never add a
// key.
func selectorLabels(agent *pgtoolboxv1alpha1.EvidenceAgent) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     componentName,
		"app.kubernetes.io/instance": agent.Name,
	}
}

// singletonLabels mark the cluster-scoped objects shared by every agent.
func singletonLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":         componentName,
		"app.kubernetes.io/component":    componentName,
		"app.kubernetes.io/part-of":      "pgtoolbox",
		pgtoolboxv1alpha1.LabelManagedBy: pgtoolboxv1alpha1.ManagedByValue,
		pgtoolboxv1alpha1.LabelReconcile: pgtoolboxv1alpha1.ReconcileLabelValue,
	}
}

func ownerReference(agent *pgtoolboxv1alpha1.EvidenceAgent) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: pgtoolboxv1alpha1.GroupVersion.String(), Kind: "EvidenceAgent",
		Name: agent.Name, UID: agent.UID, Controller: new(true), BlockOwnerDeletion: new(true),
	}
}

func objectMeta(agent *pgtoolboxv1alpha1.EvidenceAgent, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: agent.Namespace, Labels: commonLabels(agent),
		OwnerReferences: []metav1.OwnerReference{ownerReference(agent)},
	}
}

// withSpecHash stamps the content hash of the object as rendered (before the
// annotation is added) so a rollout is visible and explainable.
func withSpecHash[T interface {
	GetAnnotations() map[string]string
	SetAnnotations(map[string]string)
}](object T) T {
	// Typed Kubernetes objects of these kinds always marshal.
	encoded, _ := json.Marshal(object)
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[pgtoolboxv1alpha1.AnnotationSpecHash] = fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	object.SetAnnotations(annotations)
	return object
}

// buildServiceAccount is the agent's own identity; its token is never
// automounted (the Pod mounts a projected one explicitly).
// spec.workloadIdentity.serviceAccountAnnotations bind it to a cloud
// identity (IRSA, Azure workload identity, GKE).
func buildServiceAccount(agent *pgtoolboxv1alpha1.EvidenceAgent) *corev1.ServiceAccount {
	account := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   objectMeta(agent, agent.WorkloadName()),
		AutomountServiceAccountToken: new(false),
	}
	if identity := agent.Spec.WorkloadIdentity; identity != nil && len(identity.ServiceAccountAnnotations) > 0 {
		account.Annotations = maps.Clone(identity.ServiceAccountAnnotations)
	}
	return withSpecHash(account)
}

// buildService is the ClusterIP Service the console reaches; with the
// OpenShift service CA its annotation asks for the serving certificate.
func buildService(agent *pgtoolboxv1alpha1.EvidenceAgent, servingSecretName string) *corev1.Service {
	service := &corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: objectMeta(agent, agent.WorkloadName()),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(agent),
			Ports: []corev1.ServicePort{{
				Name: evidencev1alpha1.AgentPortName, Port: evidencev1alpha1.AgentPort, Protocol: corev1.ProtocolTCP,
				TargetPort: intstr.FromString(evidencev1alpha1.AgentPortName), AppProtocol: new("https"),
			}},
		},
	}
	if agent.CertificateIssuer() == pgtoolboxv1alpha1.CertificateIssuerOpenShiftServiceCA {
		service.Annotations = map[string]string{annotationServingCertSecret: servingSecretName}
	}
	return withSpecHash(service)
}

// buildCABundleConfigMap is the ConfigMap the OpenShift service CA injects
// its bundle into. The operator applies metadata only: the data is the
// service CA's, and SSA never claims it.
func buildCABundleConfigMap(agent *pgtoolboxv1alpha1.EvidenceAgent) *corev1.ConfigMap {
	configMap := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: objectMeta(agent, agent.CABundleConfigMapName()),
	}
	configMap.Annotations = map[string]string{annotationInjectCABundle: "true"}
	return withSpecHash(configMap)
}

// buildClusterRole is the agent's only permission: creating the two reviews.
func buildClusterRole() *rbacv1.ClusterRole {
	return withSpecHash(&rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleName, Labels: singletonLabels()},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
			{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
		},
	})
}

// agentSubject identifies one agent's ServiceAccount.
type agentSubject struct {
	Namespace string
	Name      string
}

// buildClusterRoleBinding binds every live agent ServiceAccount, in a
// deterministic order, to ClusterRoleName. It is recomputed from the full
// set of EvidenceAgents on every reconcile, so a deleted agent leaves the
// binding at its next reconcile without a finalizer.
func buildClusterRoleBinding(subjects []agentSubject) *rbacv1.ClusterRoleBinding {
	sorted := slices.Clone(subjects)
	slices.SortFunc(sorted, func(a, b agentSubject) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	sorted = slices.Compact(sorted)
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: ClusterRoleBindingName, Labels: singletonLabels()},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRoleName},
		Subjects:   []rbacv1.Subject{},
	}
	for _, subject := range sorted {
		binding.Subjects = append(binding.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Namespace: subject.Namespace, Name: subject.Name})
	}
	return withSpecHash(binding)
}

// buildDeployment renders the agent workload.
func buildDeployment(inputs renderInputs) *appsv1.Deployment {
	agent := inputs.Agent
	epoch := strconv.FormatInt(agent.Generation, 10)
	podLabels := commonLabels(agent)
	if identity := agent.Spec.WorkloadIdentity; identity != nil {
		// spec.workloadIdentity.podLabels (azure.workload.identity/use);
		// the operator's own labels win (CEL also refuses their prefixes).
		for key, value := range identity.PodLabels {
			if _, own := podLabels[key]; !own {
				podLabels[key] = string(value)
			}
		}
	}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: objectMeta(agent, agent.WorkloadName()),
		Spec: appsv1.DeploymentSpec{
			// One reader per repository: a second replica would double the
			// object-store traffic and publish a second, independent epoch.
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(agent)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: new(intstr.FromInt32(0)), MaxSurge: new(intstr.FromInt32(1)),
				},
			},
			ProgressDeadlineSeconds: new(int32(300)),
			RevisionHistoryLimit:    new(int32(3)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: map[string]string{AnnotationProducerEpoch: epoch},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:           agent.WorkloadName(),
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					// Restricted PSS; no fixed UID or fsGroup, so the
					// platform (OpenShift's SCC) chooses them.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					TerminationGracePeriodSeconds: new(int64(30)),
					ImagePullSecrets:              inputs.Image.PullSecrets,
					Containers:                    []corev1.Container{agentContainer(inputs, epoch)},
					Volumes:                       agentVolumes(inputs),
				},
			},
		},
	}
	return withSpecHash(deployment)
}

func agentContainer(inputs renderInputs, epoch string) corev1.Container {
	agent, composition := inputs.Agent, inputs.Composition
	environment := []corev1.EnvVar{
		{Name: "RUNTIME_MODE", Value: "agent"},
		// The agent never asks the node's instance metadata for AWS
		// credentials: workload identity is IRSA or EKS Pod Identity only.
		{Name: "AWS_EC2_METADATA_DISABLED", Value: "true"},
		{Name: "LISTEN_ADDR", Value: ":" + strconv.Itoa(evidencev1alpha1.AgentPort)},
		{Name: "TLS_CERT_FILE", Value: servingDirectory + "/" + corev1.TLSCertKey},
		{Name: "TLS_KEY_FILE", Value: servingDirectory + "/" + corev1.TLSPrivateKeyKey},
		{Name: "AGENT_NAME", Value: agent.Name},
		{Name: "AGENT_NAMESPACE", Value: agent.Namespace},
		{Name: "PRODUCER_EPOCH", Value: epoch},
		{Name: "CNPG_CLUSTER_NAMESPACE", Value: agent.Namespace},
		{Name: "CNPG_CLUSTER_UID", Value: composition.ClusterUID},
		{Name: "CNPG_CLUSTER_NAME", Value: composition.ClusterName},
	}
	environment = append(environment, repositoryEnvironment(composition)...)
	environment = append(environment, corev1.EnvVar{Name: "CATALOG_REFRESH_INTERVAL", Value: agent.RefreshInterval().String()})
	if composition.EndpointURL != "" {
		environment = append(environment, corev1.EnvVar{Name: "ENDPOINT_URL", Value: composition.EndpointURL})
	}
	if composition.EndpointCA != nil {
		environment = append(environment, corev1.EnvVar{Name: "ENDPOINT_CA_FILE", Value: endpointCADirectory + "/" + endpointCAFile})
	}
	if composition.Provider == evidencev1alpha1.ProviderS3 {
		switch region := composition.Region; {
		case region.SecretRef != nil:
			environment = append(environment, corev1.EnvVar{Name: "AWS_REGION", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: region.SecretRef.Name}, Key: region.SecretRef.Key},
			}})
		case region.Value != "":
			environment = append(environment, corev1.EnvVar{Name: "AWS_REGION", Value: region.Value})
		}
	}
	if composition.AzureAccount != "" {
		environment = append(environment, corev1.EnvVar{Name: "AZURE_STORAGE_ACCOUNT", Value: composition.AzureAccount})
	}
	file := func(name string) string { return storeCredentialDirectory + "/" + name }
	if composition.CipherPass != nil {
		// The pgBackRest repository passphrase, a projected file like the
		// credentials.
		environment = append(environment, corev1.EnvVar{Name: "PGBACKREST_CIPHER_PASS_FILE", Value: file(storeCipherPassFile)})
	}
	if composition.SSECustomerKey != nil {
		// Whatever the credential (key pair or workload identity).
		environment = append(environment, corev1.EnvVar{Name: "S3_SSE_CUSTOMER_KEY_FILE", Value: file(storeSSECustomerKeyFile)})
	}
	switch composition.Credential {
	case backupresolve.CredentialStaticKeys:
		environment = append(environment,
			corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "static-files"},
			corev1.EnvVar{Name: "AWS_ACCESS_KEY_ID_FILE", Value: file(storeAccessKeyFile)},
			corev1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY_FILE", Value: file(storeSecretKeyFile)},
		)
		if composition.SessionToken != nil {
			environment = append(environment, corev1.EnvVar{Name: "AWS_SESSION_TOKEN_FILE", Value: file(storeSessionTokenFile)})
		}
	case backupresolve.CredentialConnectionString:
		environment = append(environment,
			corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "connection-string-file"},
			corev1.EnvVar{Name: "AZURE_STORAGE_CONNECTION_STRING_FILE", Value: file(storeConnectionStringFile)})
	case backupresolve.CredentialStorageKey:
		environment = append(environment,
			corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "account-key-file"},
			corev1.EnvVar{Name: "AZURE_STORAGE_ACCOUNT_KEY_FILE", Value: file(storeAccountKeyFile)})
	case backupresolve.CredentialSASToken:
		environment = append(environment,
			corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "sas-token-file"},
			corev1.EnvVar{Name: "AZURE_STORAGE_SAS_TOKEN_FILE", Value: file(storeSASTokenFile)})
	case backupresolve.CredentialJSONKey:
		environment = append(environment,
			corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "json-file"},
			corev1.EnvVar{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: file(storeGoogleCredentialFile)})
	case backupresolve.CredentialAmbient:
		environment = append(environment, corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "ambient"})
	}

	mounts := []corev1.VolumeMount{
		{Name: volumeServing, MountPath: servingDirectory, ReadOnly: true},
	}
	if len(storeProjections(composition)) > 0 {
		mounts = append(mounts, corev1.VolumeMount{Name: volumeStore, MountPath: storeCredentialDirectory, ReadOnly: true})
	}
	mounts = append(mounts,
		corev1.VolumeMount{Name: volumeTmp, MountPath: "/tmp"},
		corev1.VolumeMount{Name: volumeKubeAPI, MountPath: kubeAPIDirectory, ReadOnly: true},
	)
	if composition.EndpointCA != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: volumeEndpointCA, MountPath: endpointCADirectory, ReadOnly: true})
	}
	// The probes reach the unauthenticated /healthz: readiness follows the
	// process, not the store, so stale or unknown evidence is still served
	// (and says so) while the repository is unreachable.
	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: evidencev1alpha1.HealthPath, Port: intstr.FromString(evidencev1alpha1.AgentPortName), Scheme: corev1.URISchemeHTTPS,
			}},
			PeriodSeconds: period, FailureThreshold: failures, TimeoutSeconds: 3,
		}
	}
	return corev1.Container{
		Name:            containerName,
		Image:           inputs.Image.Reference,
		ImagePullPolicy: inputs.Image.PullPolicy,
		Env:             environment,
		Ports: []corev1.ContainerPort{{
			Name: evidencev1alpha1.AgentPortName, ContainerPort: evidencev1alpha1.AgentPort, Protocol: corev1.ProtocolTCP,
		}},
		ReadinessProbe: probe(10, 3),
		LivenessProbe:  probe(20, 3),
		Resources:      containerResources(agent.Spec.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			RunAsNonRoot:             new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

// repositoryEnvironment names the repository's format, provider and
// destination, and its scope: one Barman server, or one pgBackRest stanza
// (with the WAL's own stanza when Opera splits them) of one repository.
// PGBACKREST_REPOSITORY is informational: DESTINATION_PATH already points
// at that repository's path; PGBACKREST_RETENTION_* carry
// that repository's retention.
func repositoryEnvironment(composition composition) []corev1.EnvVar {
	if composition.Format != backupresolve.FormatPgBackRest {
		return []corev1.EnvVar{
			{Name: "REPOSITORY_FORMAT", Value: backupresolve.FormatBarmanCloud},
			{Name: "PROVIDER", Value: composition.Provider},
			{Name: "DESTINATION_PATH", Value: composition.AgentDestination},
			{Name: "BARMAN_SERVER_NAMES", Value: composition.ServerName},
		}
	}
	environment := []corev1.EnvVar{
		{Name: "REPOSITORY_FORMAT", Value: backupresolve.FormatPgBackRest},
		{Name: "PROVIDER", Value: composition.Provider},
		{Name: "DESTINATION_PATH", Value: composition.AgentDestination},
		{Name: "PGBACKREST_STANZA", Value: composition.Stanza},
	}
	if composition.ArchiveStanza != "" {
		environment = append(environment, corev1.EnvVar{Name: "PGBACKREST_ARCHIVE_STANZA", Value: composition.ArchiveStanza})
	}
	environment = append(environment, corev1.EnvVar{Name: "PGBACKREST_REPOSITORY", Value: strconv.Itoa(composition.Repository)})
	// The selected repository's retention, which the agent compares the
	// stanza with; both types are always rendered, so an unset
	// repo-retention-full reads as "not configured", not as "no expectation".
	for _, variable := range composition.Retention.Variables() {
		environment = append(environment, corev1.EnvVar{Name: variable.Name, Value: variable.Value})
	}
	return environment
}

// agentVolumes: every file the agent reads is mounted 0444. The Pod has one
// container and no fsGroup (the platform picks the UID), so world-readable
// inside the Pod is the only mode an arbitrary UID can read; the Pod
// boundary is the confinement, as for any Secret volume's default 0644.
func agentVolumes(inputs renderInputs) []corev1.Volume {
	composition := inputs.Composition
	readable := new(int32(0o444))
	volumes := []corev1.Volume{
		{Name: volumeServing, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: inputs.ServingSecretName, DefaultMode: readable,
			Items: []corev1.KeyToPath{{Key: corev1.TLSCertKey, Path: corev1.TLSCertKey}, {Key: corev1.TLSPrivateKeyKey, Path: corev1.TLSPrivateKeyKey}},
		}}},
	}
	if credentials := storeProjections(composition); len(credentials) > 0 {
		volumes = append(volumes, corev1.Volume{Name: volumeStore, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: readable, Sources: credentials,
		}}})
	}
	volumes = append(volumes, []corev1.Volume{
		{Name: volumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: new(resource.MustParse("64Mi"))}}},
		{Name: volumeKubeAPI, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: readable,
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: new(int64(kubeAPITokenSeconds))}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
					Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
				}}}},
			},
		}}},
	}...)
	if composition.EndpointCA != nil {
		volumes = append(volumes, corev1.Volume{Name: volumeEndpointCA, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: composition.EndpointCA.Name, DefaultMode: readable,
			Items: []corev1.KeyToPath{{Key: composition.EndpointCA.Key, Path: endpointCAFile}},
		}}})
	}
	return volumes
}

// storeProjections are the credential files of the composition's Secret
// references (and the S3 SSE-C key); none for an ambient (workload
// identity) credential without SSE-C.
func storeProjections(composition composition) []corev1.VolumeProjection {
	var projections []corev1.VolumeProjection
	add := func(reference *secretKeyRef, path string) {
		if reference != nil {
			projections = append(projections, secretProjection(*reference, path))
		}
	}
	add(composition.AccessKeyID, storeAccessKeyFile)
	add(composition.SecretAccessKey, storeSecretKeyFile)
	add(composition.SessionToken, storeSessionTokenFile)
	add(composition.SSECustomerKey, storeSSECustomerKeyFile)
	add(composition.CipherPass, storeCipherPassFile)
	if composition.AzureSecret != nil {
		path := map[backupresolve.Credential]string{
			backupresolve.CredentialConnectionString: storeConnectionStringFile,
			backupresolve.CredentialStorageKey:       storeAccountKeyFile,
			backupresolve.CredentialSASToken:         storeSASTokenFile,
		}[composition.Credential]
		add(composition.AzureSecret, path)
	}
	add(composition.GoogleCredentials, storeGoogleCredentialFile)
	return projections
}

func secretProjection(reference secretKeyRef, path string) corev1.VolumeProjection {
	return corev1.VolumeProjection{Secret: &corev1.SecretProjection{
		LocalObjectReference: corev1.LocalObjectReference{Name: reference.Name},
		Items:                []corev1.KeyToPath{{Key: reference.Key, Path: path}},
	}}
}

// containerResources applies the spec or bounded defaults: a scan holds the
// repository's object list in memory (MAX_OBJECTS_PER_SCAN bounds it).
func containerResources(spec *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if spec != nil {
		return *spec.DeepCopy()
	}
	// The chart's defaults: a 1,000,000-object scan measured about 21 MiB.
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}
