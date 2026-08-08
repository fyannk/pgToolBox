//go:build e2e

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

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The full composition: every container the operator can put in a console
// pod, running the real family images. TestConsoleSmoke covers the fast path
// with pgAdmin off; this covers what that deliberately leaves out — the
// pgAdmin container and its PVC, the admin-sync init container and sidecar,
// and the evidence sidecar.
//
// Evidence is composed against a real ObjectStore resource but no reachable
// bucket. That is the honest boundary: what this operator owns is the
// composition — the mounts, the token, the fingerprint, the environment —
// and the viewer's liveness is deliberately independent of whether a scan
// succeeds. Whether the viewer can read a repository is pgObjectStoreViewer's
// own test suite's job, not this one's.

const (
	fullConsoleName = "full"
	fullClusterName = "pg-full"
	storeName       = "backups"
	roleName        = "readonly"
	userName        = "jane"

	// Where the admin-sync sidecar writes the credential file both it and
	// pgAdmin mount. Spelled out rather than imported: the test asserts the
	// path the running Pod actually uses, so sharing the constant would make
	// a rename agree with itself and prove nothing.
	pgAdminPassFilePath = "/run/pgadmin/passfile/pgpass"

	// The pgAdmin image is large and the CNPG cluster has to reach a running
	// primary before the operator will sync anything, so these are generous.
	compositionTimeout = 8 * time.Minute
	clusterTimeout     = 10 * time.Minute
)

var objectStoreGVK = schema.GroupVersionKind{
	Group:   "barmancloud.cnpg.io",
	Version: "v1",
	Kind:    "ObjectStore",
}

// TestFullComposition brings up a console with everything switched on.
func TestFullComposition(t *testing.T) {
	if *pgAdminImage == "" || *viewerImage == "" {
		t.Fatal("-pgadmin-image and -viewer-image are required")
	}

	createStoreCredentials(t)
	createObjectStore(t)
	createArchivingCluster(t)
	createFullConsole(t)

	t.Run("EveryContainerComposedAndReady", func(t *testing.T) { assertFullPod(t) })
	t.Run("EvidenceComposed", func(t *testing.T) { assertEvidenceComposed(t) })
	t.Run("PgAdminSyncsTheUser", func(t *testing.T) { assertPgAdminSync(t) })
	t.Run("PgAdminCanReachPostgres", func(t *testing.T) { assertPgAdminReachesPostgres(t) })
}

// createStoreCredentials writes the S3 credential Secret the ObjectStore
// references. They are the in-cluster MinIO's, so archiving actually works:
// the operator never reads the credential material, it renders file mounts
// pointing at this Secret, but CloudNativePG and the viewer do use it.
func createStoreCredentials(t *testing.T) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "store-credentials", Namespace: testNamespace},
		StringData: map[string]string{
			"ACCESS_KEY_ID":     "e2eaccesskey",
			"ACCESS_SECRET_KEY": "e2esecretkey",
		},
	}
	if err := k8s.Create(ctx(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create store credentials: %v", err)
	}
}

// createObjectStore creates the Barman Cloud Plugin ObjectStore the evidence
// composition resolves through. It is unstructured on purpose: the plugin's
// types are not a dependency of this repository, and the operator itself
// reads this object unstructured for the same reason.
func createObjectStore(t *testing.T) {
	t.Helper()
	store := &unstructured.Unstructured{}
	store.SetGroupVersionKind(objectStoreGVK)
	store.SetName(storeName)
	store.SetNamespace(testNamespace)
	if err := unstructured.SetNestedMap(store.Object, map[string]any{
		"destinationPath": "s3://e2e-backups/" + fullClusterName,
		"endpointURL":     "http://minio.minio.svc:9000",
		"s3Credentials": map[string]any{
			"accessKeyId": map[string]any{
				"name": "store-credentials",
				"key":  "ACCESS_KEY_ID",
			},
			"secretAccessKey": map[string]any{
				"name": "store-credentials",
				"key":  "ACCESS_SECRET_KEY",
			},
		},
	}, "spec", "configuration"); err != nil {
		t.Fatalf("build ObjectStore spec: %v", err)
	}
	if err := k8s.Create(ctx(), store); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ObjectStore (is the Barman Cloud Plugin installed?): %v", err)
	}
}

// createArchivingCluster creates a CNPG Cluster that archives through the
// plugin, which is what makes the console's evidence composition resolvable.
// One instance, smallest usable volume: the test needs a running primary so
// CloudNativePG applies the DatabaseRole, not a representative database.
func createArchivingCluster(t *testing.T) {
	t.Helper()
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: fullClusterName, Namespace: testNamespace},
		Spec: cnpgv1.ClusterSpec{
			Instances:            1,
			StorageConfiguration: cnpgv1.StorageConfiguration{Size: "256Mi"},
			Plugins: []cnpgv1.PluginConfiguration{{
				Name:    "barman-cloud.cloudnative-pg.io",
				Enabled: ptr(true),
				Parameters: map[string]string{
					"barmanObjectName": storeName,
				},
			}},
		},
	}
	if err := k8s.Create(ctx(), cluster); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create archiving cluster: %v", err)
	}
}

// createFullConsole declares the console with every optional component on,
// and the role and user that give pgAdmin sync something to do. No images are
// named anywhere: the operator's --default-*-image flags supply them, which
// is the path the documentation shows and the one that was broken until the
// image fields became pointers.
func createFullConsole(t *testing.T) {
	t.Helper()
	console := &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{Name: fullConsoleName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: fullClusterName},
			Proxy: pgtoolboxv1alpha1.ProxySpec{
				Authentication: pgtoolboxv1alpha1.ProxyAuthenticationSpec{
					Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeLocal,
				},
			},
			PgAdmin:  pgtoolboxv1alpha1.PgAdminSpec{Enabled: ptr(true)},
			Evidence: pgtoolboxv1alpha1.EvidenceSpec{Enabled: ptr(true)},
			// pgAdmin runs as uid 5050 and its settings PVC has to be
			// writable; the evidence socket directory has to be setgid.
			// Both come from the kubelet applying fsGroup, which kind does
			// not default.
			PodSecurityContext: pgtoolboxv1alpha1.PodSecurityContextSpec{
				FSGroup: ptr(int64(5050)),
			},
		},
	}
	if err := k8s.Create(ctx(), console); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create full PgConsole: %v", err)
	}

	role := &pgtoolboxv1alpha1.PgToolBoxRole{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgToolBoxRoleSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: fullConsoleName},
			Level:        pgtoolboxv1alpha1.RoleLevelView,
			PostgresRole: pgtoolboxv1alpha1.PostgresRoleSpec{Profile: "database-readonly"},
		},
	}
	if err := k8s.Create(ctx(), role); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create PgToolBoxRole: %v", err)
	}

	// The Secret carries a bcrypt hash, not a plaintext password: the
	// operator copies it into the proxy configuration and never hashes
	// anything itself, so a plaintext value is rejected as invalid.
	hash, err := bcrypt.GenerateFromPassword([]byte("e2e-console-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash user password: %v", err)
	}
	password := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "jane-password", Namespace: testNamespace},
		Data:       map[string][]byte{"password": hash},
	}
	if err := k8s.Create(ctx(), password); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create user password secret: %v", err)
	}

	user := &pgtoolboxv1alpha1.PgToolBoxUser{
		ObjectMeta: metav1.ObjectMeta{Name: userName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgToolBoxUserSpec{
			PgConsoleRef: pgtoolboxv1alpha1.LocalObjectReference{Name: fullConsoleName},
			Subject:      "jane@corp.example",
			RoleRef:      pgtoolboxv1alpha1.LocalObjectReference{Name: roleName},
			LocalPasswordSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{
				Name: "jane-password",
			},
		},
	}
	if err := k8s.Create(ctx(), user); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create PgToolBoxUser: %v", err)
	}
}

// assertFullPod is the composition check: every container the operator can
// add is present, running a family image, and Ready. A Ready pgAdmin also
// proves the settings PVC is writable by uid 5050, and a Ready viewer proves
// it accepted the socket directory the operator prepared for it.
func assertFullPod(t *testing.T) {
	var pod *corev1.Pod
	eventually(t, compositionTimeout, "every container in the console pod to be ready", func() error {
		found, err := consolePod(fullConsoleName)
		if err != nil {
			return err
		}
		pod = found
		// proxy, pgconsole, pgadmin, the evidence viewer, and the
		// admin-sync sidecar; admin-sync-init is an init container.
		if len(found.Spec.Containers) != 5 {
			names := make([]string, 0, len(found.Spec.Containers))
			for _, container := range found.Spec.Containers {
				names = append(names, container.Name)
			}
			return fmt.Errorf("pod has %d containers %v, want 5", len(names), names)
		}
		for _, status := range found.Status.ContainerStatuses {
			if !status.Ready {
				return fmt.Errorf("container %s not ready (restarts=%d, state=%+v)",
					status.Name, status.RestartCount, status.State)
			}
		}
		return nil
	})

	wantContainers := map[string]string{
		"proxy":             "",
		"pgconsole":         *pgConsoleImage,
		"pgadmin":           *pgAdminImage,
		"objectstoreviewer": *viewerImage,
		// The sidecar runs the operator's own binary out of the init
		// container's copy, so it carries the pgAdmin image, not ours.
		"admin-sync": *pgAdminImage,
	}
	for name, wantImage := range wantContainers {
		container := containerOf(t, pod, name)
		if wantImage != "" && container.Image != wantImage {
			t.Errorf("%s image = %q, want the family image %q", name, container.Image, wantImage)
		}
	}

	// The init container copies the operator binary in so the sidecar can
	// run it without pulling a second image.
	initNames := map[string]bool{}
	for _, container := range pod.Spec.InitContainers {
		initNames[container.Name] = true
	}
	if !initNames["admin-sync-init"] {
		t.Errorf("init container admin-sync-init missing; have %v", initNames)
	}

	// pgAdmin's settings survive restarts only if it is on the PVC.
	pgAdmin := containerOf(t, pod, "pgadmin")
	if !mountsVolume(pgAdmin, "pgadmin-settings") {
		t.Errorf("pgadmin does not mount its settings volume: %+v", pgAdmin.VolumeMounts)
	}
	// The storage class binds on first consumer, so the claim reaches Bound
	// shortly after the pod is scheduled rather than with it.
	eventually(t, 2*time.Minute, "the pgAdmin settings PVC to bind", func() error {
		var pvc corev1.PersistentVolumeClaim
		if err := k8s.Get(ctx(), key(fullConsoleName+"-pgconsole-pgadmin"), &pvc); err != nil {
			return err
		}
		if pvc.Status.Phase != corev1.ClaimBound {
			return fmt.Errorf("claim is %s", pvc.Status.Phase)
		}
		return nil
	})

	// The viewer holds no Kubernetes credential: that isolation is what lets
	// it carry object-store credentials in the same pod as the console.
	viewer := containerOf(t, pod, "objectstoreviewer")
	if mountsVolume(viewer, "kube-api-access") {
		t.Error("the evidence viewer must not mount a Kubernetes API token")
	}
	if !mountsVolume(viewer, "evidence-socket") || !mountsVolume(viewer, "evidence-token") {
		t.Errorf("viewer is missing the evidence mounts: %+v", viewer.VolumeMounts)
	}
}

// assertEvidenceComposed checks the operator's side of the evidence
// contract: the condition, the token Secret it published, and the four
// variables the console needs to consume the socket. The viewer's own
// sidecar-mode requirements are asserted here too, because getting any of
// them wrong makes the container refuse to start.
func assertEvidenceComposed(t *testing.T) {
	var live pgtoolboxv1alpha1.PgConsole
	eventually(t, 3*time.Minute, "evidence to report ready", func() error {
		if err := k8s.Get(ctx(), key(fullConsoleName), &live); err != nil {
			return err
		}
		condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionRepositoryEvidenceReady)
		if condition == nil {
			return fmt.Errorf("RepositoryEvidenceReady not published")
		}
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf("RepositoryEvidenceReady is %s: %s", condition.Status, condition.Message)
		}
		return nil
	})

	if !live.Status.Evidence.Enabled || live.Status.Evidence.TokenSecretName == "" {
		t.Fatalf("evidence status = %+v", live.Status.Evidence)
	}
	var token corev1.Secret
	if err := k8s.Get(ctx(), key(live.Status.Evidence.TokenSecretName), &token); err != nil {
		t.Fatalf("evidence token Secret: %v", err)
	}
	if token.Immutable == nil || !*token.Immutable {
		t.Error("the evidence token Secret must be immutable")
	}

	pod, err := consolePod(fullConsoleName)
	if err != nil {
		t.Fatalf("console pod: %v", err)
	}

	// The console's half of the contract: the all-or-nothing four.
	console := containerOf(t, pod, "pgconsole")
	for _, name := range []string{
		"REPOSITORY_EVIDENCE_URL",
		"REPOSITORY_EVIDENCE_TOKEN_FILE",
		"REPOSITORY_EXPECTED_FINGERPRINT",
		"REPOSITORY_BARMAN_SERVER",
	} {
		if envOf(console, name) == "" {
			t.Errorf("console is missing %s", name)
		}
	}
	if got := envOf(console, "REPOSITORY_EVIDENCE_URL"); got != "unix:///var/run/objectstoreviewer/evidence.sock" {
		t.Errorf("evidence URL = %q, not the viewer's fixed socket path", got)
	}

	// The viewer's half: sidecar mode refuses anything else, and refuses to
	// start at all if LISTEN_ADDR or TRUSTED_USER_HEADER are set.
	viewer := containerOf(t, pod, "objectstoreviewer")
	for name, want := range map[string]string{
		"RUNTIME_MODE":          "pgconsole-sidecar",
		"PROVIDER":              "s3",
		"REPOSITORY_FORMAT":     "barman-cloud",
		"STORE_CREDENTIAL_MODE": "static-files",
		"BARMAN_SERVER_NAMES":   fullClusterName,
	} {
		if got := envOf(viewer, name); got != want {
			t.Errorf("viewer env %s = %q, want %q", name, got, want)
		}
	}
	for _, forbidden := range []string{"LISTEN_ADDR", "TRUSTED_USER_HEADER", "ALLOW_DOWNLOAD"} {
		if envOf(viewer, forbidden) != "" {
			t.Errorf("viewer env %s is set; sidecar mode rejects it", forbidden)
		}
	}
}

// assertPgAdminSync waits for the whole provisioning chain: CloudNativePG
// brings the primary up, applies the DatabaseRole the PgToolBoxRole
// materialized, and only then does the operator post the user to the
// admin-sync sidecar. It is the slowest assertion here and the one that
// proves the sidecar API actually works in a pod.
func assertPgAdminSync(t *testing.T) {
	eventually(t, clusterTimeout, "the CNPG cluster to have a running primary", func() error {
		var cluster cnpgv1.Cluster
		if err := k8s.Get(ctx(), key(fullClusterName), &cluster); err != nil {
			return err
		}
		if cluster.Status.ReadyInstances < 1 {
			return fmt.Errorf("readyInstances=%d, phase=%q", cluster.Status.ReadyInstances, cluster.Status.Phase)
		}
		return nil
	})

	eventually(t, clusterTimeout, "the role's DatabaseRole to be applied", func() error {
		var role pgtoolboxv1alpha1.PgToolBoxRole
		if err := k8s.Get(ctx(), key(roleName), &role); err != nil {
			return err
		}
		if role.Status.DatabaseRoleName == "" {
			return fmt.Errorf("no databaseRoleName published yet")
		}
		return nil
	})

	eventually(t, compositionTimeout, "the console to report pgAdmin synced", func() error {
		var live pgtoolboxv1alpha1.PgConsole
		if err := k8s.Get(ctx(), key(fullConsoleName), &live); err != nil {
			return err
		}
		condition := conditionOf(&live, pgtoolboxv1alpha1.PgConsoleConditionPgAdminSynced)
		if condition == nil {
			return fmt.Errorf("PgAdminSynced not published")
		}
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf("PgAdminSynced is %s: %s", condition.Status, condition.Message)
		}
		if live.Status.UserSync.Synced < 1 {
			return fmt.Errorf("userSync = %+v", live.Status.UserSync)
		}
		return nil
	})

	// And the user itself carries the three per-user conditions.
	var user pgtoolboxv1alpha1.PgToolBoxUser
	if err := k8s.Get(ctx(), key(userName), &user); err != nil {
		t.Fatalf("get user: %v", err)
	}
	for _, want := range []string{
		pgtoolboxv1alpha1.UserConditionRoleReady,
		pgtoolboxv1alpha1.UserConditionProxySynced,
		pgtoolboxv1alpha1.UserConditionPgAdminSynced,
	} {
		condition := userConditionOf(&user, want)
		if condition == nil {
			t.Errorf("user condition %s not published", want)
			continue
		}
		if condition.Status != metav1.ConditionTrue {
			t.Errorf("user condition %s is %s: %s", want, condition.Status, condition.Message)
		}
	}
}

// consolePod returns the single pod of one console.
func consolePod(instance string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := k8s.List(ctx(), &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{"app.kubernetes.io/instance": instance},
	); err != nil {
		return nil, err
	}
	running := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp.IsZero() {
			running = append(running, pod)
		}
	}
	if len(running) != 1 {
		return nil, fmt.Errorf("expected exactly one console pod, got %d", len(running))
	}
	return &running[0], nil
}

func mountsVolume(container *corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

func userConditionOf(user *pgtoolboxv1alpha1.PgToolBoxUser, conditionType string) *metav1.Condition {
	for i := range user.Status.Conditions {
		if user.Status.Conditions[i].Type == conditionType {
			return &user.Status.Conditions[i]
		}
	}
	return nil
}

// assertPgAdminReachesPostgres is the assertion the whole pgAdmin path was
// missing. PgAdminSynced reports that the operator posted the desired state
// and the sidecar accepted it — not that pgAdmin can do anything with it,
// and two separate defects hid behind exactly that gap:
//
//   - the generated NetworkPolicy allowed egress to DNS and the Kubernetes
//     API only, so the connection to PostgreSQL was dropped and surfaced as
//     a bare 502 from the proxy's upstream timeout;
//   - the sync revision was recorded on the Deployment while the .pgpass it
//     tracked lived in an emptyDir, so a restarted console kept a matching
//     annotation, skipped the sync, and reported success over a pgAdmin with
//     no credentials at all.
//
// Both reported PgAdminSynced=True throughout. So this checks the two things
// that actually have to be true — the credential file exists in the Pod, and
// the Pod can complete a PostgreSQL protocol exchange with the server — and
// it makes the connection *from the console Pod*, because the NetworkPolicy
// selects that Pod and a connection from anywhere else proves nothing.
func assertPgAdminReachesPostgres(t *testing.T) {
	pod, err := consolePod(fullConsoleName)
	if err != nil {
		t.Fatalf("console pod: %v", err)
	}

	// One .pgpass entry per provisioned user, naming the cluster's service.
	eventually(t, compositionTimeout, "the sidecar to write the .pgpass", func() error {
		out, err := execInPod(pod, "pgadmin", "sh", "-c",
			"cut -d: -f1-4 "+pgAdminPassFilePath+" 2>&1")
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		if !strings.Contains(out, fullClusterName+"-rw."+testNamespace+".svc:5432") {
			return fmt.Errorf("no entry for the cluster service: %q", out)
		}
		return nil
	})

	// A PostgreSQL SSLRequest is the smallest exchange that proves both
	// reachability and that a PostgreSQL server is on the other end: it
	// answers with a single byte, 'S' or 'N'. A NetworkPolicy that drops the
	// connection instead hangs until the dial timeout.
	probe := fmt.Sprintf(`
import socket,sys
try:
    s=socket.create_connection(("%s-rw.%s.svc",5432),timeout=15)
    s.sendall(b"\x00\x00\x00\x08\x04\xd2\x16\x2f")
    reply=s.recv(1)
    s.close()
    print("REPLY=" + reply.decode("ascii","replace"))
except Exception as error:
    print("FAILED=" + repr(error)); sys.exit(1)
`, fullClusterName, testNamespace)

	out, err := execInPod(pod, "pgadmin", "/venv/bin/python3", "-c", probe)
	if err != nil {
		t.Fatalf("console pod cannot reach PostgreSQL: %v: %s", err, out)
	}
	if !strings.Contains(out, "REPLY=S") && !strings.Contains(out, "REPLY=N") {
		t.Fatalf("no PostgreSQL handshake from the console pod: %s", out)
	}
}
