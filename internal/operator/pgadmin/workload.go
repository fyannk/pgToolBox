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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// AnnotationClientSecretVersion on the Pod template carries the OAuth2
// client Secret's resourceVersion when that Secret is labelled for reload.
const AnnotationClientSecretVersion = pgtoolboxv1alpha1.ReservedKeyPrefix + "client-secret-version"

// credentialsVolumeBytes sizes the memory-backed credential volume: the
// file writer's own bound (adminsync.PassFileVolumeBytes) plus the CA
// files (one per Cluster and their bundle, a few KiB each).
const credentialsVolumeBytes = adminsync.PassFileVolumeBytes + 1<<20

// projectedTokenSeconds is the lifetime of the passfile-sync and
// oauth-proxy API token; the kubelet rotates it at 80%.
var projectedTokenSeconds int64 = 3600

// buildDeployment renders the pgAdmin Deployment: in.Replicas Pods (one
// with SQLite on the ReadWriteOnce settings volume; spec.replicas with
// configDatabase once the settings schema exists), Recreate. The Pod runs pgAdmin, the
// passfile-sync sidecar (the operator binary, copied in by an init
// container and run from pgAdmin's image so it runs under pgAdmin's UID and
// the 0600 files are readable by pgAdmin alone), and on OpenShift the
// oauth-proxy. The template's hash is its annotation: any change of it is
// a rollout, and nothing else is.
func buildDeployment(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) *appsv1.Deployment {
	n := namesOf(p)
	id := identity(p)
	podLabels, podAnnotations := metadata.Propagate(pgtoolboxv1alpha1.PropagationTargetPod, p.Spec.MetadataPropagation,
		id.Labels(componentPgAdmin), nil)
	if in.ClientSecretVersion != "" {
		if podAnnotations == nil {
			podAnnotations = map[string]string{}
		}
		podAnnotations[AnnotationClientSecretVersion] = in.ClientSecretVersion
	}

	containers := []corev1.Container{pgAdminContainer(p, in), passfileSyncContainer(p, in)}
	initContainers := []corev1.Container{installContainer(in)}
	if p.Spec.ConfigDatabase != nil {
		// pgAdmin opens its settings database at start, through the pgpass
		// the sidecar writes: the sidecar starts first (a native sidecar)
		// and is started once the settings database's service is written.
		sidecar := containers[1]
		sidecar.RestartPolicy = new(corev1.ContainerRestartPolicyAlways)
		sidecar.StartupProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/bin/sh", "-c", "grep -qx '\\[" + configDatabaseService + "\\]' " + serviceFilePath,
			}}},
			PeriodSeconds:    2,
			FailureThreshold: 150,
		}
		initContainers = append(initContainers, sidecar)
		containers = containers[:1]
	}
	volumes := podVolumes(p)
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		containers = append(containers, oauthProxyContainer(p, in))
	}

	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
		Spec: corev1.PodSpec{
			ServiceAccountName:            n.base,
			AutomountServiceAccountToken:  new(false),
			EnableServiceLinks:            new(false),
			SecurityContext:               specs.PodSecurityContext(in.PlatformAssignsGroups, fsGroup),
			ImagePullSecrets:              pullSecrets(in.Image, in.ProxyImage, p.Spec.Auth.OpenShiftOAuthProxy != nil),
			InitContainers:                initContainers,
			Containers:                    containers,
			Volumes:                       volumes,
			TerminationGracePeriodSeconds: new(int64(30)),
		},
	}
	hash := specs.Hash(template)
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[pgtoolboxv1alpha1.AnnotationSpecHash] = hash

	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: objectMeta(p, n.base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetWorkload),
		Spec: appsv1.DeploymentSpec{
			Replicas:             new(in.Replicas),
			RevisionHistoryLimit: new(int32(3)),
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector:             &metav1.LabelSelector{MatchLabels: id.SelectorLabels(componentPgAdmin)},
			Template:             template,
		},
	}
	specs.SetHash(deployment, hash)
	return deployment
}

func installContainer(in buildInputs) corev1.Container {
	return corev1.Container{
		Name:            "install-passfile-sync",
		Image:           in.OperatorImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/manager", "install", "--target", binPath},
		SecurityContext: specs.ContainerSecurityContext(),
		Resources:       smallResources(),
		VolumeMounts:    []corev1.VolumeMount{{Name: binVolume, MountPath: binMountPath}},
	}
}

func passfileSyncContainer(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) corev1.Container {
	return corev1.Container{
		Name:            "passfile-sync",
		Image:           in.Image.Reference,
		ImagePullPolicy: in.Image.PullPolicy,
		Command: []string{
			binPath, "passfile-sync",
			"--offer-secret", namesOf(p).offer,
			"--dir", credentialsMountPath,
		},
		Env: []corev1.EnvVar{{
			Name:      "POD_NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}},
		}},
		SecurityContext: specs.ContainerSecurityContext(),
		Resources:       smallResources(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: binVolume, MountPath: binMountPath, ReadOnly: true},
			{Name: credentialsVolume, MountPath: credentialsMountPath},
			{Name: apiAccessVolume, MountPath: apiAccessMountPath, ReadOnly: true},
		},
	}
}

func pgAdminContainer(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: settingsVolume, MountPath: settingsMountPath},
		{Name: tmpVolume, MountPath: "/tmp"},
		// The only other writable path: the image's entrypoint writes its
		// generated configuration and identity here (read-only rootfs).
		{Name: runVolume, MountPath: runMountPath},
		// Read-only: only passfile-sync writes the credential files.
		{Name: credentialsVolume, MountPath: credentialsMountPath, ReadOnly: true},
		{Name: bootstrapVolume, MountPath: bootstrapMountPath, ReadOnly: true},
	}
	if p.Spec.Auth.OAuth2 != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: oauth2Volume, MountPath: oauth2MountPath, ReadOnly: true})
	}
	ping := []string{
		"/venv/bin/python3", "-c",
		fmt.Sprintf("import urllib.request; urllib.request.urlopen('http://127.0.0.1:%d/misc/ping', timeout=4)", pgAdminPort),
	}
	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: ping}},
			PeriodSeconds:    period,
			TimeoutSeconds:   5,
			FailureThreshold: failures,
		}
	}
	container := corev1.Container{
		Name:            "pgadmin",
		Image:           in.Image.Reference,
		ImagePullPolicy: in.Image.PullPolicy,
		Env:             pgAdminEnv(p, in),
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: pgAdminPort, Protocol: corev1.ProtocolTCP}},
		// First start creates the settings database and an upgrade
		// migrates it: up to five minutes before the first ping.
		StartupProbe:   probe(5, 60),
		ReadinessProbe: probe(10, 3),
		LivenessProbe:  probe(30, 3),
		Resources:      pgAdminResources(p.Spec.Resources),
		// Read-only root filesystem: the entrypoint writes under
		// /run/pgadmin, pgAdmin under its settings volume and /tmp.
		SecurityContext: specs.ContainerSecurityContext(),
		VolumeMounts:    mounts,
	}
	return container
}

// pgAdminEnv is pgAdmin's configuration. Values under PGADMIN_CONFIG_ are
// Python expressions the image's entrypoint writes into config_distro.py;
// every string is rendered with pyString, never interpolated raw.
func pgAdminEnv(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) []corev1.EnvVar {
	proxied := p.Spec.Auth.OpenShiftOAuthProxy != nil
	listen := "0.0.0.0"
	if proxied {
		// Behind the oauth-proxy pgAdmin listens on loopback only, so the
		// identity header can come from nothing but the proxy.
		listen = "127.0.0.1"
	}
	secure := proxied || p.Spec.Exposure.TypeOrDefault() != pgtoolboxv1alpha1.ExposureTypeClusterIP
	env := []corev1.EnvVar{
		{Name: "PGADMIN_LISTEN_ADDRESS", Value: listen},
		{Name: "PGADMIN_LISTEN_PORT", Value: strconv.Itoa(int(pgAdminPort))},
		{Name: "PGADMIN_DEFAULT_EMAIL", Value: bootstrapEmail},
		// The generated configuration goes to the run volume, never to the
		// image (pgAdmin 9.0 and later honour it, upstream and hardened).
		{Name: "PGADMIN_CUSTOM_CONFIG_DISTRO_FILE", Value: configDistroPath},
		{Name: "PGADMIN_DEFAULT_PASSWORD_FILE", Value: bootstrapMountPath + "/" + bootstrapKey},
		{Name: "PGADMIN_CONFIG_SERVER_MODE", Value: "True"},
		// Passwords never enter pgAdmin's store: the credential files do.
		{Name: "PGADMIN_CONFIG_MASTER_PASSWORD_REQUIRED", Value: "False"},
		{Name: "PGADMIN_CONFIG_ALLOW_SAVE_PASSWORD", Value: "False"},
		{Name: "PGADMIN_CONFIG_ALLOW_SAVE_TUNNEL_PASSWORD", Value: "False"},
		{Name: "PGADMIN_CONFIG_UPGRADE_CHECK_ENABLED", Value: "False"},
		{Name: "PGADMIN_CONFIG_ENABLE_PSQL", Value: "False"},
		{Name: "PGADMIN_CONFIG_SESSION_COOKIE_SECURE", Value: pyBool(secure)},
		// libpq: the credentials live on the tmpfs, outside STORAGE_DIR,
		// and every connection verifies the server and requires SCRAM
		// with channel binding — also for a server a user registers.
		{Name: "PGSERVICEFILE", Value: serviceFilePath},
		{Name: "PGPASSFILE", Value: passFilePath},
		{Name: "PGSSLMODE", Value: "verify-full"},
		{Name: "PGSSLROOTCERT", Value: credentialsMountPath + "/ca/bundle.crt"},
		{Name: "PGREQUIREAUTH", Value: "scram-sha-256"},
		{Name: "PGCHANNELBINDING", Value: "require"},
	}
	if in.ConfigDatabase.URI != "" {
		// No password in it: libpq reads it from PGPASSFILE.
		env = append(env, corev1.EnvVar{Name: "PGADMIN_CONFIG_CONFIG_DATABASE_URI", Value: pyString(in.ConfigDatabase.URI)})
	}
	switch {
	case p.Spec.Auth.OAuth2 != nil:
		env = append(env,
			corev1.EnvVar{Name: "PGADMIN_CONFIG_AUTHENTICATION_SOURCES", Value: "['oauth2']"},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_OAUTH2_AUTO_CREATE_USER", Value: "True"},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_OAUTH2_CONFIG", Value: oauth2Config(p.Spec.Auth.OAuth2)},
		)
	case proxied:
		env = append(env,
			corev1.EnvVar{Name: "PGADMIN_CONFIG_AUTHENTICATION_SOURCES", Value: "['webserver']"},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_WEBSERVER_AUTO_CREATE_USER", Value: "True"},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_WEBSERVER_REMOTE_USER", Value: pyString("X-Forwarded-User")},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_WEBSERVER_REMOTE_USER_FROM_HEADER", Value: "True"},
			corev1.EnvVar{Name: "PGADMIN_CONFIG_WEBSERVER_TRUSTED_PROXIES", Value: "['127.0.0.1/32']"},
		)
	}
	return env
}

// oauth2Config renders OAUTH2_CONFIG. The client secret is read from the
// mounted file when pgAdmin loads its configuration; it is in neither the
// Pod spec nor the operator.
func oauth2Config(o *pgtoolboxv1alpha1.PgAdminOAuth2Spec) string {
	issuer := strings.TrimRight(o.IssuerURL, "/")
	display := o.DisplayName
	if display == "" {
		display = "Single sign-on"
	}
	claims := make([]string, 0, len(o.RequiredClaims))
	for _, key := range slices.Sorted(maps.Keys(o.RequiredClaims)) {
		values := make([]string, 0, len(o.RequiredClaims[key]))
		for _, value := range o.RequiredClaims[key] {
			values = append(values, pyString(value))
		}
		claims = append(claims, pyString(key)+": ["+strings.Join(values, ", ")+"]")
	}
	fields := [][2]string{
		{"OAUTH2_NAME", pyString("pgtoolbox")},
		{"OAUTH2_DISPLAY_NAME", pyString(display)},
		{"OAUTH2_CLIENT_ID", pyString(o.ClientID)},
		{"OAUTH2_CLIENT_SECRET", "open(" + pyString(oauth2MountPath+"/"+oauth2SecretFile) + ").read().strip()"},
		{"OAUTH2_SERVER_METADATA_URL", pyString(issuer + "/.well-known/openid-configuration")},
		{"OAUTH2_API_BASE_URL", pyString(issuer + "/")},
		{"OAUTH2_SCOPE", pyString(strings.Join(o.ScopesOrDefault(), " "))},
		{"OAUTH2_USERNAME_CLAIM", pyString(o.UsernameClaimOrDefault())},
		{"OAUTH2_ADDITIONAL_CLAIMS", "{" + strings.Join(claims, ", ") + "}"},
		{"OAUTH2_SSL_CERT_VERIFICATION", "True"},
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, pyString(f[0])+": "+f[1])
	}
	return "[{" + strings.Join(parts, ", ") + "}]"
}

// pyString renders s as a single-quoted Python string literal that
// evaluates to s exactly: quote and backslash escaped, every
// non-printable or non-ASCII rune as a \u or \U escape. Admission already
// refuses quotes, backslashes and line breaks in these fields; this keeps
// the configuration file a literal even if one gets through.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch {
		case r == '\\' || r == '\'':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			fmt.Fprintf(&b, "\\U%08x", r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// ConfigDatabaseURILiteral is the content of the file pgAdmin's image reads
// (PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE) for a settings database URI: the
// same quoted Python literal the operator puts inline in
// PGADMIN_CONFIG_CONFIG_DATABASE_URI (the image's entrypoint needs a literal
// to check whether the external database is initialised, and writes the value
// verbatim into config_distro.py). Surrounding whitespace, as a Secret made
// from a file carries, is dropped; a control character inside is refused.
func ConfigDatabaseURILiteral(uri string) (string, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", errors.New("the settings database URI is empty")
	}
	if strings.ContainsFunc(uri, unicode.IsControl) {
		return "", errors.New("the settings database URI contains a control character")
	}
	return pyString(uri), nil
}

func pyBool(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

// proxySAR is the SubjectAccessReview the oauth-proxy asks for each user, in
// the shape it posts as is to authorization.openshift.io/v1
// subjectaccessreviews (the group field is resourceAPIGroup there; an
// unknown field is dropped, not refused). The verb is PgAdminConnectVerb,
// which neither view nor edit carries.
func proxySAR(p *pgtoolboxv1alpha1.PgAdmin) string {
	return fmt.Sprintf(`{"namespace":%q,"verb":%q,"resourceAPIGroup":%q,"resource":"pgadmins","resourceName":%q}`,
		p.Namespace, pgtoolboxv1alpha1.PgAdminConnectVerb, pgtoolboxv1alpha1.GroupVersion.Group, p.Name)
}

func oauthProxyContainer(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) corev1.Container {
	sar := proxySAR(p)
	return corev1.Container{
		Name:            "oauth-proxy",
		Image:           in.ProxyImage.Reference,
		ImagePullPolicy: in.ProxyImage.PullPolicy,
		Args: []string{
			"--provider=openshift",
			"--https-address=:" + strconv.Itoa(int(proxyPort)),
			"--http-address=",
			fmt.Sprintf("--upstream=http://127.0.0.1:%d", pgAdminPort),
			"--openshift-service-account=" + namesOf(p).base,
			// Only who may connect to this PgAdmin — granted by the
			// pgtoolbox-console-operator role, dba only — reaches pgAdmin.
			"--openshift-sar=" + sar,
			"--tls-cert=" + proxyTLSMountPath + "/tls.crt",
			"--tls-key=" + proxyTLSMountPath + "/tls.key",
			"--cookie-secret-file=" + proxyCookiePath + "/" + "cookie-secret",
			"--cookie-secure=true",
			"--pass-user-headers=true",
			"--skip-provider-button=true",
		},
		Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: proxyPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/oauth/healthz", Port: intstr.FromString("https"), Scheme: corev1.URISchemeHTTPS,
			}},
			PeriodSeconds: 10,
		},
		SecurityContext: specs.ContainerSecurityContext(),
		Resources:       smallResources(),
		VolumeMounts: []corev1.VolumeMount{
			{Name: proxyTLSVolume, MountPath: proxyTLSMountPath, ReadOnly: true},
			{Name: proxyCookieVolume, MountPath: proxyCookiePath, ReadOnly: true},
			{Name: apiAccessVolume, MountPath: apiAccessMountPath, ReadOnly: true},
		},
	}
}

func podVolumes(p *pgtoolboxv1alpha1.PgAdmin) []corev1.Volume {
	n := namesOf(p)
	secretMode := new(int32(0o440))
	volumes := []corev1.Volume{
		settingsVolumeSource(p),
		{Name: tmpVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(512<<20, resource.BinarySI),
		}}},
		// Memory-backed so the passwords never reach a node disk or a
		// volume snapshot; outside the settings volume, whose STORAGE_DIR
		// pgAdmin's file manager serves.
		{Name: runVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(16<<20, resource.BinarySI),
		}}},
		{Name: credentialsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: resource.NewQuantity(credentialsVolumeBytes, resource.BinarySI),
		}}},
		{Name: binVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(256<<20, resource.BinarySI),
		}}},
		{Name: bootstrapVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: n.bootstrap, DefaultMode: secretMode,
		}}},
		apiAccessProjection(),
	}
	if o := p.Spec.Auth.OAuth2; o != nil {
		volumes = append(volumes, corev1.Volume{Name: oauth2Volume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: o.ClientSecretRef.Name, DefaultMode: secretMode,
			Items: []corev1.KeyToPath{{Key: o.ClientSecretRef.KeyOrDefault(), Path: oauth2SecretFile}},
		}}})
	}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		volumes = append(volumes,
			corev1.Volume{Name: proxyTLSVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: n.proxyTLS, DefaultMode: secretMode,
			}}},
			corev1.Volume{Name: proxyCookieVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: n.proxyCookie, DefaultMode: secretMode,
			}}},
		)
	}
	return volumes
}

// settingsVolumeSource is pgAdmin's DATA_DIR: the settings PVC, or with
// configDatabase a per-Pod emptyDir (sessions and STORAGE_DIR, ephemeral;
// the settings themselves are in PostgreSQL).
func settingsVolumeSource(p *pgtoolboxv1alpha1.PgAdmin) corev1.Volume {
	if p.Spec.ConfigDatabase != nil {
		return corev1.Volume{Name: settingsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: new(p.Spec.Storage.SizeOrDefault()),
		}}}
	}
	return corev1.Volume{Name: settingsVolume, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: namesOf(p).base},
	}}
}

// apiAccessProjection is the API credential, built explicitly (never
// automounted) with a fixed name and lifetime so the template is
// deterministic, and mounted only into the containers that call the API.
func apiAccessProjection() corev1.Volume {
	return corev1.Volume{Name: apiAccessVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: new(int32(0o444)),
		Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &projectedTokenSeconds}},
			{ConfigMap: &corev1.ConfigMapProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
				Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
			}},
			{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
				Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
			}}}},
		},
	}}}
}

func pullSecrets(image, proxy specsImage, withProxy bool) []corev1.LocalObjectReference {
	out := slices.Clone(image.PullSecrets)
	if withProxy {
		for _, secret := range proxy.PullSecrets {
			if !slices.Contains(out, secret) {
				out = append(out, secret)
			}
		}
	}
	return out
}

type specsImage = specs.ResolvedImage

func smallResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

// pgAdminResources returns the requested resources, or the defaults:
// pgAdmin's Python stack needs a real memory budget.
func pgAdminResources(requested *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if requested != nil {
		return *requested.DeepCopy()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
}
