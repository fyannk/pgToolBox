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

package config

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func testHash(t *testing.T) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("GenerateFromPassword: %v", err)
	}
	return string(h)
}

const validOIDC = `
session:
  cookieSecrets: ["secret-one-abcdefghij"]
server:
  listen: ":8080"
provider:
  mode: oidc
  oidc:
    issuerURL: "https://idp.corp.example"
    clientID: "pgtoolbox"
    clientSecretFile: "/var/run/secrets/clientSecret"
    redirectURL: "https://pgconsole.corp.example/auth/oidc/callback"
users:
  - {subject: "jane@corp.example", level: dba}
routes:
  - {pathPrefix: "/pgadmin", upstream: "http://127.0.0.1:8081", minLevel: dba}
  - {pathPrefix: "/", upstream: "http://127.0.0.1:8082", minLevel: view}
accessRequest: {enabled: true, consoleName: my-console, namespace: my-ns}
`

func validLocal(t *testing.T) string {
	t.Helper()
	return `
session:
  cookieSecrets: ["secret-one-abcdefghij"]
provider:
  mode: local
users:
  - {subject: "jane@corp.example", level: dba, localPasswordBcrypt: "` + testHash(t) + `"}
routes:
  - {pathPrefix: "/", upstream: "http://127.0.0.1:8082"}
`
}

func TestParseValidOIDC(t *testing.T) {
	cfg, warns, err := Parse([]byte(validOIDC))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	// Defaults.
	if cfg.Session.CookieName != "pgtoolbox_session" {
		t.Fatalf("cookieName default = %q", cfg.Session.CookieName)
	}
	if cfg.Session.MaxAge.D() != 8*time.Hour {
		t.Fatalf("maxAge default = %v", cfg.Session.MaxAge.D())
	}
	if !cfg.Session.CookieSecure() {
		t.Fatal("secure should default to true")
	}
	if cfg.Provider.OIDC.SubjectClaim != "email" {
		t.Fatalf("subjectClaim default = %q", cfg.Provider.OIDC.SubjectClaim)
	}
	if got := cfg.Server.UpstreamDialTimeout.D(); got != 10*time.Second {
		t.Fatalf("dial timeout default = %v", got)
	}
	if got := cfg.Server.UpstreamResponseHeaderTimeout.D(); got != 30*time.Second {
		t.Fatalf("response header timeout default = %v", got)
	}
}

func TestParseValidLocal(t *testing.T) {
	if _, _, err := Parse([]byte(validLocal(t))); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}

func TestValidationMatrix(t *testing.T) {
	mutate := func(s, old, new string) string {
		if !strings.Contains(s, old) {
			t.Fatalf("test setup: %q not found in config", old)
		}
		return strings.Replace(s, old, new, 1)
	}
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"missing issuer", mutate(validOIDC, `issuerURL: "https://idp.corp.example"`, ""), "issuerURL"},
		{"http issuer", mutate(validOIDC, "https://idp.corp.example", "http://idp.corp.example"), "issuerURL"},
		{"missing clientID", mutate(validOIDC, `clientID: "pgtoolbox"`, ""), "clientID"},
		{"missing redirectURL", mutate(validOIDC, `redirectURL: "https://pgconsole.corp.example/auth/oidc/callback"`, ""), "redirectURL"},
		{"bad level", mutate(validOIDC, "level: dba", "level: root"), "level"},
		{"bad minLevel", mutate(validOIDC, "minLevel: dba", "minLevel: admin"), "minLevel"},
		{"route collides with /auth", mutate(validOIDC, `pathPrefix: "/pgadmin"`, `pathPrefix: "/auth/evil"`), "reserved"},
		{"route collides with /logout", mutate(validOIDC, `pathPrefix: "/pgadmin"`, `pathPrefix: "/logout"`), "reserved"},
		{"route collides with /healthz", mutate(validOIDC, `pathPrefix: "/pgadmin"`, `pathPrefix: "/healthz"`), "reserved"},
		{"duplicate users", mutate(validOIDC,
			`- {subject: "jane@corp.example", level: dba}`,
			"- {subject: \"jane@corp.example\", level: dba}\n  - {subject: \"JANE@corp.example\", level: view}"), "duplicate"},
		{"no routes", mutate(validOIDC,
			"routes:\n  - {pathPrefix: \"/pgadmin\", upstream: \"http://127.0.0.1:8081\", minLevel: dba}\n  - {pathPrefix: \"/\", upstream: \"http://127.0.0.1:8082\", minLevel: view}\n",
			"routes: []\n"), "at least one route"},
		{"no cookie secrets", mutate(validOIDC, `cookieSecrets: ["secret-one-abcdefghij"]`, "cookieSecrets: []"), "at least one secret"},
		{"short cookie secret", mutate(validOIDC, "secret-one-abcdefghij", "short"), "too short"},
		{"accessRequest without console", mutate(validOIDC, "consoleName: my-console, ", ""), "consoleName"},
		{"openshift mode", mutate(validOIDC, "mode: oidc", "mode: openshift"), "openshift"},
		{"oidc block in local mode", mutate(validOIDC, "mode: oidc", "mode: local"), "oidc"},
		{"duplicate route prefix", strings.Replace(validOIDC,
			`{pathPrefix: "/", upstream: "http://127.0.0.1:8082", minLevel: view}`,
			"{pathPrefix: \"/\", upstream: \"http://127.0.0.1:8082\", minLevel: view}\n  - {pathPrefix: \"/pgadmin\", upstream: \"http://127.0.0.1:8083\"}", 1), "duplicate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLocalModeBcryptValidation(t *testing.T) {
	hash := testHash(t)
	base := `
session:
  cookieSecrets: ["secret-one-abcdefghij"]
provider:
  mode: local
users:
  - {subject: "jane@corp.example", level: dba, localPasswordBcrypt: "` + hash + `"}
routes:
  - {pathPrefix: "/", upstream: "http://127.0.0.1:8082"}
`
	if _, _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("valid local config: %v", err)
	}
	// Missing hash.
	missing := strings.Replace(base, `, localPasswordBcrypt: "`+hash+`"`, "", 1)
	if _, _, err := Parse([]byte(missing)); err == nil || !strings.Contains(err.Error(), "localPasswordBcrypt") {
		t.Fatalf("missing hash: err = %v", err)
	}
	// Malformed hash.
	malformed := strings.Replace(base, hash, "not-a-bcrypt-hash", 1)
	if _, _, err := Parse([]byte(malformed)); err == nil || !strings.Contains(err.Error(), "bcrypt") {
		t.Fatalf("malformed hash: err = %v", err)
	}
	// Hash is optional in oidc mode.
	if _, _, err := Parse([]byte(validOIDC)); err != nil {
		t.Fatalf("oidc mode without hashes should pass: %v", err)
	}
}

func TestNonLoopbackUpstreamWarns(t *testing.T) {
	yaml := strings.Replace(validOIDC, "http://127.0.0.1:8081", "http://pgadmin.other-ns.svc:8081", 1)
	_, warns, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "not a loopback") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected non-loopback warning, got %v", warns)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	yaml := validOIDC + "bogusField: true\n"
	if _, _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	yaml := strings.Replace(validOIDC, `cookieSecrets: ["secret-one-abcdefghij"]`,
		"cookieSecrets: [\"secret-one-abcdefghij\"]\n  maxAge: \"banana\"", 1)
	if _, _, err := Parse([]byte(yaml)); err == nil {
		t.Fatal("invalid maxAge accepted")
	}
}

func TestErrorMessagesContainNoSecrets(t *testing.T) {
	yaml := strings.Replace(validOIDC, "secret-one-abcdefghij", "ab12cd34", 1)
	_, _, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "ab12cd34") {
		t.Fatalf("error message leaks secret value: %q", err)
	}
}
