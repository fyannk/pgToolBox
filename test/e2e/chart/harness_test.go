//go:build e2echart

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

// Package chart is the e2e of charts/pgtools' console, run by
// the e2e-chart script against a kind cluster it provisions (the CNPG CRDs, the
// console image built from the working tree, and for the policy test Calico).
//
// Two tests, two CI jobs (the console binary is cmd/console):
//
//   - TestTeamConsoles installs two team consoles in two namespaces and proves,
//     with SubjectAccessReviews issued against each console's own ServiceAccount,
//     that it holds no cluster-scoped read except SubjectAccessReview creation and
//     the opt-in ClusterImageCatalog read, and nothing at all in a namespace it
//     does not watch; it exercises the availability rule and uninstall on a real
//     API server; and it signs people in (SSO through an in-cluster dex that the
//     API server trusts, and the break-glass token form) to prove the cluster
//     list is exactly what they may get, the overview is honest about what it
//     cannot see, the revocation ConfigMap kills sessions, and sign-out works.
//   - TestNetworkPolicy installs the chart with networkPolicy.enabled on a
//     policy-enforcing CNI and proves the console still works and cannot reach the
//     other namespace's instance pods or evidence agent.
package chart

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var chartPath = flag.String("chart", "../../../charts/pgtools", "path to the chart under test")

var (
	clientset *kubernetes.Clientset
	consoleImage, consoleTag,
	probeImage string
	artifactDir string
)

const (
	pollInterval = 2 * time.Second
	installWait  = "4m"
)

func TestMain(m *testing.M) {
	flag.Parse()

	ref := os.Getenv("CONSOLE_IMG")
	if ref == "" {
		fmt.Fprintln(os.Stderr, "CONSOLE_IMG is required (hack/e2e-chart.sh sets it)")
		os.Exit(1)
	}
	consoleImage, consoleTag = ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		consoleImage, consoleTag = ref[:i], ref[i+1:]
	}
	if probeImage = os.Getenv("PROBE_IMAGE"); probeImage == "" {
		probeImage = "busybox:1.37"
	}
	artifactDir = os.Getenv("E2E_ARTIFACTS")

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load kubeconfig: %v\n", err)
		os.Exit(1)
	}
	// The RBAC matrix issues thousands of SubjectAccessReviews.
	cfg.QPS, cfg.Burst = 500, 1000
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return retryTransport{next: rt} })
	if clientset, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "build clientset: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func ctx() context.Context { return context.Background() }

// eventually polls check until it returns nil or the timeout expires, then fails
// the test with the last error.
func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for {
		if err = check(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s: %v", what, timeout, err)
		}
		time.Sleep(pollInterval)
	}
}

// run executes a command with the cluster's kubeconfig (inherited) and returns
// its combined output only on failure, which carries it.
func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
}

// ---------------------------------------------------------------- namespaces

// namespace creates a namespace labelled Pod Security "restricted" (enforced), so
// that everything the chart renders is admitted under the strictest standard or
// the install fails. extra labels are merged in.
func namespace(t *testing.T, name string, extra map[string]string) {
	t.Helper()
	labels := map[string]string{
		"kubernetes.io/metadata.name":                name,
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"app.kubernetes.io/part-of":                  "pgtoolbox-e2e",
	}
	for k, v := range extra {
		labels[k] = v
	}
	_, err := clientset.CoreV1().Namespaces().Create(ctx(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// ----------------------------------------------------------------- consoles

// console describes one release of the chart under test.
type console struct {
	Release    string   // Helm release, also the prefix of its objects
	Namespace  string   // release namespace
	Watch      []string // console.watchNamespaces (empty: the release namespace)
	CatalogOpt bool     // console.clusterImageCatalogs.enabled
	Extra      []string // extra `helm upgrade --install` arguments
	// SSO is the public host (no scheme) of a console that signs in through dex:
	// the console is installed with auth.provider=oidc and publicURL http://<SSO>.
	// The host does not resolve; the test dials the console's NodePort for it.
	SSO string
	// MCP serves the MCP endpoint (console.mcp.enabled).
	MCP bool
	// MCPWrite adds the MCP write tools (console.mcp.writeTools).
	MCPWrite bool
	// Env is extra console environment (console.extraEnv).
	Env map[string]string
}

func (c console) fullname() string       { return c.Release + "-console" }
func (c console) revocations() string    { return c.fullname() + "-revocations" }
func (c console) serviceAccount() string { return c.fullname() }
func (c console) clusterRole() string    { return c.Namespace + ":" + c.fullname() }
func (c console) user() string {
	return "system:serviceaccount:" + c.Namespace + ":" + c.serviceAccount()
}

func (c console) watched() []string {
	if len(c.Watch) == 0 {
		return []string{c.Namespace}
	}
	return c.Watch
}

// valuesFile writes the values the e2e needs on top of the chart defaults: the
// image built from the working tree, side-loaded (never pulled), and, for an SSO
// console, the dex issuer, client and CA. Cookies are not Secure: the test speaks
// plain HTTP to a NodePort.
func (c console) valuesFile(t *testing.T) string {
	t.Helper()
	cons := map[string]any{
		"image": map[string]any{
			"repository": consoleImage,
			"tag":        consoleTag,
			"pullPolicy": "Never",
		},
		"cookieSecure": "false",
	}
	values := map[string]any{"console": cons}
	if c.Watch != nil {
		cons["watchNamespaces"] = c.Watch
	}
	if c.CatalogOpt {
		cons["clusterImageCatalogs"] = map[string]any{"enabled": true}
	}
	if c.MCP {
		cons["mcp"] = map[string]any{"enabled": true, "writeTools": c.MCPWrite}
	}
	if len(c.Env) > 0 {
		env := make([]map[string]string, 0, len(c.Env))
		for _, k := range sortedStrings(slices.Collect(maps.Keys(c.Env))) {
			env = append(env, map[string]string{"name": k, "value": c.Env[k]})
		}
		cons["extraEnv"] = env
	}
	if c.SSO != "" {
		cons["publicURL"] = "http://" + c.SSO
		cons["auth"] = map[string]any{
			"provider": "oidc",
			"oidc": map[string]any{
				"issuerURL":    dexIssuer(),
				"clientID":     dexClientID,
				"clientSecret": map[string]any{"existingSecret": dexClientSecretName},
				"caConfigMap":  dexCAConfigMap,
			},
		}
		if c.MCP {
			// Grants sign in with a client of their own, so that the
			// browser session keeps its refresh token at dex.
			cons["auth"].(map[string]any)["oidc"].(map[string]any)["grantClient"] = map[string]any{
				"clientID": dexGrantClientID, "clientSecret": map[string]any{"existingSecret": dexGrantClientSecretName},
			}
		}
	}
	raw, err := json.Marshal(values) // JSON is YAML
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), c.Release+"-values.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// install runs `helm upgrade --install --wait` for the console.
func (c console) install(t *testing.T, extra ...string) {
	t.Helper()
	args := make([]string, 0, 11+len(c.Extra)+len(extra))
	args = append(args,
		"upgrade", "--install", c.Release, *chartPath,
		"--namespace", c.Namespace,
		"--values", c.valuesFile(t),
		"--wait", "--timeout", installWait,
	)
	args = append(args, c.Extra...)
	args = append(args, extra...)
	if out, err := helmOutput(args...); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// helmOutput runs helm and returns its combined output. A run that failed on a
// transient API error (an etcd timeout on a loaded runner) is repeated a few
// times; `upgrade --install` is idempotent, so a repeat is safe. Any other
// failure is returned at once.
func helmOutput(args ...string) (string, error) {
	var out string
	var err error
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		cmd := exec.CommandContext(context.Background(), "helm", args...)
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		err = cmd.Run()
		out = buf.String()
		if err == nil || !helmTransient(out) || attempt == retryAttempts {
			break
		}
		fmt.Fprintf(os.Stderr, "[e2e-chart] helm %s hit a transient API error (attempt %d/%d), retrying:\n%s\n",
			strings.Join(args[:min(len(args), 3)], " "), attempt, retryAttempts, tail(out, 600))
		time.Sleep(retryBackoff(attempt))
	}
	return out, err
}

// ---------------------------------------------------------------- failure dump

// dumpOnFailure registers a cleanup that, when the test failed, prints a compact
// state dump to the log (the CI cannot upload artifacts) and writes the full one
// under E2E_ARTIFACTS.
func dumpOnFailure(t *testing.T, namespaces ...string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var all strings.Builder
		for _, ns := range namespaces {
			for _, args := range [][]string{
				{"-n", ns, "get", "deploy,rs,pods,svc,pdb,networkpolicy,pvc,cm,role,rolebinding,sa", "-o", "wide"},
				{"-n", ns, "get", "events", "--sort-by=.lastTimestamp"},
				{"-n", ns, "describe", "pods"},
				{"-n", ns, "logs", "-l", "app.kubernetes.io/name=pgtoolbox-console", "--all-containers", "--tail=80", "--prefix"},
			} {
				out, _ := exec.CommandContext(context.Background(), "kubectl", args...).CombinedOutput()
				fmt.Fprintf(&all, "\n$ kubectl %s\n%s", strings.Join(args, " "), out)
			}
		}
		out, _ := exec.CommandContext(context.Background(), "kubectl", "get", "clusterrole,clusterrolebinding", "-o", "name").CombinedOutput()
		fmt.Fprintf(&all, "\n$ kubectl get clusterrole,clusterrolebinding -o name (pgtoolbox-console only)\n%s", grep(string(out), "console"))
		report := all.String()
		t.Logf("state at failure:%s", tail(report, 12000))
		if artifactDir != "" {
			if err := os.MkdirAll(artifactDir, 0o755); err == nil {
				_ = os.WriteFile(filepath.Join(artifactDir, t.Name()+".txt"), []byte(report), 0o644)
			}
		}
	})
}

func grep(s, sub string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}

// parallel runs fn(i) for i in [0,n) on a bounded pool.
func parallel(n, workers int, fn func(i int)) {
	var wg sync.WaitGroup
	jobs := make(chan int)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

func intOrString(p int) intstr.IntOrString { return intstr.FromInt32(int32(p)) }
