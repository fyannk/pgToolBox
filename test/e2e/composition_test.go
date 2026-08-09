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
	t.Run("PgAdminOffersClusterCredentials", func(t *testing.T) { assertPgAdminSync(t) })
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
					BootstrapAdmin: pgtoolboxv1alpha1.BootstrapAdminSpec{
						Subject:           "root@corp.example",
						PasswordSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "jane-password"},
					},
					Local: &pgtoolboxv1alpha1.ProxyLocalSpec{},
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
			Level:        pgtoolboxv1alpha1.RoleLevelDBA,
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

// assertPgAdminSync waits for the connections pgAdmin should offer to be
// provisioned from the cluster's own credentials, and holds that they are
// the cluster's rather than anything derived from who signed in.
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
		return nil
	})

	pod, err := consolePod(fullConsoleName)
	if err != nil {
		t.Fatalf("console pod: %v", err)
	}

	// Connections exist per pgAdmin account, and an account exists only
	// once someone has signed in — pgAdmin creates it the first time the
	// proxy forwards an identity it has not seen. Nobody signs in during a
	// test, so one is created here the way an arrival would, and the point
	// of the assertion is that the operator then finds it on its own.
	if out, err := execInPod(pod, "admin-sync", "/venv/bin/python3", "/pgadmin4/setup.py",
		"add-external-user", "e2e-dba@pgtoolbox.dev",
		"--auth-source", "webserver", "--role", "Administrator",
		"--email", "e2e-dba@pgtoolbox.dev",
	); err != nil && !strings.Contains(out, "already exists") {
		t.Fatalf("simulate a pgAdmin sign-in: %v: %s", err, out)
	}

	// The application user is what a cluster always publishes, so it must
	// be among the offered connections whatever else is. The wait is the
	// operator's own resync: nothing in Kubernetes changed when the account
	// appeared, so only the periodic pass can notice it.
	eventually(t, compositionTimeout, "the new account to be given the cluster's connections", func() error {
		out, err := execInPod(pod, "pgadmin", "/venv/bin/python3", "-c", `
import sqlite3
c = sqlite3.connect("/var/lib/pgadmin/pgadmin4.db")
for row in c.execute("SELECT u.email, s.name, s.username FROM server s JOIN user u ON u.id = s.user_id"):
    print("SERVER %s|%s|%s" % row)
`)
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		if !strings.Contains(out, "e2e-dba@pgtoolbox.dev|application (app)|app") {
			return fmt.Errorf("not provisioned yet:\n%s", out)
		}
		return nil
	})
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

	// The credential file lives in each account's own pgAdmin storage,
	// because that is the only place pgAdmin resolves a server's passfile
	// to. The host field is the wildcard on purpose: libpq matches a line
	// against the host string it was given, so a line naming the Service
	// fails for a connection made by address.
	eventually(t, compositionTimeout, "the sidecar to write an account's pgpass", func() error {
		out, err := execInPod(pod, "pgadmin", "sh", "-c",
			"cut -d: -f1-4 /var/lib/pgadmin/storage/*/pgpass 2>&1")
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		if !strings.Contains(out, "*:5432:app:app") {
			return fmt.Errorf("no wildcard-host entry for the application user: %q", out)
		}
		return nil
	})

	// Authenticate for real, and do it twice: once by the name the server
	// definition carries and once by the address it resolves to. libpq
	// matches a pgpass line against the host string it was given rather than
	// the host it resolved, so a file spelled with the name fails by address
	// with "no password supplied" — the credential present and never
	// consulted. Connecting both ways is what holds the wildcard host field
	// in place.
	address, err := clusterServiceAddress(t)
	if err != nil {
		t.Fatalf("resolve cluster service address: %v", err)
	}
	// Authenticate through an account's own credential file, which is the
	// file pgAdmin resolves a server's passfile to — not through PGPASSFILE,
	// and not with a password passed in. Both shortcuts pass while pgAdmin
	// itself still cannot connect, which is exactly how two defects here
	// survived being "verified".
	//
	// Twice, by name and by address: libpq matches a pgpass line against the
	// host string it was given rather than the host it resolved, so only the
	// second spelling exercises the wildcard host field.
	probe := fmt.Sprintf(`
import glob, psycopg, sys
files = glob.glob("/var/lib/pgadmin/storage/*/pgpass")
if not files:
    print("no account has a credential file"); sys.exit(1)
for host in [%q, %q]:
    with psycopg.connect(host=host, port=5432, dbname="app", user="app",
                         passfile=files[0], sslmode="prefer", connect_timeout=15) as c:
        who = c.execute("select current_user").fetchone()[0]
        print("AUTHENTICATED " + host + " as " + who)
`, fullClusterName+"-rw."+testNamespace+".svc", address)

	out, err := execInPod(pod, "pgadmin", "/venv/bin/python3", "-c", probe)
	if err != nil {
		t.Fatalf("pgAdmin cannot authenticate to PostgreSQL: %v: %s", err, out)
	}
	if strings.Count(out, "AUTHENTICATED") != 2 {
		t.Fatalf("expected an authenticated connection by name and by address: %s", out)
	}
}

// clusterServiceAddress returns the ClusterIP of the cluster's read-write
// Service, so the test can connect the way that used to fail.
func clusterServiceAddress(t *testing.T) (string, error) {
	t.Helper()
	var service corev1.Service
	if err := k8s.Get(ctx(), key(fullClusterName+"-rw"), &service); err != nil {
		return "", err
	}
	if service.Spec.ClusterIP == "" {
		return "", fmt.Errorf("service %s-rw has no ClusterIP", fullClusterName)
	}
	return service.Spec.ClusterIP, nil
}

// consolePod returns the single running pod of one console.
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

// mountsVolume reports whether a container mounts a named volume.
func mountsVolume(container *corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

// The "at least one provider" rule is a CEL expression on the CRD, so only
// a real API server enforces it — no unit test can stand in for this.
func TestAuthenticationRequiresAProvider(t *testing.T) {
	console := &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{Name: "no-provider", Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: fullClusterName},
			Proxy: pgtoolboxv1alpha1.ProxySpec{
				Authentication: pgtoolboxv1alpha1.ProxyAuthenticationSpec{
					BootstrapAdmin: pgtoolboxv1alpha1.BootstrapAdminSpec{Subject: "root@corp.example"},
				},
			},
		},
	}
	err := k8s.Create(ctx(), console)
	if err == nil {
		_ = k8s.Delete(ctx(), console)
		t.Fatal("a console enabling no authentication provider was accepted")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("create error = %v, want Invalid", err)
	}
}

// Local-only with no bootstrap password is a console nobody can ever sign
// in to. The rule is CEL on the CRD, so only a real API server enforces it.
func TestLocalOnlyRequiresABootstrapPassword(t *testing.T) {
	console := &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{Name: "no-way-in", Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: fullClusterName},
			Proxy: pgtoolboxv1alpha1.ProxySpec{
				Authentication: pgtoolboxv1alpha1.ProxyAuthenticationSpec{
					BootstrapAdmin: pgtoolboxv1alpha1.BootstrapAdminSpec{Subject: "root@corp.example"},
					Local:          &pgtoolboxv1alpha1.ProxyLocalSpec{},
				},
			},
		},
	}
	err := k8s.Create(ctx(), console)
	if err == nil {
		_ = k8s.Delete(ctx(), console)
		t.Fatal("a local-only console with no bootstrap password was accepted")
	}
	if !apierrors.IsInvalid(err) {
		t.Fatalf("create error = %v, want Invalid", err)
	}
}

// The first administrator is derived from the console, not maintained by
// hand: deleting the object gets it back.
func TestBootstrapAdminIsRestored(t *testing.T) {
	key := client.ObjectKey{Namespace: testNamespace, Name: fullConsoleName + "-bootstrap-admin"}
	var user pgtoolboxv1alpha1.PgToolBoxUser
	if err := k8s.Get(ctx(), key, &user); err != nil {
		t.Fatalf("the console has no bootstrap admin: %v", err)
	}
	if user.Spec.Level != pgtoolboxv1alpha1.RoleLevelDBA {
		t.Fatalf("bootstrap admin level = %q, want dba", user.Spec.Level)
	}
	if err := k8s.Delete(ctx(), &user); err != nil {
		t.Fatalf("delete bootstrap admin: %v", err)
	}
	eventually(t, 2*time.Minute, "the operator restores the bootstrap admin", func() error {
		var back pgtoolboxv1alpha1.PgToolBoxUser
		return k8s.Get(ctx(), key, &back)
	})
}
