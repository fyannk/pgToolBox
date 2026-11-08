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

package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
)

// fakeKube is the API server the wired console talks to: SelfSubjectReview
// and SelfSubjectAccessReview answered per user token (no
// SubjectAccessReview path: the console must not use one), the revocation
// ConfigMap absent and watched.
func fakeKube(t *testing.T) *httptest.Server {
	t.Helper()
	users := map[string]string{"jane-token": "jane", "prom-token": "system:serviceaccount:monitoring:prometheus"}
	grants := map[string]bool{
		"jane get clusters team-a/orders":                          true,
		"system:serviceaccount:monitoring:prometheus get /metrics": true,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authz := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case r.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews":
			name, ok := users[authz]
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`)
				return
			}
			_ = json.NewEncoder(w).Encode(authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: name}}})
		case r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			// Answered for the token's own user: the console never asks
			// with its own credential (it holds no review grant).
			name, ok := users[authz]
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`)
				return
			}
			var ssar authorizationv1.SelfSubjectAccessReview
			_ = json.NewDecoder(r.Body).Decode(&ssar)
			key := name + " "
			if ra := ssar.Spec.ResourceAttributes; ra != nil {
				key += ra.Verb + " " + ra.Resource + " " + ra.Namespace + "/" + ra.Name
			} else {
				key += ssar.Spec.NonResourceAttributes.Verb + " " + ssar.Spec.NonResourceAttributes.Path
			}
			ssar.Status.Allowed = grants[key]
			_ = json.NewEncoder(w).Encode(ssar)
		case strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/team-a/configmaps"):
			if r.URL.Query().Get("watch") == "true" {
				w.WriteHeader(http.StatusOK)
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func freePort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// The whole wiring against a fake API server: sign in with a token, list
// clusters filtered by SelfSubjectAccessReview, readiness, authenticated
// /metrics, graceful stop.
func TestRunAppEndToEnd(t *testing.T) {
	t.Parallel()
	kube := fakeKube(t)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "session-key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", 48)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.LoadConfig(func(k string) (string, bool) {
		v, ok := map[string]string{
			"AUTH_PROVIDER": "token", "SESSION_KEY_FILE": keyFile, "COOKIE_SECURE": "false",
			"REVOCATION_CONFIGMAP": "revoked", "POD_NAMESPACE": "team-a",
		}[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	listen, probe := freePort(t), freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunApp(ctx, AppOptions{
			RestConfig: &rest.Config{Host: kube.URL, BearerToken: "console-sa", ContentConfig: rest.ContentConfig{ContentType: "application/json"}},
			Auth:       cfg, ListenAddr: listen, ProbeAddr: probe,
			Source: api.FixtureSource{}, Log: slog.New(slog.DiscardHandler),
		})
	}()
	base, probeBase := "http://"+listen, "http://"+probe
	waitHTTP(t, probeBase+"/readyz")

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	form := url.Values{"token": {"jane-token"}, "rd": {"/"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", base)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("sign-in = %d", resp.StatusCode)
	}
	body := getBody(t, client, base+"/api/v1/clusters", http.StatusOK)
	if !strings.Contains(body, `"name":"orders"`) || strings.Contains(body, `"name":"billing"`) {
		t.Fatalf("cluster list not filtered by SAR: %s", body)
	}
	getBody(t, client, base+"/api/v1/namespaces/team-a/clusters/billing", http.StatusForbidden)

	for tok, want := range map[string]int{"": http.StatusUnauthorized, "jane-token": http.StatusForbidden, "prom-token": http.StatusOK} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, probeBase+"/metrics", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("/metrics with %q = %d", tok, resp.StatusCode)
		}
		if want == http.StatusOK {
			for _, m := range []string{"pgtoolbox_console_sar_reviews_total", `pgtoolbox_console_build_info{goversion=`, `pgtoolbox_console_config_info{name="auth_providers",value="token"}`} {
				if !strings.Contains(string(b), m) {
					t.Errorf("/metrics lacks %s", m)
				}
			}
			if strings.Contains(string(b), strings.Repeat("k", 48)) {
				t.Error("the session key reached /metrics")
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunApp = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunApp did not stop")
	}
}

func waitHTTP(t *testing.T, u string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", u)
}

func getBody(t *testing.T, c *http.Client, u string, want int) string {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d: %s", u, resp.StatusCode, want, b)
	}
	return string(b)
}
