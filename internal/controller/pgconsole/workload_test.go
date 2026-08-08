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
	"reflect"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/evidence"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// testInputs is the workload input set matching testConsole.
func testInputs() workloadInputs {
	return workloadInputs{
		ProxyImage:     "example.com/proxy:1.0.0",
		ConsoleImage:   "example.com/pgconsole:1.0.0",
		PgAdminImage:   "example.com/pgadmin:8.0",
		ConfigChecksum: "deadbeef",
	}
}

func testComposition() *evidenceComposition {
	return &evidenceComposition{
		ObjectStoreName: "store-1",
		ClusterUID:      "uid-cluster",
		ServerName:      "cluster-1",
		DestinationPath: "s3://backups/cluster-1",
		Destination:     evidence.Destination{Bucket: "backups", Prefix: "cluster-1"},
		AccessKeyID:     secretKeyRef{Name: "store-creds", Key: "ACCESS_KEY_ID"},
		SecretAccessKey: secretKeyRef{Name: "store-creds", Key: "SECRET_ACCESS_KEY"},
		Fingerprint:     "sha256:abc",
	}
}

func containerByName(t *testing.T, pod *corev1.PodSpec, name string) *corev1.Container {
	t.Helper()
	container := findContainer(pod, name)
	if container == nil {
		t.Fatalf("container %q missing from pod (have %v)", name, containerNames(pod))
	}
	return container
}

func containerNames(pod *corev1.PodSpec) []string {
	names := make([]string, 0, len(pod.Containers))
	for i := range pod.Containers {
		names = append(names, pod.Containers[i].Name)
	}
	return names
}

func envValue(container *corev1.Container, name string) (string, bool) {
	for _, env := range container.Env {
		if env.Name == name {
			return env.Value, true
		}
	}
	return "", false
}

func volumeByName(pod *corev1.PodSpec, name string) *corev1.Volume {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == name {
			return &pod.Volumes[i]
		}
	}
	return nil
}

func hasMount(container *corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func buildDeployment(t *testing.T, console *pgtoolboxv1alpha1.PgConsole, inputs workloadInputs) *appsv1.Deployment {
	t.Helper()
	r, _ := newTestReconciler(t)
	deployment, err := r.deployment(console, inputs)
	if err != nil {
		t.Fatalf("build deployment: %v", err)
	}
	return deployment
}

func TestDeploymentContainersAndEnv(t *testing.T) {
	console := testConsole()
	deployment := buildDeployment(t, console, testInputs())
	pod := &deployment.Spec.Template.Spec

	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"proxy", "pgconsole", "pgadmin"}) {
		t.Fatalf("containers = %v", got)
	}

	proxy := containerByName(t, pod, "proxy")
	if proxy.Image != "example.com/proxy:1.0.0" {
		t.Fatalf("proxy image = %q", proxy.Image)
	}
	if value, ok := envValue(proxy, "PROXY_CONFIG_FILE"); !ok || value != "/etc/pgtoolbox-proxy/config.yaml" {
		t.Fatalf("proxy PROXY_CONFIG_FILE = %q", value)
	}
	if !hasMount(proxy, proxyConfigVolume) || !hasMount(proxy, kubeAPIAccessVolume) {
		t.Fatalf("proxy mounts = %+v", proxy.VolumeMounts)
	}

	app := containerByName(t, pod, "pgconsole")
	wantEnv := map[string]string{
		"CLUSTER_NAME":        "cluster-1",
		"NAMESPACE":           "test",
		"TRUSTED_USER_HEADER": "X-Forwarded-User",
		"ALLOW_OPERATIONS":    "true",
		"ALLOW_LOGS":          "true",
	}
	for name, want := range wantEnv {
		if value, ok := envValue(app, name); !ok || value != want {
			t.Fatalf("pgconsole env %s = %q, want %q", name, value, want)
		}
	}

	pgAdmin := containerByName(t, pod, "pgadmin")
	// The proxy forwards /pgadmin without stripping the prefix — stripping
	// would send pgAdmin's own absolute links to the console instead — so
	// pgAdmin has to be told the prefix it is mounted under. Without it
	// every request under /pgadmin is a 404 from pgAdmin itself.
	if value, _ := envValue(pgAdmin, "SCRIPT_NAME"); value != "/pgadmin" {
		t.Fatalf("pgAdmin SCRIPT_NAME = %q, want the proxy route prefix", value)
	}
	// pgAdmin trusts the identity the proxy already established, so a user
	// who reached /pgadmin is not asked for credentials a second time. The
	// admin-sync sidecar creates webserver-auth accounts with no password
	// of their own, so an internal login form could never admit them.
	for name, want := range map[string]string{
		"PGADMIN_CONFIG_AUTHENTICATION_SOURCES":   "['webserver']",
		"PGADMIN_CONFIG_WEBSERVER_REMOTE_USER":    "'X-Forwarded-User'",
		"PGADMIN_CONFIG_MASTER_PASSWORD_REQUIRED": "False",
	} {
		if value, _ := envValue(pgAdmin, name); value != want {
			t.Fatalf("pgAdmin %s = %q, want %q", name, value, want)
		}
	}
	// The header pgAdmin trusts must be the one the proxy sets, or it
	// admits nobody — and it is only trustworthy because the proxy strips
	// any client copy and the NetworkPolicy confines ingress to it.
	if value, _ := envValue(pgAdmin, "PGADMIN_CONFIG_WEBSERVER_REMOTE_USER"); value != "'"+consoleTrustedUserHeader+"'" {
		t.Fatalf("pgAdmin trusts %s, not the header the proxy sets", value)
	}
	if value, _ := envValue(pgAdmin, "PGADMIN_LISTEN_PORT"); value != "8081" {
		t.Fatalf("pgAdmin listen port = %q", value)
	}
	if value, _ := envValue(pgAdmin, "PGADMIN_LISTEN_ADDRESS"); value != "127.0.0.1" {
		t.Fatalf("pgAdmin listen address = %q", value)
	}
	if !hasMount(pgAdmin, pgAdminSettingsVolume) {
		t.Fatalf("pgAdmin must mount the settings volume")
	}

	settings := volumeByName(pod, pgAdminSettingsVolume)
	if settings == nil || settings.PersistentVolumeClaim == nil ||
		settings.PersistentVolumeClaim.ClaimName != "console-pgconsole-pgadmin" {
		t.Fatalf("settings volume = %+v", settings)
	}
	config := volumeByName(pod, proxyConfigVolume)
	if config == nil || config.Secret == nil || config.Secret.SecretName != "console-pgconsole-proxy" {
		t.Fatalf("proxy config volume = %+v", config)
	}
}

// TestDeploymentUsesOperatorDefaultImages covers the spec the quick start
// actually shows: one that names no images at all and expects the operator's
// --default-*-image flags to supply them.
//
// The image fields are pointers for this reason. Go does not omit an empty
// struct, so a value field serialized as `image: {}` and the API server
// rejected it against ImageSpec's required repository and tag — making the
// operator defaults unreachable and the documented quick start invalid. Only
// a real API server enforces that; the e2e smoke test caught it.
func TestDeploymentUsesOperatorDefaultImages(t *testing.T) {
	console := testConsole()
	console.Spec.Image = nil
	console.Spec.Proxy.Image = nil
	console.Spec.PgAdmin.Image = nil

	r, _ := newTestReconciler(t)
	r.DefaultImages = DefaultImages{
		PgConsole: "ghcr.io/example/pgconsole:0.1.0",
		Proxy:     "ghcr.io/example/pgtoolbox-proxy:0.1.0",
		PgAdmin:   "ghcr.io/fyannk/pgadmin:0.1.0",
	}

	inputs := workloadInputs{ConfigChecksum: "deadbeef"}
	var err error
	if inputs.ProxyImage, err = resolveImage(console.Spec.Proxy.Image, r.DefaultImages.Proxy); err != nil {
		t.Fatalf("resolve proxy image: %v", err)
	}
	if inputs.ConsoleImage, err = resolveImage(console.Spec.Image, r.DefaultImages.PgConsole); err != nil {
		t.Fatalf("resolve console image: %v", err)
	}
	if inputs.PgAdminImage, err = resolveImage(console.Spec.PgAdmin.Image, r.DefaultImages.PgAdmin); err != nil {
		t.Fatalf("resolve pgAdmin image: %v", err)
	}

	pod := &buildDeployment(t, console, inputs).Spec.Template.Spec
	for name, want := range map[string]string{
		"proxy":     "ghcr.io/example/pgtoolbox-proxy:0.1.0",
		"pgconsole": "ghcr.io/example/pgconsole:0.1.0",
		"pgadmin":   "ghcr.io/fyannk/pgadmin:0.1.0",
	} {
		if got := containerByName(t, pod, name).Image; got != want {
			t.Errorf("%s image = %q, want the operator default %q", name, got, want)
		}
	}

	// With neither a spec image nor an operator default, the container
	// cannot be composed and that must be an error, not an empty reference.
	if _, err := resolveImage(nil, ""); err == nil {
		t.Error("expected an error when both the spec image and the default are absent")
	}
}

// TestDeploymentFSGroup covers the one pod-level security setting the
// operator does not decide. pgObjectStoreViewer refuses to serve unless the
// shared socket directory is setgid with group rwx, and on an emptyDir that
// mode comes from the kubelet applying fsGroup — so on a cluster that does
// not default one, an unset fsGroup is why evidence never comes up.
func TestDeploymentFSGroup(t *testing.T) {
	// Unset by default: OpenShift's SCC allocates one from the namespace
	// range, and a value outside that range is rejected outright.
	unset := buildDeployment(t, testConsole(), testInputs())
	if group := unset.Spec.Template.Spec.SecurityContext.FSGroup; group != nil {
		t.Fatalf("fsGroup = %d, want unset so the platform may allocate it", *group)
	}

	console := testConsole()
	console.Spec.PodSecurityContext.FSGroup = ptrTo(int64(65532))
	set := buildDeployment(t, console, testInputs())

	pod := set.Spec.Template.Spec.SecurityContext
	if pod.FSGroup == nil || *pod.FSGroup != 65532 {
		t.Fatalf("fsGroup = %v, want 65532", pod.FSGroup)
	}
	// The hardening is not configurable and must survive alongside it.
	if pod.RunAsNonRoot == nil || !*pod.RunAsNonRoot ||
		pod.SeccompProfile == nil || pod.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod hardening weakened by setting fsGroup: %+v", pod)
	}
}

func TestDeploymentFrame(t *testing.T) {
	console := testConsole()
	deployment := buildDeployment(t, console, testInputs())

	if *deployment.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d, want 1", *deployment.Spec.Replicas)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q, want Recreate (settings PVC cannot be shared)", deployment.Spec.Strategy.Type)
	}
	pod := &deployment.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatalf("automount must be disabled; containers opt in explicitly")
	}
	if pod.ServiceAccountName != "console-pgconsole" {
		t.Fatalf("service account = %q", pod.ServiceAccountName)
	}
	if got := deployment.Spec.Template.Annotations[pgtoolboxv1alpha1.ConfigChecksumAnnotation]; got != "deadbeef" {
		t.Fatalf("config checksum annotation = %q", got)
	}
	if len(deployment.OwnerReferences) != 1 || deployment.OwnerReferences[0].UID != console.UID {
		t.Fatalf("owner references = %+v", deployment.OwnerReferences)
	}
}

func TestDeploymentDeterministic(t *testing.T) {
	console := testConsole()
	first := buildDeployment(t, console, testInputs())
	second := buildDeployment(t, console, testInputs())
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("identical inputs built different deployments")
	}

	// The checksum annotation is the rollout trigger: a changed rendered
	// config must change the pod template.
	changed := testInputs()
	changed.ConfigChecksum = "cafebabe"
	third := buildDeployment(t, console, changed)
	if reflect.DeepEqual(first.Spec.Template, third.Spec.Template) {
		t.Fatalf("a changed config checksum must change the pod template")
	}
}

func TestDeploymentEvidenceComposition(t *testing.T) {
	console := testConsole()
	inputs := testInputs()
	inputs.Composition = testComposition()
	inputs.TokenSecretName = "console-pgconsole-evidence-t1"
	inputs.ViewerImage = "example.com/objectstoreviewer:1.0.0"
	deployment := buildDeployment(t, console, inputs)
	pod := &deployment.Spec.Template.Spec

	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"proxy", "pgconsole", "pgadmin", "objectstoreviewer"}) {
		t.Fatalf("containers = %v", got)
	}

	app := containerByName(t, pod, "pgconsole")
	for _, name := range []string{
		"REPOSITORY_EVIDENCE_URL",
		"REPOSITORY_EVIDENCE_TOKEN_FILE",
		"REPOSITORY_EXPECTED_FINGERPRINT",
		"REPOSITORY_BARMAN_SERVER",
	} {
		if _, ok := envValue(app, name); !ok {
			t.Fatalf("pgconsole env %s missing; the evidence contract is all-or-nothing", name)
		}
	}
	if !hasMount(app, evidenceSocketVolume) || !hasMount(app, evidenceTokenVolume) {
		t.Fatalf("pgconsole must mount the evidence socket and token")
	}

	viewer := containerByName(t, pod, "objectstoreviewer")
	if viewer.Image != "example.com/objectstoreviewer:1.0.0" {
		t.Fatalf("viewer image = %q", viewer.Image)
	}
	if hasMount(viewer, kubeAPIAccessVolume) {
		t.Fatalf("the viewer must hold no Kubernetes API credential")
	}
	if !hasMount(viewer, evidenceSocketVolume) || !hasMount(viewer, evidenceTokenVolume) ||
		!hasMount(viewer, storeCredentialVolume) {
		t.Fatalf("viewer mounts = %+v", viewer.VolumeMounts)
	}

	token := volumeByName(pod, evidenceTokenVolume)
	if token == nil || token.Secret == nil || token.Secret.SecretName != "console-pgconsole-evidence-t1" {
		t.Fatalf("evidence token volume = %+v", token)
	}
	creds := volumeByName(pod, storeCredentialVolume)
	if creds == nil || creds.Projected == nil || len(creds.Projected.Sources) != 2 {
		t.Fatalf("store credential volume = %+v", creds)
	}
}

func TestDeploymentOIDCModeMountsClientSecret(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication = pgtoolboxv1alpha1.ProxyAuthenticationSpec{
		Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC,
		OIDC: &pgtoolboxv1alpha1.ProxyOIDCSpec{
			IssuerURL:       "https://idp.example.com",
			ClientID:        "pgconsole",
			ClientSecretRef: pgtoolboxv1alpha1.SecretKeyReference{Name: "oidc-client"},
		},
	}
	deployment := buildDeployment(t, console, testInputs())
	pod := &deployment.Spec.Template.Spec

	volume := volumeByName(pod, oidcClientVolume)
	if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "oidc-client" {
		t.Fatalf("oidc client volume = %+v", volume)
	}
	if len(volume.Secret.Items) != 1 ||
		volume.Secret.Items[0].Key != defaultOIDCClientSecretKey ||
		volume.Secret.Items[0].Path != oidcClientSecretFile {
		t.Fatalf("oidc client items = %+v", volume.Secret.Items)
	}
	proxy := containerByName(t, pod, "proxy")
	if !hasMount(proxy, oidcClientVolume) {
		t.Fatalf("proxy must mount the OIDC client secret")
	}
}

func TestDeploymentPgAdminDisabled(t *testing.T) {
	console := testConsole()
	disabled := false
	console.Spec.PgAdmin.Enabled = &disabled
	deployment := buildDeployment(t, console, testInputs())
	pod := &deployment.Spec.Template.Spec

	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"proxy", "pgconsole"}) {
		t.Fatalf("containers = %v", got)
	}
	if volumeByName(pod, pgAdminSettingsVolume) != nil {
		t.Fatalf("no settings volume without pgAdmin")
	}
}

func initContainerNames(pod *corev1.PodSpec) []string {
	names := make([]string, 0, len(pod.InitContainers))
	for i := range pod.InitContainers {
		names = append(names, pod.InitContainers[i].Name)
	}
	return names
}

func initContainerByName(t *testing.T, pod *corev1.PodSpec, name string) *corev1.Container {
	t.Helper()
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == name {
			return &pod.InitContainers[i]
		}
	}
	t.Fatalf("init container %q missing from pod (have %v)", name, initContainerNames(pod))
	return nil
}

func TestDeploymentAdminSyncInjected(t *testing.T) {
	console := testConsole()
	r, _ := newTestReconciler(t)
	r.OperatorImage = "example.com/manager:1.0.0"
	inputs := testInputs()
	inputs.AdminSyncSecretVersion = "v42"

	deployment, err := r.deployment(console, inputs)
	if err != nil {
		t.Fatalf("build deployment: %v", err)
	}
	pod := &deployment.Spec.Template.Spec

	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"proxy", "pgconsole", "pgadmin", "admin-sync"}) {
		t.Fatalf("containers = %v", got)
	}
	if got := initContainerNames(pod); !reflect.DeepEqual(got, []string{"admin-sync-init"}) {
		t.Fatalf("init containers = %v", got)
	}

	if got := deployment.Spec.Template.Annotations[pgtoolboxv1alpha1.AdminSyncSecretVersionAnnotation]; got != "v42" {
		t.Fatalf("admin-sync secret version annotation = %q, want v42", got)
	}

	for _, name := range []string{adminSyncBinVolume, adminSyncTLSVolume, adminSyncPassfileVolume} {
		if volumeByName(pod, name) == nil {
			t.Fatalf("missing volume %q", name)
		}
	}
	tlsVolume := volumeByName(pod, adminSyncTLSVolume)
	if tlsVolume.Secret == nil || tlsVolume.Secret.SecretName != "console-pgconsole-pgadmin-sync" {
		t.Fatalf("admin-sync TLS volume = %+v", tlsVolume)
	}

	initContainer := initContainerByName(t, pod, "admin-sync-init")
	if initContainer.Image != "example.com/manager:1.0.0" {
		t.Fatalf("admin-sync-init image = %q", initContainer.Image)
	}
	if got := initContainer.Command; len(got) != 4 || got[3] != adminSyncBinMountPath+"/manager" {
		t.Fatalf("admin-sync-init command = %v", got)
	}

	sidecar := containerByName(t, pod, "admin-sync")
	if sidecar.Image != console.Spec.PgAdmin.Image.Repository+":"+console.Spec.PgAdmin.Image.Tag {
		t.Fatalf("admin-sync sidecar image = %q", sidecar.Image)
	}
	if !hasMount(sidecar, adminSyncBinVolume) || !hasMount(sidecar, adminSyncTLSVolume) || !hasMount(sidecar, adminSyncPassfileVolume) {
		t.Fatalf("admin-sync sidecar mounts = %+v", sidecar.VolumeMounts)
	}
	if value, _ := envValue(sidecar, "NOT_APPLICABLE"); value != "" {
		t.Fatalf("admin-sync sidecar should have no env")
	}

	pgAdmin := containerByName(t, pod, "pgadmin")
	if !hasMount(pgAdmin, adminSyncPassfileVolume) {
		t.Fatalf("pgadmin must mount the admin-sync passfile volume")
	}
}

func TestDeploymentAdminSyncSkippedWithoutOperatorImage(t *testing.T) {
	console := testConsole()
	deployment := buildDeployment(t, console, testInputs())
	pod := &deployment.Spec.Template.Spec

	if got := containerNames(pod); !reflect.DeepEqual(got, []string{"proxy", "pgconsole", "pgadmin"}) {
		t.Fatalf("containers = %v", got)
	}
	if len(pod.InitContainers) != 0 {
		t.Fatalf("init containers = %v, want none", initContainerNames(pod))
	}
	for _, name := range []string{adminSyncBinVolume, adminSyncTLSVolume, adminSyncPassfileVolume} {
		if volumeByName(pod, name) != nil {
			t.Fatalf("volume %q must not be present without operator image", name)
		}
	}
	if _, ok := deployment.Spec.Template.Annotations[pgtoolboxv1alpha1.AdminSyncSecretVersionAnnotation]; ok {
		t.Fatalf("admin-sync secret version annotation must not be present without operator image")
	}
}

// TestNetworkPolicyAllowsPostgres holds the rule that made "connect to
// server" fail as a bare 502: the embedded pgAdmin exists to reach this one
// cluster, and a default-deny egress policy that omits PostgreSQL forbids
// the only thing it is for. libpq waits on the dropped connection until the
// proxy's upstream timeout, so nothing in any log names the cause.
func TestNetworkPolicyAllowsPostgres(t *testing.T) {
	r, _ := newTestReconciler(t)

	policy, err := r.networkPolicy(testConsole())
	if err != nil {
		t.Fatalf("build network policy: %v", err)
	}
	found := false
	for _, rule := range policy.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port == nil || port.Port.IntVal != 5432 {
				continue
			}
			found = true
			// Scoped to this cluster's instances, not PostgreSQL at large.
			if len(rule.To) != 1 || rule.To[0].PodSelector == nil ||
				rule.To[0].PodSelector.MatchLabels["cnpg.io/cluster"] != "cluster-1" {
				t.Fatalf("postgres egress is not scoped to the target cluster: %+v", rule.To)
			}
		}
	}
	if !found {
		t.Fatalf("no egress rule to PostgreSQL; pgAdmin cannot reach its cluster")
	}

	// With pgAdmin off there is nothing in the Pod that speaks to PostgreSQL.
	noPgAdmin := testConsole()
	noPgAdmin.Spec.PgAdmin.Enabled = ptrTo(false)
	policy, err = r.networkPolicy(noPgAdmin)
	if err != nil {
		t.Fatalf("build network policy: %v", err)
	}
	for _, rule := range policy.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 5432 {
				t.Fatalf("postgres egress granted with pgAdmin disabled")
			}
		}
	}
}
