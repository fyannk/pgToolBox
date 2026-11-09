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
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestNetworkPolicy is the policy half of the team-console isolation: on a CNI that
// enforces NetworkPolicy, a console installed with networkPolicy.enabled still
// works and can no longer reach the other namespace's instance pods or evidence
// agent. NetworkPolicy is the installing admin's choice, so this is
// an additional check, run on its own cluster with Calico.
//
// The console image is distroless, so the connections are made from an ephemeral
// container added to the console Pod itself: it shares the Pod's network
// namespace and identity, so it is subject to exactly the policy the console is.
// Before the policy is enabled the same probes must all connect, which is what
// makes "blocked" afterwards a statement about the policy and not about the test.
func TestNetworkPolicy(t *testing.T) {
	dumpOnFailure(t, "team-a", "team-b", "router-sim")

	for _, ns := range []string{"team-a", "team-b", "router-sim"} {
		namespace(t, ns, nil)
	}
	ca, err := os.ReadFile("../testdata/instance-ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().ConfigMaps("team-a").Create(ctx(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pgtoolbox-console-instance-ca", Namespace: "team-a"},
		Data:       map[string]string{"ca.crt": string(ca)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Stand-ins for what the console talks to. They carry the labels CloudNativePG
	// and the EvidenceAgent put on their pods, and listen on the ports the policy
	// names, plus one it does not.
	instA := standIn(t, "team-a", "inst-a", map[string]string{"cnpg.io/podRole": "instance", "cnpg.io/cluster": "orders"}, 8000, 9187, 8080)
	poolerA := standIn(t, "team-a", "pooler-a", map[string]string{"cnpg.io/podRole": "pooler"}, 9127)
	evidenceA := standIn(t, "team-a", "evidence-a", map[string]string{"app.kubernetes.io/name": "evidence-agent"}, 8443)
	otherA := standIn(t, "team-a", "other-a", map[string]string{"app": "something-else"}, 8000)
	instB := standIn(t, "team-b", "inst-b", map[string]string{"cnpg.io/podRole": "instance", "cnpg.io/cluster": "billing"}, 8000, 9187)
	evidenceB := standIn(t, "team-b", "evidence-b", map[string]string{"app.kubernetes.io/name": "evidence-agent"}, 8443)

	// The API server's own EndpointSlice, which the chart looks up.
	api, err := clientset.DiscoveryV1().EndpointSlices("default").Get(ctx(), "kubernetes", metav1.GetOptions{})
	if err != nil || len(api.Endpoints) == 0 || len(api.Endpoints[0].Addresses) == 0 || len(api.Ports) == 0 || api.Ports[0].Port == nil {
		t.Fatalf("the kubernetes EndpointSlice: %v", err)
	}
	apiSvc, err := clientset.CoreV1().Services("default").Get(ctx(), "kubernetes", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	targets := []target{
		{"team-a instance :8000 (status)", instA, 8000, true},
		{"team-a instance :9187 (metrics)", instA, 9187, true},
		{"team-a pooler :9127", poolerA, 9127, true},
		{"team-a evidence agent :8443", evidenceA, 8443, true},
		{"API server (Service)", apiSvc.Spec.ClusterIP, int(apiSvc.Spec.Ports[0].Port), true},
		{"API server (endpoint)", api.Endpoints[0].Addresses[0], int(*api.Ports[0].Port), true},
		{"DNS", "", 0, true},
		{"team-a instance :8080 (a port the policy does not name)", instA, 8080, false},
		{"team-a other pod :8000 (not an instance)", otherA, 8000, false},
		{"team-b instance :8000 (status)", instB, 8000, false},
		{"team-b instance :9187 (metrics)", instB, 9187, false},
		{"team-b evidence agent :8443", evidenceB, 8443, false},
	}

	// The router the policy admits is a namespace; Ingress objects need no controller.
	a := console{Release: "pgc-a", Namespace: "team-a", Extra: []string{
		"--set", "console.exposure.type=ingress",
		"--set", "console.exposure.host=pgc-a.example.test",
		"--set", "console.exposure.routerNamespace=router-sim",
	}}

	t.Run("without the policy everything is reachable", func(t *testing.T) {
		a.install(t)
		pod := consolePods(t, a)[0]
		p := startProber(t, pod, targets)
		eventually(t, 90*time.Second, "every probe connecting before any policy exists", func() error {
			return p.expect(targets, func(target) bool { return true })
		})
		for _, pr := range ingressProbes(t, a) {
			if got := pr.run(t); !got.open {
				t.Errorf("before the policy, %s should connect, got: %s", pr.name, got.detail)
			}
		}
	})

	t.Run("with networkPolicy.enabled only what the console needs is reachable", func(t *testing.T) {
		// The API server addresses come from the `kubernetes` EndpointSlice at install
		// time: no addresses are given, so the chart's lookup is exercised for real.
		a.install(t, "--set", "console.networkPolicy.enabled=true")
		np, err := clientset.NetworkingV1().NetworkPolicies("team-a").Get(ctx(), a.fullname(), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("the chart rendered no NetworkPolicy: %v", err)
		}
		if got := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/instance"]; got != "pgc-a" {
			t.Errorf("policy selects %v", np.Spec.PodSelector.MatchLabels)
		}
		var cidrs []string
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.IPBlock != nil {
					cidrs = append(cidrs, to.IPBlock.CIDR)
				}
			}
		}
		if want := api.Endpoints[0].Addresses[0] + "/32"; len(cidrs) != 1 || cidrs[0] != want {
			t.Errorf("API-server egress = %v, want [%s] (looked up from the kubernetes EndpointSlice)", cidrs, want)
		}

		pod := consolePods(t, a)[0]
		p := startProber(t, pod, targets)
		// Calico programs the rule asynchronously: wait for the blocked ones to
		// close, then require the allowed ones to have stayed open.
		eventually(t, 120*time.Second, "the policy to take effect on the console Pod", func() error {
			return p.expect(targets, func(tg target) bool { return tg.allowed })
		})
		for _, tg := range targets {
			t.Logf("%-58s %s", tg.name, map[bool]string{true: "reachable", false: "blocked"}[tg.allowed])
		}

		// Ingress: only the router namespace, only the console port.
		eventually(t, 60*time.Second, "ingress to follow the policy", func() error {
			var bad []string
			for _, pr := range ingressProbes(t, a) {
				if got := pr.run(t); got.open != pr.allowed {
					bad = append(bad, fmt.Sprintf("%s: open=%v want %v (%s)", pr.name, got.open, pr.allowed, got.detail))
				}
			}
			if len(bad) > 0 {
				return fmt.Errorf("%s", strings.Join(bad, "; "))
			}
			return nil
		})
	})

	t.Run("the console still works under the policy", func(t *testing.T) {
		// A restart under the policy: the new Pods must reach the API server and
		// answer their readiness probe (which asks the API server, so a Ready Pod
		// is one whose egress works), then serve a request from the router namespace.
		run(t, "kubectl", "-n", "team-a", "rollout", "restart", "deployment/"+a.fullname())
		run(t, "kubectl", "-n", "team-a", "rollout", "status", "deployment/"+a.fullname(), "--timeout=180s")
		eventually(t, 2*time.Minute, "two Ready console Pods", func() error {
			pods := consolePodsNoFail(a)
			if len(pods) != 2 {
				return fmt.Errorf("%d pods", len(pods))
			}
			for _, p := range pods {
				if err := podReady(p); err != nil {
					return err
				}
			}
			return nil
		})
		dep, err := clientset.AppsV1().Deployments("team-a").Get(ctx(), a.fullname(), metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if dep.Status.AvailableReplicas != 2 {
			t.Errorf("available replicas = %d", dep.Status.AvailableReplicas)
		}
		assertCondition(t, dep, appsv1.DeploymentAvailable)

		for _, pr := range ingressProbes(t, a) {
			if !pr.allowed || !pr.wantHTTP {
				continue
			}
			got := pr.run(t)
			if !got.open || !got.http {
				t.Errorf("%s: the console must answer HTTP under the policy, got: %s", pr.name, got.detail)
			}
			t.Logf("%s: %s", pr.name, got.detail)
		}

		// A person signs in through the router namespace and the console answers
		// from its read model: this crosses the policy twice (in from the router,
		// out to the API server for the SelfSubjectReview and the SubjectAccessReviews).
		createCluster(t, newDynamic(t), "team-a", "orders", nil)
		if _, err := clientset.CoreV1().ServiceAccounts("team-a").Create(ctx(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "dba", Namespace: "team-a"}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := clientset.RbacV1().Roles("team-a").Create(ctx(), &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-view", Namespace: "team-a"},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"postgresql.cnpg.io"}, Resources: []string{"clusters"}, Verbs: []string{"get", "list", "watch"}}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := clientset.RbacV1().RoleBindings("team-a").Create(ctx(), &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "e2e-view", Namespace: "team-a"},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "e2e-view"},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "dba", Namespace: "team-a"}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		eventually(t, 2*time.Minute, "a signed-in user to list their Cluster through the router namespace", func() error {
			out := runClient(t, "router-sim", clientScript, map[string]string{
				"CONSOLE_HOST": a.fullname() + ".team-a.svc",
				"ORIGIN":       "https://pgc-a.example.test",
				"TOKEN":        saToken(t, "dba"),
			})
			if !strings.Contains(out, `"name":"orders"`) {
				var logs strings.Builder
				for _, pod := range consolePodsNoFail(a) {
					raw, _ := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: pod.Spec.Containers[0].Name, TailLines: new(int64(15))}).DoRaw(ctx())
					fmt.Fprintf(&logs, "\n-- %s\n%s", pod.Name, raw)
				}
				return fmt.Errorf("the cluster list lacks orders: %s\nconsole log:%s", tail(out, 800), tail(logs.String(), 5000))
			}
			return nil
		})

		// Nothing in the console's own log says the API server was unreachable.
		for _, pod := range consolePodsNoFail(a) {
			logs := podLogs(t, pod, pod.Spec.Containers[0].Name)
			if !strings.Contains(logs, "\"msg\":\"console listening\"") && !strings.Contains(logs, "\"msg\":\"listening\"") {
				t.Errorf("%s never reported listening:\n%s", pod.Name, tail(logs, 2000))
			}
			for _, bad := range []string{"i/o timeout", "connection refused", "no route to host", "TLS handshake timeout"} {
				if strings.Contains(logs, bad) {
					t.Errorf("%s logged %q under the policy:\n%s", pod.Name, bad, tail(logs, 3000))
				}
			}
		}

		// And the probes are unchanged on the new Pods.
		pod := consolePods(t, a)[0]
		p := startProber(t, pod, targets)
		eventually(t, 90*time.Second, "the policy on the restarted Pod", func() error {
			return p.expect(targets, func(tg target) bool { return tg.allowed })
		})
	})
}

// clientScript signs in with a ServiceAccount token and lists the Clusters, from a
// Pod in the router namespace: the only place the policy admits traffic from.
// It speaks raw HTTP/1.0 through nc, holding stdin open while the answer comes
// (busybox nc closes the connection at EOF and the console cancels the request): busybox wget follows redirects and keeps no
// cookies, which hides exactly the responses that matter. It prints each status
// line, so a failure says which step failed and why.
const clientScript = `H="$CONSOLE_HOST"
BODY="token=$TOKEN&rd=/"
(printf 'POST /auth/token HTTP/1.0\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: %s\r\n\r\n%s' "$H" "$ORIGIN" "${#BODY}" "$BODY"; sleep 4) | nc -w 10 "$H" 80 | tr -d '\r' >/tmp/signin
echo "SIGNIN $(head -1 /tmp/signin)"
C=$(grep -i '^Set-Cookie:' /tmp/signin | sed 's/^[Ss]et-[Cc]ookie: *//; s/;.*//' | tr '\n' ';')
echo "COOKIES $(echo "$C" | tr ';' '\n' | grep -c .)"
if [ -z "$C" ]; then tail -5 /tmp/signin; fi
(printf 'GET /api/v1/clusters HTTP/1.0\r\nHost: %s\r\nCookie: %s\r\n\r\n' "$H" "$C"; sleep 3) | nc -w 10 "$H" 80 | tr -d '\r' >/tmp/list
echo "LIST $(head -1 /tmp/list)"
tail -1 /tmp/list
`

// runClient runs script in a one-shot Pod of ns with env, and returns its log.
func runClient(t *testing.T, ns, script string, env map[string]string) string {
	t.Helper()
	name := fmt.Sprintf("signin-%d", time.Now().UnixNano()%1_000_000_000)
	vars := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		vars = append(vars, corev1.EnvVar{Name: k, Value: v})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "client", Image: probeImage, ImagePullPolicy: corev1.PullNever,
				Command: []string{"sh", "-c", script}, Env: vars, SecurityContext: restrictedContext(),
				VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes:                       []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			TerminationGracePeriodSeconds: new(int64),
		},
	}
	if _, err := clientset.CoreV1().Pods(ns).Create(ctx(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = clientset.CoreV1().Pods(ns).Delete(ctx(), name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	}()
	eventually(t, 90*time.Second, "client "+name+" to finish", func() error {
		p, err := clientset.CoreV1().Pods(ns).Get(ctx(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			return fmt.Errorf("phase %s", p.Status.Phase)
		}
		return nil
	})
	return podLogs(t, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, "client")
}

func assertCondition(t *testing.T, dep *appsv1.Deployment, kind appsv1.DeploymentConditionType) {
	t.Helper()
	for _, c := range dep.Status.Conditions {
		if c.Type == kind {
			if c.Status != corev1.ConditionTrue {
				t.Errorf("deployment condition %s = %s (%s)", kind, c.Status, c.Message)
			}
			return
		}
	}
	t.Errorf("deployment has no %s condition", kind)
}

func podLogs(t *testing.T, pod corev1.Pod, container string) string {
	t.Helper()
	raw, err := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container}).DoRaw(ctx())
	if err != nil {
		t.Fatalf("logs of %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return string(raw)
}

// ------------------------------------------------------------------ stand-ins

// restrictedContext is a securityContext that Pod Security "restricted" admits for
// an image that runs as root by default.
func restrictedContext() *corev1.SecurityContext {
	f, tr := false, true
	uid := int64(65534)
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &f,
		RunAsNonRoot:             &tr,
		RunAsUser:                &uid,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// standIn runs busybox httpd on the given ports under the given labels and waits
// until it is Ready, returning its Pod IP.
func standIn(t *testing.T, ns, name string, labels map[string]string, ports ...int) string {
	t.Helper()
	var script strings.Builder
	script.WriteString("mkdir -p /tmp/www && echo ok > /tmp/www/index.html\n")
	for i, p := range ports {
		cmd := fmt.Sprintf("httpd -f -p %d -h /tmp/www", p)
		if i < len(ports)-1 {
			cmd += " &"
		} else {
			cmd = "exec " + cmd
		}
		script.WriteString(cmd + "\n")
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "httpd", Image: probeImage, ImagePullPolicy: corev1.PullNever,
				Command:         []string{"sh", "-c", script.String()},
				SecurityContext: restrictedContext(),
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Port: intOrString(ports[0])},
				}, PeriodSeconds: 1},
			}},
			TerminationGracePeriodSeconds: new(int64),
		},
	}
	if _, err := clientset.CoreV1().Pods(ns).Create(ctx(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create stand-in %s/%s: %v", ns, name, err)
	}
	var ip string
	eventually(t, 2*time.Minute, "stand-in "+ns+"/"+name+" Ready", func() error {
		p, err := clientset.CoreV1().Pods(ns).Get(ctx(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := podReady(*p); err != nil {
			return err
		}
		ip = p.Status.PodIP
		return nil
	})
	return ip
}

// ------------------------------------------------------------------ prober

// target is one TCP connection the console may or may not make. A target with no
// host is the DNS lookup.
type target struct {
	name    string
	host    string
	port    int
	allowed bool // with the policy enabled
}

func (tg target) key() string {
	s := strings.NewReplacer(" ", "_", "(", "", ")", "", ":", "-", "/", "-").Replace(tg.name)
	return s
}

// prober is an ephemeral container in the console Pod that connects to every
// target in a loop and logs one block of RESULT lines per round.
type prober struct {
	pod       corev1.Pod
	container string
}

var probeSeq int

func startProber(t *testing.T, pod corev1.Pod, targets []target) *prober {
	t.Helper()
	var script strings.Builder
	script.WriteString("check() { if nc -w 3 \"$2\" \"$3\" </dev/null >/dev/null 2>&1; then echo \"RESULT $1 open\"; else echo \"RESULT $1 blocked\"; fi; }\n")
	script.WriteString("dns() { if nslookup kubernetes.default.svc.cluster.local 2>&1 | grep -q '^Name:'; then echo \"RESULT $1 open\"; else echo \"RESULT $1 blocked\"; fi; }\n")
	script.WriteString("while true; do\n")
	for _, tg := range targets {
		if tg.host == "" {
			fmt.Fprintf(&script, "  dns %s &\n", tg.key())
		} else {
			fmt.Fprintf(&script, "  check %s %s %d &\n", tg.key(), tg.host, tg.port)
		}
	}
	script.WriteString("  wait\n  echo ROUND\n  sleep 1\ndone\n")

	probeSeq++
	name := fmt.Sprintf("probe-%d", probeSeq)
	current, err := clientset.CoreV1().Pods(pod.Namespace).Get(ctx(), pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Spec.EphemeralContainers = append(current.Spec.EphemeralContainers, corev1.EphemeralContainer{
		TargetContainerName: pod.Spec.Containers[0].Name,
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: name, Image: probeImage, ImagePullPolicy: corev1.PullNever,
			Command:         []string{"sh", "-c", script.String()},
			SecurityContext: restrictedContext(),
		},
	})
	if _, err := clientset.CoreV1().Pods(pod.Namespace).UpdateEphemeralContainers(ctx(), pod.Name, current, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("add the probe to %s/%s: %v", pod.Namespace, pod.Name, err)
	}
	return &prober{pod: pod, container: name}
}

// latest returns the last complete round of results.
func (p *prober) latest() (map[string]bool, error) {
	raw, err := clientset.CoreV1().Pods(p.pod.Namespace).GetLogs(p.pod.Name, &corev1.PodLogOptions{Container: p.container, TailLines: new(int64(400))}).DoRaw(ctx())
	if err != nil {
		return nil, err
	}
	var rounds []map[string]bool
	cur := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case len(f) == 3 && f[0] == "RESULT":
			cur[f[1]] = f[2] == "open"
		case len(f) == 1 && f[0] == "ROUND":
			rounds = append(rounds, cur)
			cur = map[string]bool{}
		}
	}
	if len(rounds) == 0 {
		return nil, errors.New("the probe has not completed a round yet")
	}
	return rounds[len(rounds)-1], nil
}

// expect fails until the latest round shows every target open or blocked as want says.
func (p *prober) expect(targets []target, want func(target) bool) error {
	got, err := p.latest()
	if err != nil {
		return err
	}
	var bad []string
	for _, tg := range targets {
		open, ok := got[tg.key()]
		if !ok {
			bad = append(bad, tg.name+": no result")
			continue
		}
		if open != want(tg) {
			bad = append(bad, fmt.Sprintf("%s: open=%v, want %v", tg.name, open, want(tg)))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// ------------------------------------------------------------ ingress probes

// ingressProbe is a short-lived client Pod in some namespace connecting to the
// console, which the policy admits only from the router namespace, only on the
// console port.
type ingressProbe struct {
	name     string
	ns       string
	url      string // http URL, or empty with host:port for a bare TCP connect
	host     string
	port     int
	allowed  bool
	wantHTTP bool
}

type ingressResult struct {
	open, http bool
	detail     string
}

func ingressProbes(t *testing.T, c console) []ingressProbe {
	t.Helper()
	svc := c.fullname() + "." + c.Namespace + ".svc"
	pod := consolePods(t, c)[0]
	return []ingressProbe{
		{name: "router namespace -> console Service :80", ns: "router-sim", url: "http://" + svc + "/", allowed: true, wantHTTP: true},
		{name: "router namespace -> console Pod :3001 (the probe port)", ns: "router-sim", host: pod.Status.PodIP, port: 3001, allowed: false},
		{name: "team-b -> console Service :80", ns: "team-b", url: "http://" + svc + "/", allowed: false},
		{name: "team-a (same namespace) -> console Service :80", ns: "team-a", url: "http://" + svc + "/", allowed: false},
	}
}

func (pr ingressProbe) run(t *testing.T) ingressResult {
	t.Helper()
	script := ""
	if pr.url != "" {
		script = fmt.Sprintf("out=$(wget -S -O /dev/null -T 3 -t 1 %s 2>&1); echo \"$out\" | grep -m1 'HTTP/' || echo 'no HTTP response'", pr.url)
	} else {
		script = fmt.Sprintf("if nc -w 3 %s %d </dev/null >/dev/null 2>&1; then echo connected; else echo 'no connection'; fi", pr.host, pr.port)
	}
	name := fmt.Sprintf("client-%d", time.Now().UnixNano()%1_000_000_000)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pr.ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "client", Image: probeImage, ImagePullPolicy: corev1.PullNever,
				Command: []string{"sh", "-c", script}, SecurityContext: restrictedContext(),
			}},
			TerminationGracePeriodSeconds: new(int64),
		},
	}
	if _, err := clientset.CoreV1().Pods(pr.ns).Create(ctx(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create client %s/%s: %v", pr.ns, name, err)
	}
	defer func() {
		_ = clientset.CoreV1().Pods(pr.ns).Delete(ctx(), name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)})
	}()
	var phase corev1.PodPhase
	eventually(t, 90*time.Second, "client "+name+" to finish", func() error {
		p, err := clientset.CoreV1().Pods(pr.ns).Get(ctx(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if phase = p.Status.Phase; phase != corev1.PodSucceeded && phase != corev1.PodFailed {
			return fmt.Errorf("phase %s", phase)
		}
		return nil
	})
	out := strings.TrimSpace(podLogs(t, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pr.ns}}, "client"))
	res := ingressResult{detail: out}
	switch {
	case strings.Contains(out, "HTTP/"):
		res.open, res.http = true, true
	case out == "connected":
		res.open = true
	}
	return res
}
