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

// Package e2e is the smoke test that runs against a real cluster, provisioned
// by hack/e2e.sh.
//
// It exists for the two failure modes the unit tests structurally cannot
// reach. A fake client accepts any Role: only a real API server enforces
// escalation prevention, which refuses to let the operator grant a rule it
// does not itself hold — so a missing kubebuilder RBAC marker is invisible
// until here. And a fake client never starts a container: only the real
// pgConsole binary can reject the environment the operator rendered for it,
// which is exactly the class of defect this alignment work fixed.
//
// It is deliberately a smoke test. It proves the stack comes up and the
// contracts hold; it does not exercise the console's screens, which is
// pgConsole's own test suite's job.
package e2e

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var (
	pgConsoleImage = flag.String("pgconsole-image", "", "pgConsole image the console should run")
	pgAdminImage   = flag.String("pgadmin-image", "", "pgAdmin image the console should run")
	viewerImage    = flag.String("viewer-image", "", "pgObjectStoreViewer image the evidence sidecar should run")
)

const (
	testNamespace = "pgtoolbox-e2e"
	consoleName   = "smoke"
	clusterName   = "pg-smoke"

	// Generous: the console pod pulls nothing (images are side-loaded) but
	// the proxy and console both have to pass their own readiness probes.
	readyTimeout = 5 * time.Minute
	pollInterval = 3 * time.Second
)

var (
	k8s        client.Client
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
)

// execInPod runs a command in one container and returns its combined output.
// It exists so a test can make a network call *from the console Pod*: the
// generated NetworkPolicy selects that Pod, so a connection made anywhere
// else proves nothing about it.
func execInPod(pod *corev1.Pod, container string, command ...string) (string, error) {
	request := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod.Name).Namespace(pod.Namespace).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", request.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx(), remotecommand.StreamOptions{
		Stdout: &stdout, Stderr: &stderr,
	})
	return stdout.String() + stderr.String(), err
}

func TestMain(m *testing.M) {
	flag.Parse()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		pgtoolboxv1alpha1.AddToScheme,
		cnpgv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			fmt.Fprintf(os.Stderr, "register scheme: %v\n", err)
			os.Exit(1)
		}
	}

	var err error
	if restConfig, err = config.GetConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "load kubeconfig: %v\n", err)
		os.Exit(1)
	}
	if k8s, err = client.New(restConfig, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(restConfig); err != nil {
		fmt.Fprintf(os.Stderr, "build clientset: %v\n", err)
		os.Exit(1)
	}

	if err := setupNamespace(); err != nil {
		fmt.Fprintf(os.Stderr, "prepare namespace: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if os.Getenv("KEEP_CLUSTER") != "1" {
		_ = k8s.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
		})
	}
	os.Exit(code)
}

// setupNamespace makes the test namespace exist and be Active. It waits out a
// namespace still terminating from a previous run rather than creating into
// it, which succeeds and then has everything swept away underneath the test.
func setupNamespace() error {
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var live corev1.Namespace
		err := k8s.Get(context.Background(), client.ObjectKey{Name: testNamespace}, &live)
		switch {
		case apierrors.IsNotFound(err):
			created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			if err := k8s.Create(context.Background(), created); err != nil &&
				!apierrors.IsAlreadyExists(err) {
				return err
			}
		case err != nil:
			return err
		case live.Status.Phase == corev1.NamespaceActive:
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s did not become active", testNamespace)
		}
		time.Sleep(pollInterval)
	}
}

// eventually polls until check passes or the deadline expires, reporting the
// last failure reason rather than a bare timeout.
func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = check(); last == nil {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out waiting for %s: %v", what, last)
}

func ctx() context.Context { return context.Background() }

// TestConsoleSmoke brings one console up end to end and holds the contracts
// that only a real cluster can check.
func TestConsoleSmoke(t *testing.T) {
	if *pgConsoleImage == "" {
		t.Fatal("-pgconsole-image is required")
	}

	createCluster(t)
	console := createConsole(t)

	t.Run("ReachesReady", func(t *testing.T) { assertConsoleReady(t) })
	t.Run("RolesWereGranted", func(t *testing.T) { assertRolesGranted(t) })
	t.Run("ContainersAcceptTheirConfig", func(t *testing.T) { assertContainersReady(t) })
	t.Run("CapabilitiesWithdrawAuthority", func(t *testing.T) { assertCapabilityWithdrawal(t, console) })
}

// createCluster creates the CNPG Cluster the console attaches to. The test
// never waits for PostgreSQL: the console reads the Cluster object, and its
// readiness does not depend on the database being up, so waiting for an
// instance would add minutes and prove nothing this test is about.
func createCluster(t *testing.T) {
	t.Helper()
	cluster := &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: testNamespace},
		Spec: cnpgv1.ClusterSpec{
			Instances: 1,
			StorageConfiguration: cnpgv1.StorageConfiguration{
				Size: "128Mi",
			},
		},
	}
	if err := k8s.Create(ctx(), cluster); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create CNPG cluster: %v", err)
	}
}

// createConsole declares the console under test: pgAdmin off to keep the
// smoke test to the operator/pgConsole contract it exists to prove, local
// authentication so no identity provider is needed.
func createConsole(t *testing.T) *pgtoolboxv1alpha1.PgConsole {
	t.Helper()
	console := &pgtoolboxv1alpha1.PgConsole{
		ObjectMeta: metav1.ObjectMeta{Name: consoleName, Namespace: testNamespace},
		Spec: pgtoolboxv1alpha1.PgConsoleSpec{
			CNPGClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: clusterName},
			Proxy: pgtoolboxv1alpha1.ProxySpec{
				Authentication: pgtoolboxv1alpha1.ProxyAuthenticationSpec{
					Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeLocal,
				},
			},
			PgAdmin: pgtoolboxv1alpha1.PgAdminSpec{Enabled: ptr(false)},
		},
	}
	if err := k8s.Create(ctx(), console); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create PgConsole: %v", err)
	}
	return console
}

func assertConsoleReady(t *testing.T) {
	eventually(t, readyTimeout, "the console to report Ready", func() error {
		var live pgtoolboxv1alpha1.PgConsole
		if err := k8s.Get(ctx(), key(consoleName), &live); err != nil {
			return err
		}
		for _, want := range []string{
			pgtoolboxv1alpha1.PgConsoleConditionConfigurationValid,
			pgtoolboxv1alpha1.PgConsoleConditionProxyConfigReady,
			pgtoolboxv1alpha1.PgConsoleConditionClusterReady,
			pgtoolboxv1alpha1.PgConsoleConditionReady,
		} {
			condition := conditionOf(&live, want)
			if condition == nil {
				return fmt.Errorf("condition %s not published yet", want)
			}
			if condition.Status != metav1.ConditionTrue {
				return fmt.Errorf("condition %s is %s: %s", want, condition.Status, condition.Message)
			}
		}
		return nil
	})
}

// assertRolesGranted is the escalation-prevention check. A real API server
// refuses to let the operator create a Role carrying a rule its own
// ClusterRole does not hold, so a rule reaching the live object at all is
// proof the operator was permitted to grant it — which no fake client can
// establish.
func assertRolesGranted(t *testing.T) {
	var readRole rbacv1.Role
	if err := k8s.Get(ctx(), key(consoleName+"-pgconsole"), &readRole); err != nil {
		t.Fatalf("get read Role: %v", err)
	}

	// The rules most recently added, and so most at risk of a missing
	// kubebuilder marker on the manager's own ClusterRole.
	for _, want := range []struct{ group, resource string }{
		{"postgresql.cnpg.io", "poolers"},
		{"postgresql.cnpg.io", "databases"},
		{"postgresql.cnpg.io", "failoverquorums"},
		{"postgresql.cnpg.io", "imagecatalogs"},
		{"apps", "replicasets"},
		{"", "services"},
		{"", "persistentvolumeclaims"},
		{"", "configmaps"},
		{"policy", "poddisruptionbudgets"},
		{"rbac.authorization.k8s.io", "roles"},
		{"batch", "jobs"},
	} {
		if !roleGrants(readRole.Rules, want.group, want.resource) {
			t.Errorf("read Role does not grant %s/%s — a manager RBAC marker is probably missing",
				want.group, want.resource)
		}
	}

	// The read-only guarantee, checked against the object the API server
	// actually stored rather than the one the controller intended.
	if roleGrants(readRole.Rules, "", "secrets") {
		t.Error("read Role grants something on secrets")
	}

	var operateRole rbacv1.Role
	if err := k8s.Get(ctx(), key(consoleName+"-pgconsole-operate"), &operateRole); err != nil {
		t.Fatalf("get operate Role: %v", err)
	}
	if !roleGrants(operateRole.Rules, "pgtoolbox.fyannk.dev", "pgtoolboxaccessrequests/status") {
		t.Error("operate Role does not carry the access-request decision rule")
	}
}

// assertContainersReady is the other check only a real cluster can make: the
// pgConsole binary validates its whole configuration at startup and refuses
// to serve on a value it rejects, so a Ready container is proof the rendered
// environment is one the real application accepts.
func assertContainersReady(t *testing.T) {
	var pods corev1.PodList
	if err := k8s.List(ctx(), &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{"app.kubernetes.io/instance": consoleName},
	); err != nil {
		t.Fatalf("list console pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected exactly one console pod, got %d", len(pods.Items))
	}
	pod := pods.Items[0]

	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			t.Errorf("container %s is not ready (restarts=%d, state=%+v)",
				status.Name, status.RestartCount, status.State)
		}
		if status.RestartCount > 0 {
			t.Errorf("container %s restarted %d times — it is rejecting its configuration",
				status.Name, status.RestartCount)
		}
	}

	// The variables whose absence made whole features unreachable. Asserted
	// on the live pod so a regression in rendering is caught where it
	// matters, not only in the builder's unit test.
	console := containerOf(t, &pod, "pgconsole")
	for name, want := range map[string]string{
		"TRUSTED_LEVEL_HEADER": "X-PgToolBox-Level",
		"TRUSTED_USER_HEADER":  "X-Forwarded-User",
		"ALLOW_ACCESS_REVIEW":  "true",
		"ALLOW_OPERATIONS":     "true",
		"ALLOW_LOGS":           "true",
	} {
		if got := envOf(console, name); got != want {
			t.Errorf("console env %s = %q, want %q", name, got, want)
		}
	}
	if console.Image != *pgConsoleImage {
		t.Errorf("console image = %q, want %q", console.Image, *pgConsoleImage)
	}
}

// assertCapabilityWithdrawal proves the coupling end to end: switching both
// write capabilities off deletes the operate Role on a live cluster, so the
// API server denies the mutation regardless of what the application is told.
func assertCapabilityWithdrawal(t *testing.T, console *pgtoolboxv1alpha1.PgConsole) {
	var live pgtoolboxv1alpha1.PgConsole
	if err := k8s.Get(ctx(), key(consoleName), &live); err != nil {
		t.Fatalf("get console: %v", err)
	}
	live.Spec.Console.AllowOperations = ptr(false)
	live.Spec.Console.AllowAccessReview = ptr(false)
	if err := k8s.Update(ctx(), &live); err != nil {
		t.Fatalf("update console: %v", err)
	}

	eventually(t, 2*time.Minute, "the operate Role to be withdrawn", func() error {
		var role rbacv1.Role
		err := k8s.Get(ctx(), key(consoleName+"-pgconsole-operate"), &role)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("operate Role still exists with %d rules", len(role.Rules))
	})

	// And the deployment rolls to a console told the same thing.
	eventually(t, 3*time.Minute, "the console to be told operations are off", func() error {
		var deployment appsv1.Deployment
		if err := k8s.Get(ctx(), key(consoleName+"-pgconsole"), &deployment); err != nil {
			return err
		}
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name != "pgconsole" {
				continue
			}
			if got := envOf(container, "ALLOW_OPERATIONS"); got != "false" {
				return fmt.Errorf("ALLOW_OPERATIONS = %q", got)
			}
			return nil
		}
		return fmt.Errorf("no pgconsole container in the deployment")
	})
	_ = console
}

func key(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: name}
}

func conditionOf(console *pgtoolboxv1alpha1.PgConsole, conditionType string) *metav1.Condition {
	for i := range console.Status.Conditions {
		if console.Status.Conditions[i].Type == conditionType {
			return &console.Status.Conditions[i]
		}
	}
	return nil
}

func roleGrants(rules []rbacv1.PolicyRule, group, resource string) bool {
	for _, rule := range rules {
		if !contains(rule.APIGroups, group) {
			continue
		}
		if contains(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containerOf(t *testing.T, pod *corev1.Pod, name string) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	t.Fatalf("container %q not found in pod %s", name, pod.Name)
	return nil
}

func envOf(container *corev1.Container, name string) string {
	for _, entry := range container.Env {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}

func ptr[T any](value T) *T { return &value }
