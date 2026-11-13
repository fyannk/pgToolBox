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

package chart

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestTeamConsoles is the isolation of team consoles, console half: two team
// consoles in two namespaces, each holding no cluster-scoped read permission
// except SubjectAccessReview creation and the opt-in ClusterImageCatalog read.
func TestTeamConsoles(t *testing.T) {
	dumpOnFailure(t, "team-a", "team-b", "team-c")

	for _, ns := range []string{"team-a", "team-b", "team-c"} {
		namespace(t, ns, nil)
	}
	ca, err := os.ReadFile("../testdata/instance-ca.crt")
	if err != nil {
		t.Fatal(err)
	}

	// Team A: the defaults. Team B: two namespaces it owns, and the opt-in read of
	// ClusterImageCatalogs. Team C has no console of its own: it is the second
	// namespace of B and must stay invisible to A.
	// Console A's public host is a loopback name: the MCP Go SDK's OAuth
	// client accepts plain-HTTP metadata only from a loopback host, and the
	// OAuth scenario below runs against A. The test still dials the NodePort.
	a := console{
		Release: "pgc-a", Namespace: "team-a", SSO: "localhost:18080", MCP: true, MCPWrite: true,
		Env: map[string]string{"MCP_OAUTH_ACCESS_TTL": oauthAccessTTL.String()},
	}
	b := console{Release: "pgc-b", Namespace: "team-b", Watch: []string{"team-b", "team-c"}, CatalogOpt: true, SSO: "pgc-b.e2e.test", MCP: true}
	consoles := []console{a, b}

	// Dex first: a console discovers its issuer at start. Three people: dba,
	// ops, and stranger, who is bound to nothing.
	dex := startDex(t, []string{a.SSO, b.SSO}, "dba", "ops", "stranger")
	for _, c := range consoles {
		dex.forNamespace(t, c.Namespace)
	}

	// The CA bundle is the installing admin's job: the chart mounts
	// it. Team A makes one; team B does not, which is the "missing CA" case: its
	// instances must read "not verifiable", and the console must still start.
	for _, c := range consoles[:1] {
		_, err := clientset.CoreV1().ConfigMaps(c.Namespace).Create(ctx(), &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console-instance-ca", Namespace: c.Namespace},
			Data:       map[string]string{"ca.crt": string(ca)},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create the CA bundle in %s: %v", c.Namespace, err)
		}
	}

	// Install both releases at once: two kubeadm-sized waits would be the whole job.
	var wg sync.WaitGroup
	errs := make([]error, len(consoles))
	for i, c := range consoles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			args := []string{
				"upgrade", "--install", c.Release, *chartPath, "--namespace", c.Namespace,
				"--values", c.valuesFile(t), "--wait", "--timeout", installWait,
			}
			if out, err := helmOutput(args...); err != nil {
				errs[i] = fmt.Errorf("helm install %s: %w\n%s", c.Release, err, out)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("each console is ready, hardened and available", func(t *testing.T) {
		for _, c := range consoles {
			assertConsoleRunning(t, c)
		}
	})

	t.Run("the revocation list and the CA bundle are wired", func(t *testing.T) {
		for _, c := range consoles {
			cm, err := clientset.CoreV1().ConfigMaps(c.Namespace).Get(ctx(), c.revocations(), metav1.GetOptions{})
			if err != nil {
				t.Fatalf("%s: revocation ConfigMap: %v", c.Release, err)
			}
			if len(cm.Data) != 0 {
				t.Errorf("%s: the revocation list must start empty, got %v", c.Release, cm.Data)
			}
			pod := consolePods(t, c)[0]
			env := envOf(&pod.Spec.Containers[0])
			if env["INSTANCE_STATUS_CA_FILE"] != "/var/run/console/instance-ca/ca.crt" {
				t.Errorf("%s: INSTANCE_STATUS_CA_FILE = %q", c.Release, env["INSTANCE_STATUS_CA_FILE"])
			}
			if env["REVOCATION_CONFIGMAP"] != c.revocations() {
				t.Errorf("%s: REVOCATION_CONFIGMAP = %q, want %q", c.Release, env["REVOCATION_CONFIGMAP"], c.revocations())
			}
			if want := strings.Join(c.watched(), ","); env["WATCH_NAMESPACES"] != want {
				t.Errorf("%s: WATCH_NAMESPACES = %q, want %q", c.Release, env["WATCH_NAMESPACES"], want)
			}
			// A mounted directory, so that the kubelet refreshes the bundle without a restart.
			for _, m := range pod.Spec.Containers[0].VolumeMounts {
				if m.Name == "instance-ca" && (m.SubPath != "" || !m.ReadOnly) {
					t.Errorf("%s: the CA bundle mount must be a read-only directory mount, got %+v", c.Release, m)
				}
			}
		}
	})

	t.Run("no cluster-scoped read, and nothing outside the watched namespaces", func(t *testing.T) {
		resources := discover(t)
		t.Logf("API discovery: %d resources and subresources", len(resources))
		// What each console must not reach: the namespaces of the other team, and
		// the platform's own.
		unwatched := func(c console) []string {
			var out []string
			for _, ns := range []string{"team-a", "team-b", "team-c", "kube-system", "default"} {
				if !contains(c.watched(), ns) {
					out = append(out, ns)
				}
			}
			return out
		}
		for _, c := range consoles {
			start := time.Now()
			c.proveScope(t, resources, unwatched(c))
			t.Logf("%s proven in %s", c.Release, time.Since(start).Round(time.Second))
		}
	})

	t.Run("the ClusterRoles are exactly the documented ones", func(t *testing.T) {
		for _, c := range consoles {
			roles, err := clientset.RbacV1().ClusterRoles().List(ctx(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + c.Release + ",app.kubernetes.io/name=pgtoolbox-console"})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, r := range roles.Items {
				names = append(names, r.Name)
			}
			// The console asks with SelfSubjectAccessReviews, which need no grant:
			// its only ClusterRole is the opt-in ClusterImageCatalog read.
			var want []string
			if c.CatalogOpt {
				want = append(want, c.clusterRole()+":clusterimagecatalogs")
			}
			if strings.Join(sortedStrings(names), ",") != strings.Join(sortedStrings(want), ",") {
				t.Errorf("%s ClusterRoles = %v, want %v", c.Release, names, want)
			}
			bindings, err := clientset.RbacV1().ClusterRoleBindings().List(ctx(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/instance=" + c.Release + ",app.kubernetes.io/name=pgtoolbox-console"})
			if err != nil {
				t.Fatal(err)
			}
			if len(bindings.Items) != len(want) {
				t.Errorf("%s has %d ClusterRoleBindings, want %d", c.Release, len(bindings.Items), len(want))
			}
			for _, b := range bindings.Items {
				if len(b.Subjects) != 1 || b.Subjects[0].Kind != "ServiceAccount" || b.Subjects[0].Name != c.serviceAccount() || b.Subjects[0].Namespace != c.Namespace {
					t.Errorf("%s binds %+v; only the console ServiceAccount may be bound", b.Name, b.Subjects)
				}
			}
		}
	})

	httpA, probeA := exposeConsole(t, a)
	httpB, _ := exposeConsole(t, b)
	node := nodeAddr(t)
	hosts := map[string]string{
		hostPort(a.SSO):                   fmt.Sprintf("%s:%d", node, httpA),
		hostPort(b.SSO):                   fmt.Sprintf("%s:%d", node, httpB),
		os.Getenv("E2E_DEX_IP") + ":5556": fmt.Sprintf("%s:%d", node, dex.NodePort),
		// One replica serves MCP: its paths go to the leader's Service.
		leaderRoute(hostPort(a.SSO)): fmt.Sprintf("%s:%d", node, exposeMCPLeader(t, a)),
		leaderRoute(hostPort(b.SSO)): fmt.Sprintf("%s:%d", node, exposeMCPLeader(t, b)),
	}
	baseA, baseB := "http://"+a.SSO, "http://"+b.SSO
	sso := func(base string, who string) *browser {
		br := newBrowser(t, hosts, dex.CA)
		br.signInSSO(base, dex.Users[who])
		return br
	}
	seedClusters(t)

	t.Run("SSO: a DBA sees exactly the clusters they may get", func(t *testing.T) {
		want := []struct {
			base, who string
			clusters  []string
		}{
			{baseA, "dba", []string{"team-a/orders"}},
			{baseA, "ops", []string{}},
			{baseA, "stranger", []string{}},
			{baseB, "dba", []string{"team-b/billing"}},
			{baseB, "ops", []string{"team-b/audit", "team-b/billing", "team-c/ledger"}},
			{baseB, "stranger", []string{}},
		}
		for _, w := range want {
			br := sso(w.base, w.who)
			sess, st := br.session(w.base)
			if st != 200 || sess.Provider != "oidc" || sess.Username != dex.Users[w.who].username() {
				t.Fatalf("%s on %s: session %d %+v, want oidc as %s", w.who, w.base, st, sess, dex.Users[w.who].username())
			}
			got, st := br.clusterNames(w.base)
			if st != 200 || strings.Join(got, ",") != strings.Join(w.clusters, ",") {
				t.Errorf("%s on %s sees %v (HTTP %d), want exactly %v", w.who, w.base, got, st, w.clusters)
			}
		}
	})

	t.Run("MCP: an agent with a person's token sees exactly what the person sees", func(t *testing.T) {
		want := []struct {
			base, who string
			clusters  []string
		}{
			{baseA, "dba", []string{"team-a/orders"}},
			{baseA, "ops", []string{}},
			{baseA, "stranger", []string{}},
			{baseB, "dba", []string{"team-b/billing"}},
			{baseB, "ops", []string{"team-b/audit", "team-b/billing", "team-c/ledger"}},
			{baseB, "stranger", []string{}},
		}
		tokens := map[string]string{}
		for _, w := range want {
			if tokens[w.who] == "" {
				tokens[w.who] = dex.passwordToken(t, hosts, w.who)
			}
			got := mcpClusterNames(t, hosts, w.base, tokens[w.who])
			if strings.Join(got, ",") != strings.Join(w.clusters, ",") {
				t.Errorf("MCP: %s on %s sees %v, want exactly %v", w.who, w.base, got, w.clusters)
			}
		}
		// A Cluster dba may not get is a tool error, not data.
		mcpForbidden(t, hosts, baseB, tokens["dba"], "team-c", "ledger")
		// Without a token: the RFC 9728 challenge, and its metadata.
		mcpChallenge(t, hosts, baseA)
	})

	t.Run("MCP: an assistant signs in through the console (OAuth)", func(t *testing.T) {
		mcpOAuthScenario(t, a, dex, hosts, baseA, map[string][]string{
			"dba": {"team-a/orders"}, "ops": {}, "stranger": {},
		})
	})

	t.Run("MCP: a write tool runs only confirmed, and only for who may", func(t *testing.T) {
		mcpWriteScenario(t, dex, hosts, baseA, baseB)
	})

	t.Run("a Cluster the caller may not get is a 403, existing or not", func(t *testing.T) {
		dba := sso(baseB, "dba")
		for _, path := range []string{
			"/api/v1/namespaces/team-b/clusters/audit",
			"/api/v1/namespaces/team-b/clusters/audit/instances",
			"/api/v1/namespaces/team-c/clusters/ledger",
			"/api/v1/namespaces/team-b/clusters/ghost",
		} {
			if r := dba.get(baseB + path); r.Status != http.StatusForbidden {
				t.Errorf("dba GET %s = %d, want 403 (%s)", path, r.Status, tailBytes(r.Body))
			}
		}
		if r := dba.get(baseB + "/api/v1/namespaces/team-b/clusters/billing"); r.Status != http.StatusOK {
			t.Errorf("dba GET billing = %d, want 200: the resourceNames grant must be honoured", r.Status)
		}
		// team-b is not watched by console A: nothing is served, not even to
		// someone who may get the object.
		dbaA := sso(baseA, "dba")
		if r := dbaA.get(baseA + "/api/v1/namespaces/team-b/clusters/billing"); r.Status == http.StatusOK {
			t.Errorf("console A served a Cluster of a namespace it does not watch")
		}
		// An unsafe request without the CSRF header is refused whatever the session.
		r := dba.do(http.MethodPost, baseB+"/api/v1/session", nil, map[string]string{"Origin": origin(baseB)})
		if r.Status != http.StatusForbidden {
			t.Errorf("POST without %s = %d, want 403", "X-PgToolBox-Request", r.Status)
		}
	})

	t.Run("the overview is honest about what it cannot see", func(t *testing.T) {
		ops := sso(baseB, "ops")
		// Team B made no CA bundle: the instance's status port cannot be verified,
		// and the console says so instead of reading it unverified or showing green.
		var inst struct {
			Items []struct {
				Name       string `json:"name"`
				Health     string `json:"health"`
				StatusRead struct {
					State  string `json:"state"`
					Reason string `json:"reason"`
				} `json:"statusRead"`
			} `json:"items"`
		}
		eventually(t, 90*time.Second, "billing-1 to read notVerifiable", func() error {
			r := ops.get(baseB + "/api/v1/namespaces/team-b/clusters/billing/instances")
			if r.Status != 200 {
				return fmt.Errorf("HTTP %d: %s", r.Status, tailBytes(r.Body))
			}
			inst.Items = nil
			if err := json.Unmarshal(r.Body, &inst); err != nil {
				return err
			}
			if len(inst.Items) != 1 {
				return fmt.Errorf("%d instance rows", len(inst.Items))
			}
			if got := inst.Items[0].StatusRead.State; got != "notVerifiable" {
				return fmt.Errorf("statusRead = %q (%s)", got, inst.Items[0].StatusRead.Reason)
			}
			return nil
		})
		if row := inst.Items[0]; row.Health == "ok" || row.StatusRead.Reason == "" {
			t.Errorf("an unverifiable instance reads health %q with reason %q", row.Health, row.StatusRead.Reason)
		}
		overview := func(base, ns, name string, br *browser) map[string]any {
			r := br.get(base + "/api/v1/namespaces/" + ns + "/clusters/" + name)
			if r.Status != 200 {
				t.Fatalf("overview of %s/%s = %d: %s", ns, name, r.Status, tailBytes(r.Body))
			}
			var v map[string]any
			if err := json.Unmarshal(r.Body, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
		billing := overview(baseB, "team-b", "billing", ops)
		if spots, _ := billing["blindSpots"].([]any); len(spots) == 0 {
			t.Errorf("the overview of billing lists no blind spot: the instance does not report volume usage, disk or replication fully")
		}
		if sources, _ := billing["sources"].([]any); len(sources) == 0 {
			t.Errorf("the overview of billing names no source")
		}
		if verdict, _ := billing["verdict"].(map[string]any); verdict == nil || verdict["health"] == "ok" {
			t.Errorf("billing verdict %v: a cluster whose only instance cannot be read must not be green", verdict)
		}
		ledger := overview(baseB, "team-c", "ledger", ops)
		if fencing, _ := ledger["fencing"].(map[string]any); fencing["state"] != "all" {
			t.Errorf("ledger carries cnpg.io/fencedInstances=[\"*\"]; fencing = %v", fencing)
		}
		// Team A has a bundle; its Cluster has no pods yet, and says that rather
		// than inventing instances.
		orders := overview(baseA, "team-a", "orders", sso(baseA, "dba"))
		if present, _ := orders["present"].(bool); !present {
			t.Errorf("orders is not present: %v", orders["ref"])
		}
	})

	t.Run("the break-glass token form works beside SSO", func(t *testing.T) {
		token := saToken(t, "breakglass")
		br := newBrowser(t, hosts, nil)
		if r := br.signInToken(baseA, token); r.Status != http.StatusFound {
			t.Fatalf("token sign-in = %d: %s", r.Status, tailBytes(r.Body))
		}
		sess, st := br.session(baseA)
		if st != 200 || sess.Provider != "token" || sess.Username != "system:serviceaccount:team-a:breakglass" {
			t.Fatalf("session %d %+v", st, sess)
		}
		if got, _ := br.clusterNames(baseA); strings.Join(got, ",") != "team-a/orders" {
			t.Errorf("breakglass sees %v", got)
		}
		// A token the API server does not accept never makes a session.
		if r := br.signInToken(baseA, token+"x"); r.Status == http.StatusFound {
			t.Errorf("a corrupted token signed in")
		}
	})

	t.Run("/metrics asks the API server who the caller is", func(t *testing.T) {
		probe := fmt.Sprintf("http://%s:%d/metrics", node, probeA)
		plain := newBrowser(t, nil, nil)
		get := func(token string) int {
			h := map[string]string{}
			if token != "" {
				h["Authorization"] = "Bearer " + token
			}
			return plain.do(http.MethodGet, probe, nil, h).Status
		}
		if st := get(""); st != http.StatusUnauthorized {
			t.Errorf("/metrics without a token = %d, want 401", st)
		}
		if st := get(saToken(t, "breakglass")); st != http.StatusForbidden {
			t.Errorf("/metrics for a user who may not get it = %d, want 403", st)
		}
		if st := get(saToken(t, "scraper")); st != http.StatusOK {
			t.Errorf("/metrics for the scraper = %d, want 200", st)
		}
	})

	t.Run("the revocation ConfigMap ends sessions on every replica", func(t *testing.T) {
		cm := a.revocations()
		set := func(data map[string]string) {
			t.Helper()
			cur, err := clientset.CoreV1().ConfigMaps(a.Namespace).Get(ctx(), cm, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			cur.Data = data
			if _, err := clientset.CoreV1().ConfigMaps(a.Namespace).Update(ctx(), cur, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { set(nil) })
		set(nil)

		s1, s2 := sso(baseA, "dba"), sso(baseA, "dba")
		id1 := mustSession(t, s1, baseA)
		c1, c2 := s1.cookieHeader(baseA), s2.cookieHeader(baseA)
		s1.allReplicasAnswer(t, baseA, c1, 200, "session 1 alive on every replica")

		// sessions: one named session dies, the user's other session does not.
		set(map[string]string{"sessions": id1 + "\n"})
		s1.allReplicasAnswer(t, baseA, c1, 401, "the named session to die everywhere")
		s2.allReplicasAnswer(t, baseA, c2, 200, "the user's other session to live on")

		// users: everything the user signed in before the instant dies; a later
		// sign-in lives.
		time.Sleep(1100 * time.Millisecond)
		set(map[string]string{"users": dex.Users["dba"].username() + " " + time.Now().UTC().Format(time.RFC3339) + "\n"})
		s2.allReplicasAnswer(t, baseA, c2, 401, "the user's sessions to die everywhere")
		s3 := sso(baseA, "dba")
		c3 := s3.cookieHeader(baseA)
		s3.allReplicasAnswer(t, baseA, c3, 200, "a sign-in after the revocation to work")

		// notBefore: everybody signed in before the instant dies.
		s4 := sso(baseA, "ops")
		c4 := s4.cookieHeader(baseA)
		time.Sleep(1100 * time.Millisecond)
		set(map[string]string{"notBefore": time.Now().UTC().Format(time.RFC3339)})
		s3.allReplicasAnswer(t, baseA, c3, 401, "notBefore to end dba's session")
		s4.allReplicasAnswer(t, baseA, c4, 401, "notBefore to end ops' session")
		s5 := sso(baseA, "ops")
		s5.allReplicasAnswer(t, baseA, s5.cookieHeader(baseA), 200, "a sign-in after notBefore to work")

		// A list nobody can parse must not revoke everybody or nobody silently:
		// the last good list stays in force and the console stays Ready.
		set(map[string]string{"notBefore": "yesterday"})
		time.Sleep(5 * time.Second)
		s5.allReplicasAnswer(t, baseA, s5.cookieHeader(baseA), 200, "a malformed list to leave the last good one in force")
		for _, pod := range consolePods(t, a) {
			if err := podReady(pod); err != nil {
				t.Errorf("a malformed revocation list made the console unready: %v", err)
			}
		}
		set(nil)
	})

	t.Run("sign-out ends the session", func(t *testing.T) {
		br := sso(baseA, "dba")
		id := mustSession(t, br, baseA)
		cookie := br.cookieHeader(baseA)
		r := br.do(http.MethodPost, baseA+"/logout", nil, map[string]string{"Origin": origin(baseA)})
		if r.Status != http.StatusOK {
			t.Fatalf("POST /logout = %d: %s", r.Status, tailBytes(r.Body))
		}
		if _, st := br.session(baseA); st != http.StatusUnauthorized {
			t.Errorf("session after sign-out = %d, want 401", st)
		}
		// A copy of the cookie dies at once on the replica that handled the
		// sign-out and on the others once the session is on the revocation list
		// (the documented gap: sign-out itself is not shared between replicas).
		cur, err := clientset.CoreV1().ConfigMaps(a.Namespace).Get(ctx(), a.revocations(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cur.Data = map[string]string{"sessions": id + "\n"}
		if _, err := clientset.CoreV1().ConfigMaps(a.Namespace).Update(ctx(), cur, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		br.allReplicasAnswer(t, baseA, cookie, 401, "a copy of the signed-out cookie to be refused everywhere")
		cur.Data = nil
		_, _ = clientset.CoreV1().ConfigMaps(a.Namespace).Update(ctx(), cur, metav1.UpdateOptions{})
	})

	t.Run("persistent history runs a StatefulSet with one claim per replica", func(t *testing.T) {
		// On a real API server: the history file takes an exclusive
		// lock, so each replica gets its own claim and the console keeps its
		// replicas, rolling update and PodDisruptionBudget.
		a.install(t, "--set", "console.history.persistent=true", "--set", "console.history.size=64Mi")
		sts, err := clientset.AppsV1().StatefulSets(a.Namespace).Get(ctx(), a.fullname(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		replicas := int(*sts.Spec.Replicas)
		if replicas < 2 {
			t.Errorf("replicas = %d, want the configured 2", replicas)
		}
		if sts.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
			t.Errorf("update strategy = %s, want RollingUpdate", sts.Spec.UpdateStrategy.Type)
		}
		eventually(t, 3*time.Minute, "every replica Ready once the Deployment's pods are gone", func() error {
			pods := consolePodsNoFail(a)
			if len(pods) != replicas {
				return fmt.Errorf("%d console pods, want %d", len(pods), replicas)
			}
			for _, p := range pods {
				if err := podReady(p); err != nil {
					return err
				}
			}
			return nil
		})
		for i := range replicas {
			name := fmt.Sprintf("history-%s-%d", a.fullname(), i)
			pvc, err := clientset.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx(), name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if pvc.Status.Phase != corev1.ClaimBound {
				t.Errorf("history claim %s is %s, want Bound", name, pvc.Status.Phase)
			}
		}
		if _, err := clientset.AppsV1().Deployments(a.Namespace).Get(ctx(), a.fullname(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("the Deployment must be gone once the StatefulSet replaces it (err=%v)", err)
		}
		if _, err := clientset.PolicyV1().PodDisruptionBudgets(a.Namespace).Get(ctx(), a.fullname(), metav1.GetOptions{}); err != nil {
			t.Errorf("a multi-replica console keeps its PodDisruptionBudget: %v", err)
		}
	})

	t.Run("uninstall leaves nothing cluster-scoped behind", func(t *testing.T) {
		for _, c := range consoles {
			run(t, "helm", "uninstall", c.Release, "--namespace", c.Namespace, "--wait")
			for _, name := range []string{c.clusterRole(), c.clusterRole() + ":clusterimagecatalogs"} {
				if _, err := clientset.RbacV1().ClusterRoles().Get(ctx(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Errorf("ClusterRole %s outlived %s (err=%v)", name, c.Release, err)
				}
				if _, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Errorf("ClusterRoleBinding %s outlived %s (err=%v)", name, c.Release, err)
				}
			}
			for _, ns := range c.watched() {
				if _, err := clientset.RbacV1().Roles(ns).Get(ctx(), c.fullname(), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Errorf("Role %s/%s outlived %s (err=%v)", ns, c.fullname(), c.Release, err)
				}
			}
		}
	})
}

// hostPort is the dial address of a public host: port 80 unless it names one.
func hostPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return host + ":80"
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func consolePodsNoFail(c console) []corev1.Pod {
	list, err := clientset.CoreV1().Pods(c.Namespace).List(ctx(), metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=pgtoolbox-console,app.kubernetes.io/instance=" + c.Release,
	})
	if err != nil {
		return nil
	}
	var out []corev1.Pod
	for _, p := range list.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

func consolePods(t *testing.T, c console) []corev1.Pod {
	t.Helper()
	pods := consolePodsNoFail(c)
	if len(pods) == 0 {
		t.Fatalf("%s: no console pods", c.Release)
	}
	return pods
}

func podReady(p corev1.Pod) error {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			if cond.Status == corev1.ConditionTrue {
				return nil
			}
			return fmt.Errorf("pod %s is not Ready: %s %s", p.Name, cond.Reason, cond.Message)
		}
	}
	return fmt.Errorf("pod %s has no Ready condition", p.Name)
}

func envOf(c *corev1.Container) map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		out[e.Name] = e.Value
	}
	return out
}

// assertConsoleRunning: two Ready replicas rolling, a PodDisruptionBudget, and
// Pods that meet Pod Security "restricted" without a fixed UID, with no API token
// automounted and no Secret reachable through the environment.
func assertConsoleRunning(t *testing.T, c console) {
	t.Helper()
	dep, err := clientset.AppsV1().Deployments(c.Namespace).Get(ctx(), c.fullname(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("%s: %v", c.Release, err)
	}
	if got := *dep.Spec.Replicas; got != 2 || dep.Status.ReadyReplicas != 2 {
		t.Errorf("%s: replicas %d, ready %d; the default is two", c.Release, got, dep.Status.ReadyReplicas)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("%s: strategy %s, want RollingUpdate", c.Release, dep.Spec.Strategy.Type)
	}
	if ru := dep.Spec.Strategy.RollingUpdate; ru == nil || ru.MaxUnavailable == nil || *ru.MaxUnavailable != intstr.FromInt32(0) {
		t.Errorf("%s: a rolling update must never take a replica away first: %+v", c.Release, ru)
	}
	eventually(t, time.Minute, c.Release+" PodDisruptionBudget to see both pods", func() error {
		pdb, err := clientset.PolicyV1().PodDisruptionBudgets(c.Namespace).Get(ctx(), c.fullname(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pdb.Status.CurrentHealthy < 2 {
			return fmt.Errorf("%d healthy pods", pdb.Status.CurrentHealthy)
		}
		return nil
	})

	pods := consolePods(t, c)
	if len(pods) != 2 {
		t.Errorf("%s: %d pods, want 2", c.Release, len(pods))
	}
	nodes := map[string]bool{}
	for _, pod := range pods {
		nodes[pod.Spec.NodeName] = true
		if err := podReady(pod); err != nil {
			t.Errorf("%s: %v", c.Release, err)
		}
		if pod.Spec.ServiceAccountName != c.serviceAccount() {
			t.Errorf("%s: runs as %s, want %s", pod.Name, pod.Spec.ServiceAccountName, c.serviceAccount())
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			t.Errorf("%s: the API token must not be automounted pod-wide", pod.Name)
		}
		sc := pod.Spec.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.SeccompProfile == nil || sc.RunAsUser != nil {
			t.Errorf("%s: pod securityContext %+v is not restricted-without-a-fixed-UID", pod.Name, sc)
		}
		ctr := pod.Spec.Containers[0]
		if csc := ctr.SecurityContext; csc == nil || csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation ||
			csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem || csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s: container securityContext %+v is not restricted", pod.Name, csc)
		}
		for _, e := range ctr.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				t.Errorf("%s: env %s reads a Secret; secrets are mounted files", pod.Name, e.Name)
			}
		}
		if len(pod.Spec.Containers) != 1 {
			t.Errorf("%s: %d containers; the console is alone in its Pod", pod.Name, len(pod.Spec.Containers))
		}
	}
	// A single-node kind cluster cannot spread: only require that the preference exists.
	if dep.Spec.Template.Spec.Affinity == nil || dep.Spec.Template.Spec.Affinity.PodAntiAffinity == nil {
		t.Errorf("%s: two replicas must prefer different nodes", c.Release)
	}
	_ = nodes
}

// mustSession returns the session ID of a signed-in browser.
func mustSession(t *testing.T, br *browser, base string) string {
	t.Helper()
	v, st := br.session(base)
	if st != http.StatusOK {
		t.Fatalf("session = %d", st)
	}
	return v.SessionID
}
