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

package evidenceagent

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/fyannk/pgToolBox/internal/backupresolve"
)

// The pgtools chart renders, without the operator, the agent the operator's
// EvidenceAgent builds. This test renders the chart's evidence
// section and compares its Deployment and Service with buildDeployment and
// buildService for the equivalent EvidenceAgent: a setting changed on one
// side and not the other fails here. What differs by design is listed by
// name below, each with its reason; owner references and the labels each
// side adds are not compared (the chart has none of the first, and the
// labels carry the release or the object's own name).

const chartAgentImageDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// chartEvidenceValues: one agent for the Cluster "orders", served over TLS
// (the operator's agent always is).
const chartEvidenceValues = `
evidence:
  tls:
    mode: certManager
    certManager:
      issuerRef: {name: ca, kind: Issuer, group: cert-manager.io}
  image: {repository: registry.example/pgtoolbox-evidence-agent, digest: ` + chartAgentImageDigest + `}
  agents:
    - {cluster: orders, secrets: [minio-credentials]}
`

// parityEnvDifferences: environment variables that exist on one side only,
// or whose value is each side's own, by design.
var parityEnvDifferences = map[string]string{
	"AGENT_RESOLVE":                 "chart-only: the chart agent resolves the repository from the Cluster itself",
	"AGENT_ALLOWED_SECRETS":         "chart-only: the Secrets the chart's Role grants get on",
	"AGENT_AUTH":                    "chart-only: the console presents a shared token; the operator's agent uses TokenReview and SAR",
	"AGENT_TOKEN_FILE":              "chart-only: the file of that shared token",
	"AGENT_PLAIN_HTTP":              "chart-only: tls.mode none; the operator's agent always serves TLS",
	"AGENT_ALLOW_INSECURE_ENDPOINT": "chart-only: the entry's opt-in (the operator reads spec instead)",
	"AGENT_WORKLOAD_IDENTITY":       "chart-only: the entry's opt-in (the operator reads spec instead)",
	"LOG_LEVEL":                     "chart-only: evidence.logLevel",
	"AGENT_NAME":                    "each side's own object name",
	"PRODUCER_EPOCH":                "repository variable: the operator injects it",
	"CNPG_CLUSTER_NAMESPACE":        "repository variable: the operator injects it",
	"CNPG_CLUSTER_UID":              "repository variable: the operator injects it",
	"REPOSITORY_FORMAT":             "repository variable: the operator injects it",
	"PROVIDER":                      "repository variable: the operator injects it",
	"DESTINATION_PATH":              "repository variable: the operator injects it",
	"BARMAN_SERVER_NAMES":           "repository variable: the operator injects it",
	"CATALOG_REFRESH_INTERVAL":      "repository variable: the operator injects it",
	"STORE_CREDENTIAL_MODE":         "repository variable: the operator injects it",
}

// parityVolumeDifferences: volumes on one side only, by design.
var parityVolumeDifferences = map[string]string{
	"evidence-token": "chart-only: the shared token Secret",
}

// renderEvidenceChart runs helm template on the pgtools chart with values;
// the test is skipped where helm is not installed (CI's gate job has it).
func renderEvidenceChart(t *testing.T, values string) map[string][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join("..", "..", "..", "charts", "pgtools")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "evi", chart, // #nosec G204 -- fixed arguments
		"--namespace", "team-a", "--set", "console.enabled=false", "-f", file).CombinedOutput()
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
		t.Fatalf("the chart renders no %s (it renders %v)", key, slices.Sorted(maps.Keys(docs)))
	}
	obj := new(T)
	if err := yaml.Unmarshal(doc, obj); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return obj
}

// withProbeDefaults fills the API server's defaults the chart leaves out, so
// both sides compare as the kubelet runs them.
func withProbeDefaults(p *corev1.Probe) *corev1.Probe {
	if p == nil {
		return nil
	}
	out := p.DeepCopy()
	for _, field := range []*int32{&out.SuccessThreshold, &out.TimeoutSeconds} {
		if *field == 0 {
			*field = 1
		}
	}
	if out.FailureThreshold == 0 {
		out.FailureThreshold = 3
	}
	if out.PeriodSeconds == 0 {
		out.PeriodSeconds = 10
	}
	return out
}

func envByName(env []corev1.EnvVar) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		if e.ValueFrom != nil {
			out[e.Name] = "(valueFrom)"
			continue
		}
		out[e.Name] = e.Value
	}
	return out
}

func TestEvidenceAgentChartParity(t *testing.T) {
	docs := renderEvidenceChart(t, chartEvidenceValues)
	const chartName = "evi-evidence-orders"
	chartDep := chartObject[appsv1.Deployment](t, docs, "Deployment/"+chartName)
	chartSvc := chartObject[corev1.Service](t, docs, "Service/"+chartName)

	agent := testAgent()
	in := testInputs(agent)
	// A repository that needs no credential file, so the operator's Pod
	// carries no volume the chart's agent (which reads the Secrets itself)
	// has no counterpart for.
	in.Composition = composition{
		ClusterName: "orders", ClusterUID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", ServerName: "orders",
		Provider: "s3", DestinationPath: "s3://backups/team-a", AgentDestination: "s3://backups/team-a",
		Credential: backupresolve.CredentialAmbient,
	}
	in.Image = resolveImage(nil, "registry.example/pgtoolbox-evidence-agent@"+chartAgentImageDigest)
	opDep := buildDeployment(in)
	opSvc := buildService(agent, in.ServingSecretName)

	chartSpec, opSpec := chartDep.Spec.Template.Spec, opDep.Spec.Template.Spec
	if len(chartSpec.Containers) != 1 || len(opSpec.Containers) != 1 {
		t.Fatalf("containers: chart %d, operator %d", len(chartSpec.Containers), len(opSpec.Containers))
	}
	chartC, opC := chartSpec.Containers[0], opSpec.Containers[0]

	t.Run("deployment", func(t *testing.T) {
		if !equality.Semantic.DeepEqual(chartDep.Spec.Replicas, opDep.Spec.Replicas) {
			t.Errorf("replicas: chart %d, operator %d", *chartDep.Spec.Replicas, *opDep.Spec.Replicas)
		}
		if !equality.Semantic.DeepEqual(chartDep.Spec.Strategy, opDep.Spec.Strategy) {
			t.Errorf("strategy: chart %+v, operator %+v", chartDep.Spec.Strategy, opDep.Spec.Strategy)
		}
		if !equality.Semantic.DeepEqual(chartDep.Spec.RevisionHistoryLimit, opDep.Spec.RevisionHistoryLimit) {
			t.Errorf("revisionHistoryLimit: chart %d, operator %d", *chartDep.Spec.RevisionHistoryLimit, *opDep.Spec.RevisionHistoryLimit)
		}
		if *chartSpec.AutomountServiceAccountToken != *opSpec.AutomountServiceAccountToken ||
			*chartSpec.EnableServiceLinks != *opSpec.EnableServiceLinks ||
			*chartSpec.TerminationGracePeriodSeconds != *opSpec.TerminationGracePeriodSeconds {
			t.Error("automount, service links or grace period differ")
		}
		if !equality.Semantic.DeepEqual(chartSpec.SecurityContext, opSpec.SecurityContext) {
			t.Errorf("pod securityContext: chart %+v, operator %+v", chartSpec.SecurityContext, opSpec.SecurityContext)
		}
	})

	t.Run("container", func(t *testing.T) {
		if chartC.Image != opC.Image || chartC.ImagePullPolicy != opC.ImagePullPolicy {
			t.Errorf("image: chart %s (%s), operator %s (%s)", chartC.Image, chartC.ImagePullPolicy, opC.Image, opC.ImagePullPolicy)
		}
		if !slices.Equal(chartC.Command, opC.Command) || !slices.Equal(chartC.Args, opC.Args) {
			t.Errorf("command and args: chart %v %v, operator %v %v", chartC.Command, chartC.Args, opC.Command, opC.Args)
		}
		if !equality.Semantic.DeepEqual(chartC.Ports, opC.Ports) {
			t.Errorf("ports: chart %+v, operator %+v", chartC.Ports, opC.Ports)
		}
		if !equality.Semantic.DeepEqual(chartC.Resources, opC.Resources) {
			t.Errorf("resources: chart %+v, operator %+v", chartC.Resources, opC.Resources)
		}
		if !equality.Semantic.DeepEqual(chartC.SecurityContext, opC.SecurityContext) {
			t.Errorf("container securityContext: chart %+v, operator %+v", chartC.SecurityContext, opC.SecurityContext)
		}
	})

	t.Run("probes", func(t *testing.T) {
		pairs := map[string][2]*corev1.Probe{
			"readiness": {chartC.ReadinessProbe, opC.ReadinessProbe},
			"liveness":  {chartC.LivenessProbe, opC.LivenessProbe},
			"startup":   {chartC.StartupProbe, opC.StartupProbe},
		}
		for name, pair := range pairs {
			if got, want := withProbeDefaults(pair[0]), withProbeDefaults(pair[1]); !equality.Semantic.DeepEqual(got, want) {
				t.Errorf("%s probe: chart %+v, operator %+v", name, got, want)
			}
		}
	})

	t.Run("environment", func(t *testing.T) {
		chartEnv, opEnv := envByName(chartC.Env), envByName(opC.Env)
		for name, chartValue := range chartEnv {
			opValue, both := opEnv[name]
			_, listed := parityEnvDifferences[name]
			if name == "TLS_CERT_FILE" || name == "TLS_KEY_FILE" {
				// The serving directory is each side's own (the chart's
				// is /var/run/pgtools/tls); the file names are the agent's.
				chartValue, opValue = path.Base(chartValue), path.Base(opValue)
			}
			switch {
			case both && !listed && chartValue != opValue:
				t.Errorf("%s: chart %q, operator %q", name, chartValue, opValue)
			case !both && !listed:
				t.Errorf("the chart sets %s, the operator does not, and it is not a listed difference", name)
			}
		}
		for name := range opEnv {
			if _, both := chartEnv[name]; !both {
				if _, listed := parityEnvDifferences[name]; !listed {
					t.Errorf("the operator sets %s, the chart does not, and it is not a listed difference", name)
				}
			}
		}
	})

	t.Run("mounts and volumes", func(t *testing.T) {
		compareVolumes(t, chartSpec.Volumes, opSpec.Volumes, chartC.VolumeMounts, opC.VolumeMounts)
	})

	t.Run("service", func(t *testing.T) {
		if chartSvc.Spec.Type != opSvc.Spec.Type {
			t.Errorf("type: chart %s, operator %s", chartSvc.Spec.Type, opSvc.Spec.Type)
		}
		if len(chartSvc.Spec.Ports) != 1 || len(opSvc.Spec.Ports) != 1 {
			t.Fatalf("ports: chart %+v, operator %+v", chartSvc.Spec.Ports, opSvc.Spec.Ports)
		}
		chartPort, opPort := chartSvc.Spec.Ports[0], opSvc.Spec.Ports[0]
		chartApp, opApp := chartPort.AppProtocol, opPort.AppProtocol
		// appProtocol is a hint to meshes and gateways; it is compared on
		// its own, below, because the two sides differ on it.
		chartPort.AppProtocol, opPort.AppProtocol = nil, nil
		if !equality.Semantic.DeepEqual(chartPort, opPort) {
			t.Errorf("ports: chart %+v, operator %+v", chartPort, opPort)
		}
		if !equality.Semantic.DeepEqual(chartApp, opApp) {
			t.Errorf("port appProtocol: chart %s, operator %s (the operator declares its TLS port https; the chart's TLS modes do not)",
				ptrString(chartApp), ptrString(opApp))
		}
		// Each Service selects exactly its own Deployment's Pods, under
		// the name the console's NetworkPolicy matches.
		sides := map[string]struct {
			svc *corev1.Service
			dep *appsv1.Deployment
		}{"chart": {chartSvc, chartDep}, "operator": {opSvc, opDep}}
		for what, side := range sides {
			selector := side.svc.Spec.Selector
			if !equality.Semantic.DeepEqual(selector, side.dep.Spec.Selector.MatchLabels) {
				t.Errorf("%s: service selector %v, deployment selector %v", what, selector, side.dep.Spec.Selector.MatchLabels)
			}
			for key, value := range selector {
				if got := side.dep.Spec.Template.Labels[key]; got != value {
					t.Errorf("%s: pod label %s=%q does not satisfy the selector %q", what, key, got, value)
				}
			}
			if selector["app.kubernetes.io/name"] != "evidence-agent" {
				t.Errorf("%s: selector %v lacks app.kubernetes.io/name=evidence-agent", what, selector)
			}
		}
	})
}

// defaultFieldRefVersion fills the apiVersion the API server defaults on a
// downward API fieldRef, which the chart leaves out.
func defaultFieldRefVersion(v *corev1.Volume) {
	if v.Projected == nil {
		return
	}
	v.Projected = v.Projected.DeepCopy()
	for _, source := range v.Projected.Sources {
		if source.DownwardAPI == nil {
			continue
		}
		for i := range source.DownwardAPI.Items {
			if ref := source.DownwardAPI.Items[i].FieldRef; ref != nil && ref.APIVersion == "" {
				ref.APIVersion = "v1"
			}
		}
	}
}

func ptrString(s *string) string {
	if s == nil {
		return "<unset>"
	}
	return *s
}

// servingVolume: the two sides name the serving certificate volume and its
// Secret after their own objects.
func servingVolume(name string) bool { return name == "tls" || name == "serving-certificate" }

// compareVolumes: the volumes both sides have are equal, mounted read-only
// at the paths that matter; a volume on one side only must be listed.
func compareVolumes(t *testing.T, chartVols, opVols []corev1.Volume, chartMounts, opMounts []corev1.VolumeMount) {
	t.Helper()
	canonical := func(name string) string {
		if servingVolume(name) {
			return "serving"
		}
		return name
	}
	volumes := func(list []corev1.Volume) map[string]corev1.Volume {
		out := map[string]corev1.Volume{}
		for _, v := range list {
			out[canonical(v.Name)] = v
		}
		return out
	}
	mounts := func(list []corev1.VolumeMount) map[string]corev1.VolumeMount {
		out := map[string]corev1.VolumeMount{}
		for _, m := range list {
			out[canonical(m.Name)] = m
		}
		return out
	}
	cv, ov, cm, om := volumes(chartVols), volumes(opVols), mounts(chartMounts), mounts(opMounts)
	for key, a := range cv {
		b, both := ov[key]
		if !both {
			if _, listed := parityVolumeDifferences[a.Name]; !listed {
				t.Errorf("the chart has volume %s, the operator does not, and it is not a listed difference", a.Name)
			}
			continue
		}
		a.Name, b.Name = "", ""
		defaultFieldRefVersion(&a)
		defaultFieldRefVersion(&b)
		if key == "serving" {
			// The Secret is cert-manager's on one side and the
			// operator's on the other; items and mode must agree.
			a.Secret.SecretName, b.Secret.SecretName = "", ""
		}
		if !equality.Semantic.DeepEqual(a, b) {
			t.Errorf("volume %s: chart %+v, operator %+v", key, a, b)
		}
		am, bm := cm[key], om[key]
		if am.ReadOnly != bm.ReadOnly || am.SubPath != bm.SubPath {
			t.Errorf("mount of %s: chart %+v, operator %+v", key, am, bm)
		}
		// The serving directory differs by design (the chart's is
		// /var/run/pgtools/tls); every other path is the agent's.
		if key != "serving" && am.MountPath != bm.MountPath {
			t.Errorf("mount path of %s: chart %s, operator %s", key, am.MountPath, bm.MountPath)
		}
	}
	for key, b := range ov {
		if _, both := cv[key]; !both {
			if _, listed := parityVolumeDifferences[b.Name]; !listed {
				t.Errorf("the operator has volume %s, the chart does not, and it is not a listed difference", b.Name)
			}
		}
	}
	for key := range cm {
		if _, ok := cv[key]; !ok {
			t.Errorf("the chart mounts %s without a volume", key)
		}
	}
	for key := range om {
		if _, ok := ov[key]; !ok {
			t.Errorf("the operator mounts %s without a volume", key)
		}
	}
}
