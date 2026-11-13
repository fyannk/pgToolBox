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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/fyannk/pgToolBox/internal/console/mcpleader"
)

// The identity provider of the SSO scenarios: dex, pinned by digest in
// the e2e-chart script, run in namespace "sso" with an in-memory store and a static
// password database. The API server of the kind cluster trusts it through an
// AuthenticationConfiguration (the script writes it before the cluster exists),
// with usernames "oidc:<email>". Its issuer is an IP address, the dex Service's
// fixed ClusterIP, because the API server runs on the node and cannot resolve
// cluster DNS.
const (
	dexNamespace        = "sso"
	dexClientID         = "pgtoolbox-console"
	dexClientSecretName = "oidc-client"
	// The OIDC grant client of the MCP consoles; the kind API server
	// accepts its audience too (hack/e2e-chart.sh).
	dexGrantClientID         = "pgtoolbox-console-grants"
	dexGrantClientSecretName = "oidc-grant-client"
	dexCAConfigMap           = "dex-ca"
	dexPort                  = 5556
	// dexIDTokenTTL is dex's ID token lifetime (expiry.idTokens): a little
	// more than the console's two-minute renewal lead.
	dexIDTokenTTL = 3 * time.Minute
)

func dexIssuer() string { return fmt.Sprintf("https://%s:%d/dex", os.Getenv("E2E_DEX_IP"), dexPort) }

// dexUser is a static password account.
type dexUser struct {
	Email, Password string
}

// username is what the API server calls the user (claimMappings prefix).
func (u dexUser) username() string { return "oidc:" + u.Email }

type dexFixture struct {
	client string
	// grantClient is the secret of the OIDC grant client
	// (dexGrantClientID): MCP consoles sign their grants in with it, so
	// that dex's one refresh token per person and client leaves the
	// browser session's alone.
	grantClient string
	NodePort    int
	CA          []byte
	Users       map[string]dexUser // by short name
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func mustEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required (hack/e2e-chart.sh scope sets it)", name)
	}
	return v
}

// startDex deploys dex with a client whose redirect URIs are the given consoles'
// callbacks, and waits until it is Ready. Users are random-password accounts.
func startDex(t *testing.T, redirectHosts []string, names ...string) *dexFixture {
	t.Helper()
	dir := mustEnv(t, "E2E_DEX_DIR")
	image := mustEnv(t, "E2E_DEX_IMAGE")
	ip := mustEnv(t, "E2E_DEX_IP")
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	crt, key, ca := read("dex.crt"), read("dex.key"), read("ca.crt")

	namespace(t, dexNamespace, nil)
	f := &dexFixture{CA: ca, Users: map[string]dexUser{}}
	passwords := make([]map[string]any, 0, len(names))
	for _, n := range names {
		u := dexUser{Email: n + "@example.test", Password: randHex(12)}
		f.Users[n] = u
		hash, err := bcrypt.GenerateFromPassword([]byte(u.Password), 10) // dex refuses a lower cost
		if err != nil {
			t.Fatal(err)
		}
		passwords = append(passwords, map[string]any{
			"email": u.Email, "hash": string(hash), "username": n, "userID": "uid-" + n,
		})
	}
	redirects := make([]string, 0, len(redirectHosts))
	for _, h := range redirectHosts {
		redirects = append(redirects, "http://"+h+"/auth/oidc/callback")
	}
	secret, grantSecret := randHex(16), randHex(16)
	cfg, err := json.Marshal(map[string]any{ // JSON is YAML
		"issuer":  dexIssuer(),
		"storage": map[string]any{"type": "memory"},
		"web": map[string]any{
			"https": fmt.Sprintf("0.0.0.0:%d", dexPort), "tlsCert": "/tls/tls.crt", "tlsKey": "/tls/tls.key",
		},
		// passwordConnector: the password grant, so the MCP test can take
		// a person's id_token without a browser (scope_test.go).
		"oauth2": map[string]any{"skipApprovalScreen": true, "responseTypes": []string{"code"}, "passwordConnector": "local"},
		// Short ID tokens: the OAuth scenario lives past a renewal of a
		// console grant's IdP token, with the grant's own refresh token,
		// and of the browser session's (dex keeps one per person and
		// client: the grant client keeps them apart). Every other subtest
		// signs in afresh and is done well within this.
		"expiry":           map[string]any{"idTokens": dexIDTokenTTL.String()},
		"enablePasswordDB": true,
		"staticPasswords":  passwords,
		"staticClients": []map[string]any{{
			"id": dexClientID, "name": "pgtoolbox console", "secret": secret, "redirectURIs": redirects,
		}, {
			"id": dexGrantClientID, "name": "pgtoolbox console grants", "secret": grantSecret, "redirectURIs": redirects,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := clientset.CoreV1()
	mk := func(s *corev1.Secret) {
		if _, err := cs.Secrets(s.Namespace).Create(ctx(), s, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create secret %s/%s: %v", s.Namespace, s.Name, err)
		}
	}
	mk(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dex-config", Namespace: dexNamespace}, Data: map[string][]byte{"config.yaml": cfg}})
	mk(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dex-tls", Namespace: dexNamespace}, Data: map[string][]byte{"tls.crt": crt, "tls.key": key}})
	// The client secret the consoles mount, and the CA they trust, per namespace.
	f.client, f.grantClient = secret, grantSecret

	labels := map[string]string{"app": "dex"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: dexNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "dex", Image: image, ImagePullPolicy: corev1.PullNever,
						Command: []string{"/usr/local/bin/dex", "serve", "/etc/dex/config.yaml"},
						SecurityContext: func() *corev1.SecurityContext {
							sc := restrictedContext()
							uid := int64(1001)
							sc.RunAsUser = &uid
							sc.ReadOnlyRootFilesystem = new(true)
							return sc
						}(),
						Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: dexPort}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
								Path: "/dex/healthz", Port: intstr.FromInt32(dexPort), Scheme: corev1.URISchemeHTTPS,
							}},
							PeriodSeconds: 2,
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/etc/dex", ReadOnly: true},
							{Name: "tls", MountPath: "/tls", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "dex-config"}}},
						{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "dex-tls"}}},
					},
				},
			},
		},
	}
	if _, err := clientset.AppsV1().Deployments(dexNamespace).Create(ctx(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: dexNamespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort, ClusterIP: ip, Selector: labels,
			Ports: []corev1.ServicePort{{Name: "https", Port: dexPort, TargetPort: intstr.FromInt32(dexPort)}},
		},
	}
	created, err := cs.Services(dexNamespace).Create(ctx(), svc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the dex Service with ClusterIP %s: %v", ip, err)
	}
	f.NodePort = int(created.Spec.Ports[0].NodePort)
	eventually(t, 3*time.Minute, "dex Ready", func() error {
		d, err := clientset.AppsV1().Deployments(dexNamespace).Get(ctx(), "dex", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if d.Status.ReadyReplicas < 1 {
			return errors.New("not ready")
		}
		return nil
	})
	return f
}

// forNamespace gives a console namespace what auth.provider=oidc mounts: the
// client secret and the CA that signs dex's certificate.
func (f *dexFixture) forNamespace(t *testing.T, ns string) {
	t.Helper()
	if _, err := clientset.CoreV1().Secrets(ns).Create(ctx(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dexClientSecretName, Namespace: ns},
		Data:       map[string][]byte{"client-secret": []byte(f.client)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().Secrets(ns).Create(ctx(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: dexGrantClientSecretName, Namespace: ns},
		Data:       map[string][]byte{"client-secret": []byte(f.grantClient)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := clientset.CoreV1().ConfigMaps(ns).Create(ctx(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: dexCAConfigMap, Namespace: ns},
		Data:       map[string]string{"ca.crt": string(f.CA)},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// ------------------------------------------------------------------- browser

// nodeAddr is an address of the kind node, which is where every NodePort lives.
func nodeAddr(t *testing.T) string {
	t.Helper()
	nodes, err := clientset.CoreV1().Nodes().List(ctx(), metav1.ListOptions{})
	if err != nil || len(nodes.Items) == 0 {
		t.Fatalf("list nodes: %v", err)
	}
	for _, a := range nodes.Items[0].Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			if ip := net.ParseIP(a.Address); ip != nil && ip.To4() != nil {
				return a.Address
			}
		}
	}
	t.Fatal("the kind node has no IPv4 InternalIP")
	return ""
}

// exposeConsole adds a NodePort Service in front of a console's pods (the chart's
// own Service is ClusterIP) and returns the node ports of the API/UI listener and
// of the probe listener.
func exposeConsole(t *testing.T, c console) (httpPort, probePort int) {
	t.Helper()
	svc, err := clientset.CoreV1().Services(c.Namespace).Create(ctx(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: c.fullname() + "-e2e-access", Namespace: c.Namespace},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app.kubernetes.io/name": "pgtoolbox-console", "app.kubernetes.io/instance": c.Release},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 3000, TargetPort: intstr.FromString("http")},
				{Name: "probes", Port: 3001, TargetPort: intstr.FromString("probes")},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("expose %s: %v", c.Release, err)
	}
	for _, p := range svc.Spec.Ports {
		switch p.Name {
		case "http":
			httpPort = int(p.NodePort)
		case "probes":
			probePort = int(p.NodePort)
		}
	}
	return httpPort, probePort
}

// leaderRoute is the hosts key of a console's MCP leader: requests to
// host on mcpleader.Paths are sent to that address.
func leaderRoute(host string) string { return "mcp " + host }

// exposeMCPLeader opens the console's leader-only Service (<fullname>-mcp,
// backed by the slice the MCP leader writes) on a NodePort.
func exposeMCPLeader(t *testing.T, c console) int {
	t.Helper()
	svc, err := clientset.CoreV1().Services(c.Namespace).Get(ctx(), c.fullname()+"-mcp", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the MCP leader Service of %s: %v", c.Release, err)
	}
	svc.Spec.Type = corev1.ServiceTypeNodePort
	if svc, err = clientset.CoreV1().Services(c.Namespace).Update(ctx(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("expose the MCP leader of %s: %v", c.Release, err)
	}
	return int(svc.Spec.Ports[0].NodePort)
}

// browser is a browser-less HTTP client: it follows redirects, keeps cookies, and
// dials the cluster addresses the test names (a console's public host, dex's
// issuer IP) at the node's NodePorts. Dex is verified against the e2e CA.
type browser struct {
	t      *testing.T
	jar    http.CookieJar
	hosts  map[string]string // "host:port" -> "nodeIP:nodePort"
	roots  *x509.CertPool
	client *http.Client
}

func newBrowser(t *testing.T, hosts map[string]string, ca []byte) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if ca != nil && !roots.AppendCertsFromPEM(ca) {
		t.Fatal("the dex CA does not parse")
	}
	b := &browser{t: t, jar: jar, hosts: hosts, roots: roots}
	b.client = b.build(false)
	return b
}

func (b *browser) build(noKeepAlive bool) *http.Client {
	return &http.Client{
		Jar:     b.jar,
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: noKeepAlive,
			// The console closes idle connections after 120 s (its server's
			// IdleTimeout); tests sleep 70 s to 3 min on token and session
			// lifetimes, and a pooled connection reused after that sleep fails a
			// POST (which Go does not retry) with EOF/reset. Drop idle
			// connections well before the server does.
			IdleConnTimeout: 30 * time.Second,
			TLSClientConfig: &tls.Config{RootCAs: b.roots, MinVersion: tls.VersionTLS12},
			// The MCP paths go to the leader's Service, as the chart's
			// exposure sends them: plain HTTP, so a proxy hop keeps the
			// request as it is.
			Proxy: func(r *http.Request) (*url.URL, error) {
				to, ok := b.hosts[leaderRoute(hostPort(r.URL.Host))]
				if ok && r.URL.Scheme == "http" && slices.ContainsFunc(mcpleader.Paths, func(p mcpleader.Path) bool { return p.Match(r.URL.Path) }) {
					return &url.URL{Scheme: "http", Host: to}, nil
				}
				return nil, nil
			},
			DialContext: func(c context.Context, network, addr string) (net.Conn, error) {
				if to, ok := b.hosts[addr]; ok {
					addr = to
				}
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(c, network, addr)
			},
		},
	}
}

// noRedirect is a client sharing the cookies that returns the first response.
func (b *browser) noRedirect() *browser {
	c := *b
	c.client = b.build(false)
	c.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// stopAt is a client sharing the cookies that follows redirects until one
// goes to a URL starting with prefix, and returns that redirect.
func (b *browser) stopAt(prefix string) *browser {
	c := *b
	c.client = b.build(false)
	c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), prefix) {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

type reply struct {
	Status int
	Body   []byte
	URL    *url.URL
	Header http.Header
}

func (b *browser) do(method, target string, form url.Values, header map[string]string) reply {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(context.Background(), method, target, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	// Connection-level errors are retried for 30 s: a NodePort answers a moment
	// after the Deployment is Ready, once kube-proxy has programmed it. HTTP
	// statuses are final.
	var resp *http.Response
	deadline := time.Now().Add(30 * time.Second)
	for {
		if form != nil {
			req.Body = io.NopCloser(strings.NewReader(form.Encode()))
		}
		var err error
		resp, err = b.client.Do(req)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("%s %s: %v", method, target, err)
		}
		time.Sleep(time.Second)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return reply{Status: resp.StatusCode, Body: raw, URL: resp.Request.URL, Header: resp.Header}
}

func (b *browser) get(target string) reply { return b.do(http.MethodGet, target, nil, nil) }

// origin of a URL, for the Origin header of an unsafe request.
func origin(target string) string {
	u, _ := url.Parse(target)
	return u.Scheme + "://" + u.Host
}

var formAction = regexp.MustCompile(`(?is)<form[^>]*\saction="([^"]*)"`)

// signInSSO drives the authorization-code flow: the console's login start, the
// redirect to dex, dex's password form, the redirect back with the code, the
// console's callback and its session cookie.
//
// The console budgets sign-ins per client address (10 a minute per replica) and
// every request here comes from the node's address, so a 429 is waited out.
// A 403 is waited out too: right after kind starts, the API server's JWT
// authenticator may not have fetched dex's keys yet and refuses the token,
// which the console reports as an untrusted issuer. A lasting refusal still
// fails the caller's session check after two minutes.
func (b *browser) signInSSO(base string, u dexUser) {
	b.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		st := b.signInSSOOnce(base, u)
		if (st != http.StatusTooManyRequests && st != http.StatusForbidden) || time.Now().After(deadline) {
			return
		}
		time.Sleep(8 * time.Second)
	}
}

func (b *browser) signInSSOOnce(base string, u dexUser) int {
	b.t.Helper()
	page := b.get(base + "/auth/oidc/login?rd=%2F")
	if page.Status != http.StatusOK {
		b.t.Fatalf("the login page of dex answered %d (at %s): %s", page.Status, page.URL, tailBytes(page.Body))
	}
	m := formAction.FindSubmatch(page.Body)
	if m == nil {
		b.t.Fatalf("no login form at %s: %s", page.URL, tailBytes(page.Body))
	}
	action, err := page.URL.Parse(html.UnescapeString(string(m[1])))
	if err != nil {
		b.t.Fatal(err)
	}
	done := b.do(http.MethodPost, action.String(), url.Values{"login": {u.Email}, "password": {u.Password}}, nil)
	if done.URL.Host != hostOf(base) {
		b.t.Fatalf("after the password the browser ended at %s (status %d), not on the console: %s", done.URL, done.Status, tailBytes(done.Body))
	}
	if done.Status == http.StatusTooManyRequests {
		return done.Status
	}
	if done.Status >= 500 {
		b.t.Fatalf("the console's callback failed with %d: %s", done.Status, tailBytes(done.Body))
	}
	return done.Status
}

func hostOf(base string) string {
	u, _ := url.Parse(base)
	return u.Host
}

// signInToken is the break-glass form: a pasted API-server token.
func (b *browser) signInToken(base, token string) reply {
	b.t.Helper()
	for attempt := 0; ; attempt++ {
		r := b.noRedirect().do(http.MethodPost, base+"/auth/token", url.Values{"token": {token}, "rd": {"/"}}, map[string]string{"Origin": origin(base)})
		if r.Status != http.StatusTooManyRequests || attempt == 8 {
			return r
		}
		time.Sleep(8 * time.Second)
	}
}

func tailBytes(b []byte) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

// sessionView is the part of /api/v1/session the scenarios read.
type sessionView struct {
	Username  string `json:"username"`
	Provider  string `json:"provider"`
	SessionID string `json:"sessionId"`
	// TokenExpiresAt and Refreshable: whether the session renewed its token.
	TokenExpiresAt *time.Time `json:"tokenExpiresAt"`
	Refreshable    bool       `json:"refreshable"`
}

func (b *browser) session(base string) (sessionView, int) {
	b.t.Helper()
	r := b.get(base + "/api/v1/session")
	var v sessionView
	if r.Status == http.StatusOK {
		if err := json.Unmarshal(r.Body, &v); err != nil {
			b.t.Fatalf("session body: %v: %s", err, tailBytes(r.Body))
		}
	}
	return v, r.Status
}

// clusterNames lists the Clusters the signed-in user sees, as namespace/name.
func (b *browser) clusterNames(base string) ([]string, int) {
	b.t.Helper()
	r := b.get(base + "/api/v1/clusters")
	if r.Status != http.StatusOK {
		return nil, r.Status
	}
	var list struct {
		Items []struct {
			Ref struct{ Namespace, Name string } `json:"ref"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		b.t.Fatalf("cluster list: %v: %s", err, tailBytes(r.Body))
	}
	out := []string{}
	for _, it := range list.Items {
		out = append(out, it.Ref.Namespace+"/"+it.Ref.Name)
	}
	return sortedStrings(out), r.Status
}

// cookieHeader snapshots the cookies the browser would send to base.
func (b *browser) cookieHeader(base string) string {
	u, _ := url.Parse(base)
	cookies := b.jar.Cookies(u)
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// replay answers the status of GET path with exactly the cookie header given, on
// a new connection each time (so that the Service picks a replica at random) and
// without a cookie jar: a "401, cookie cleared, 401" sequence cannot hide a replica
// that still accepts the session.
func (b *browser) replay(base, path, cookie string) int {
	b.t.Helper()
	d := newBrowser(b.t, b.hosts, nil)
	d.roots = b.roots
	d.client = d.build(true)
	d.client.Jar = nil
	return d.do(http.MethodGet, base+path, nil, map[string]string{"Cookie": cookie}).Status
}

// allReplicasAnswer polls until n consecutive replays all return want.
func (b *browser) allReplicasAnswer(t *testing.T, base, cookie string, want int, what string) {
	t.Helper()
	eventually(t, 60*time.Second, what, func() error {
		for i := 0; i < 12; i++ {
			if got := b.replay(base, "/api/v1/session", cookie); got != want {
				return fmt.Errorf("request %d answered %d, want %d", i, got, want)
			}
		}
		return nil
	})
}
