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
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// The composed Pod, per the decided evidence API contract. The mount set is
// the confinement boundary: the socket and token volumes reach exactly the
// console and viewer containers, store credentials exactly the viewer, and
// the Kubernetes API projection exactly the console and proxy. Every path
// below is fixed by the contract or by this operator, never configurable.
const (
	// evidenceSocketURL is fixed by the contract.
	evidenceSocketURL       = "unix:///var/run/objectstoreviewer/evidence.sock"
	evidenceSocketDirectory = "/var/run/objectstoreviewer"

	// evidenceTokenFilePath is where the single-key subPath mount places
	// the token in both containers. subPath produces a stable regular file
	// — not the Secret volume's projected-directory symlink — which makes
	// the contract's forbidden live-update structurally impossible.
	// #nosec G101 -- mount path only; no secret material is embedded.
	evidenceTokenFilePath = "/run/secrets/pgtoolbox/evidence-token"

	// storeCredentialDirectory holds the S3 credential files, viewer only.
	// #nosec G101 -- mount path only; no secret material is embedded.
	storeCredentialDirectory = "/run/secrets/pgtoolbox/store"
	storeAccessKeyFile       = "access-key-id"
	storeSecretKeyFile       = "secret-access-key"
	storeSessionTokenFile    = "session-token"

	// endpointCADirectory holds the optional endpoint CA bundle.
	endpointCADirectory = "/run/secrets/pgtoolbox/endpoint-ca"
	endpointCAFile      = "ca.crt"

	evidenceSocketVolume  = "evidence-socket"
	evidenceTokenVolume   = "evidence-token"
	storeCredentialVolume = "evidence-store-credentials"
	endpointCAVolume      = "evidence-endpoint-ca"
	viewerScratchVolume   = "viewer-tmp"
)

// viewerImage resolves the sidecar image reference: the spec image when
// set, otherwise the operator's configured default, otherwise empty (the
// caller reports ReasonImageRequired).
func (r *Reconciler) viewerImage(console *pgtoolboxv1alpha1.PgConsole) string {
	if console.Spec.Evidence.Image != nil {
		return imageReference(*console.Spec.Evidence.Image)
	}
	return r.DefaultImages.ObjectStoreViewer
}

// composeEvidence grafts the viewer sidecar onto the built Deployment: the
// extra container, the shared socket and token volumes, the consumer
// variables on the console, and the credential mounts the viewer alone
// receives. It mutates the template exactly once, so the caller's
// deterministic-render property is preserved.
func composeEvidence(
	deployment *appsv1.Deployment,
	console *pgtoolboxv1alpha1.PgConsole,
	viewerImage string,
	composition *evidenceComposition,
	tokenSecretName string,
) {
	template := &deployment.Spec.Template.Spec
	mode0440 := int32(0o440)
	mode0444 := int32(0o444)

	// The socket volume is mounted writable in both containers: the viewer
	// creates the socket, and connect(2) needs write permission on it.
	socketMount := corev1.VolumeMount{Name: evidenceSocketVolume, MountPath: evidenceSocketDirectory}
	tokenMount := corev1.VolumeMount{
		Name:      evidenceTokenVolume,
		MountPath: evidenceTokenFilePath,
		SubPath:   evidenceTokenKey,
		ReadOnly:  true,
	}

	template.Volumes = append(template.Volumes,
		corev1.Volume{
			Name:         evidenceSocketVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		corev1.Volume{
			Name: evidenceTokenVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  tokenSecretName,
				DefaultMode: &mode0440,
				Items:       []corev1.KeyToPath{{Key: evidenceTokenKey, Path: evidenceTokenKey}},
			}},
		},
		corev1.Volume{
			Name:         viewerScratchVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		corev1.Volume{
			Name: storeCredentialVolume,
			VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				DefaultMode: &mode0440,
				Sources:     credentialProjections(composition),
			}},
		},
	)
	if composition.EndpointCA != nil {
		template.Volumes = append(template.Volumes, corev1.Volume{
			Name: endpointCAVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  composition.EndpointCA.Name,
				DefaultMode: &mode0444,
				Items:       []corev1.KeyToPath{{Key: composition.EndpointCA.Key, Path: endpointCAFile}},
			}},
		})
	}

	// Console: the four consumer variables (all-or-nothing per the
	// contract) plus the socket and token mounts.
	consoleContainer := findContainer(template, "pgconsole")
	if consoleContainer == nil {
		return
	}
	consoleContainer.Env = append(consoleContainer.Env,
		corev1.EnvVar{Name: "REPOSITORY_EVIDENCE_URL", Value: evidenceSocketURL},
		corev1.EnvVar{Name: "REPOSITORY_EVIDENCE_TOKEN_FILE", Value: evidenceTokenFilePath},
		corev1.EnvVar{Name: "REPOSITORY_EXPECTED_FINGERPRINT", Value: composition.Fingerprint},
		corev1.EnvVar{Name: "REPOSITORY_BARMAN_SERVER", Value: composition.ServerName},
	)
	consoleContainer.VolumeMounts = append(consoleContainer.VolumeMounts, socketMount, tokenMount)

	template.Containers = append(template.Containers,
		viewerContainer(console, viewerImage, composition, socketMount, tokenMount))
}

// findContainer locates a built container by name.
func findContainer(template *corev1.PodSpec, name string) *corev1.Container {
	for i := range template.Containers {
		if template.Containers[i].Name == name {
			return &template.Containers[i]
		}
	}
	return nil
}

// credentialProjections assembles the store credential files from the
// Secrets the ObjectStore names. A projected volume tolerates the key pair
// living in one Secret or two.
func credentialProjections(composition *evidenceComposition) []corev1.VolumeProjection {
	projections := []corev1.VolumeProjection{
		{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: composition.AccessKeyID.Name},
			Items:                []corev1.KeyToPath{{Key: composition.AccessKeyID.Key, Path: storeAccessKeyFile}},
		}},
		{Secret: &corev1.SecretProjection{
			LocalObjectReference: corev1.LocalObjectReference{Name: composition.SecretAccessKey.Name},
			Items:                []corev1.KeyToPath{{Key: composition.SecretAccessKey.Key, Path: storeSecretKeyFile}},
		}},
	}
	if composition.SessionToken != nil {
		projections = append(projections, corev1.VolumeProjection{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: composition.SessionToken.Name},
				Items:                []corev1.KeyToPath{{Key: composition.SessionToken.Key, Path: storeSessionTokenFile}},
			},
		})
	}
	return projections
}

// viewerContainer builds the ObjectStoreViewer sidecar. It receives the
// socket, the token, its scratch space and the store credentials — and
// deliberately NOT the kube-api-access projection: the viewer holds no
// Kubernetes API credential, which is half of the contract's isolation
// argument.
func viewerContainer(
	console *pgtoolboxv1alpha1.PgConsole,
	viewerImage string,
	composition *evidenceComposition,
	socketMount, tokenMount corev1.VolumeMount,
) corev1.Container {
	pullPolicy := corev1.PullIfNotPresent
	if console.Spec.Evidence.Image != nil {
		pullPolicy = imagePullPolicy(*console.Spec.Evidence.Image)
	}

	environment := []corev1.EnvVar{
		{Name: "RUNTIME_MODE", Value: "pgconsole-sidecar"},
		{Name: "EVIDENCE_TOKEN_FILE", Value: evidenceTokenFilePath},
		{Name: "CNPG_CLUSTER_NAMESPACE", Value: console.Namespace},
		{Name: "CNPG_CLUSTER_UID", Value: composition.ClusterUID},
		{Name: "CNPG_CLUSTER_NAME", Value: console.Spec.CNPGClusterRef.Name},
		{Name: "REPOSITORY_FORMAT", Value: "barman-cloud"},
		{Name: "PROVIDER", Value: "s3"},
		{Name: "DESTINATION_PATH", Value: composition.DestinationPath},
	}
	if composition.EndpointURL != "" {
		environment = append(environment,
			corev1.EnvVar{Name: "ENDPOINT_URL", Value: composition.EndpointURL})
	}
	if composition.EndpointCA != nil {
		environment = append(environment,
			corev1.EnvVar{Name: "ENDPOINT_CA_FILE", Value: endpointCADirectory + "/" + endpointCAFile})
	}
	environment = append(environment,
		corev1.EnvVar{Name: "BARMAN_SERVER_NAMES", Value: composition.ServerName},
		corev1.EnvVar{Name: "STORE_CREDENTIAL_MODE", Value: "static-files"},
		corev1.EnvVar{Name: "AWS_ACCESS_KEY_ID_FILE", Value: storeCredentialDirectory + "/" + storeAccessKeyFile},
		corev1.EnvVar{Name: "AWS_SECRET_ACCESS_KEY_FILE", Value: storeCredentialDirectory + "/" + storeSecretKeyFile},
	)
	if composition.SessionToken != nil {
		environment = append(environment,
			corev1.EnvVar{Name: "AWS_SESSION_TOKEN_FILE", Value: storeCredentialDirectory + "/" + storeSessionTokenFile})
	}

	mounts := []corev1.VolumeMount{
		socketMount,
		tokenMount,
		{Name: viewerScratchVolume, MountPath: "/tmp"},
		{Name: storeCredentialVolume, MountPath: storeCredentialDirectory, ReadOnly: true},
	}
	if composition.EndpointCA != nil {
		mounts = append(mounts,
			corev1.VolumeMount{Name: endpointCAVolume, MountPath: endpointCADirectory, ReadOnly: true})
	}

	return corev1.Container{
		Name:            "objectstoreviewer",
		Image:           viewerImage,
		ImagePullPolicy: pullPolicy,
		Env:             environment,
		// Liveness only, via the probe subcommand: it reads the token file
		// and calls the authenticated /healthz over the socket. The
		// viewer's /readyz must never be a kubelet probe — console
		// readiness stays independent of evidence (contract).
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
				Command: []string{"/objectstoreviewer", "probe"},
			}},
		},
		Resources: containerResources(console.Spec.Evidence.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}
