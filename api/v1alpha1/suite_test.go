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

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// One API server for every CEL/schema test of the package, started on
// first use (make test-envtest provides KUBEBUILDER_ASSETS, on the oldest
// Kubernetes the bundle supports) and stopped by TestMain.
var (
	apiServerOnce   sync.Once
	apiServerEnv    *envtest.Environment
	apiServerClient client.Client
	errAPIServer    error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if apiServerEnv != nil {
		_ = apiServerEnv.Stop()
	}
	os.Exit(code)
}

// apiServer returns a client of an API server with the generated CRDs
// installed, or skips the test when envtest binaries are absent.
func apiServer(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; CEL rules need a real API server (make test-envtest)")
	}
	apiServerOnce.Do(func() {
		apiServerEnv = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfCRDPathMissing: true,
		}
		config, err := apiServerEnv.Start()
		if err != nil {
			errAPIServer = err
			return
		}
		scheme := runtime.NewScheme()
		if err := pgtoolboxv1alpha1.AddToScheme(scheme); err != nil {
			errAPIServer = err
			return
		}
		apiServerClient, errAPIServer = client.New(config, client.Options{Scheme: scheme})
	})
	if errAPIServer != nil {
		t.Fatalf("start envtest: %v", errAPIServer)
	}
	return apiServerClient
}
