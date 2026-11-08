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

package passfilesync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

// projectCAs writes CA ConfigMaps into dir the way the kubelet's atomic
// writer fills a projected volume: the files in a fresh hidden directory,
// a ..data link swapped onto it by a rename, and one top-level link per
// ConfigMap through ..data. Only the rename touches dir itself, which is
// what the sidecar has to notice.
func projectCAs(t *testing.T, dir string, cas map[string]string) {
	t.Helper()
	version := fmt.Sprintf("..%d", time.Now().UnixNano())
	for name, pem := range cas {
		if err := os.MkdirAll(filepath.Join(dir, version, name), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, version, name, declared.CAConfigMapKey), []byte(pem), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(version, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range cas {
		link := filepath.Join(dir, name)
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join("..data", name), link); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRunDeclaredCAFromAMountedConfigMap: with caConfigMap the sidecar
// reads no CA Secret through the API (its Role names none); a server
// whose optional ConfigMap is not there yet has no section and says why;
// once the kubelet projects it the server is offered, and a renewed CA
// replaces the old one without a restart.
func TestRunDeclaredCAFromAMountedConfigMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheme := runtime.NewScheme()
	if err := cnpgv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, ordersCluster())
	// No CA Secret exists: reading one would find nothing.
	client := fake.NewClientset(secret("orders-app", "5", map[string]string{"username": "shop_owner", "password": "pw-app"}))
	doc := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
	}}
	files := Files{Dir: t.TempDir()}
	caDir := t.TempDir()
	logs := &capture{}
	done := make(chan error, 1)
	go func() {
		done <- RunDeclared(ctx, DeclaredOptions{
			Client: client, Dynamic: dyn, Namespace: "team", Document: doc, Files: files, CADir: caDir,
			Log: logs.logger(), Debounce: 20 * time.Millisecond, Backoff: 50 * time.Millisecond,
		})
	}()

	eventually(t, "the absent ConfigMap explained", func() bool {
		return strings.Contains(logs.all(), "CA ConfigMap orders-ca-crt is absent or has no ca.crt")
	})
	if fileContains(files.ServiceFile(), "orders-rw") {
		t.Fatal("a server without its CA must have no section")
	}

	projectCAs(t, caDir, map[string]string{"orders-ca-crt": caPEM1})
	caFile := filepath.Join(files.Dir, "ca", "orders.crt")
	eventually(t, "the projected CA", func() bool {
		return fileContains(caFile, "CLUSTER-ONE") && fileContains(files.PassFile(), "pw-app") && fileContains(files.ServiceFile(), "orders-rw.team.svc")
	})

	// CloudNativePG renews its CA; the ConfigMap is updated, the kubelet
	// swaps ..data: the new CA reaches libpq's files.
	projectCAs(t, caDir, map[string]string{"orders-ca-crt": caPEM2})
	eventually(t, "the renewed CA", func() bool { return fileContains(caFile, "CLUSTER-TWO") && !fileContains(caFile, "CLUSTER-ONE") })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunDeclared: %v", err)
	}
	for _, action := range client.Actions() {
		if !action.Matches("get", "secrets") && !action.Matches("watch", "secrets") && !action.Matches("list", "secrets") {
			continue
		}
		name := ""
		switch a := action.(type) {
		case k8stesting.GetAction:
			name = a.GetName()
		case k8stesting.WatchAction:
			name = a.GetWatchRestrictions().Fields.String()
		case k8stesting.ListAction:
			name = a.GetListRestrictions().Fields.String()
		}
		if !strings.Contains(name, "orders-app") {
			t.Errorf("the sidecar read a Secret it was not given: %s %q", action.GetVerb(), name)
		}
	}
	if strings.Contains(logs.all(), "pw-app") {
		t.Fatalf("a password reached the log:\n%s", logs.all())
	}
}

// TestRunDeclaredCAConfigMapNeedsTheDirectory: a list naming a caConfigMap
// without the directory it is mounted in is refused at start, not run with
// every such server silently unconnectable.
func TestRunDeclaredCAConfigMapNeedsTheDirectory(t *testing.T) {
	doc := declared.Document{Version: 1, Servers: []declared.Server{
		{Cluster: "orders", Endpoint: "rw", CAConfigMap: "orders-ca-crt", Credentials: app("orders-app")},
	}}
	err := RunDeclared(t.Context(), DeclaredOptions{
		Client: fake.NewClientset(), Namespace: "team", Document: doc, Files: Files{Dir: t.TempDir()}, Log: (&capture{}).logger(),
	})
	if err == nil || !strings.Contains(err.Error(), "CA directory is required") {
		t.Fatalf("RunDeclared = %v, want the missing CA directory refused", err)
	}
	if err := RunDeclared(t.Context(), DeclaredOptions{
		Client: fake.NewClientset(), Namespace: "team", Document: doc, Files: Files{Dir: t.TempDir()}, Log: (&capture{}).logger(),
		CADir: filepath.Join(t.TempDir(), "absent"),
	}); err == nil || !strings.Contains(err.Error(), "watch the CA ConfigMaps") {
		t.Fatalf("RunDeclared = %v, want an unwatchable CA directory refused", err)
	}
}
