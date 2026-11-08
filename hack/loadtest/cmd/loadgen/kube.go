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

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The only context loadgen talks to. cluster.sh creates the kind cluster
// "pgtoolbox-load" with a private kubeconfig, and kind names its context
// "kind-<cluster>".
const defaultContext = "kind-pgtoolbox-load"

// Labels the harness agrees on with cluster.sh.
const (
	// kwokNodeLabel selects the fake nodes cluster.sh creates.
	kwokNodeLabel = "type=kwok"
	// clusterLabel is CloudNativePG's cluster label on every owned object.
	clusterLabel = "cnpg.io/cluster"
	// harnessLabel marks every object loadgen creates, for cleanup.
	harnessLabel = "loadtest.pgtoolbox.fyannk.dev/generated"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// kubeFlags are the flags every command that talks to the API server shares.
type kubeFlags struct {
	kubeconfig string
	context    string
	inCluster  bool
	qps        float64
	burst      int
}

func (k *kubeFlags) register(fs *flag.FlagSet, qps float64, burst int) {
	fs.StringVar(&k.kubeconfig, "kubeconfig", os.Getenv("LOADTEST_KUBECONFIG"), "kubeconfig of the load-test cluster (env LOADTEST_KUBECONFIG); the default ~/.kube/config is never used")
	fs.StringVar(&k.context, "context", defaultContext, "the only context loadgen accepts")
	fs.BoolVar(&k.inCluster, "in-cluster", false, "use the Pod's service account instead of a kubeconfig (instance-sim)")
	fs.Float64Var(&k.qps, "qps", qps, "client-side rate limit toward the API server (requests/s)")
	fs.IntVar(&k.burst, "burst", burst, "client-side burst toward the API server")
}

// kube bundles the clients a command uses.
type kube struct {
	cfg    *rest.Config
	client client.Client
	clset  kubernetes.Interface
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(cnpgv1.AddToScheme(s))
	return s
}

func (k *kubeFlags) connect() (*kube, error) {
	var cfg *rest.Config
	if k.inCluster {
		c, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("in-cluster configuration: %w", err)
		}
		cfg = c
	} else {
		if k.kubeconfig == "" {
			return nil, errors.New("no kubeconfig: pass --kubeconfig or source hack/loadtest/.state/env (LOADTEST_KUBECONFIG); the default kubeconfig is never used")
		}
		loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: k.kubeconfig},
			&clientcmd.ConfigOverrides{CurrentContext: k.context})
		raw, err := loader.RawConfig()
		if err != nil {
			return nil, fmt.Errorf("read kubeconfig %s: %w", k.kubeconfig, err)
		}
		if _, ok := raw.Contexts[k.context]; !ok {
			return nil, fmt.Errorf("kubeconfig %s has no context %q", k.kubeconfig, k.context)
		}
		if strings.Contains(k.context, "e2e") || strings.HasSuffix(k.context, "pgtoolbox-lab") {
			return nil, fmt.Errorf("context %q is not a load-test cluster", k.context)
		}
		c, err := loader.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("client configuration: %w", err)
		}
		cfg = c
	}
	cfg.QPS = float32(k.qps)
	cfg.Burst = k.burst
	cfg.UserAgent = "loadgen"
	cl, err := client.New(cfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &kube{cfg: cfg, client: cl, clset: cs}, nil
}

// nsName is the harness namespace i (1-based): lt-01, lt-02, ...
func nsName(prefix string, i int) string { return fmt.Sprintf("%s-%02d", prefix, i) }

// clusterName is Cluster j (1-based) of every namespace: c-0001, ...
func clusterName(j int) string { return fmt.Sprintf("c-%04d", j) }

// instanceName is the Pod name CloudNativePG gives member n (1-based).
func instanceName(cluster string, n int) string { return fmt.Sprintf("%s-%d", cluster, n) }
