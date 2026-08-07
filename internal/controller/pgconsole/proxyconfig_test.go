/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package pgconsole

import (
	"context"
	"testing"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	proxyconfig "github.com/fyannk/pgtoolbox/internal/proxy/config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sessionKey is a fixed render input so tests never depend on randomness.
const testSessionKey = "0123456789abcdef0123456789abcdef0123456789a"

func renderAndParse(t *testing.T, console *pgtoolboxv1alpha1.PgConsole, pgAdminEnabled bool) *proxyconfig.Config {
	return renderAndParseWithUsers(t, console, pgAdminEnabled, nil)
}

func renderAndParseWithUsers(t *testing.T, console *pgtoolboxv1alpha1.PgConsole, pgAdminEnabled bool, users []proxyconfig.User) *proxyconfig.Config {
	t.Helper()
	rendered, err := renderProxyConfig(console, testSessionKey, pgAdminEnabled, users)
	if err != nil {
		t.Fatalf("render proxy config: %v", err)
	}
	cfg, _, err := proxyconfig.Parse(rendered)
	if err != nil {
		t.Fatalf("rendered config does not round-trip through the proxy parser: %v", err)
	}
	return cfg
}

func TestRenderProxyConfigLocalMode(t *testing.T) {
	cfg := renderAndParse(t, testConsole(), true)

	if cfg.Provider.Mode != proxyconfig.ModeLocal {
		t.Fatalf("provider mode = %q, want local", cfg.Provider.Mode)
	}
	if cfg.Provider.OIDC != nil {
		t.Fatalf("local mode must not carry an oidc block")
	}
	if len(cfg.Session.CookieSecrets) != 1 || cfg.Session.CookieSecrets[0] != testSessionKey {
		t.Fatalf("cookie secrets = %v, want the session key", cfg.Session.CookieSecrets)
	}
	if len(cfg.Users) != 0 {
		t.Fatalf("users = %v, want empty until PgToolBoxUser lands", cfg.Users)
	}
	if !cfg.AccessRequest.Enabled ||
		cfg.AccessRequest.ConsoleName != "console" ||
		cfg.AccessRequest.Namespace != "test" {
		t.Fatalf("access request config = %+v", cfg.AccessRequest)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("listen = %q, want :8080", cfg.Server.Listen)
	}
}

func TestRenderProxyConfigRoutes(t *testing.T) {
	console := testConsole()
	console.Spec.PgAdmin.AccessMinLevel = "poweruser"
	cfg := renderAndParse(t, console, true)

	if len(cfg.Routes) != 2 {
		t.Fatalf("routes = %+v, want /pgadmin and /", cfg.Routes)
	}
	pgAdminRoute := cfg.Routes[0]
	if pgAdminRoute.PathPrefix != "/pgadmin" ||
		pgAdminRoute.Upstream != "http://127.0.0.1:8081" ||
		pgAdminRoute.MinLevel != proxyconfig.LevelPowerUser {
		t.Fatalf("pgadmin route = %+v", pgAdminRoute)
	}
	consoleRoute := cfg.Routes[1]
	if consoleRoute.PathPrefix != "/" ||
		consoleRoute.Upstream != "http://127.0.0.1:3000" ||
		consoleRoute.MinLevel != proxyconfig.LevelView {
		t.Fatalf("console route = %+v", consoleRoute)
	}
}

func TestRenderProxyConfigPgAdminDisabled(t *testing.T) {
	cfg := renderAndParse(t, testConsole(), false)
	if len(cfg.Routes) != 1 || cfg.Routes[0].PathPrefix != "/" {
		t.Fatalf("routes = %+v, want only / when pgAdmin is disabled", cfg.Routes)
	}
}

func TestRenderProxyConfigPgAdminDefaultMinLevel(t *testing.T) {
	cfg := renderAndParse(t, testConsole(), true)
	if cfg.Routes[0].MinLevel != proxyconfig.LevelDBA {
		t.Fatalf("default pgadmin minLevel = %q, want dba", cfg.Routes[0].MinLevel)
	}
}

func TestRenderProxyConfigOIDCMode(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication = pgtoolboxv1alpha1.ProxyAuthenticationSpec{
		Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC,
		OIDC: &pgtoolboxv1alpha1.ProxyOIDCSpec{
			IssuerURL: "https://idp.example.com",
			ClientID:  "pgconsole",
			ClientSecretRef: pgtoolboxv1alpha1.SecretKeyReference{
				Name: "oidc-client",
			},
		},
	}
	console.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type:     pgtoolboxv1alpha1.ExposureTypeIngress,
		Hostname: "pgconsole.apps.example.com",
	}
	cfg := renderAndParse(t, console, true)

	if cfg.Provider.Mode != proxyconfig.ModeOIDC || cfg.Provider.OIDC == nil {
		t.Fatalf("provider = %+v, want oidc", cfg.Provider)
	}
	oidc := cfg.Provider.OIDC
	if oidc.IssuerURL != "https://idp.example.com" || oidc.ClientID != "pgconsole" {
		t.Fatalf("oidc config = %+v", oidc)
	}
	if oidc.ClientSecretFile != "/etc/pgtoolbox-proxy/oidc/client-secret" {
		t.Fatalf("client secret file = %q", oidc.ClientSecretFile)
	}
	if oidc.RedirectURL != "https://pgconsole.apps.example.com/auth/oidc/callback" {
		t.Fatalf("redirect URL = %q", oidc.RedirectURL)
	}
}

func TestRenderProxyConfigOpenShift(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication.Mode = pgtoolboxv1alpha1.ProxyAuthenticationModeOpenShift
	console.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{
		Type:     pgtoolboxv1alpha1.ExposureTypeRoute,
		Hostname: "pgconsole.apps.example.com",
	}
	cfg := renderAndParse(t, console, true)

	if cfg.Provider.Mode != proxyconfig.ModeOpenShift || cfg.Provider.OpenShift == nil {
		t.Fatalf("provider = %+v, want openshift", cfg.Provider)
	}
	oc := cfg.Provider.OpenShift
	wantClientID := "system:serviceaccount:test:console-pgconsole"
	if oc.ClientID != wantClientID {
		t.Fatalf("clientID = %q, want %q", oc.ClientID, wantClientID)
	}
	if oc.RedirectURL != "https://pgconsole.apps.example.com/auth/openshift/callback" {
		t.Fatalf("redirectURL = %q", oc.RedirectURL)
	}
	if oc.ClientSecretFile != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatalf("clientSecretFile = %q", oc.ClientSecretFile)
	}
	if oc.CAFile != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" {
		t.Fatalf("caFile = %q", oc.CAFile)
	}
}

func TestRenderProxyConfigDeterministic(t *testing.T) {
	console := testConsole()
	first, err := renderProxyConfig(console, testSessionKey, true, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := renderProxyConfig(console, testSessionKey, true, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("identical inputs rendered different configs:\n%s\n---\n%s", first, second)
	}
}

func TestReconcileProxyConfigSecretSessionKeyStability(t *testing.T) {
	console := testConsole()
	r, c := newTestReconciler(t, console)
	ctx := context.Background()

	checksum, issue, err := r.reconcileProxyConfigSecret(ctx, console, nil)
	if err != nil || issue != nil {
		t.Fatalf("first reconcile: issue=%v err=%v", issue, err)
	}
	if checksum == "" {
		t.Fatalf("checksum must be reported")
	}

	key := client.ObjectKey{Namespace: "test", Name: "console-pgconsole-proxy"}
	var firstSecret corev1.Secret
	if err := c.Get(ctx, key, &firstSecret); err != nil {
		t.Fatalf("read rendered secret: %v", err)
	}
	if len(firstSecret.Data[proxySessionKeyName]) != 43 {
		t.Fatalf("session key length = %d, want 43 (32 bytes base64url)",
			len(firstSecret.Data[proxySessionKeyName]))
	}
	if len(firstSecret.Data[proxyConfigFileName]) == 0 {
		t.Fatalf("config.yaml must be rendered into the secret")
	}
	owner := metav1.GetControllerOf(&firstSecret)
	if owner == nil || owner.UID != console.UID {
		t.Fatalf("secret owner = %+v, want the console", owner)
	}

	// A second reconcile must reuse the session key and produce the same
	// checksum — sessions are never rotated silently.
	checksum2, issue, err := r.reconcileProxyConfigSecret(ctx, console, nil)
	if err != nil || issue != nil {
		t.Fatalf("second reconcile: issue=%v err=%v", issue, err)
	}
	if checksum != checksum2 {
		t.Fatalf("checksum changed across no-op reconciles: %q → %q", checksum, checksum2)
	}
	var secondSecret corev1.Secret
	if err := c.Get(ctx, key, &secondSecret); err != nil {
		t.Fatalf("re-read rendered secret: %v", err)
	}
	if string(firstSecret.Data[proxySessionKeyName]) != string(secondSecret.Data[proxySessionKeyName]) {
		t.Fatalf("session key rotated across reconciles")
	}
	if string(firstSecret.Data[proxyConfigFileName]) != string(secondSecret.Data[proxyConfigFileName]) {
		t.Fatalf("rendered config changed across no-op reconciles")
	}
}

func TestReconcileProxyConfigSecretOpenShiftRenders(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication.Mode = pgtoolboxv1alpha1.ProxyAuthenticationModeOpenShift
	r, _ := newTestReconciler(t, console)

	_, issue, err := r.reconcileProxyConfigSecret(context.Background(), console, nil)
	if err != nil || issue != nil {
		t.Fatalf("openshift mode must render: issue=%v err=%v", issue, err)
	}
}

func TestReconcileProxyConfigSecretOIDCMissingClientSecret(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication = pgtoolboxv1alpha1.ProxyAuthenticationSpec{
		Mode: pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC,
		OIDC: &pgtoolboxv1alpha1.ProxyOIDCSpec{
			IssuerURL:       "https://idp.example.com",
			ClientID:        "pgconsole",
			ClientSecretRef: pgtoolboxv1alpha1.SecretKeyReference{Name: "oidc-client"},
		},
	}
	r, _ := newTestReconciler(t, console)

	_, issue, err := r.reconcileProxyConfigSecret(context.Background(), console, nil)
	if err != nil {
		t.Fatalf("missing client secret must report, not fail: %v", err)
	}
	if issue == nil || issue.reason != pgtoolboxv1alpha1.ReasonSecretNotFound {
		t.Fatalf("issue = %+v, want SecretNotFound", issue)
	}
}

func TestRenderProxyConfigUsers(t *testing.T) {
	console := testConsole()
	console.Spec.Proxy.Authentication.Mode = pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC
	console.Spec.Proxy.Authentication.OIDC = &pgtoolboxv1alpha1.ProxyOIDCSpec{
		IssuerURL:       "https://idp.example.com",
		ClientID:        "pgconsole",
		ClientSecretRef: pgtoolboxv1alpha1.SecretKeyReference{Name: "oidc-client"},
	}
	users := []proxyconfig.User{
		{Subject: "bob@example.com", Level: proxyconfig.LevelView},
		{Subject: "alice@example.com", Level: proxyconfig.LevelDBA},
	}
	cfg := renderAndParseWithUsers(t, console, true, users)

	if len(cfg.Users) != 2 {
		t.Fatalf("users = %v", cfg.Users)
	}
	// Users must be emitted in the order supplied (already sorted by caller).
	if cfg.Users[0].Subject != "bob@example.com" || cfg.Users[0].Level != proxyconfig.LevelView {
		t.Fatalf("first user = %+v", cfg.Users[0])
	}
	if cfg.Users[1].Subject != "alice@example.com" || cfg.Users[1].Level != proxyconfig.LevelDBA {
		t.Fatalf("second user = %+v", cfg.Users[1])
	}
}

func TestRenderProxyConfigLocalUserHasBcrypt(t *testing.T) {
	console := testConsole()
	users := []proxyconfig.User{
		{Subject: "alice@example.com", Level: proxyconfig.LevelDBA, LocalPasswordBcrypt: "$2a$10$abcdefghijklmnopqrstuuxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
	}
	// The proxy parser validates bcrypt format only when mode is local.
	_, err := renderProxyConfig(console, testSessionKey, true, users)
	if err != nil {
		t.Fatalf("render with local user: %v", err)
	}
}
