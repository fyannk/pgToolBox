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

package passfilesync

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

const (
	caPEM1 = "-----BEGIN CERTIFICATE-----\nCLUSTER-ONE\n-----END CERTIFICATE-----\n"
	caPEM2 = "-----BEGIN CERTIFICATE-----\nCLUSTER-TWO\n-----END CERTIFICATE-----\n"
	caKey  = "-----BEGIN EC PRIVATE KEY-----\nSIGNING-KEY-MUST-NOT-LEAK\n-----END EC PRIVATE KEY-----\n"
)

func server(cluster, user, secret, version string) offer.Server {
	return offer.Server{
		Name: cluster + " rw (" + user + ")", Cluster: cluster, Endpoint: "rw",
		Host: cluster + "-rw.team.svc", Port: 5432, DBName: "app", User: user,
		Service: "pgtoolbox-" + cluster + "-" + user, SecretName: secret, SecretResourceVersion: version,
		CASecretName: cluster + "-ca",
	}
}

func secret(name, version string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", ResourceVersion: version}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func caSecret(name, pem string) *corev1.Secret {
	return secret(name, "1", map[string]string{"ca.crt": pem, "ca.key": caKey})
}

func baseSecrets() map[string]*corev1.Secret {
	return map[string]*corev1.Secret{
		"one-app":  secret("one-app", "5", map[string]string{"username": "app", "password": "pw-one"}),
		"two-app":  secret("two-app", "6", map[string]string{"username": "app", "password": "pw-two"}),
		"owner-pw": secret("owner-pw", "42", map[string]string{"username": "owner", "password": "pw-owner"}),
		"one-ca":   caSecret("one-ca", caPEM1),
		"two-ca":   caSecret("two-ca", caPEM2),
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRenderWritesRestrictedFiles(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	doc := offer.Document{Version: offer.Version, Servers: []offer.Server{
		server("one", "app", "one-app", ""),
		server("two", "app", "two-app", ""),
		server("two", "owner", "owner-pw", "42"),
	}}
	outcome, err := Render(files, doc, baseSecrets())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Offered != 3 || len(outcome.Refused) != 0 {
		t.Fatalf("outcome = %+v", outcome)
	}
	service := read(t, files.ServiceFile())
	for _, want := range []string{
		"[pgtoolbox-one-app]", "[pgtoolbox-two-owner]",
		"sslrootcert=" + offer.CAFile(files.Dir, "one"),
		"sslrootcert=" + offer.CAFile(files.Dir, "two"),
		"password=pw-owner", "sslmode=verify-full",
	} {
		if !strings.Contains(service, want) {
			t.Errorf("service file lacks %q:\n%s", want, service)
		}
	}
	if pgpass := read(t, files.PassFile()); !strings.Contains(pgpass, "one-rw.team.svc:5432:*:app:pw-one") {
		t.Errorf("pgpass = %q", pgpass)
	}
	if got := read(t, offer.CAFile(files.Dir, "one")); got != caPEM1 {
		t.Errorf("CA of one = %q", got)
	}
	if got := read(t, offer.CABundleFile(files.Dir)); got != caPEM1+caPEM2 {
		t.Errorf("bundle = %q", got)
	}
	err = filepath.WalkDir(files.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", path, info.Mode().Perm())
		}
		if strings.Contains(read(t, path), "SIGNING-KEY") {
			t.Errorf("%s carries the CA signing key", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderRefusals(t *testing.T) {
	cases := []struct {
		name    string
		server  offer.Server
		secrets func(map[string]*corev1.Secret)
		want    string
	}{
		{"absent secret", server("one", "app", "missing", ""), nil, "absent or not readable"},
		{"wrong resource version", server("two", "owner", "owner-pw", "41"), nil, "not at the version"},
		{"username mismatch", server("one", "app", "one-app", ""), func(s map[string]*corev1.Secret) {
			s["one-app"].Data["username"] = []byte("postgres")
		}, "names another role"},
		{"empty password", server("one", "app", "one-app", ""), func(s map[string]*corev1.Secret) {
			s["one-app"].Data["password"] = nil
		}, "has no password"},
		{"missing CA", server("one", "app", "one-app", ""), func(s map[string]*corev1.Secret) {
			delete(s["one-ca"].Data, "ca.crt")
		}, "has no ca.crt"},
		{"CA secret absent", server("one", "app", "one-app", ""), func(s map[string]*corev1.Secret) {
			delete(s, "one-ca")
		}, "CA Secret one-ca is absent"},
		{"non-PEM CA", server("one", "app", "one-app", ""), func(s map[string]*corev1.Secret) {
			s["one-ca"].Data["ca.crt"] = []byte("garbage")
		}, "not PEM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := Files{Dir: t.TempDir()}
			secrets := baseSecrets()
			if tc.secrets != nil {
				tc.secrets(secrets)
			}
			outcome, err := Render(files, offer.Document{Version: offer.Version, Servers: []offer.Server{tc.server}}, secrets)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Offered != 0 || len(outcome.Refused) != 1 || !strings.Contains(outcome.Refused[0], tc.want) {
				t.Fatalf("outcome = %+v, want one refusal containing %q", outcome, tc.want)
			}
			if !strings.HasPrefix(outcome.Refused[0], tc.server.Name+":") {
				t.Errorf("refusal does not name the server: %q", outcome.Refused[0])
			}
			for _, pw := range []string{"pw-one", "pw-two", "pw-owner"} {
				if strings.Contains(strings.Join(outcome.Refused, " "), pw) {
					t.Fatalf("refusal carries a password: %q", outcome.Refused)
				}
			}
			if service := read(t, files.ServiceFile()); strings.Contains(service, "[") {
				t.Errorf("refused server still in the service file:\n%s", service)
			}
		})
	}
}

func TestRenderRemovesCAOfDroppedCluster(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	both := offer.Document{Version: offer.Version, Servers: []offer.Server{
		server("one", "app", "one-app", ""), server("two", "app", "two-app", ""),
	}}
	if _, err := Render(files, both, baseSecrets()); err != nil {
		t.Fatal(err)
	}
	only := offer.Document{Version: offer.Version, Servers: []offer.Server{server("one", "app", "one-app", "")}}
	if _, err := Render(files, only, baseSecrets()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(offer.CAFile(files.Dir, "two")); !os.IsNotExist(err) {
		t.Fatalf("CA of the dropped cluster remains: %v", err)
	}
	if _, err := os.Stat(offer.CABundleFile(files.Dir)); err != nil {
		t.Fatalf("bundle removed: %v", err)
	}
	if strings.Contains(read(t, files.PassFile()), "pw-two") {
		t.Fatal("dropped server still offered")
	}
}

func TestRenderEmptyOffer(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	outcome, err := Render(files, offer.Document{Version: offer.Version}, nil)
	if err != nil || outcome.Offered != 0 {
		t.Fatalf("%+v %v", outcome, err)
	}
	if read(t, files.PassFile()) != "" {
		t.Fatal("pgpass not empty")
	}
}

func TestRenderRefusesOversizedSecret(t *testing.T) {
	files := Files{Dir: t.TempDir()}
	secrets := baseSecrets()
	secrets["one-app"].Data["padding"] = []byte(strings.Repeat("x", MaxSecretBytes))
	outcome, err := Render(files, offer.Document{Version: offer.Version, Servers: []offer.Server{server("one", "app", "one-app", "")}}, secrets)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Offered != 0 || !strings.Contains(strings.Join(outcome.Refused, ""), "too large") {
		t.Fatalf("outcome = %+v", outcome)
	}
}
