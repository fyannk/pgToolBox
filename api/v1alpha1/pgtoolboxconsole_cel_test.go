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

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// TestPgToolBoxConsoleCEL applies PgToolBoxConsoles to a real API server and checks the
// CRD's admission rules (and that their cost fits the budget).
func TestPgToolBoxConsoleCEL(t *testing.T) {
	c := apiServer(t)
	valid := func() *pgtoolboxv1alpha1.PgToolBoxConsole {
		return &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "default"}}
	}
	oidc := func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
		p.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC
		p.Spec.Auth.OIDC = &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec{
			IssuerURL: "https://idp.example", ClientID: "pgtoolbox-console",
			ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgtoolbox-console-oidc"},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*pgtoolboxv1alpha1.PgToolBoxConsole)
		wantErr string
	}{
		{name: "minimal"},
		{name: "oidc on an ingress", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			oidc(p)
			p.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "console.example"}
		}},
		{name: "oidc without its settings", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC
		}, wantErr: "auth.oidc is required"},
		{name: "oidc with a grant client", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			oidc(p)
			p.Spec.Auth.OIDC.GrantClient = &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCClient{
				ClientID: "pgtoolbox-console-grants", ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgtoolbox-console-grants"},
			}
		}},
		{name: "oidc grant client is the console's own", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			oidc(p)
			p.Spec.Auth.OIDC.GrantClient = &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCClient{
				ClientID: "pgtoolbox-console", ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgtoolbox-console-grants"},
			}
		}, wantErr: "oidc.grantClient.clientID must differ from oidc.clientID"},
		{name: "openshift with a route", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift
			p.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "console.apps.example"}
		}},
		{name: "route with its default policy and named proxies", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeRoute, Hostname: "console.apps.example"}
			p.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{Enabled: new(true)}
			p.Spec.Exposure.TrustedProxy = &pgtoolboxv1alpha1.PgToolBoxConsoleTrustedProxySpec{Hops: new(int32(2)), CIDRs: []string{"10.128.0.0/14", "fd00::/64"}}
		}},
		{name: "ingress policy without peers", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "console.example"}
			p.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{Enabled: new(true)}
		}, wantErr: "networkPolicy.enabled needs networkPolicy.from"},
		{name: "ingress policy with its controller's namespace", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.ExposureSpec = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "console.example"}
			p.Spec.Exposure.NetworkPolicy = &pgtoolboxv1alpha1.PgToolBoxConsoleExposurePolicySpec{Enabled: new(true), From: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ingress-nginx"}},
			}}}
		}},
		{name: "trusted proxy that is not a CIDR", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.TrustedProxy = &pgtoolboxv1alpha1.PgToolBoxConsoleTrustedProxySpec{CIDRs: []string{"the-router"}}
		}, wantErr: "trustedProxy.cidrs entries must be CIDRs"},
		{name: "too many trusted hops", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.TrustedProxy = &pgtoolboxv1alpha1.PgToolBoxConsoleTrustedProxySpec{Hops: new(int32(9))}
		}, wantErr: "spec.exposure.trustedProxy.hops"},
		{name: "openshift with nowhere to return to", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOpenShift
		}, wantErr: "needs publicURL or exposure.hostname"},
		{name: "public URL in clear", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) { p.Spec.PublicURL = "http://console.example" }, wantErr: "spec.publicURL"},
		{name: "prometheus without a token, in clear", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{URL: "http://prometheus:9090"}
		}},
		{name: "prometheus projected token", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{URL: "https://thanos-querier:9091", ProjectedToken: true, Namespaced: true}
		}},
		{name: "prometheus token in clear", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{URL: "http://prometheus:9090", ProjectedToken: true}
		}, wantErr: "token is never sent in clear"},
		{name: "prometheus two token sources", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Prometheus = &pgtoolboxv1alpha1.PgToolBoxConsolePrometheusSpec{
				URL: "https://prometheus", ProjectedToken: true,
				TokenSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "prom", Key: "token"},
			}
		}, wantErr: "not both"},
		{name: "idle timeout above max age", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Auth.SessionIdleTimeout = &metav1.Duration{Duration: 2 * time.Hour}
			p.Spec.Auth.SessionMaxAge = &metav1.Duration{Duration: time.Hour}
		}, wantErr: "must not exceed sessionMaxAge"},
		{name: "evidence too fast", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Evidence.Interval = &metav1.Duration{Duration: 5 * time.Second}
		}, wantErr: "between 30s and 1h"},
		{name: "too many replicas", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) { six := int32(6); p.Spec.Replicas = &six }, wantErr: "spec.replicas"},
		{name: "monitoring link in clear", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) { p.Spec.MonitoringURL = "http://grafana" }, wantErr: "https or root-relative"},
		{name: "digest delivered with persistent history", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(true)
			p.Spec.Digest.Hour = new(int32(6))
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
				ChatWebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-chat", Key: "url"},
				SMTP: &pgtoolboxv1alpha1.PgToolBoxConsoleDigestSMTPSpec{
					Address: "smtp.example.com:587", From: "pgtoolbox-console@example.com", To: []string{"dba@example.com"},
					Username: "pgtoolbox-console", PasswordSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-smtp", Key: "password"},
				},
			}
		}},
		// history.persistent defaults to true, so a delivery with
		// nothing said about the history is valid.
		{name: "digest delivered with the default history", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
				WebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-webhook", Key: "url"},
			}
		}},
		{name: "digest delivered with in-memory history", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(false)
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
				WebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-webhook", Key: "url"},
			}
		}, wantErr: "digest.delivery needs history.persistent"},
		{name: "digest delivery switched off", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(true)
			p.Spec.Digest.Enabled = new(false)
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
				WebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-webhook", Key: "url"},
			}
		}, wantErr: "digest.delivery needs digest.enabled"},
		{name: "digest delivery naming no channel", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(true)
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{}
		}, wantErr: "names no channel"},
		{name: "digest mail in clear", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(true)
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{SMTP: &pgtoolboxv1alpha1.PgToolBoxConsoleDigestSMTPSpec{
				Address: "smtp.example.com:25", TLS: "none", From: "a@example.com", To: []string{"b@example.com"},
			}}
		}, wantErr: "spec.digest.delivery.smtp.tls"},
		{name: "digest mail user without password", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.History.Persistent = new(true)
			p.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{SMTP: &pgtoolboxv1alpha1.PgToolBoxConsoleDigestSMTPSpec{
				Address: "smtp.example.com:587", Username: "pgtoolbox-console", From: "a@example.com", To: []string{"b@example.com"},
			}}
		}, wantErr: "set together"},
		{name: "digest hour 24", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) { p.Spec.Digest.Hour = new(int32(24)) }, wantErr: "spec.digest.hour"},
		{name: "restore drills", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			cpu, more, size := resource.MustParse("500m"), resource.MustParse("1"), resource.MustParse("20Gi")
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{
				Enabled: true, Prefix: "rd-", TimeLimit: &metav1.Duration{Duration: 3 * time.Hour},
				Resources: &pgtoolboxv1alpha1.PgToolBoxConsoleDrillResources{
					Requests: &pgtoolboxv1alpha1.PgToolBoxConsoleDrillQuantities{CPU: &cpu},
					Limits:   &pgtoolboxv1alpha1.PgToolBoxConsoleDrillQuantities{CPU: &more},
				},
				Storage: pgtoolboxv1alpha1.PgToolBoxConsoleDrillStorage{Size: &size, StorageClass: "fast-ssd"},
			}
		}},
		{name: "mcp endpoint", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.PublicURL = "https://console.example"
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true}
		}},
		{name: "mcp endpoint on the exposure host", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Exposure.Hostname = "console.example"
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true}
		}},
		{name: "mcp endpoint without a public URL", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true}
		}, wantErr: "mcp needs publicURL or exposure.hostname"},
		{name: "mcp write tools with the endpoint", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.PublicURL = "https://console.example"
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true, WriteTools: true}
		}},
		{name: "mcp write tools without the endpoint", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{WriteTools: true}
		}, wantErr: "mcp.writeTools requires mcp.enabled"},
		// The tuning knobs, bounded as the console reads them
		// (internal/console/config/mcp.go).
		{name: "mcp tuning knobs at their bounds", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.PublicURL = "https://console.example"
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
				Enabled: true, MaxResultBytes: new(int32(16 << 10)),
				CallTimeout:                  &metav1.Duration{Duration: 50 * time.Second},
				OAuthAccessTTL:               &metav1.Duration{Duration: 10 * time.Second},
				OAuthClientMetadataDocuments: true,
				OAuthClientMetadataHosts:     []string{"claude.ai", "chatgpt.com"},
			}
		}},
		// No rule ties the metadata switch to its host list or to the
		// endpoint: like the other knobs, each is used only with enabled,
		// and the hosts only with the switch, so neither is refused.
		{name: "mcp metadata hosts without the documents switch", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataHosts: []string{"claude.ai"}}
		}},
		{name: "mcp metadata documents without the endpoint", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataDocuments: true}
		}},
		{name: "mcp result cap at the maximum", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{MaxResultBytes: new(int32(4 << 20))}
		}},
		{name: "mcp result cap below the minimum", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{MaxResultBytes: new(int32(16<<10 - 1))}
		}, wantErr: "spec.mcp.maxResultBytes"},
		{name: "mcp result cap above the maximum", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{MaxResultBytes: new(int32(4<<20 + 1))}
		}, wantErr: "spec.mcp.maxResultBytes"},
		{name: "mcp call timeout below 1s", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{CallTimeout: &metav1.Duration{Duration: 500 * time.Millisecond}}
		}, wantErr: "mcp.callTimeout must be between 1s and 50s"},
		{name: "mcp call timeout above 50s", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{CallTimeout: &metav1.Duration{Duration: time.Minute}}
		}, wantErr: "mcp.callTimeout must be between 1s and 50s"},
		{name: "mcp access TTL below 10s", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthAccessTTL: &metav1.Duration{Duration: 9 * time.Second}}
		}, wantErr: "mcp.oauthAccessTTL must be between 10s and 1h"},
		{name: "mcp access TTL above 1h", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthAccessTTL: &metav1.Duration{Duration: time.Hour + time.Second}}
		}, wantErr: "mcp.oauthAccessTTL must be between 10s and 1h"},
		{name: "mcp metadata host that is a URL", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataHosts: []string{"https://claude.ai"}}
		}, wantErr: "spec.mcp.oauthClientMetadataHosts[0]"},
		{name: "mcp metadata host in upper case", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataHosts: []string{"Claude.ai"}}
		}, wantErr: "spec.mcp.oauthClientMetadataHosts[0]"},
		{name: "mcp metadata host listed twice", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{OAuthClientMetadataHosts: []string{"claude.ai", "claude.ai"}}
		}, wantErr: "spec.mcp.oauthClientMetadataHosts[1]"},
		{name: "tracing with every setting", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{
				Endpoint: "https://api.smith.langchain.com/otel", ServiceName: "pgtoolbox-console-team-a",
				HeadersSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "langsmith", Key: "headers"},
				Sampler:          "parentbased_traceidratio", SamplerArg: "0.25", IncludePayloads: true,
			}
		}},
		{name: "tracing without an endpoint", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{IncludePayloads: true}
		}, wantErr: "spec.tracing.endpoint"},
		{name: "tracing endpoint with user information", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{Endpoint: "https://u:p@collector:4318"}
		}, wantErr: "spec.tracing.endpoint"},
		{name: "tracing sampler argument without a ratio sampler", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{Endpoint: "http://tempo:4318", Sampler: "always_on", SamplerArg: "0.5"}
		}, wantErr: "tracing.samplerArg needs a traceidratio sampler"},
		{name: "tracing ratio above one", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{Endpoint: "http://tempo:4318", Sampler: "traceidratio", SamplerArg: "1.5"}
		}, wantErr: "spec.tracing.samplerArg"},
		{name: "restore drills without operations", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			off := false
			p.Spec.Operations = &off
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
		}, wantErr: "restoreDrills needs operations"},
		{name: "restore drills off without operations", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			off := false
			p.Spec.Operations = &off
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Prefix: "rd-"}
		}},
		{name: "restore drill time limit too short", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true, TimeLimit: &metav1.Duration{Duration: time.Minute}}
		}, wantErr: "between 10m and 24h"},
		{name: "restore drill prefix", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true, Prefix: "Drill_"}
		}, wantErr: "spec.restoreDrills.prefix"},
		{name: "restore drill cap and NetworkPolicy", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{
				Enabled: true, MaxConcurrent: drillCap(20),
				NetworkPolicy: pgtoolboxv1alpha1.PgToolBoxConsoleDrillNetworkPolicy{
					OperatorNamespace: "cnpg", APIServerCIDRs: []string{"10.0.0.1/32"}, ObjectStorePorts: []int32{9000},
				},
			}
		}},
		{name: "restore drill cap of 0", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true, MaxConcurrent: drillCap(0)}
		}, wantErr: "spec.restoreDrills.maxConcurrent"},
		{name: "restore drill cap above 20", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true, MaxConcurrent: drillCap(21)}
		}, wantErr: "spec.restoreDrills.maxConcurrent"},
		{name: "restore drill API server address that is not a CIDR", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) {
			p.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{
				Enabled:       true,
				NetworkPolicy: pgtoolboxv1alpha1.PgToolBoxConsoleDrillNetworkPolicy{APIServerCIDRs: []string{"api.example.org"}},
			}
		}, wantErr: "apiServerCIDRs"},
		{name: "name too long", mutate: func(p *pgtoolboxv1alpha1.PgToolBoxConsole) { p.Name = "a" + strings.Repeat("b", 40) }, wantErr: "DNS-1035 label of at most 40"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := valid()
			if tc.mutate != nil {
				tc.mutate(obj)
			}
			err := c.Create(context.Background(), obj)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				_ = c.Delete(context.Background(), obj)
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				if err == nil {
					_ = c.Delete(context.Background(), obj)
				}
				t.Fatalf("create error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestPgToolBoxConsoleSchemaDefaults: an empty spec reads back as the chart's
// defaults (two replicas, token sign-in, a ClusterIP Service), and with
// persistent history (the operator's default, not the chart's).
func TestPgToolBoxConsoleSchemaDefaults(t *testing.T) {
	c := apiServer(t)
	obj := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "default"}}
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(context.Background(), obj) }()
	got := &pgtoolboxv1alpha1.PgToolBoxConsole{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("replicas = %v, want 2", got.Spec.Replicas)
	}
	if got.Spec.Auth.Provider != pgtoolboxv1alpha1.PgToolBoxConsoleAuthToken {
		t.Errorf("auth.provider = %q, want token", got.Spec.Auth.Provider)
	}
	if got.Spec.Exposure.Type != pgtoolboxv1alpha1.ExposureTypeClusterIP {
		t.Errorf("exposure.type = %q, want clusterIP", got.Spec.Exposure.Type)
	}
	if p := got.Spec.History.Persistent; p == nil || !*p {
		t.Errorf("history.persistent = %v, want true", p)
	}
	// The explicit opt-out is kept as written.
	memory := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "in-memory", Namespace: "default"}}
	memory.Spec.History.Persistent = new(false)
	if err := c.Create(context.Background(), memory); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Delete(context.Background(), memory) }()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(memory), got); err != nil {
		t.Fatal(err)
	}
	if p := got.Spec.History.Persistent; p == nil || *p || got.HistoryPersistent() {
		t.Errorf("history.persistent = %v, want false", p)
	}
}

func drillCap(n int32) *int32 { return &n }
