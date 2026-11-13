//go:build e2e

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

// Package pgadmin is the PgAdmin scenario of the operator on kind,
// provisioned by run.sh: a CloudNativePG Cluster, a PgAdmin registered
// against it, and the facts only a real cluster shows — the registration
// Job's shared servers in pgAdmin's own database, the credential files
// with mode 0600 on the tmpfs and no password under pgAdmin's storage, a
// libpq connection through the service file with verify-full, SCRAM and
// channel binding, and a password rotation reaching the files without a
// rollout. It never prints a password: values are compared by hash inside
// the Pod.
package pgadmin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

const (
	namespace   = "pgadmin-e2e"
	clusterName = "orders"
	pgAdminName = "team"
	timeout     = 8 * time.Minute
	poll        = 3 * time.Second
)

var (
	k8s        client.Client
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, pgtoolboxv1alpha1.AddToScheme, cnpgv1.AddToScheme} {
		if err := add(scheme); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	var err error
	if restConfig, err = config.GetConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if k8s, err = client.New(restConfig, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if clientset, err = kubernetes.NewForConfig(restConfig); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ctx() context.Context { return context.Background() }

func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = check(); last == nil {
			return
		}
		time.Sleep(poll)
	}
	t.Fatalf("%s: not reached in %s: %v", what, timeout, last)
}

func execIn(pod *corev1.Pod, command ...string) (string, error) {
	request := clientset.CoreV1().RESTClient().Post().Resource("pods").Name(pod.Name).Namespace(pod.Namespace).
		SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: "pgadmin", Command: command, Stdout: true, Stderr: true},
		clientgoscheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", request.URL())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx(), remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String() + stderr.String(), err
}

func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(ctx(), obj); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func pgAdminCondition(p *pgtoolboxv1alpha1.PgAdmin, conditionType string) metav1.Condition {
	if c := meta.FindStatusCondition(p.Status.Conditions, conditionType); c != nil {
		return *c
	}
	return metav1.Condition{Status: "absent"}
}

func pgAdminPod(t *testing.T) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := k8s.List(ctx(), &pods, client.InNamespace(namespace), client.MatchingLabels{
		pgtoolboxv1alpha1.PgAdminLabelKey: pgAdminName, "app.kubernetes.io/component": "pgadmin",
	}); err != nil {
		t.Fatal(err)
	}
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil && pods.Items[i].Status.Phase == corev1.PodRunning {
			return &pods.Items[i]
		}
	}
	t.Fatal("no running pgAdmin Pod")
	return nil
}

// psqlScript is a shell script that runs query through the libpq service
// with whichever psql the pgAdmin image ships: on PATH, or the newest of its
// per-major copies (/usr/local/pgsql-<major>/psql in the hardened image,
// .../bin/psql in others). No major is hard-coded.
func psqlScript(service, query string) string {
	return `PSQL=""; for p in /usr/local/pgsql-*/psql /usr/local/pgsql-*/bin/psql $(command -v psql 2>/dev/null); do [ -x "$p" ] && PSQL="$p"; done; ` +
		`[ -n "$PSQL" ] || { echo "no psql in the pgAdmin image" >&2; exit 127; }; ` +
		`PGSERVICE=` + service + ` "$PSQL" -XAtc '` + query + `'`
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestPgAdmin(t *testing.T) {
	create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	create(t, &cnpgv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       cnpgv1.ClusterSpec{Instances: 1, StorageConfiguration: cnpgv1.StorageConfiguration{Size: "1Gi"}},
	})
	// The issuer is never contacted in this scenario (nobody signs in);
	// pgAdmin reads the discovery document lazily.
	create(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pgadmin-oidc", Namespace: namespace},
		StringData: map[string]string{"client-secret": "e2e-client-secret"},
	})
	create(t, &pgtoolboxv1alpha1.PgAdmin{
		ObjectMeta: metav1.ObjectMeta{Name: pgAdminName, Namespace: namespace},
		Spec: pgtoolboxv1alpha1.PgAdminSpec{
			ClusterRefs: []pgtoolboxv1alpha1.LocalObjectReference{{Name: clusterName}},
			Auth: pgtoolboxv1alpha1.PgAdminAuthSpec{OAuth2: &pgtoolboxv1alpha1.PgAdminOAuth2Spec{
				IssuerURL: "https://idp.invalid", ClientID: "pgadmin",
				ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgadmin-oidc"},
				RequiredClaims:  map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}},
			}},
			Admins: []string{"alice@example.com"},
		},
	})

	t.Run("Ready", func(t *testing.T) {
		eventually(t, "PgAdmin Ready", func() error {
			var p pgtoolboxv1alpha1.PgAdmin
			if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: pgAdminName}, &p); err != nil {
				return err
			}
			if c := pgAdminCondition(&p, "Ready"); c.Status != metav1.ConditionTrue {
				return fmt.Errorf("Ready=%s %s: %s", c.Status, c.Reason, c.Message)
			}
			if p.Status.PgAdminVersion == "" || p.Status.RegisteredHash == "" {
				return fmt.Errorf("status %+v", p.Status)
			}
			if c := pgAdminCondition(&p, "PgAdminEgressConfined"); c.Status != metav1.ConditionFalse || c.Reason != "PgAdminEgressOpen" {
				return fmt.Errorf("without a NetworkPolicy the egress statement must be False/PgAdminEgressOpen, got %+v", c)
			}
			return nil
		})
		// CloudNativePG 1.30 and later serve DatabaseRole and the note is
		// absent. An install without the kind offers none with the
		// default spec (databaseRoles: true), says so, and is Ready all
		// the same.
		_, err := k8s.RESTMapper().RESTMapping(cnpgv1.SchemeGroupVersion.WithKind("DatabaseRole").GroupKind(), cnpgv1.SchemeGroupVersion.Version)
		if err != nil && !meta.IsNoMatchError(err) {
			t.Fatalf("discover DatabaseRole: %v", err)
		}
		served := err == nil
		var p pgtoolboxv1alpha1.PgAdmin
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: pgAdminName}, &p); err != nil {
			t.Fatal(err)
		}
		c := pgAdminCondition(&p, "ConfigurationValid")
		if noted := strings.Contains(c.Message, "no DatabaseRole offered"); noted == served {
			t.Errorf("DatabaseRole served=%t, but ConfigurationValid says %q", served, c.Message)
		}
	})

	t.Run("CredentialFiles", func(t *testing.T) {
		pod := pgAdminPod(t)
		out, err := execIn(pod, "sh", "-c",
			"stat -c '%a %n' /run/pgadmin/credentials/pgpass /run/pgadmin/credentials/pg_service.conf /run/pgadmin/credentials/ca/"+clusterName+".crt")
		if err != nil {
			t.Fatalf("stat: %v: %s", err, out)
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if !strings.HasPrefix(line, "600 ") {
				t.Errorf("credential file not 0600: %s", line)
			}
		}
		if out, _ := execIn(pod, "sh", "-c", "grep -c '^\\[pgtoolbox-' /run/pgadmin/credentials/pg_service.conf"); strings.TrimSpace(out) == "0" {
			t.Errorf("no managed section in the service file")
		}
		if out, _ := execIn(pod, "sh", "-c", "grep -l 'BEGIN.*PRIVATE KEY' -r /run/pgadmin/credentials || true"); strings.TrimSpace(out) != "" {
			t.Errorf("a private key reached the credential volume: %s", out)
		}
	})

	t.Run("NoPasswordInStorage", func(t *testing.T) {
		pod := pgAdminPod(t)
		// The application password, by hash: grep for it in pgAdmin's
		// settings volume (its STORAGE_DIR and database) inside the Pod.
		var secret corev1.Secret
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: clusterName + "-app"}, &secret); err != nil {
			t.Fatal(err)
		}
		password := string(secret.Data["password"])
		// No pgpass anywhere under the settings volume (STORAGE_DIR is
		// what pgAdmin's file manager serves).
		out, err := execIn(pod, "sh", "-c", "find /var/lib/pgadmin -name pgpass -o -name .pgpass -o -name pg_service.conf")
		if err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("a credential file under pgAdmin's settings volume: %q (%v)", out, err)
		}
		// The direct check: does any file under the settings volume
		// contain the password? The password is passed on stdin-less
		// argv of a python one-liner and compared there; only paths are
		// printed.
		out, err = execIn(pod, "/venv/bin/python3", "-c", `
import os, sys
needle = sys.argv[1].encode()
for root, _, files in os.walk("/var/lib/pgadmin"):
    for name in files:
        path = os.path.join(root, name)
        try:
            if needle in open(path, "rb").read():
                print(path)
        except OSError:
            pass
`, password)
		if err != nil || strings.TrimSpace(out) != "" {
			t.Errorf("the password is stored under pgAdmin's settings volume: %q (%v)", out, err)
		}
	})

	t.Run("SharedServersRegistered", func(t *testing.T) {
		pod := pgAdminPod(t)
		out, err := execIn(pod, "sh", "-c",
			"cd /pgadmin4 && /venv/bin/python3 setup.py dump-servers /tmp/e2e-dump.json --user pgtoolbox@pgtoolbox.fyannk.dev >/dev/null 2>&1 && cat /tmp/e2e-dump.json && rm -f /tmp/e2e-dump.json")
		if err != nil {
			t.Fatalf("dump-servers: %v: %s", err, out)
		}
		// setup.py's own warnings may surround the document.
		start, end := strings.Index(out, "{"), strings.LastIndex(out, "}")
		if start < 0 || end < start {
			t.Fatalf("no JSON document in the dump:\n%s", out)
		}
		var dump struct {
			Servers map[string]map[string]any `json:"Servers"`
		}
		if err := json.Unmarshal([]byte(out[start:end+1]), &dump); err != nil {
			t.Fatalf("parse the dump: %v\n%s", err, out)
		}
		var managed map[string]any
		for _, server := range dump.Servers {
			if server["Service"] == "pgtoolbox-orders-rw-app" {
				managed = server
			}
		}
		if managed == nil || managed["Group"] != "PgToolBox" || managed["Shared"] != true {
			t.Fatalf("no shared PgToolBox server for orders rw (app) in:\n%s", out)
		}
		// pgAdmin's export always carries its own defaults (for instance
		// TunnelPromptPassword: 0): what must never be there is a saved
		// password, a passfile, or the password itself.
		for _, server := range dump.Servers {
			for _, key := range []string{"Password", "PassFile", "SavePassword"} {
				if _, ok := server[key]; ok {
					t.Errorf("server %v carries %s", server["Name"], key)
				}
			}
			if parameters, ok := server["ConnectionParameters"].(map[string]any); ok {
				for _, key := range []string{"password", "passfile"} {
					if _, ok := parameters[key]; ok {
						t.Errorf("server %v carries the connection parameter %s", server["Name"], key)
					}
				}
			}
		}
		var secret corev1.Secret
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: clusterName + "-app"}, &secret); err != nil {
			t.Fatal(err)
		}
		if password := string(secret.Data["password"]); password != "" && strings.Contains(out, password) {
			t.Error("the application password appears in pgAdmin's server definitions")
		}
	})

	t.Run("LibpqConnects", func(t *testing.T) {
		pod := pgAdminPod(t)
		// CloudNativePG may still recreate the instance Pod just after the
		// Cluster first reads healthy: a refused connection is retried,
		// any other failure (TLS, SCRAM, the service file) is not.
		eventually(t, "psql through the service file", func() error {
			out, err := execIn(pod, "sh", "-c",
				psqlScript("pgtoolbox-orders-rw-app", "select ssl, current_user from pg_stat_ssl join pg_stat_activity using (pid) where pid = pg_backend_pid()"))
			if err != nil && strings.Contains(out, "Connection refused") {
				return fmt.Errorf("%w: %s", err, out)
			}
			if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "t|app") {
				t.Fatalf("psql through the service file (verify-full, SCRAM, channel binding): %v: %s", err, out)
			}
			return nil
		})
	})

	t.Run("Rotation", func(t *testing.T) {
		var before appsv1.Deployment
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: pgAdminName + "-pgadmin"}, &before); err != nil {
			t.Fatal(err)
		}
		var secret corev1.Secret
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: clusterName + "-app"}, &secret); err != nil {
			t.Fatal(err)
		}
		rotated := "rotated-" + sha(time.Now().String())[:16]
		secret.Data["password"] = []byte(rotated)
		delete(secret.Data, "pgpass")
		delete(secret.Data, "uri")
		delete(secret.Data, "jdbc-uri")
		if err := k8s.Update(ctx(), &secret); err != nil {
			t.Fatal(err)
		}
		pod := pgAdminPod(t)
		eventually(t, "the rotated password in pgpass", func() error {
			out, err := execIn(pod, "/venv/bin/python3", "-c",
				"import sys; print(sys.argv[1] in open('/run/pgadmin/credentials/pgpass').read())", rotated)
			if err != nil || strings.TrimSpace(out) != "True" {
				return fmt.Errorf("not yet: %w", err)
			}
			return nil
		})
		eventually(t, "a connection with the rotated password", func() error {
			out, err := execIn(pod, "sh", "-c", psqlScript("pgtoolbox-orders-rw-app", "select 1"))
			if err != nil || strings.TrimSpace(out) != "1" {
				return fmt.Errorf("%w: %s", err, out)
			}
			return nil
		})
		var after appsv1.Deployment
		if err := k8s.Get(ctx(), client.ObjectKey{Namespace: namespace, Name: pgAdminName + "-pgadmin"}, &after); err != nil {
			t.Fatal(err)
		}
		if after.Generation != before.Generation {
			t.Errorf("a password rotation rolled the Deployment (generation %d -> %d)", before.Generation, after.Generation)
		}
	})
}
