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
	"fmt"
	"reflect"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// The in-Pod ports. Only the proxy port ever leaves the Pod: the console,
// pgAdmin and the evidence viewer are loopback upstreams of the proxy.
const (
	consolePort int32 = 3000
	proxyPort   int32 = 8080
	pgAdminPort int32 = 8081

	// oauthRedirectAnnotation registers the exposure hostname as the OAuth
	// redirect URI on the workload ServiceAccount, which is the OAuth client
	// under OpenShift's service-account OAuth flow.
	oauthRedirectAnnotation = "serviceaccounts.openshift.io/oauth-redirecturi.pgconsole"

	// pgAdminSettingsMountPath is pgAdmin's data directory, backed by the
	// settings PVC so accounts and server definitions survive restarts.
	pgAdminSettingsMountPath = "/var/lib/pgadmin"
	pgAdminSettingsSuffix    = "-pgadmin"
	// defaultPgAdminSettingsSize backs the settings database when the spec
	// asks for no explicit size.
	defaultPgAdminSettingsSize = "1Gi"

	proxyConfigVolume     = "proxy-config"
	oidcClientVolume      = "oidc-client"
	pgAdminSettingsVolume = "pgadmin-settings"

	adminSyncBinVolume      = "admin-sync-bin"
	adminSyncTLSVolume      = "admin-sync-tls"
	adminSyncPassfileVolume = "admin-sync-passfile"

	adminSyncBinMountPath = "/run/pgadmin/admin-sync"
	adminSyncTLSMountPath = "/run/secrets/pgadmin-sync"
	// #nosec G101 -- filesystem path; no credential material.
	adminSyncPassfileMountPath = "/run/pgadmin/passfile"

	// kubeAPIAccessVolume replicates the volume automount would inject,
	// under a fixed name so the build stays deterministic. It exists so the
	// Pod can run with automountServiceAccountToken false and each container
	// opts in explicitly — the property the evidence sidecar depends on,
	// since the viewer must hold no Kubernetes API credential.
	kubeAPIAccessVolume = "kube-api-access"
	serviceAccountRoot  = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// projectedTokenExpiration matches the expiration automount would request.
var projectedTokenExpiration = int64(3600)

// workloadInputs carries the resolved values the Deployment build needs that
// are not pure functions of the spec: resolved image references, the
// rendered-configuration checksum and the evidence composition.
type workloadInputs struct {
	ProxyImage             string
	ConsoleImage           string
	PgAdminImage           string
	ViewerImage            string
	ConfigChecksum         string
	AdminSyncSecretVersion string
	Composition            *evidenceComposition
	TokenSecretName        string
}

// pgAdminEnabled reports whether the pgAdmin container is composed; the
// field defaults to true in the API.
func pgAdminEnabled(console *pgtoolboxv1alpha1.PgConsole) bool {
	return console.Spec.PgAdmin.Enabled == nil || *console.Spec.PgAdmin.Enabled
}

// resolveImage picks the spec image, falling back to the operator's
// configured default; with neither, the container cannot be composed.
func resolveImage(spec *pgtoolboxv1alpha1.ImageSpec, fallback string) (string, error) {
	if spec != nil && spec.Repository != "" {
		return imageReference(*spec), nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no image configured (spec image and operator default are both empty)")
}

// imageReference resolves a container image reference, preferring a digest
// over the tag so a pinned deployment stays immutable.
func imageReference(image pgtoolboxv1alpha1.ImageSpec) string {
	if image.Digest != "" {
		return image.Repository + "@" + image.Digest
	}
	return image.Repository + ":" + image.Tag
}

// imagePullPolicy applies the spec pull policy, defaulting to IfNotPresent.
func imagePullPolicy(image *pgtoolboxv1alpha1.ImageSpec) corev1.PullPolicy {
	if image != nil && image.PullPolicy != "" {
		return image.PullPolicy
	}
	return corev1.PullIfNotPresent
}

// adminSyncResources budgets the two containers that run pgAdmin's Python
// stack — pgAdmin itself and the admin-sync sidecar. Neither can share the
// console-wide default: every sync shells out to pgAdmin's setup.py, which
// boots a Flask application and runs the settings-database migrations, and
// 256Mi is not enough to do that — the sidecar was OOMKilled mid-sync and
// the console reported a sync failure with no hint of the cause.
//
// A spec budget still wins, so an operator who has measured their own can
// set one on spec.pgAdmin.resources.
func adminSyncResources(resources corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(resources.Requests) > 0 || len(resources.Limits) > 0 {
		return *resources.DeepCopy()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// containerResources applies the spec budget, falling back to the published
// provisional default: 25m/64Mi requests, 500m/256Mi limits.
func containerResources(resources corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(resources.Requests) > 0 || len(resources.Limits) > 0 {
		return *resources.DeepCopy()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("25m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
}

// serviceAccount builds the workload ServiceAccount. It is the identity the
// console watches its cluster with and the identity the proxy creates
// PgToolBoxAccessRequests with — the generated Roles are its entire
// authority.
func (r *Reconciler) serviceAccount(console *pgtoolboxv1alpha1.PgConsole) (*corev1.ServiceAccount, error) {
	var annotations map[string]string
	if console.Spec.Proxy.Authentication.Mode == pgtoolboxv1alpha1.ProxyAuthenticationModeOpenShift &&
		console.Spec.Exposure.Hostname != "" {
		annotations = map[string]string{
			oauthRedirectAnnotation: "https://" + console.Spec.Exposure.Hostname,
		}
	}
	serviceAccount := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        application.ResourceName(console.Name, ""),
			Namespace:   console.Namespace,
			Labels:      application.CommonLabels(console.Name),
			Annotations: annotations,
		},
	}
	if err := controllerutil.SetControllerReference(console, serviceAccount, r.Scheme); err != nil {
		return nil, err
	}
	return serviceAccount, nil
}

// service builds the ClusterIP Service in front of the console Pod. Only
// the proxy's plain-HTTP listener is exposed — the console's own port never
// leaves the Pod. Both 80 and 443 are published, like the reference oauth2
// mode, so every exposure kind (Ingress on 80, Gateway backends on 443,
// Route edge on the http port) finds its backend.
func (r *Reconciler) service(console *pgtoolboxv1alpha1.PgConsole) (*corev1.Service, error) {
	service := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, ""),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: application.SelectorLabels(console.Name),
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt32(proxyPort),
				},
				{
					Name:       "https",
					Protocol:   corev1.ProtocolTCP,
					Port:       443,
					TargetPort: intstr.FromInt32(proxyPort),
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(console, service, r.Scheme); err != nil {
		return nil, err
	}
	return service, nil
}

// deployment builds the console Deployment: the proxy, the pgConsole
// container, the embedded pgAdmin when enabled, and the evidence sidecar
// when a composition was resolved.
//
// The strategy is Recreate: the pgAdmin settings PVC cannot be shared
// across a rolling overlap, and the console is a singleton for which a
// brief replacement gap is acceptable. Replicas is pinned to one for the
// same reason.
//
// The rendered proxy configuration does not live in this template, so the
// template carries its checksum as an annotation: a configuration change
// rolls the Pods exactly once, and a no-op reconcile produces a
// byte-identical Deployment. Environment variables are emitted in a fixed
// order and every list is built deterministically, so identical specs
// render byte-identical templates.
func (r *Reconciler) deployment(
	console *pgtoolboxv1alpha1.PgConsole,
	inputs workloadInputs,
) (*appsv1.Deployment, error) {
	replicas := int32(1)
	serviceAccountName := application.ResourceName(console.Name, "")
	defaultMode := int32(0o444)

	podAnnotations := map[string]string{
		pgtoolboxv1alpha1.ConfigChecksumAnnotation: inputs.ConfigChecksum,
	}
	if inputs.AdminSyncSecretVersion != "" {
		podAnnotations[pgtoolboxv1alpha1.AdminSyncSecretVersionAnnotation] = inputs.AdminSyncSecretVersion
	}

	volumes := []corev1.Volume{
		{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		kubeAPIAccessProjection(),
		{
			Name: proxyConfigVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  application.ResourceName(console.Name, proxyConfigSecretSuffix),
				DefaultMode: &defaultMode,
				Items:       []corev1.KeyToPath{{Key: proxyConfigFileName, Path: proxyConfigFileName}},
			}},
		},
	}

	containers := []corev1.Container{
		r.proxyContainer(console, inputs.ProxyImage),
		r.consoleContainer(console, inputs.ConsoleImage),
	}

	if auth := console.Spec.Proxy.Authentication; auth.Mode == pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC {
		volumes = append(volumes, corev1.Volume{
			Name: oidcClientVolume,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName:  auth.OIDC.ClientSecretRef.Name,
				DefaultMode: &defaultMode,
				Items: []corev1.KeyToPath{{
					Key:  oidcClientSecretKey(auth.OIDC.ClientSecretRef),
					Path: oidcClientSecretFile,
				}},
			}},
		})
	}

	var initContainers []corev1.Container
	if pgAdminEnabled(console) {
		containers = append(containers, r.pgAdminContainer(console, inputs.PgAdminImage))
		volumes = append(volumes,
			corev1.Volume{
				Name: pgAdminSettingsVolume,
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: application.ResourceName(console.Name, pgAdminSettingsSuffix),
				}},
			},
			corev1.Volume{
				Name: pgAdminBootstrapVolume,
				VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  pgAdminBootstrapSecretName(console.Name),
					DefaultMode: ptrTo(int32(0o440)),
				}},
			},
		)
		if r.OperatorImage != "" {
			volumes = append(volumes,
				corev1.Volume{Name: adminSyncBinVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				corev1.Volume{Name: adminSyncTLSVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
					SecretName:  adminsync.SidecarSecretName(console.Name),
					DefaultMode: ptrTo(int32(0o440)),
				}}},
				corev1.Volume{Name: adminSyncPassfileVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			)
			initContainers = append(initContainers, r.adminSyncInitContainer())
			containers = append(containers, r.adminSyncSidecarContainer(console, inputs.PgAdminImage))
		}
	}

	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: application.SelectorLabels(console.Name)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      application.CommonLabels(console.Name),
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccountName,
					// The token is opted into per container through the
					// projected volume below, never ambient, so an injected
					// container starts with no API credential.
					AutomountServiceAccountToken: ptrTo(false),
					RestartPolicy:                corev1.RestartPolicyAlways,
					DNSPolicy:                    corev1.DNSClusterFirst,
					// The hardening is the operator's and is not configurable.
					// fsGroup is the exception: only the platform knows what
					// value is admissible, and the evidence sidecar needs one
					// on any cluster that does not default it.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptrTo(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						FSGroup:        console.Spec.PodSecurityContext.FSGroup,
					},
					InitContainers: initContainers,
					Containers:     containers,
					Volumes:        volumes,
				},
			},
		},
	}
	if inputs.Composition != nil {
		composeEvidence(deployment, console, inputs.ViewerImage, inputs.Composition, inputs.TokenSecretName)
	}
	if err := controllerutil.SetControllerReference(console, deployment, r.Scheme); err != nil {
		return nil, err
	}
	return deployment, nil
}

// proxyContainer builds the pgtoolbox-proxy container: the single
// authentication and coarse authorization boundary of the console. It reads
// its configuration from the rendered Secret and the OIDC client secret from
// the referenced Secret, and it holds the API credential that creates
// PgToolBoxAccessRequests.
func (r *Reconciler) proxyContainer(console *pgtoolboxv1alpha1.PgConsole, image string) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: proxyConfigVolume, MountPath: proxyConfigMountPath, ReadOnly: true},
		{Name: kubeAPIAccessVolume, MountPath: serviceAccountRoot, ReadOnly: true},
	}
	if console.Spec.Proxy.Authentication.Mode == pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC {
		mounts = append(mounts, corev1.VolumeMount{
			Name: oidcClientVolume, MountPath: oidcClientSecretMountPath, ReadOnly: true,
		})
	}
	return corev1.Container{
		Name:            "proxy",
		Image:           image,
		ImagePullPolicy: imagePullPolicy(console.Spec.Proxy.Image),
		Env: []corev1.EnvVar{
			{Name: "PROXY_CONFIG_FILE", Value: proxyConfigMountPath + "/" + proxyConfigFileName},
		},
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: proxyPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(proxyPort),
			}},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(proxyPort),
			}},
		},
		Resources: containerResources(console.Spec.Proxy.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

// consoleContainer builds the pgConsole container: environment in the fixed
// order of the application's configuration contract, its own health
// endpoints as probes, and the restricted security context its image is
// built for. The namespace is rendered from the owning resource rather than
// the downward API so the build stays a pure function of the spec.
func (r *Reconciler) consoleContainer(console *pgtoolboxv1alpha1.PgConsole, image string) corev1.Container {
	return corev1.Container{
		Name:            "pgconsole",
		Image:           image,
		ImagePullPolicy: imagePullPolicy(console.Spec.Image),
		Env:             consoleEnv(console),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: consolePort,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/healthz", Port: intstr.FromInt32(consolePort),
			}},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(consolePort),
			}},
		},
		Resources: containerResources(console.Spec.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "tmp", MountPath: "/tmp"},
			{Name: kubeAPIAccessVolume, MountPath: serviceAccountRoot, ReadOnly: true},
		},
	}
}

// pgAdminContainer builds the embedded pgAdmin, dedicated to this console's
// one cluster. It listens on the loopback interface only — the /pgadmin
// route of the proxy is its single entry point — and keeps its settings
// database on the settings PVC. The root filesystem stays writable: stock
// pgAdmin writes its runtime configuration under its home, and the hardened
// subset (no privilege escalation, all capabilities dropped, non-root pod)
// is what this step needs; user and server sync lands in a later step.
func (r *Reconciler) pgAdminContainer(console *pgtoolboxv1alpha1.PgConsole, image string) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: pgAdminSettingsVolume, MountPath: pgAdminSettingsMountPath},
		{Name: pgAdminBootstrapVolume, MountPath: pgAdminBootstrapMountPath, ReadOnly: true},
	}
	// The .pgpass the sidecar writes exists only when the sidecar does; a
	// mount naming a volume the Pod never declared is rejected outright.
	if r.OperatorImage != "" {
		mounts = append(mounts, corev1.VolumeMount{
			Name: adminSyncPassfileVolume, MountPath: adminSyncPassfileMountPath,
		})
	}
	mounts = append(mounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})

	return corev1.Container{
		Name:            "pgadmin",
		Image:           image,
		ImagePullPolicy: imagePullPolicy(console.Spec.PgAdmin.Image),
		Env: []corev1.EnvVar{
			{Name: "PGADMIN_LISTEN_ADDRESS", Value: "127.0.0.1"},
			{Name: "PGADMIN_LISTEN_PORT", Value: portString(pgAdminPort)},
			// Without an initial account pgAdmin refuses to start, and its
			// settings database is never initialized — which is what makes
			// setup.py, and so the whole admin-sync path, work at all. The
			// password arrives as a file so it never appears in the Pod spec.
			// pgAdmin is reached through the proxy under a path prefix, and
			// the proxy forwards the path as-is rather than stripping it —
			// stripping would make pgAdmin's own absolute links (/static,
			// /login) escape the prefix and land on the console. SCRIPT_NAME
			// is how a WSGI application is told the prefix it is mounted
			// under, so it both serves and generates URLs there. Without it
			// every request under /pgadmin is a 404 from pgAdmin itself.
			{Name: "SCRIPT_NAME", Value: pgAdminLinkPath},
			{Name: "PGADMIN_DEFAULT_EMAIL", Value: pgAdminBootstrapEmail},
			{
				Name:  "PGADMIN_DEFAULT_PASSWORD_FILE",
				Value: pgAdminBootstrapMountPath + "/" + pgAdminBootstrapPasswordKey,
			},
			// Trust the identity the proxy already established rather than
			// asking for it a second time. Anyone reaching /pgadmin has been
			// authenticated by pgtoolbox-proxy and carries the same
			// X-Forwarded-User the console reads, and the accounts the
			// admin-sync sidecar creates are webserver-auth accounts with no
			// password of their own — so without this pgAdmin offers a login
			// form that none of them could ever satisfy.
			//
			// The header is trustworthy here for the same reason it is for
			// the console: the proxy strips any client-supplied copy before
			// setting its own, and the generated NetworkPolicy confines
			// ingress to the proxy. pgAdmin falls back to reading it from
			// the request headers when it is absent from the WSGI environ.
			{Name: "PGADMIN_CONFIG_AUTHENTICATION_SOURCES", Value: "['webserver']"},
			{Name: "PGADMIN_CONFIG_WEBSERVER_REMOTE_USER", Value: "'" + consoleTrustedUserHeader + "'"},
			// With an external authentication source pgAdmin would still
			// demand a master password to unlock its own credential store.
			// There is nothing in it to unlock: server passwords reach
			// PostgreSQL through the .pgpass file the admin-sync sidecar
			// writes, never through pgAdmin's saved-password store.
			{Name: "PGADMIN_CONFIG_MASTER_PASSWORD_REQUIRED", Value: "False"},
		},
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: pgAdminPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		Resources: adminSyncResources(console.Spec.PgAdmin.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: mounts,
	}
}

// adminSyncInitContainer copies the operator binary into the shared emptyDir
// so the sidecar container can run it without pulling a second image.
func (r *Reconciler) adminSyncInitContainer() corev1.Container {
	return corev1.Container{
		Name:            "admin-sync-init",
		Image:           r.OperatorImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/manager", "admin-sync-init", "--target", adminSyncBinMountPath + "/manager"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: adminSyncBinVolume, MountPath: adminSyncBinMountPath},
		},
	}
}

// adminSyncSidecarContainer serves the in-pod admin-sync API that the
// operator uses to apply pgAdmin user and server state.
func (r *Reconciler) adminSyncSidecarContainer(console *pgtoolboxv1alpha1.PgConsole, image string) corev1.Container {
	return corev1.Container{
		Name:            "admin-sync",
		Image:           image,
		ImagePullPolicy: imagePullPolicy(console.Spec.PgAdmin.Image),
		Command: []string{
			adminSyncBinMountPath + "/manager", "admin-sync-sidecar",
			"--cert-dir", adminSyncTLSMountPath,
			"--token-file", adminSyncTLSMountPath + "/token",
			"--pass-file", adminSyncPassfileMountPath + "/pgpass",
		},
		Ports: []corev1.ContainerPort{{
			Name:          "admin-sync",
			ContainerPort: adminsync.SidecarPort,
			Protocol:      corev1.ProtocolTCP,
		}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(adminsync.SidecarPort)},
			},
			InitialDelaySeconds: 2,
			PeriodSeconds:       10,
		},
		Resources: adminSyncResources(console.Spec.PgAdmin.Resources),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptrTo(false),
			ReadOnlyRootFilesystem:   ptrTo(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: adminSyncBinVolume, MountPath: adminSyncBinMountPath, ReadOnly: true},
			{Name: adminSyncTLSVolume, MountPath: adminSyncTLSMountPath, ReadOnly: true},
			{Name: adminSyncPassfileVolume, MountPath: adminSyncPassfileMountPath},
			// setup.py is the only sanctioned way to change a pgAdmin user,
			// and it operates on the settings database itself. Without this
			// mount every sync fails with a FileNotFoundError for a SQLite
			// path the sidecar cannot see.
			{Name: pgAdminSettingsVolume, MountPath: pgAdminSettingsMountPath},
			{Name: "tmp", MountPath: "/tmp"},
		},
	}
}

// kubeAPIAccessProjection hand-builds the volume automount would inject,
// under a fixed name and expiry so the template stays deterministic. The
// API-speaking containers opt in; the point of building it explicitly is
// that the viewer never gets one.
func kubeAPIAccessProjection() corev1.Volume {
	defaultMode := int32(0o444)
	return corev1.Volume{
		Name: kubeAPIAccessVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: &defaultMode,
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Path:              "token",
					ExpirationSeconds: &projectedTokenExpiration,
				}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{DownwardAPI: &corev1.DownwardAPIProjection{
					Items: []corev1.DownwardAPIVolumeFile{{
						Path:     "namespace",
						FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
					}},
				}},
			},
		}},
	}
}

// pgAdminSettingsPVC builds the PVC backing pgAdmin's settings database.
func (r *Reconciler) pgAdminSettingsPVC(console *pgtoolboxv1alpha1.PgConsole) (*corev1.PersistentVolumeClaim, error) {
	size := console.Spec.PgAdmin.Storage.Size
	if size.IsZero() {
		size = resource.MustParse(defaultPgAdminSettingsSize)
	}
	pvc := &corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, pgAdminSettingsSuffix),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if storageClass := console.Spec.PgAdmin.Storage.StorageClass; storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}
	if err := controllerutil.SetControllerReference(console, pvc, r.Scheme); err != nil {
		return nil, err
	}
	return pvc, nil
}

// reconcilePgAdminSettingsPVC creates the settings PVC when pgAdmin is
// enabled and the claim does not exist yet. An existing claim is never
// rewritten: resizing is an administrative action, and a shrink would be
// rejected anyway.
func (r *Reconciler) reconcilePgAdminSettingsPVC(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	if !pgAdminEnabled(console) {
		return nil
	}
	pvc, err := r.pgAdminSettingsPVC(console)
	if err != nil {
		return err
	}
	var existing corev1.PersistentVolumeClaim
	err = r.Get(ctx, client.ObjectKeyFromObject(pvc), &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, pvc)
}

// reconcileDeployment applies desired unless the live Deployment already
// matches on the operator-managed fields, returning the object to base
// status on: the existing one when it was already up to date, otherwise the
// applied desired state.
func (r *Reconciler) reconcileDeployment(
	ctx context.Context,
	desired *appsv1.Deployment,
) (*appsv1.Deployment, error) {
	var existing appsv1.Deployment
	err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && deploymentMatches(&existing, desired) {
		return &existing, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.ApplyObject(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

// applyServiceAccount writes the ServiceAccount only on drift.
func (r *Reconciler) applyServiceAccount(ctx context.Context, desired *corev1.ServiceAccount) error {
	var existing corev1.ServiceAccount
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && metadataMatches(&existing, desired) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, desired)
}

// applyService writes the Service only on drift, tolerating the fields the
// platform sets (clusterIP, IP families) by comparing only what this
// operator renders.
func (r *Reconciler) applyService(ctx context.Context, desired *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && metadataMatches(&existing, desired) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) &&
		apiequality.Semantic.DeepEqual(existing.Spec.Ports, desired.Spec.Ports) {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return r.ApplyObject(ctx, desired)
}

// deploymentMatches reports whether the live Deployment already carries the
// desired operator-managed fields, comparing only what this operator sets so
// API-server defaulting of the remainder never causes an update loop.
func deploymentMatches(existing, desired *appsv1.Deployment) bool {
	if !metadataMatches(existing, desired) ||
		!reflect.DeepEqual(existing.Spec.Replicas, desired.Spec.Replicas) ||
		!reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) ||
		!reflect.DeepEqual(existing.Spec.Strategy, desired.Spec.Strategy) ||
		!reflect.DeepEqual(existing.Spec.Template.Labels, desired.Spec.Template.Labels) ||
		!reflect.DeepEqual(existing.Spec.Template.Annotations, desired.Spec.Template.Annotations) {
		return false
	}

	existingPod := &existing.Spec.Template.Spec
	desiredPod := &desired.Spec.Template.Spec
	if existingPod.ServiceAccountName != desiredPod.ServiceAccountName ||
		!reflect.DeepEqual(existingPod.AutomountServiceAccountToken, desiredPod.AutomountServiceAccountToken) ||
		!reflect.DeepEqual(existingPod.SecurityContext, desiredPod.SecurityContext) ||
		len(existingPod.Volumes) != len(desiredPod.Volumes) ||
		len(existingPod.Containers) != len(desiredPod.Containers) {
		return false
	}
	// Full volume comparison, not names only: token rotation changes
	// exactly one volume's secretName, and a matcher that missed it would
	// silently never roll the rotated token out.
	if !reflect.DeepEqual(existingPod.Volumes, desiredPod.Volumes) {
		return false
	}
	for i := range desiredPod.Containers {
		if !containerMatches(&existingPod.Containers[i], &desiredPod.Containers[i]) {
			return false
		}
	}
	return true
}

// containerMatches compares the container fields this operator renders.
func containerMatches(existing, desired *corev1.Container) bool {
	return existing.Name == desired.Name &&
		existing.Image == desired.Image &&
		existing.ImagePullPolicy == desired.ImagePullPolicy &&
		reflect.DeepEqual(existing.Args, desired.Args) &&
		reflect.DeepEqual(existing.Env, desired.Env) &&
		reflect.DeepEqual(existing.Resources, desired.Resources) &&
		reflect.DeepEqual(existing.SecurityContext, desired.SecurityContext) &&
		reflect.DeepEqual(existing.VolumeMounts, desired.VolumeMounts)
}

// metadataMatches reports whether the live object still carries the desired
// labels, annotations and controller owner.
func metadataMatches(existing, desired client.Object) bool {
	return shared.LabelsContain(existing.GetLabels(), desired.GetLabels()) &&
		shared.LabelsContain(existing.GetAnnotations(), desired.GetAnnotations()) &&
		shared.ControllerOwnerMatches(existing, desired)
}

func ptrTo[T any](value T) *T { return &value }
