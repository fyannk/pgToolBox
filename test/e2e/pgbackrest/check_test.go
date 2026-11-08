//go:build e2epgbackrest

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

// Package pgbackrest holds the assertions of the pgBackRest kind scenario
// (test/e2e/pgbackrest/run.sh). The script provisions the cluster, applies the
// objects and breaks things on purpose; this test is what it calls, once per
// assertion, to read the two things the feature promises: what the evidence
// agent publishes (pgbackrest/v1alpha1 on the evidence/v1alpha1 channel) and
// what the console's cluster backups API says for the Cluster. Each check polls
// until it holds or its timeout expires, and then fails with every difference
// it saw rather than the first.
package pgbackrest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

var (
	check     = flag.String("check", "", "which assertion to run: console-live, console-unavailable, console-gap, console-drill, agent-live, agent-gap")
	namespace = flag.String("namespace", "", "the namespace of the cell")
	cluster   = flag.String("cluster", "ledger", "the CloudNativePG Cluster")
	flavor    = flag.String("plugin", "", "dalibo or opera")
	stanza    = flag.String("stanza", "ledger", "the stanza the cell's plugin object names")
	object    = flag.String("object", "store", "the Stanza or Archive the Cluster names")
	backups   = flag.Int("backups", 2, "completed Backup objects the cell took")
	consoleIn = flag.String("console", "", "app.kubernetes.io/instance of the console to read (console: the PgToolBoxConsole; team: the chart)")
	consoleSA = flag.String("console-sa", "viewer", "ServiceAccount whose token signs in to the console")
	agentIn   = flag.String("agent", "", "app.kubernetes.io/instance of the evidence agent to read (ledger: the EvidenceAgent; team: the chart's)")
	agentAuth = flag.String("agent-auth", "", "operator:<EvidenceAgent>:<ServiceAccount> or shared:<token Secret>")
	timeout   = flag.Duration("timeout", 4*time.Minute, "how long the assertion may take to hold")
)

// A flavor is what differs between the two plugins in what the console reports.
type plugin struct {
	name, flavor, kind, display string
}

var plugins = map[string]plugin{
	"dalibo": {"pgbackrest.dalibo.com", "dalibo", "Stanza", "pgBackRest (Dalibo)"},
	"opera":  {"pgbackrest.cnpg.opera.com", "opera", "Archive", "pgBackRest (Opera)"},
}

var (
	cfg *rest.Config
	cs  *kubernetes.Clientset
	dyn dynamic.Interface
)

func TestMain(m *testing.M) {
	flag.Parse()
	if *check == "" {
		fmt.Fprintln(os.Stderr, "-check is required (this test is run by test/e2e/pgbackrest/run.sh)")
		os.Exit(2)
	}
	var err error
	if cfg, err = config.GetConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "load kubeconfig: %v\n", err)
		os.Exit(2)
	}
	if cs, err = kubernetes.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "build clientset: %v\n", err)
		os.Exit(2)
	}
	if dyn, err = dynamic.NewForConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "build dynamic client: %v\n", err)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestCheck(t *testing.T) {
	p, ok := plugins[*flavor]
	if !ok {
		t.Fatalf("-plugin %q: want dalibo or opera", *flavor)
	}
	if *namespace == "" {
		t.Fatal("-namespace is required")
	}
	switch *check {
	case "console-live":
		pollConsole(t, func(b viewmodel.Backups) error { return consoleLive(b, p) })
	case "console-unavailable":
		pollConsole(t, consoleUnavailable)
	case "console-gap":
		pollConsole(t, func(b viewmodel.Backups) error { return consoleGap(b, p) })
	case "console-drill":
		runDrill(t, p)
	case "agent-live":
		pollAgent(t, func(a *agent) error { return agentLive(a, p) })
	case "agent-gap":
		pollAgent(t, agentGap)
	default:
		t.Fatalf("unknown -check %q", *check)
	}
}

// poll runs f until it returns nil; the timeout fails with f's last error.
func poll(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(*timeout)
	var last error
	for {
		if last = f(); last == nil {
			t.Logf("ok: %s", what)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s:\n%v", what, *timeout, last)
		}
		time.Sleep(5 * time.Second)
	}
}

// problems collects every difference an assertion finds.
type problems []error

func (p *problems) want(ok bool, format string, args ...any) {
	if !ok {
		*p = append(*p, fmt.Errorf(format, args...))
	}
}

func (p problems) err() error { return errors.Join(p...) }

// ---- port-forward and sign-in -------------------------------------------

// forward port-forwards a local port to the first Pod matching selector.
func forward(t *testing.T, selector string, port int) (string, func()) {
	t.Helper()
	pods, err := cs.CoreV1().Pods(*namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("Pod %q in %s: %v (%d found)", selector, *namespace, err, len(pods.Items))
	}
	pod := pods.Items[0]
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward").URL().String())
	if err != nil {
		t.Fatal(err)
	}
	stop, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target),
		[]string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	failed := make(chan error, 1)
	go func() { failed <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		t.Fatalf("port-forward to %s: %v", pod.Name, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("port-forward to %s did not become ready", pod.Name)
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		t.Fatalf("forwarded ports: %v", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local), func() { close(stop) }
}

func token(t *testing.T, account string, audiences ...string) string {
	t.Helper()
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: new(int64(900)), Audiences: audiences,
	}}
	response, err := cs.CoreV1().ServiceAccounts(*namespace).CreateToken(context.Background(), account, request, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("token for %s/%s: %v", *namespace, account, err)
	}
	return response.Status.Token
}

// console is a signed-in client of one console, through a port-forward.
type console struct {
	base string
	http *http.Client
}

func signIn(t *testing.T, instance, account string) (*console, func()) {
	t.Helper()
	address, stop := forward(t, "app.kubernetes.io/component=console,app.kubernetes.io/instance="+instance, 3000)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &console{base: "http://" + address, http: &http.Client{
		Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	form := url.Values{"token": {token(t, account)}, "rd": {"/"}}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+"/auth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", c.base)
	var status int
	for range 10 { // the console budgets sign-ins per client address
		response, err := c.http.Do(request.Clone(context.Background()))
		if err == nil {
			status = response.StatusCode
			_ = response.Body.Close()
			if status == http.StatusFound {
				return c, stop
			}
		}
		time.Sleep(6 * time.Second)
	}
	stop()
	t.Fatalf("sign-in to console %q as %s: HTTP %d", instance, account, status)
	return nil, nil
}

func (c *console) backups(t *testing.T) (viewmodel.Backups, error) {
	t.Helper()
	var b viewmodel.Backups
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		fmt.Sprintf("%s/api/v1/namespaces/%s/clusters/%s/backups", c.base, *namespace, *cluster), nil)
	if err != nil {
		return b, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return b, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return b, err
	}
	if response.StatusCode != http.StatusOK {
		return b, fmt.Errorf("GET backups: HTTP %d: %.200s", response.StatusCode, body)
	}
	return b, json.Unmarshal(body, &b)
}

func pollConsole(t *testing.T, f func(viewmodel.Backups) error) {
	t.Helper()
	c, stop := signIn(t, *consoleIn, *consoleSA)
	defer stop()
	poll(t, fmt.Sprintf("console %q on %s/%s: %s", *consoleIn, *namespace, *cluster, *check), func() error {
		b, err := c.backups(t)
		if err != nil {
			return err
		}
		return f(b)
	})
}

// ---- the console's view -------------------------------------------------

func consoleLive(b viewmodel.Backups, p plugin) error {
	var pr problems
	pr.want(b.Summary.PluginBased && b.Method.PluginBased, "the Cluster is not reported as plugin-based")
	m := b.Method.PgBackRest
	pr.want(m != nil, "method.pgbackrest is absent: the plugin was not detected as a pgBackRest method")
	if m != nil {
		pr.want(m.Plugin == p.name && m.Flavor == p.flavor && m.ObjectKind == p.kind && m.ObjectName == *object && m.Display == p.display,
			"method = %+v, want %s/%s %s %s", *m, p.name, p.flavor, p.kind, *object)
		pr.want(m.Stanza == *stanza, "method stanza = %q, want %q", m.Stanza, *stanza)
	}
	e := b.Evidence
	pr.want(e.State == viewmodel.EvidenceLive, "evidence.state = %q (%s), want live", e.State, e.Reason)
	r := e.Repository
	if r == nil || r.PgBackRest == nil {
		pr.want(false, "evidence.repository.pgbackrest is absent")
		return pr.err()
	}
	pg := r.PgBackRest
	pr.want(pg.Stanza == *stanza && pg.ArchiveStanza == *stanza, "stanza = %q / %q, want %q", pg.Stanza, pg.ArchiveStanza, *stanza)
	pr.want(pg.Repository == 1, "repository = %d, want 1", pg.Repository)
	pr.want(pg.RepositoryFormat != nil && *pg.RepositoryFormat == 5, "repository format = %v, want 5", pg.RepositoryFormat)
	pr.want(pg.Encrypted != nil && !*pg.Encrypted, "encrypted = %v, want false", pg.Encrypted)
	// The reader's fixtures are pgBackRest 2.60.0's; the plugins' sidecar
	// images write the version they ship (2.58.0 at the pinned tags), which
	// the reader must accept all the same.
	pr.want(strings.HasPrefix(pg.PgBackRestVersion, "2."), "pgbackrest version = %q, want a 2.x", pg.PgBackRestVersion)
	pr.want(r.Overall.Code == "snapshot-healthy", "overall = %s (%s)", r.Overall.Code, r.Overall.Message)
	pr.want(pg.Catalog.Code == "catalog-healthy", "catalog = %s (%s)", pg.Catalog.Code, pg.Catalog.Message)
	pr.want(r.WAL.Code == "wal-healthy", "wal = %s (%s)", r.WAL.Code, r.WAL.Message)
	pr.want(r.Timeline.Code == "timeline-healthy", "timeline = %s (%s)", r.Timeline.Code, r.Timeline.Message)
	pr.want(r.Coverage.Code == "coverage-healthy", "coverage = %s (%s)", r.Coverage.Code, r.Coverage.Message)
	for _, f := range pg.Findings {
		pr.want(f.Severity != "error", "finding %s: %s", f.Code, f.Message)
	}
	pr.want(len(pg.Backups) >= *backups, "the agent lists %d backups, want at least %d", len(pg.Backups), *backups)
	pr.want(r.StructurallyUsable != nil && *r.StructurallyUsable >= int64(*backups), "structurally usable = %v, want at least %d", r.StructurallyUsable, *backups)

	// The recoverability window: the point of the feature, since neither
	// plugin writes Cluster.status.firstRecoverabilityPoint.
	w := pg.RecoveryWindow
	pr.want(w != nil, "no recovery window")
	if w != nil {
		pr.want(!w.StartAt.IsZero() && w.EndWAL != "" && w.EndTimeline >= 1 && w.UsableBackups >= int64(*backups),
			"recovery window = %+v, want a start, an end WAL and %d usable backups", *w, *backups)
		if len(pg.Backups) > 0 {
			pr.want(w.NewestBackup == pg.Backups[0].Label, "window newest backup = %q, newest listed = %q", w.NewestBackup, pg.Backups[0].Label)
		}
	}
	pr.want(b.RecoveryTimeline.Basis == "evidence" && b.RecoveryTimeline.State == "healthy",
		"recovery timeline = %s/%s (%s), want evidence/healthy", b.RecoveryTimeline.Basis, b.RecoveryTimeline.State, b.RecoveryTimeline.Reason)

	// The chain: a full and a differential that names it.
	var full, diff *viewmodel.PgBackRestRepoBackup
	for i := range pg.Backups {
		switch pg.Backups[i].Type {
		case "full":
			full = &pg.Backups[i]
		case "diff":
			diff = &pg.Backups[i]
		}
	}
	pr.want(full != nil && diff != nil, "want a full and a differential backup, got %+v", labels(pg.Backups))
	if full != nil && diff != nil {
		pr.want(diff.Full == full.Label && diff.Prior == full.Label && diff.References >= 1, "the differential %s does not chain to %s: %+v", diff.Label, full.Label, *diff)
		pr.want(full.Usable && diff.Usable, "backups not both usable: %+v", labels(pg.Backups))
	}

	// The Backup objects join the repository's labels, both ways.
	completed := 0
	for _, item := range b.Items {
		if item.Phase != "completed" {
			continue
		}
		completed++
		ref := item.PgBackRest
		if ref == nil {
			pr.want(false, "Backup %s has no pgBackRest label (backupId %q)", item.Name, item.BackupID)
			continue
		}
		i := slices.IndexFunc(pg.Backups, func(x viewmodel.PgBackRestRepoBackup) bool { return x.Label == ref.Label })
		pr.want(i >= 0, "Backup %s names label %q, which is not in the repository", item.Name, ref.Label)
		if i >= 0 {
			pr.want(pg.Backups[i].BackupName == item.Name, "repository backup %s is joined to %q, want %s", ref.Label, pg.Backups[i].BackupName, item.Name)
			pr.want(ref.Usable != nil && *ref.Usable && ref.Stanza == *stanza, "Backup %s ref = %+v", item.Name, *ref)
		}
		pr.want(item.Repository == viewmodel.RepositoryPresent, "Backup %s repository presence = %q, want present", item.Name, item.Repository)
	}
	pr.want(completed == *backups, "%d completed Backup objects, want %d", completed, *backups)
	return pr.err()
}

func labels(backups []viewmodel.PgBackRestRepoBackup) []string {
	out := make([]string, 0, len(backups))
	for _, b := range backups {
		out = append(out, b.Type+":"+b.Label)
	}
	return out
}

// consoleUnavailable is the bad-CA case: the agent could not verify the
// repository's certificate, so the console must not call the repository live
// or healthy. The agent's own log carries the TLS cause (checked by run.sh).
func consoleUnavailable(b viewmodel.Backups) error {
	var pr problems
	e := b.Evidence
	pr.want(e.State == viewmodel.EvidenceStale || e.State == viewmodel.EvidenceUnavailable,
		"evidence.state = %q (%s), want stale or unavailable", e.State, e.Reason)
	pr.want(e.Health != viewmodel.HealthOK, "evidence.health = %q: a failed scan is shown healthy", e.Health)
	if e.Repository != nil {
		pr.want(e.Repository.Overall.Health != viewmodel.HealthOK, "overall health = %q (%s)", e.Repository.Overall.Health, e.Repository.Overall.Code)
		pr.want(e.Repository.Coverage.Health != viewmodel.HealthOK, "coverage health = %q: recoverability is claimed without a scan", e.Repository.Coverage.Health)
		if pg := e.Repository.PgBackRest; pg != nil {
			pr.want(pg.RecoveryWindow == nil, "a recovery window is shown (%+v) although the scan failed", pg.RecoveryWindow)
		}
	}
	pr.want(b.RecoveryTimeline.State != "healthy", "the recovery timeline is healthy (%s)", b.RecoveryTimeline.Reason)
	return pr.err()
}

// consoleGap is the missing-WAL case: a confirmed hole in the archive, so the
// recent end of the window is not restorable and the console says which.
func consoleGap(b viewmodel.Backups, p plugin) error {
	var pr problems
	e := b.Evidence
	pr.want(e.State == viewmodel.EvidenceLive, "evidence.state = %q (%s), want live: a gap is evidence, not an outage", e.State, e.Reason)
	r := e.Repository
	if r == nil || r.PgBackRest == nil {
		pr.want(false, "evidence.repository.pgbackrest is absent")
		return pr.err()
	}
	pg := r.PgBackRest
	pr.want(r.WAL.Code == "wal-unhealthy" && r.WAL.Health == viewmodel.HealthCritical, "wal = %s/%s (%s), want wal-unhealthy/critical", r.WAL.Code, r.WAL.Health, r.WAL.Message)
	pr.want(r.Coverage.Code == "coverage-unhealthy", "coverage = %s (%s), want coverage-unhealthy", r.Coverage.Code, r.Coverage.Message)
	pr.want(r.Overall.Health == viewmodel.HealthCritical, "overall health = %q, want critical", r.Overall.Health)
	gap := slices.IndexFunc(pg.Findings, func(f viewmodel.PgBackRestFinding) bool {
		return f.Code == evidencev1alpha1.FindingWALGap && f.Severity == "error" && strings.Contains(f.Message, "confirmed")
	})
	pr.want(gap >= 0, "no confirmed WALGap finding among %+v", pg.Findings)
	pr.want(b.RecoveryTimeline.State == "unhealthy" && slices.ContainsFunc(b.RecoveryTimeline.Segments, func(s viewmodel.RecoverySegment) bool { return s.Kind == "missingWAL" }),
		"recovery timeline = %s with segments %+v, want unhealthy with a missingWAL span", b.RecoveryTimeline.State, b.RecoveryTimeline.Segments)
	// The base backups stay usable: it is their WAL after the hole that is lost.
	pr.want(pg.RecoveryWindow != nil && pg.RecoveryWindow.UsableBackups >= 1, "recovery window = %+v: the backups before the hole are still restorable", pg.RecoveryWindow)
	_ = p
	return pr.err()
}

// runDrill starts a restore drill as the dba account and waits for its verdict.
func runDrill(t *testing.T, p plugin) {
	t.Helper()
	c, stop := signIn(t, *consoleIn, "dba")
	defer stop()

	body := fmt.Sprintf(`{"kind":"restoreDrill","confirmation":%q}`, *cluster)
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("%s/api/v1/namespaces/%s/clusters/%s/operations", c.base, *namespace, *cluster), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.base)
	request.Header.Set("X-PgToolBox-Request", "1")
	response, err := c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	accepted, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("start the drill: HTTP %d: %.300s", response.StatusCode, accepted)
	}
	t.Logf("drill accepted: %.200s", accepted)

	var last viewmodel.RestoreDrill
	poll(t, "the restore drill reaches a verdict", func() error {
		b, err := c.backups(t)
		if err != nil {
			return err
		}
		if len(b.Drills.Items) == 0 {
			return errors.New("the console records no drill yet")
		}
		last = b.Drills.Items[0]
		if last.Result == viewmodel.DrillRunning {
			return fmt.Errorf("drill %s running: %+v", last.Drill.Name, last.Checks)
		}
		return nil
	})
	var pr problems
	pr.want(last.Result == viewmodel.DrillPassed, "drill %s = %s (%s): %+v", last.Drill.Name, last.Result, last.Reason, last.Checks)
	pr.want(last.BackupLabel != "", "the drill names no pgBackRest backup label")
	// The drill must have been promoted and read back over verified TLS (the
	// CA the drill's server certificate is signed by is the console's to trust).
	seen := map[viewmodel.DrillCheckName]viewmodel.DrillCheckOutcome{}
	for _, check := range last.Checks {
		seen[check.Name] = check.Outcome
	}
	pr.want(seen[viewmodel.DrillCheckPromoted] == viewmodel.DrillCheckPassed, "drill check %s = %q, want %s", viewmodel.DrillCheckPromoted, seen[viewmodel.DrillCheckPromoted], viewmodel.DrillCheckPassed)
	pr.want(seen[viewmodel.DrillCheckHealthy] == viewmodel.DrillCheckPassed, "drill check %s = %q, want %s", viewmodel.DrillCheckHealthy, seen[viewmodel.DrillCheckHealthy], viewmodel.DrillCheckPassed)
	t.Logf("drill %s of console %q: %s, backup %s, checks %v", last.Drill.Name, *consoleIn, last.Result, last.BackupLabel, seen)
	for _, check := range last.Checks {
		pr.want(check.Outcome == viewmodel.DrillCheckPassed || (check.Name == viewmodel.DrillCheckSize && check.Outcome == viewmodel.DrillCheckNotChecked),
			"drill check %s = %s (%s)", check.Name, check.Outcome, check.Text)
	}
	b, err := c.backups(t)
	if err != nil {
		t.Fatal(err)
	}
	if pg := b.Evidence.Repository; pg != nil && pg.PgBackRest != nil && len(pg.PgBackRest.Backups) > 0 {
		// The drill restores the newest backup of the agent's window.
		pr.want(last.BackupLabel == pg.PgBackRest.Backups[0].Label, "the drill restored %q, the agent's newest backup is %q", last.BackupLabel, pg.PgBackRest.Backups[0].Label)
	}
	if err := pr.err(); err != nil {
		t.Fatal(err)
	}
	// The teardown is the console's own: the drill Cluster is gone.
	poll(t, "the drill Cluster is deleted", func() error {
		list, err := dyn.Resource(schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}).
			Namespace(*namespace).List(context.Background(), metav1.ListOptions{LabelSelector: "pgtoolbox.fyannk.dev/restore-drill-of=" + *cluster})
		if err != nil {
			return err
		}
		if len(list.Items) != 0 {
			return fmt.Errorf("%d drill Cluster(s) left, first %s", len(list.Items), list.Items[0].GetName())
		}
		return nil
	})
	_ = p
}

// ---- the agent's own publication ----------------------------------------

// agent reads the evidence channel of one agent, through a port-forward.
type agent struct {
	base   string
	bearer string
	http   *http.Client
}

func connectAgent(t *testing.T) (*agent, func()) {
	t.Helper()
	kind, rest, _ := strings.Cut(*agentAuth, ":")
	switch kind {
	case "operator":
		// operator:<EvidenceAgent>:<ServiceAccount allowed to get it>. The
		// agent serves TLS with the CA the operator publishes in its status,
		// and takes a bound token of this namespace's audience.
		name, account, _ := strings.Cut(rest, ":")
		obj, err := dyn.Resource(schema.GroupVersionResource{Group: "pgtoolbox.fyannk.dev", Version: "v1alpha1", Resource: "evidenceagents"}).
			Namespace(*namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("EvidenceAgent %s: %v", name, err)
		}
		bundle, _, _ := unstructured.NestedString(obj.Object, "status", "caBundle")
		endpoint, _, _ := unstructured.NestedString(obj.Object, "status", "endpoint")
		u, err := url.Parse(endpoint)
		if err != nil || bundle == "" {
			t.Fatalf("EvidenceAgent %s has no usable status (endpoint %q, caBundle %d bytes)", name, endpoint, len(bundle))
		}
		pem, err := decodeBase64(bundle)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("status.caBundle holds no certificate")
		}
		audience, err := evidencev1alpha1.TokenAudience(*namespace)
		if err != nil {
			t.Fatal(err)
		}
		address, stop := forward(t, "app.kubernetes.io/component=evidence-agent,app.kubernetes.io/instance="+*agentIn, evidencev1alpha1.AgentPort)
		return &agent{base: "https://" + address, bearer: token(t, account, audience), http: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: u.Hostname(), MinVersion: tls.VersionTLS13},
		}}}, stop
	case "shared":
		// shared:<Secret>: the chart's agents take the shared token over plain
		// HTTP inside the namespace.
		secret, err := cs.CoreV1().Secrets(*namespace).Get(context.Background(), rest, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("token Secret %s: %v", rest, err)
		}
		address, stop := forward(t, "app.kubernetes.io/component=evidence-agent,app.kubernetes.io/instance="+*agentIn, evidencev1alpha1.AgentPort)
		return &agent{base: "http://" + address, bearer: strings.TrimSpace(string(secret.Data["token"])), http: &http.Client{Timeout: 20 * time.Second}}, stop
	}
	t.Fatalf("-agent-auth %q: want operator:<EvidenceAgent>:<ServiceAccount> or shared:<Secret>", *agentAuth)
	return nil, nil
}

// revision is the page query every collection route requires: the snapshot's
// revision, so that a page is of the generation the summary describes.
func revision(s evidencev1alpha1.RepositoryEvidenceSnapshot) string {
	return fmt.Sprintf("?revision=%d", s.Revision)
}

func (a *agent) get(path string, into any) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.bearer)
	response, err := a.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, evidencev1alpha1.MaximumSnapshotBytes+1))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %.200s", path, response.StatusCode, body)
	}
	return json.Unmarshal(body, into)
}

func pollAgent(t *testing.T, f func(*agent) error) {
	t.Helper()
	a, stop := connectAgent(t)
	defer stop()
	poll(t, fmt.Sprintf("agent %q: %s", *agentIn, *check), func() error { return f(a) })
}

// snapshot reads and validates the agent's snapshot and returns its pgBackRest
// summary.
func (a *agent) snapshot() (evidencev1alpha1.RepositoryEvidenceSnapshot, *evidencev1alpha1.PGBackRestSummary, problems) {
	var s evidencev1alpha1.RepositoryEvidenceSnapshot
	var pr problems
	if err := a.get(evidencev1alpha1.SnapshotPath, &s); err != nil {
		pr.want(false, "%v", err)
		return s, nil, pr
	}
	if err := s.Validate(); err != nil {
		pr.want(false, "the snapshot does not validate: %v", err)
	}
	repo := s.Identity.Repository
	pr.want(repo.Format == evidencev1alpha1.FormatPGBackRest && repo.Scope.Kind == evidencev1alpha1.ScopeKindStanza && repo.Scope.Name == *stanza,
		"identity = %s / scope %s %q, want pgbackrest / stanza %q", repo.Format, repo.Scope.Kind, repo.Scope.Name, *stanza)
	pr.want(s.Details.Type == evidencev1alpha1.PGBackRestDetailsType && s.Details.PGBackRest != nil, "details type = %q, want %s", s.Details.Type, evidencev1alpha1.PGBackRestDetailsType)
	return s, s.Details.PGBackRest, pr
}

func agentLive(a *agent, p plugin) error {
	s, pg, pr := a.snapshot()
	if pg == nil {
		return pr.err()
	}
	pr.want(s.State == evidencev1alpha1.Healthy && !s.Stale, "snapshot state = %s stale=%v (%s), want healthy", s.State, s.Stale, s.Reason.Message)
	pr.want(pg.Stanza == *stanza && pg.ArchiveStanza == *stanza && pg.Repository == 1, "stanza = %s/%s repo%d", pg.Stanza, pg.ArchiveStanza, pg.Repository)
	pr.want(pg.RepositoryFormat != nil && *pg.RepositoryFormat == 5 && pg.Encrypted != nil && !*pg.Encrypted, "format/encrypted = %v/%v", pg.RepositoryFormat, pg.Encrypted)
	pr.want(pg.Catalog.State == evidencev1alpha1.Healthy && pg.WAL.State == evidencev1alpha1.Healthy && pg.Timeline.State == evidencev1alpha1.Healthy && pg.Coverage.State == evidencev1alpha1.Healthy,
		"catalog/wal/timeline/coverage = %s/%s/%s/%s, want all healthy", pg.Catalog.State, pg.WAL.State, pg.Timeline.State, pg.Coverage.State)
	pr.want(pg.UsableBackups != nil && *pg.UsableBackups >= 2 && pg.BackupItems != nil && *pg.BackupItems >= 2, "backups = %v, usable %v, want at least 2", pg.BackupItems, pg.UsableBackups)
	w := pg.RecoveryWindow
	pr.want(w != nil && w.EndWAL != "" && w.EndReceiptAt != nil && w.UsableBackups >= 2, "recovery window = %+v", w)
	pr.want(pg.WALSegmentSizeBytes != nil && *pg.WALSegmentSizeBytes == 16<<20, "WAL segment size = %v", pg.WALSegmentSizeBytes)
	pr.want(pg.WALCounts != nil && pg.WALCounts.Segment >= 4, "WAL counts = %+v, want the switched segments", pg.WALCounts)

	var backupPage evidencev1alpha1.PGBackRestBackupPage
	if err := a.get(evidencev1alpha1.BackupsPath+revision(s), &backupPage); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(backupPage.Validate() == nil && len(backupPage.Items) >= 2, "backup page: %d items, validate %v", len(backupPage.Items), backupPage.Validate())
		var types []string
		for _, b := range backupPage.Items {
			types = append(types, b.Type)
			pr.want(b.Usable, "backup %s is not usable", b.Label)
			// Only Opera's plugin annotates the backup with the Backup object's name.
			pr.want(p.flavor != "opera" || b.CNPGBackupName != nil, "backup %s: cnpg_backup_name is absent", b.Label)
		}
		slices.Sort(types)
		pr.want(slices.Equal(types, []string{"diff", "full"}), "backup types = %v, want [diff full]", types)
	}
	var ranges evidencev1alpha1.PGBackRestWALRangePage
	if err := a.get(evidencev1alpha1.WALRangesPath+revision(s), &ranges); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(ranges.Validate() == nil && len(ranges.Items) >= 1, "WAL range page: %d items, validate %v", len(ranges.Items), ranges.Validate())
	}
	var gaps evidencev1alpha1.PGBackRestWALGapPage
	if err := a.get(evidencev1alpha1.WALGapsPath+revision(s), &gaps); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(gaps.Validate() == nil && len(gaps.Items) == 0, "WAL gap page: %d items, validate %v", len(gaps.Items), gaps.Validate())
	}
	var paths evidencev1alpha1.PGBackRestRecoveryPathPage
	if err := a.get(evidencev1alpha1.RecoveryPathsPath+revision(s), &paths); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(paths.Validate() == nil && len(paths.Items) >= 2, "recovery path page: %d items, validate %v", len(paths.Items), paths.Validate())
		for _, p := range paths.Items {
			pr.want(p.State == evidencev1alpha1.Healthy && p.Consistent && p.Stop == evidencev1alpha1.CoverageFrontier, "recovery path %s = %s consistent=%v stop=%s", p.Label, p.State, p.Consistent, p.Stop)
		}
	}
	return pr.err()
}

func agentGap(a *agent) error {
	s, pg, pr := a.snapshot()
	if pg == nil {
		return pr.err()
	}
	pr.want(s.State == evidencev1alpha1.Unhealthy, "snapshot state = %s (%s), want unhealthy", s.State, s.Reason.Message)
	pr.want(pg.WAL.State == evidencev1alpha1.Unhealthy && pg.Coverage.State == evidencev1alpha1.Unhealthy, "wal/coverage = %s/%s, want unhealthy", pg.WAL.State, pg.Coverage.State)
	pr.want(slices.ContainsFunc(pg.Findings, func(f evidencev1alpha1.PGBackRestFinding) bool {
		return f.Code == evidencev1alpha1.FindingWALGap && f.Severity == evidencev1alpha1.FindingError
	}), "no WALGap error finding among %+v", pg.Findings)
	var gaps evidencev1alpha1.PGBackRestWALGapPage
	if err := a.get(evidencev1alpha1.WALGapsPath+revision(s), &gaps); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(gaps.Validate() == nil && slices.ContainsFunc(gaps.Items, func(g evidencev1alpha1.PGBackRestWALGap) bool { return g.Status == evidencev1alpha1.GapConfirmed }),
			"WAL gap page has no confirmed gap: %+v", gaps.Items)
	}
	var paths evidencev1alpha1.PGBackRestRecoveryPathPage
	if err := a.get(evidencev1alpha1.RecoveryPathsPath+revision(s), &paths); err != nil {
		pr.want(false, "%v", err)
	} else {
		pr.want(slices.ContainsFunc(paths.Items, func(p evidencev1alpha1.PGBackRestRecoveryPath) bool {
			return p.Stop == evidencev1alpha1.CoverageGapLimited
		}),
			"no recovery path stops at the gap: %+v", paths.Items)
	}
	return pr.err()
}

func decodeBase64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
