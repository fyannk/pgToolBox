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

// Package e2e holds the operator scenarios that run against a real cluster
// (test/e2e/evidenceagent.sh provisions it). A fake client cannot fail the two
// ways a real cluster can: RBAC escalation prevention refusing a Role rule the
// operator does not itself hold, and a container rejecting the environment it
// was rendered. This file is the shared harness: the clients, the test
// namespace and the polling helper.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	testNamespace = "pgtoolbox-e2e"

	// Generous: images are side-loaded, but every Pod still has to pass its
	// own readiness probe.
	readyTimeout = 5 * time.Minute
	pollInterval = 3 * time.Second
)

var (
	k8s        client.Client
	clientset  *kubernetes.Clientset
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	flag.Parse()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
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
