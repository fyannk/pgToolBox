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

package console

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

var updateGolden = flag.Bool("update", false, "rewrite the rendered-object golden files")

// TestWorkloadGolden pins the console workload byte for byte for each
// family of optional settings: a StatefulSet with persistent history (the
// default), a Deployment without. The chart parity test compares env,
// mounts and volumes with the chart's rendering where helm is installed;
// this one pins the whole object without helm, so a refactor of the
// builders that reorders an env var or drops a probe fails here. Rewrite
// with -update.
func TestWorkloadGolden(t *testing.T) {
	t.Parallel()
	image := specs.ResolveImage(nil, "registry.example/pgtoolbox-console:1.2.0")
	cases := map[string]struct {
		mutate func(*pgtoolboxv1alpha1.PgToolBoxConsole)
		in     deploymentInputs
	}{
		"defaults":  {},
		"in-memory": {mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) { c.Spec.History.Persistent = new(false) }},
		"oidc-in-memory": {mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.History.Persistent = new(false)
			c.Spec.ClusterSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}}
			c.Spec.Auth = pgtoolboxv1alpha1.PgToolBoxConsoleAuthSpec{
				Provider: pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC,
				OIDC: &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec{
					IssuerURL: "https://sso.example/realms/a", ClientID: "console",
					ClientSecretRef:      pgtoolboxv1alpha1.SecretKeySelector{Name: "oidc"},
					RedirectURL:          "https://console.example/auth/callback",
					Scopes:               []string{"openid", "email"},
					CAConfigMapRef:       &pgtoolboxv1alpha1.LocalObjectReference{Name: "sso-ca"},
					RequireEmailVerified: new(false),
					GrantClient: &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCClient{
						ClientID: "console-grants", ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "oidc-grants"},
					},
				},
				TokenLogin:         new(false),
				SessionIdleTimeout: &metav1.Duration{Duration: 30 * time.Minute},
				SessionMaxAge:      &metav1.Duration{Duration: 12 * time.Hour},
			}
			c.Spec.PublicURL = "https://console.example/"
			c.Spec.Evidence.Enabled = new(false)
			c.Spec.Operations = new(false)
			c.Spec.PodLogs = new(false)
			c.Spec.MonitoringURL = "https://grafana.example"
		}},
		"openshift-prometheus-secret": {
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				c.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift
				c.Spec.Auth.OpenShift = &pgtoolboxv1alpha1.PgToolBoxConsoleOpenShiftSpec{
					ClientSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "oauth", Key: "secret"},
					CAConfigMapRef:  &pgtoolboxv1alpha1.LocalObjectReference{Name: "oauth-ca"},
				}
				c.Spec.Exposure.Type = pgtoolboxv1alpha1.ExposureTypeRoute
				c.Spec.Exposure.Hostname = "console.apps.example"
				c.Spec.Evidence.Interval = &metav1.Duration{Duration: 10 * time.Minute}
				c.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{
					URL:            "https://thanos.example",
					TokenSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "prom", Key: "token"},
					CAConfigMapRef: &pgtoolboxv1alpha1.LocalObjectReference{Name: "prom-ca"},
					Namespaced:     true,
					Range:          &metav1.Duration{Duration: 6 * time.Hour},
				}
				c.Spec.Replicas = new(int32(3))
			},
			in: deploymentInputs{OpenShift: true},
		},
		"openshift-without-ca-projected-token-history": {mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift
			c.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{URL: "https://prometheus.example", ProjectedToken: true}
			c.Spec.History.Persistent = new(true)
			c.Spec.History.Size = new(resource.MustParse("4Gi"))
			c.Spec.History.StorageClassName = new("fast")
			c.Spec.Resources = &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			}
		}},
		"history-on-openshift": {
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) { c.Spec.History.Persistent = new(true) },
			in:     deploymentInputs{OpenShift: true},
		},
		"single-replica-digest-delivery": {mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
			c.Spec.Replicas = new(int32(1))
			c.Spec.Digest = pgtoolboxv1alpha1.PgToolBoxConsoleDigestSpec{
				Enabled: new(true), Hour: new(int32(7)),
				Delivery: &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
					WebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "hook", Key: "url"},
				},
			}
			c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
			c.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
				Enabled: true, WriteTools: true, MaxResultBytes: new(int32(1 << 20)),
				CallTimeout:                  &metav1.Duration{Duration: 45 * time.Second},
				OAuthAccessTTL:               &metav1.Duration{Duration: 15 * time.Minute},
				OAuthClientMetadataDocuments: true,
				OAuthClientMetadataHosts:     []string{"claude.ai", "chatgpt.com"},
			}
			c.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{
				Endpoint: "https://api.smith.langchain.com/otel", ServiceName: "pgtoolbox-console-team-a",
				HeadersSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "langsmith", Key: "headers"},
				Sampler:          "parentbased_traceidratio", SamplerArg: "0.25", IncludePayloads: true,
			}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
			if tc.mutate != nil {
				tc.mutate(console)
			}
			in := tc.in
			in.Image = image
			got, err := yaml.Marshal(buildWorkload(console, in))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", "workload-"+name+".golden.yaml")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file (run with -update to accept)", path)
			}
		})
	}
}

// TestMCPEnv pins spec.mcp's rendering: nothing while the endpoint is off
// (whatever else is set, like the chart), and each tuning knob only when
// set, so the console's own default applies otherwise.
func TestMCPEnv(t *testing.T) {
	t.Parallel()
	knobs := func(enabled bool) *pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec {
		return &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
			Enabled:                      enabled,
			MaxResultBytes:               new(int32(1 << 20)),
			CallTimeout:                  &metav1.Duration{Duration: 45 * time.Second},
			OAuthAccessTTL:               &metav1.Duration{Duration: 15 * time.Minute},
			OAuthClientMetadataDocuments: true,
			OAuthClientMetadataHosts:     []string{"claude.ai", "chatgpt.com"},
		}
	}
	cases := map[string]struct {
		mcp  *pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec
		want []string
	}{
		"absent":                     {},
		"disabled":                   {mcp: &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{}},
		"knobs without the endpoint": {mcp: knobs(false)},
		"enabled, console defaults": {
			mcp:  &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true},
			want: []string{"MCP_ENABLED=true"},
		},
		"write tools": {
			mcp:  &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true, WriteTools: true},
			want: []string{"MCP_ENABLED=true", "MCP_WRITE_TOOLS=true"},
		},
		"every knob": {
			mcp: knobs(true),
			want: []string{
				"MCP_ENABLED=true",
				"MCP_MAX_RESULT_BYTES=1048576",
				"MCP_CALL_TIMEOUT=45s",
				"MCP_OAUTH_ACCESS_TTL=15m",
				"MCP_OAUTH_CLIENT_METADATA_DOCUMENTS=true",
				"MCP_OAUTH_CLIENT_METADATA_HOSTS=claude.ai,chatgpt.com",
			},
		},
		"metadata documents from any public address": {
			mcp:  &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true, OAuthClientMetadataDocuments: true},
			want: []string{"MCP_ENABLED=true", "MCP_OAUTH_CLIENT_METADATA_DOCUMENTS=true"},
		},
		"metadata hosts without the documents switch": {
			mcp: &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
				Enabled: true, OAuthClientMetadataHosts: []string{"claude.ai"},
			},
			want: []string{"MCP_ENABLED=true", "MCP_OAUTH_CLIENT_METADATA_HOSTS=claude.ai"},
		},
		"metadata documents without the endpoint": {
			mcp: &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataDocuments: true},
		},
		"hour-long TTL in the console's spelling": {
			mcp: &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
				Enabled: true, OAuthAccessTTL: &metav1.Duration{Duration: time.Hour},
			},
			want: []string{"MCP_ENABLED=true", "MCP_OAUTH_ACCESS_TTL=1h"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{}
			c.Spec.MCP = tc.mcp
			envs := mcpEnv(c)
			got := make([]string, 0, len(envs))
			for _, e := range envs {
				if e.ValueFrom != nil {
					t.Fatalf("%s is rendered from a reference", e.Name)
				}
				got = append(got, e.Name+"="+e.Value)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("env %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrustedProxyHops: an exposed console trusts the one hop of its
// Route, Ingress or Gateway, so that each client has its own sign-in
// budget; an unexposed one trusts no forwarded header.
func TestTrustedProxyHops(t *testing.T) {
	t.Parallel()
	cases := map[pgtoolboxv1alpha1.ExposureType]string{
		"":                                      "",
		pgtoolboxv1alpha1.ExposureTypeClusterIP: "",
		pgtoolboxv1alpha1.ExposureTypeRoute:     "1",
		pgtoolboxv1alpha1.ExposureTypeIngress:   "1",
		pgtoolboxv1alpha1.ExposureTypeGateway:   "1",
	}
	for typ, want := range cases {
		t.Run(string(typ), func(t *testing.T) {
			t.Parallel()
			c := &pgtoolboxv1alpha1.PgToolBoxConsole{}
			c.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{Type: typ, Hostname: "console.example"}
			got, n := "", 0
			for _, e := range consoleEnv(c) {
				if e.Name == "TRUSTED_PROXY_HOPS" {
					got, n = e.Value, n+1
				}
			}
			if got != want || n > 1 {
				t.Errorf("TRUSTED_PROXY_HOPS = %q (%d times), want %q", got, n, want)
			}
		})
	}
}
