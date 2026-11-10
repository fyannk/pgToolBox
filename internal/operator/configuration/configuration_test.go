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

package configuration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func env(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := values[key]; return v, ok }
}

func newReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

var objects = Objects{Namespace: "ops", ConfigMap: "cfg", Secret: "sec"}

func TestLoadOverlayOrder(t *testing.T) {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: "ops"},
		Data:       map[string]string{"PGADMIN_IMAGE": "from/configmap:1", "MAX_CONCURRENT_RECONCILES": "8", "OAUTH_PROXY_IMAGE": "cm/proxy:1"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "ops"},
		Data:       map[string][]byte{"OAUTH_PROXY_IMAGE": []byte("secret/proxy:1")},
	}
	loaded, err := Load(context.Background(), newReader(t, configMap, secret),
		env(map[string]string{"OPERATOR_IMAGE": "op:1", "PGADMIN_IMAGE": "from/env:1", "WATCH_NAMESPACES": "b, a,b"}), objects)
	if err != nil {
		t.Fatal(err)
	}
	c := loaded.Config
	if c.PgAdminImage != "from/configmap:1" || loaded.Sources["PGADMIN_IMAGE"] != SourceConfigMap {
		t.Errorf("configmap must win over env: %q (%s)", c.PgAdminImage, loaded.Sources["PGADMIN_IMAGE"])
	}
	if c.OAuthProxyImage != "secret/proxy:1" || loaded.Sources["OAUTH_PROXY_IMAGE"] != SourceSecret {
		t.Errorf("secret must win: %q", c.OAuthProxyImage)
	}
	if c.MaxConcurrentReconciles != 8 || c.OperatorImage != "op:1" {
		t.Errorf("got %+v", c)
	}
	if strings.Join(c.WatchNamespaces, ",") != "a,b" {
		t.Errorf("namespaces %v", c.WatchNamespaces)
	}
	if c.ReconciliationTimeout != 2*time.Minute || loaded.Sources["RECONCILIATION_TIMEOUT"] != SourceDefault {
		t.Errorf("default lost: %s", c.ReconciliationTimeout)
	}
	for _, kv := range loaded.Effective() {
		if kv[0] == "OAUTH_PROXY_IMAGE" && kv[1] != Redacted {
			t.Errorf("secret-sourced value exported: %q", kv[1])
		}
	}
	if err := loaded.RegisterInfoMetric(prometheus.NewRegistry()); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAbsentObjectsAreOptional(t *testing.T) {
	if _, err := Load(context.Background(), newReader(t), env(map[string]string{"OPERATOR_IMAGE": "op:1"}), objects); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReadErrorFailsTheStart(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("connection refused")
		},
	}).Build()
	if _, err := Load(context.Background(), reader, env(map[string]string{"OPERATOR_IMAGE": "op:1"}), objects); err == nil {
		t.Fatal("a read error other than NotFound must fail the start")
	}
}

func TestLoadValidationIsOneStructuredError(t *testing.T) {
	_, err := Load(context.Background(), newReader(t), env(map[string]string{
		"MAX_CONCURRENT_RECONCILES":      "zero",
		"RECONCILIATION_TIMEOUT":         "1s",
		"WATCH_NAMESPACES":               "Bad_NS",
		"LEADER_ELECTION_RENEW_DEADLINE": "1m",
	}), objects)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("error %v is not an InvalidError", err)
	}
	for _, want := range []string{"MAX_CONCURRENT_RECONCILES", "RECONCILIATION_TIMEOUT", "WATCH_NAMESPACES", "leader election", "OPERATOR_IMAGE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// FuzzLoadEnvironment feeds arbitrary values to every key: Load must never
// panic, and a configuration it accepts must satisfy its own invariants.
func FuzzLoadEnvironment(f *testing.F) {
	f.Add("4", "2m", "a,b", "30s")
	f.Add("-1", "x", "", "0")
	f.Fuzz(func(t *testing.T, workers, timeout, namespaces, lease string) {
		loaded, err := Load(context.Background(), newReader(t), env(map[string]string{
			"OPERATOR_IMAGE": "op:1", "MAX_CONCURRENT_RECONCILES": workers,
			"RECONCILIATION_TIMEOUT": timeout, "WATCH_NAMESPACES": namespaces,
			"LEADER_ELECTION_LEASE_DURATION": lease,
		}), objects)
		if err != nil {
			return
		}
		c := loaded.Config
		if c.MaxConcurrentReconciles < 1 || c.ReconciliationTimeout < 10*time.Second || c.RenewDeadline >= c.LeaseDuration {
			t.Fatalf("accepted an invalid configuration: %+v", c)
		}
	})
}

// TestOverrideFlag: a flag wins over every loaded source, is validated,
// and says "flag" as its source; an empty flag changes nothing.
func TestOverrideFlag(t *testing.T) {
	loaded := Loaded{Config: Defaults(), Sources: map[string]Source{"PGADMIN_IMAGE_REPOSITORIES": SourceEnv}}
	loaded.Config.OperatorImage = "registry.example/pgtoolbox:1"
	loaded.Config.PgAdminImageRepositories = []string{"from-env/"}
	if err := loaded.OverrideFlag("PGADMIN_IMAGE_REPOSITORIES", ""); err != nil || loaded.Config.PgAdminImageRepositories[0] != "from-env/" {
		t.Fatalf("an empty flag changed the value: %v %v", loaded.Config.PgAdminImageRepositories, err)
	}
	if err := loaded.OverrideFlag("PGADMIN_IMAGE_REPOSITORIES", " mirror.example/ , registry.example/pgadmin "); err != nil {
		t.Fatal(err)
	}
	if got := loaded.Config.PgAdminImageRepositories; len(got) != 2 || got[0] != "mirror.example/" || got[1] != "registry.example/pgadmin" {
		t.Fatalf("repositories = %q", got)
	}
	if loaded.Sources["PGADMIN_IMAGE_REPOSITORIES"] != SourceFlag {
		t.Fatalf("source = %s", loaded.Sources["PGADMIN_IMAGE_REPOSITORIES"])
	}
	if err := loaded.OverrideFlag("NO_SUCH_KEY", "x"); err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if err := loaded.OverrideFlag("MAX_CONCURRENT_RECONCILES", "0"); err == nil {
		t.Fatal("an invalid flag value was accepted")
	}
}

// TestDefaultPgAdminImageIsTheHardenedBuild: a PgAdmin without spec.image
// runs the hardened image built from the project's own pgAdmin repository
// (github.com/fyannk/pgAdmin, mirrored from github.com/fyannk/pgAdmin),
// pinned by digest, never the upstream dpage/pgadmin4.
func TestDefaultPgAdminImageIsTheHardenedBuild(t *testing.T) {
	const repository = "ghcr.io/fyannk/pgadmin:"
	if !strings.HasPrefix(DefaultPgAdminImage, repository) {
		t.Fatalf("DefaultPgAdminImage = %s, want the project's own image %s…", DefaultPgAdminImage, repository)
	}
	if !strings.Contains(DefaultPgAdminImage, "-hardened@sha256:") {
		t.Fatalf("DefaultPgAdminImage = %s, want a hardened tag pinned by digest", DefaultPgAdminImage)
	}
}

func TestExposureAllowedHostSuffixes(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        []string
		invalid     bool
	}{
		{"unset keeps any hostname", "", nil, false},
		{"a list, lowered, deduplicated and sorted", " Apps.Example.com, .lab.internal ,apps.example.com", []string{".lab.internal", "apps.example.com"}, false},
		{"a trailing dot", "apps.example.com.", []string{"apps.example.com."}, false},
		{"a wildcard is not a suffix", "*.example.com", nil, true},
		{"a URL is not a suffix", "https://apps.example.com", nil, true},
		{"a bare dot", ".", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{"OPERATOR_IMAGE": "op:1"}
			if tc.value != "" {
				values["EXPOSURE_ALLOWED_HOST_SUFFIXES"] = tc.value
			}
			loaded, err := Load(context.Background(), newReader(t), env(values), objects)
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "EXPOSURE_ALLOWED_HOST_SUFFIXES") {
					t.Fatalf("Load = %v, want a problem naming EXPOSURE_ALLOWED_HOST_SUFFIXES", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Config.ExposureAllowedHostSuffixes; !slices.Equal(got, tc.want) {
				t.Fatalf("suffixes = %q, want %q", got, tc.want)
			}
		})
	}
}
