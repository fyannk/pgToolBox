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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// The two install paths run the same console: these tests render the chart's
// console half and compare it with what the operator builds for the
// equivalent PgToolBoxConsole. A setting added to one and not the other fails here.

// renderChart runs helm template on the pgtools chart's console; the test is
// skipped where helm is not installed (CI's gate job has it).
func renderChart(t *testing.T, sets ...string) map[string][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	args := make([]string, 0, 11+2*len(sets))
	// A PgToolBoxConsole exists only where the operator runs, so the chart renders as
	// on a cluster that serves its kinds (console.rbac.operatorKinds: auto).
	args = append(args, "template", "console", filepath.Join("..", "..", "..", "charts", "pgtools"), "--namespace", "team-a",
		"--api-versions", "pgtoolbox.fyannk.dev/v1alpha1", "--set", "console.fullnameOverride=console",
		// The operator renders no insight pack, so its console reads the
		// name of deploy/insight-pack; the chart's own is per release.
		"--set", "insightPack.configMapName=pgtoolbox-insight-pack")
	for _, set := range sets {
		args = append(args, "--set", set)
	}
	out, err := exec.CommandContext(t.Context(), "helm", args...).CombinedOutput() // #nosec G204 -- fixed arguments
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	docs := map[string][]byte{}
	for _, doc := range bytes.Split(out, []byte("\n---")) {
		meta := struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}{}
		if err := yaml.Unmarshal(doc, &meta); err != nil || meta.Kind == "" {
			continue
		}
		docs[meta.Kind+"/"+meta.Metadata.Name] = doc
	}
	return docs
}

func chartObject[T any](t *testing.T, docs map[string][]byte, key string) *T {
	t.Helper()
	doc, ok := docs[key]
	if !ok {
		t.Fatalf("the chart renders no %s", key)
	}
	out := new(T)
	if err := yaml.Unmarshal(doc, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ruleSet flattens rules to group/resource/verb triples, so that order and
// grouping do not matter.
func ruleSet(rules []rbacv1.PolicyRule) []string {
	var out []string
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					out = append(out, fmt.Sprintf("%s/%s:%s%v", group, resource, verb, rule.ResourceNames))
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func shippedRules(t *testing.T, file string) []rbacv1.PolicyRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "rbac", file)) // #nosec G304 -- repository manifest
	if err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatal(err)
	}
	return role.Rules
}

// TestShippedRolesAreTheChartRole: pgtoolbox-console is the chart's console
// Role without pod logs, pgtoolbox-console-pod-logs is exactly what pod logs
// add, and pgtoolbox-console-restore-drills is the chart's drill teardown Role.
func TestShippedRolesAreTheChartRole(t *testing.T) {
	console := shippedRules(t, "pgtoolbox_console_clusterrole.yaml")
	podLogs := shippedRules(t, "pgtoolbox_console_pod_logs_clusterrole.yaml")
	without := chartObject[rbacv1.Role](t, renderChart(t, "console.rbac.podLogs=false"), "Role/console")
	if got, want := ruleSet(console), ruleSet(without.Rules); !slices.Equal(got, want) {
		t.Errorf("pgtoolbox-console rules\n got %v\nwant %v (the chart's console Role)", got, want)
	}
	with := chartObject[rbacv1.Role](t, renderChart(t, "console.rbac.podLogs=true"), "Role/console")
	if got, want := ruleSet(append(slices.Clone(console), podLogs...)), ruleSet(with.Rules); !slices.Equal(got, want) {
		t.Errorf("pgtoolbox-console + pod-logs rules\n got %v\nwant %v (the chart's console Role with podLogs)", got, want)
	}
	// The shipped role also carries the operator delete guard's marker
	// (config/admission), which no API serves; the pgtools guard names its
	// console literally and needs none. Everything else is the same.
	marker := rbacv1.PolicyRule{APIGroups: []string{"pgtoolbox.fyannk.dev"}, Resources: []string{"restoredrills"}, Verbs: []string{"teardown"}}
	shipped := shippedRules(t, "pgtoolbox_console_restore_drills_clusterrole.yaml")
	drills := slices.DeleteFunc(slices.Clone(shipped), func(r rbacv1.PolicyRule) bool {
		return slices.Equal(r.APIGroups, marker.APIGroups) && slices.Equal(r.Resources, marker.Resources) && slices.Equal(r.Verbs, marker.Verbs)
	})
	if len(drills) != len(shipped)-1 {
		t.Errorf("pgtoolbox-console-restore-drills lacks the delete guard's marker %v", marker)
	}
	teardown := chartObject[rbacv1.Role](t, renderChart(t, "console.restoreDrills.enabled=true"), "Role/console-restore-drills")
	if got, want := ruleSet(drills), ruleSet(teardown.Rules); !slices.Equal(got, want) {
		t.Errorf("pgtoolbox-console-restore-drills rules\n got %v\nwant %v (the chart's drill teardown Role)", got, want)
	}
}

type mountFact struct{ Name, Path, SubPath string }

func containerFacts(spec *corev1.PodSpec) (env []string, mounts []mountFact, volumes []string) {
	c := spec.Containers[0]
	for _, e := range c.Env {
		value := e.Value
		if e.ValueFrom != nil {
			value = "<ref>"
		}
		env = append(env, e.Name+"="+value)
	}
	for _, m := range c.VolumeMounts {
		mounts = append(mounts, mountFact{m.Name, m.MountPath, m.SubPath})
	}
	for _, v := range spec.Volumes {
		volumes = append(volumes, v.Name)
	}
	return env, mounts, volumes
}

// inMemory is the chart's default history, which a PgToolBoxConsole must ask for.
func inMemory(c *pgtoolboxv1alpha1.PgToolBoxConsole) { c.Spec.History.Persistent = new(false) }

// adminBundle is the chart's instance CA bundle, one the user keeps.
func adminBundle(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
	c.Spec.InstanceCABundleRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "pgtoolbox-console-instance-ca"}
}

// TestDeploymentMatchesTheChart compares the console workload the operator
// builds with the chart's: the container for every family of settings, and
// for persistent history the StatefulSet itself.
func TestDeploymentMatchesTheChart(t *testing.T) {
	cases := []struct {
		name   string
		sets   []string
		mutate func(*pgtoolboxv1alpha1.PgToolBoxConsole)
	}{
		{name: "in-memory history (the chart's default)", mutate: inMemory},
		{
			// Behind an Ingress both trust one forwarded hop, so that the
			// sign-in budgets are per client rather than one for everyone.
			name: "exposed through an Ingress",
			sets: []string{"console.exposure.type=ingress", "console.exposure.host=console.example"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.Exposure = pgtoolboxv1alpha1.ExposureSpec{Type: pgtoolboxv1alpha1.ExposureTypeIngress, Hostname: "console.example"}
			},
		},
		{
			name: "persistent history (the operator's default) of a set size and class",
			sets: []string{"console.history.persistent=true", "console.history.size=4Gi", "console.history.storageClassName=fast"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				c.Spec.History.Size = new(resource.MustParse("4Gi"))
				c.Spec.History.StorageClassName = new("fast")
			},
		},
		{
			// The chart mounts the evidence token on its own switch; the
			// operator mounts it only when evidence is on.
			name: "oidc with evidence off and pod logs off",
			sets: []string{
				"console.auth.provider=oidc", "console.auth.oidc.issuerURL=https://idp.example", "console.auth.oidc.clientID=pgtoolbox-console",
				"console.auth.oidc.clientSecret.existingSecret=pgtoolbox-console-oidc", "console.publicURL=https://console.example",
				"console.auth.oidc.grantClient.clientID=pgtoolbox-console-grants",
				"console.auth.oidc.grantClient.clientSecret.existingSecret=pgtoolbox-console-grants-oidc",
				"console.evidence.enabled=false", "console.evidenceToken.enabled=false", "console.rbac.podLogs=false",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.Auth.Provider = pgtoolboxv1alpha1.PgToolBoxConsoleAuthOIDC
				c.Spec.Auth.OIDC = &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCSpec{
					IssuerURL: "https://idp.example", ClientID: "pgtoolbox-console",
					ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgtoolbox-console-oidc"},
					GrantClient: &pgtoolboxv1alpha1.PgToolBoxConsoleOIDCClient{
						ClientID: "pgtoolbox-console-grants", ClientSecretRef: pgtoolboxv1alpha1.SecretKeySelector{Name: "pgtoolbox-console-grants-oidc"},
					},
				}
				c.Spec.PublicURL = "https://console.example"
				c.Spec.Evidence.Enabled = new(false)
				c.Spec.PodLogs = new(false)
			},
		},
		{
			// The daily digest with every delivery channel: the same
			// DIGEST_* in the same order, the same files mounted.
			name: "digest delivered by chat, webhook and mail",
			sets: []string{
				"console.history.persistent=true", "console.digest.hour=6",
				"console.digest.delivery.chat.existingSecret=digest-chat",
				"console.digest.delivery.webhook.existingSecret=digest-webhook", "console.digest.delivery.webhook.key=endpoint",
				"console.digest.delivery.smtp.address=smtp.example.com:465", "console.digest.delivery.smtp.tls=tls",
				"console.digest.delivery.smtp.username=pgtoolbox-console", "console.digest.delivery.smtp.password.existingSecret=digest-smtp",
				"console.digest.delivery.smtp.from=pgtoolbox-console@example.com", "console.digest.delivery.smtp.to={dba@example.com,oncall@example.com}",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				c.Spec.History.Persistent = new(true)
				c.Spec.Digest.Hour = new(int32(6))
				c.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{
					ChatWebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-chat", Key: "url"},
					WebhookSecretRef:     &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-webhook", Key: "endpoint"},
					SMTP: &pgtoolboxv1alpha1.PgToolBoxConsoleDigestSMTPSpec{
						Address: "smtp.example.com:465", TLS: "tls", Username: "pgtoolbox-console",
						PasswordSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "digest-smtp", Key: "password"},
						From:              "pgtoolbox-console@example.com", To: []string{"dba@example.com", "oncall@example.com"},
					},
				}
			},
		},
		{
			name: "restore drills with their own resources",
			sets: []string{
				"console.restoreDrills.enabled=true", "console.restoreDrills.prefix=rd-", "console.restoreDrills.timeLimit=3h",
				"console.restoreDrills.resources.requests.cpu=500m", "console.restoreDrills.resources.limits.memory=2Gi",
				"console.restoreDrills.storage.size=20Gi", "console.restoreDrills.storage.storageClass=fast",
				"console.restoreDrills.maxConcurrent=5",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				adminBundle(c)
				cpu, memory, size := resource.MustParse("500m"), resource.MustParse("2Gi"), resource.MustParse("20Gi")
				c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{
					Enabled: true, Prefix: "rd-", TimeLimit: &metav1.Duration{Duration: 3 * time.Hour},
					Resources: &pgtoolboxv1alpha1.PgToolBoxConsoleDrillResources{
						Requests: &pgtoolboxv1alpha1.PgToolBoxConsoleDrillQuantities{CPU: &cpu},
						Limits:   &pgtoolboxv1alpha1.PgToolBoxConsoleDrillQuantities{Memory: &memory},
					},
					Storage:       pgtoolboxv1alpha1.PgToolBoxConsoleDrillStorage{Size: &size, StorageClass: "fast"},
					MaxConcurrent: new(int32(5)),
				}
			},
		},
		{
			name: "the MCP endpoint with the reserved write tools",
			sets: []string{"console.mcp.enabled=true", "console.mcp.writeTools=true", "console.publicURL=https://console.example"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.PublicURL = "https://console.example"
				c.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{Enabled: true, WriteTools: true}
			},
		},
		{
			name: "the MCP endpoint with every tuning knob",
			sets: []string{
				"console.mcp.enabled=true", "console.publicURL=https://console.example",
				"console.mcp.maxResultBytes=1048576", "console.mcp.callTimeout=45s", "console.mcp.oauthAccessTTL=15m",
				"console.mcp.oauthClientMetadataHosts={claude.ai,chatgpt.com}",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.PublicURL = "https://console.example"
				c.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
					Enabled: true, MaxResultBytes: new(int32(1 << 20)),
					CallTimeout:              &metav1.Duration{Duration: 45 * time.Second},
					OAuthAccessTTL:           &metav1.Duration{Duration: 15 * time.Minute},
					OAuthClientMetadataHosts: []string{"claude.ai", "chatgpt.com"},
				}
			},
		},
		{
			name: "the MCP endpoint with client ID metadata documents",
			sets: []string{
				"console.mcp.enabled=true", "console.publicURL=https://console.example",
				"console.mcp.oauthClientMetadataDocuments=true", "console.mcp.oauthClientMetadataHosts={claude.ai}",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.PublicURL = "https://console.example"
				c.Spec.MCP = &pgtoolboxv1alpha1.PgToolBoxConsoleMCPSpec{
					Enabled: true, OAuthClientMetadataDocuments: true, OAuthClientMetadataHosts: []string{"claude.ai"},
				}
			},
		},
		{
			name: "tracing with every setting",
			sets: []string{
				"console.tracing.endpoint=https://api.smith.langchain.com/otel", "console.tracing.serviceName=pgtoolbox-console-team-a",
				"console.tracing.headersSecret.existingSecret=langsmith", "console.tracing.headersSecret.key=otlp",
				"console.tracing.sampler=parentbased_traceidratio", "console.tracing.samplerArg=0.25", "console.tracing.includePayloads=true",
			},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.Tracing = &pgtoolboxv1alpha1.PgToolBoxConsoleTracingSpec{
					Endpoint: "https://api.smith.langchain.com/otel", ServiceName: "pgtoolbox-console-team-a",
					HeadersSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "langsmith", Key: "otlp"},
					Sampler:          "parentbased_traceidratio", SamplerArg: "0.25", IncludePayloads: true,
				}
			},
		},
		{
			// The chart's bundle is the admin's, a PgToolBoxConsole's with
			// instanceCABundleRef: both sign drills with the source's CA.
			name: "restore drills with the defaults",
			sets: []string{"console.restoreDrills.enabled=true"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				adminBundle(c)
				c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
			},
		},
		{
			// The drill seal's own keys: the same file, the same mount.
			name: "restore drills with the seal's own keys",
			sets: []string{"console.restoreDrills.enabled=true", "console.restoreDrills.sealKey.existingSecret=drill-seal"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				adminBundle(c)
				c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{
					Enabled: true, SealKeySecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "drill-seal", Key: "keys"},
				}
			},
		},
		{
			// The operator's mirror follows drills created later: their own CA.
			name: "restore drills with the operator's instance CA mirror",
			sets: []string{"console.restoreDrills.enabled=true", "console.restoreDrills.serverCA.mode=own", "console.instanceStatus.caBundleConfigMap=console-instance-ca"},
			mutate: func(c *pgtoolboxv1alpha1.PgToolBoxConsole) {
				inMemory(c)
				c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs := renderChart(t, tc.sets...)
			console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
			if tc.mutate != nil {
				tc.mutate(console)
			}
			var chartPod, ourPod *corev1.PodTemplateSpec
			if console.HistoryPersistent() {
				chart := chartObject[appsv1.StatefulSet](t, docs, "StatefulSet/console")
				ours := buildStatefulSet(console, deploymentInputs{Image: specs.ResolveImage(nil, chart.Spec.Template.Spec.Containers[0].Image)})
				compareStatefulSets(t, ours, chart)
				chartPod, ourPod = &chart.Spec.Template, &ours.Spec.Template
			} else {
				chart := chartObject[appsv1.Deployment](t, docs, "Deployment/console")
				ours := buildDeployment(console, deploymentInputs{Image: specs.ResolveImage(nil, chart.Spec.Template.Spec.Containers[0].Image)})
				if got, want := ours.Spec.Strategy, chart.Spec.Strategy; got.Type != want.Type ||
					got.RollingUpdate.MaxSurge.String() != want.RollingUpdate.MaxSurge.String() ||
					got.RollingUpdate.MaxUnavailable.String() != want.RollingUpdate.MaxUnavailable.String() {
					t.Errorf("strategy %+v, chart %+v", got, want)
				}
				chartPod, ourPod = &chart.Spec.Template, &ours.Spec.Template
			}

			chartEnv, chartMounts, chartVolumes := containerFacts(&chartPod.Spec)
			ourEnv, ourMounts, ourVolumes := containerFacts(&ourPod.Spec)
			if !slices.Equal(ourEnv, chartEnv) {
				t.Errorf("env\n got %v\nwant %v (the chart's)", ourEnv, chartEnv)
			}
			slices.SortFunc(chartMounts, func(a, b mountFact) int { return cmpString(a.Path+a.SubPath, b.Path+b.SubPath) })
			slices.SortFunc(ourMounts, func(a, b mountFact) int { return cmpString(a.Path+a.SubPath, b.Path+b.SubPath) })
			if !slices.Equal(ourMounts, chartMounts) {
				t.Errorf("mounts\n got %v\nwant %v (the chart's)", ourMounts, chartMounts)
			}
			slices.Sort(chartVolumes)
			slices.Sort(ourVolumes)
			if !slices.Equal(ourVolumes, chartVolumes) {
				t.Errorf("volumes\n got %v\nwant %v (the chart's)", ourVolumes, chartVolumes)
			}
			if got, want := ourPod.Spec.Containers[0].Ports, chartPod.Spec.Containers[0].Ports; len(got) != len(want) {
				t.Errorf("ports %v, chart %v", got, want)
			}
		})
	}
}

// compareStatefulSets holds the operator's StatefulSet to the chart's on
// what makes it one: replicas, Service, pod management, update strategy,
// claim retention and the claim template.
func compareStatefulSets(t *testing.T, ours, chart *appsv1.StatefulSet) {
	t.Helper()
	o, c := ours.Spec, chart.Spec
	if *o.Replicas != *c.Replicas || o.ServiceName != c.ServiceName || o.PodManagementPolicy != c.PodManagementPolicy ||
		o.UpdateStrategy.Type != c.UpdateStrategy.Type {
		t.Errorf("StatefulSet replicas %d/%s/%s/%s, chart %d/%s/%s/%s", *o.Replicas, o.ServiceName, o.PodManagementPolicy, o.UpdateStrategy.Type,
			*c.Replicas, c.ServiceName, c.PodManagementPolicy, c.UpdateStrategy.Type)
	}
	if *o.PersistentVolumeClaimRetentionPolicy != *c.PersistentVolumeClaimRetentionPolicy {
		t.Errorf("claim retention %+v, chart %+v", *o.PersistentVolumeClaimRetentionPolicy, *c.PersistentVolumeClaimRetentionPolicy)
	}
	if len(o.VolumeClaimTemplates) != 1 || len(c.VolumeClaimTemplates) != 1 {
		t.Fatalf("claim templates %d, chart %d: want one each", len(o.VolumeClaimTemplates), len(c.VolumeClaimTemplates))
	}
	oc, cc := o.VolumeClaimTemplates[0], c.VolumeClaimTemplates[0]
	if oc.Name != cc.Name || !slices.Equal(oc.Spec.AccessModes, cc.Spec.AccessModes) ||
		oc.Spec.Resources.Requests.Storage().Cmp(*cc.Spec.Resources.Requests.Storage()) != 0 ||
		ptrValue(oc.Spec.StorageClassName, "") != ptrValue(cc.Spec.StorageClassName, "") {
		t.Errorf("claim template %s %v %s %q, chart %s %v %s %q", oc.Name, oc.Spec.AccessModes, oc.Spec.Resources.Requests.Storage(), ptrValue(oc.Spec.StorageClassName, ""),
			cc.Name, cc.Spec.AccessModes, cc.Spec.Resources.Requests.Storage(), ptrValue(cc.Spec.StorageClassName, ""))
	}
}

// TestDigestLeaseRoleMatchesTheChart: the digest Lease Role is the chart's,
// rule for rule (the Lease's name aside, which is the console's).
func TestDigestLeaseRoleMatchesTheChart(t *testing.T) {
	chart := chartObject[rbacv1.Role](t, renderChart(t, "console.history.persistent=true", "console.digest.delivery.chat.existingSecret=chat"), "Role/console-digest")
	console := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
	console.Spec.Digest.Delivery = &pgtoolboxv1alpha1.PgToolBoxConsoleDigestDeliverySpec{ChatWebhookSecretRef: &pgtoolboxv1alpha1.SecretKeyReference{Name: "chat", Key: "url"}}
	ours, _ := buildDigestLeaseRBAC(console)
	if ours.Name != chart.Name {
		t.Errorf("Role %s, chart %s", ours.Name, chart.Name)
	}
	if got, want := ruleSet(ours.Rules), ruleSet(chart.Rules); !slices.Equal(got, want) {
		t.Errorf("digest Lease rules\n got %v\nwant %v (the chart's)", got, want)
	}
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
