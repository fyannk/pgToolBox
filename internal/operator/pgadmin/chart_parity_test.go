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

package pgadmin

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
)

// The pgtools chart renders, without the operator, the Pod the operator's
// PgAdmin builds. These tests render the chart's pgadmin
// section and compare it with buildDeployment and the registration and
// backup Jobs for the equivalent PgAdmin: a setting changed on one side and
// not the other fails here. What differs by design is skipped by name: the
// chart reads a declared list instead of the offer, and runs the
// registration and the backup inside the Pod.

const chartOIDCValues = `
pgadmin:
  enabled: true
  admins: [alice@example.com]
  auth:
    provider: oidc
    oidc:
      issuerURL: https://idp.example.com/realms/corp/
      clientID: pgadmin
      clientSecret: {existingSecret: pgadmin-oidc}
      requiredClaims: {groups: [dba]}
  servers:
    - {cluster: orders, endpoint: rw, credentials: {application: {secret: orders-app}}}
`

const chartOpenShiftValues = `
pgadmin:
  enabled: true
  admins: [alice@example.com]
  fsGroup: null
  auth: {provider: openshift}
  exposure: {type: route, host: pgadmin.apps.example.com}
  servers:
    - {cluster: orders, endpoint: rw, credentials: {application: {secret: orders-app}}}
`

// renderPgAdminChart runs helm template on the pgtools chart with values;
// the test is skipped where helm is not installed (CI's gate job has it).
func renderPgAdminChart(t *testing.T, values string) map[string][]byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join("..", "..", "..", "charts", "pgtools")
	out, err := exec.CommandContext(t.Context(), "helm", "template", "pga", chart, // #nosec G204 -- fixed arguments
		"--namespace", testNamespace, "--set", "pgadmin.fullnameOverride=chart-pgadmin", "-f", file).CombinedOutput()
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
	obj := new(T)
	if err := yaml.Unmarshal(doc, obj); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return obj
}

func containerNamed(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s", name)
	return corev1.Container{}
}

func volumeNamed(t *testing.T, volumes []corev1.Volume, name string) corev1.Volume {
	t.Helper()
	for _, v := range volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume %s", name)
	return corev1.Volume{}
}

// envMap: name -> value of the plain variables, name -> "(fieldRef path)"
// of the downward ones.
func envMap(env []corev1.EnvVar) map[string]string {
	out := map[string]string{}
	for _, e := range env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			out[e.Name] = "(" + e.ValueFrom.FieldRef.FieldPath + ")"
			continue
		}
		out[e.Name] = e.Value
	}
	return out
}

// flagValues: every "--flag value" pair of a command line, repeated flags
// in order; bare flags map to "".
func flagValues(args []string) map[string][]string {
	out := map[string][]string{}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			continue
		}
		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			out[args[i]] = append(out[args[i]], args[i+1])
			i++
			continue
		}
		out[args[i]] = append(out[args[i]], "")
	}
	return out
}

func sameContainerShape(t *testing.T, what string, chart, operator corev1.Container) {
	t.Helper()
	if !equality.Semantic.DeepEqual(chart.SecurityContext, operator.SecurityContext) {
		t.Errorf("%s securityContext: chart %+v, operator %+v", what, chart.SecurityContext, operator.SecurityContext)
	}
	if !equality.Semantic.DeepEqual(chart.Resources, operator.Resources) {
		t.Errorf("%s resources: chart %+v, operator %+v", what, chart.Resources, operator.Resources)
	}
	// Every mount the operator gives, the chart gives (it may add the
	// declared list).
	for _, want := range operator.VolumeMounts {
		if !slices.ContainsFunc(chart.VolumeMounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, want) }) {
			t.Errorf("%s: the chart does not mount %+v", what, want)
		}
	}
}

func TestChartPgAdminImagesAreTheOperatorDefaults(t *testing.T) {
	docs := renderPgAdminChart(t, chartOpenShiftValues)
	dep := chartObject[appsv1.Deployment](t, docs, "Deployment/chart-pgadmin")
	spec := dep.Spec.Template.Spec
	if got := containerNamed(t, spec.Containers, "pgadmin").Image; got != configuration.DefaultPgAdminImage {
		t.Errorf("chart pgadmin.image %s, operator default %s", got, configuration.DefaultPgAdminImage)
	}
	if got := containerNamed(t, spec.Containers, "oauth-proxy").Image; got != configuration.DefaultOAuthProxyImage {
		t.Errorf("chart pgadmin.auth.openshift.image %s, operator default %s", got, configuration.DefaultOAuthProxyImage)
	}
	// The sidecars run the copied binary inside pgAdmin's image, as the
	// operator's do; only the installer runs the tools image.
	for _, name := range []string{"passfile-sync"} {
		if got := containerNamed(t, spec.Containers, name).Image; got != configuration.DefaultPgAdminImage {
			t.Errorf("%s runs %s, not pgAdmin's image", name, got)
		}
	}
	for _, name := range []string{"pre-upgrade-backup", "registration"} {
		if got := containerNamed(t, spec.InitContainers, name).Image; got != configuration.DefaultPgAdminImage {
			t.Errorf("%s runs %s, not pgAdmin's image", name, got)
		}
	}
	if got := containerNamed(t, spec.InitContainers, "install-passfile-sync").Image; !strings.HasPrefix(got, "ghcr.io/fyannk/pgtoolbox-operator:") {
		t.Errorf("install-passfile-sync runs %s, not the pgtoolbox image", got)
	}
}

// TestChartPgAdminCAConfigMapKeepsTheOperatorShape: a server's caConfigMap
// adds only the chart's own mount and flag to passfile-sync (the operator
// reads the CA through its offer and has neither); everything the two Pods
// share still matches, and the ConfigMap reaches passfile-sync alone.
func TestChartPgAdminCAConfigMapKeepsTheOperatorShape(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	in := testInputs(p, false)
	want := buildDeployment(p, in)
	values := strings.Replace(chartOIDCValues, "{cluster: orders, endpoint: rw,", "{cluster: orders, endpoint: rw, caConfigMap: orders-ca-crt,", 1)
	got := chartObject[appsv1.Deployment](t, renderPgAdminChart(t, values), "Deployment/chart-pgadmin")
	comparePods(t, got, want, in, false)

	spec := got.Spec.Template.Spec
	if dir := flagValues(containerNamed(t, spec.Containers, "passfile-sync").Command)["--ca-dir"]; !slices.Equal(dir, []string{"/run/pgtoolbox/ca"}) {
		t.Errorf("passfile-sync --ca-dir %v", dir)
	}
	if v := volumeNamed(t, spec.Volumes, "server-ca"); v.Projected == nil || len(v.Projected.Sources) != 1 ||
		v.Projected.Sources[0].ConfigMap == nil || v.Projected.Sources[0].ConfigMap.Name != "orders-ca-crt" {
		t.Errorf("server-ca volume %+v", v)
	}
	for _, c := range append(slices.Clone(spec.Containers), spec.InitContainers...) {
		mounted := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "server-ca" })
		if mounted != (c.Name == "passfile-sync") {
			t.Errorf("container %s mounts the CA ConfigMaps: %v", c.Name, mounted)
		}
	}
}

// TestChartPgAdminConfigDatabaseMatchesTheOperator: with an external
// settings database both Pods drop the SQLite volume for a per-Pod emptyDir
// and carry the same environment. The URI differs by delivery only: the
// operator inlines the quoted Python literal in
// PGADMIN_CONFIG_CONFIG_DATABASE_URI; the chart's URI is a Secret the team
// names (never inline), so an init container writes the same literal to a
// file the image reads (PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE, exclusive
// with the variable). The operator's Pod has no backup init container (its
// backup is a Job, skipped for a configDatabase); the chart renders neither
// one nor a PVC.
func TestChartPgAdminConfigDatabaseMatchesTheOperator(t *testing.T) {
	p := oauth2PgAdmin()
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	p.Spec.ConfigDatabase = &pgtoolboxv1alpha1.PgAdminConfigDatabaseSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: "orders"}}
	in := testInputs(p, false)
	const uri = "postgresql://pgadmin@orders-rw.team.svc:5432/pgadmin"
	in.ConfigDatabase = configDatabaseState{URI: uri}
	want := buildDeployment(p, in)
	values := strings.Replace(chartOIDCValues, "  servers:", "  configDatabase: {existingSecret: pgadmin-settings-db}\n  servers:", 1)
	docs := renderPgAdminChart(t, values)
	got := chartObject[appsv1.Deployment](t, docs, "Deployment/chart-pgadmin")

	chartEnv := envMap(containerNamed(t, got.Spec.Template.Spec.Containers, "pgadmin").Env)
	opEnv := envMap(containerNamed(t, want.Spec.Template.Spec.Containers, "pgadmin").Env)
	if opEnv["PGADMIN_CONFIG_CONFIG_DATABASE_URI"] != pyString(uri) {
		t.Fatalf("operator CONFIG_DATABASE_URI %q", opEnv["PGADMIN_CONFIG_CONFIG_DATABASE_URI"])
	}
	if chartEnv["PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE"] != "/run/pgadmin/config_database_uri" {
		t.Errorf("chart URI file %q", chartEnv["PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE"])
	}
	if _, both := chartEnv["PGADMIN_CONFIG_CONFIG_DATABASE_URI"]; both {
		t.Error("the image refuses the variable and its _FILE together")
	}
	delete(opEnv, "PGADMIN_CONFIG_CONFIG_DATABASE_URI")
	delete(chartEnv, "PGADMIN_CONFIG_CONFIG_DATABASE_URI_FILE")
	if !equality.Semantic.DeepEqual(chartEnv, opEnv) {
		t.Errorf("pgadmin env differs\nchart    %v\noperator %v", chartEnv, opEnv)
	}

	// What the init container writes is what the operator inlines, and the
	// Secret reaches it by secretKeyRef.
	init := containerNamed(t, got.Spec.Template.Spec.InitContainers, "config-database-uri")
	if len(init.Env) != 1 || init.Env[0].ValueFrom == nil || init.Env[0].ValueFrom.SecretKeyRef == nil ||
		init.Env[0].ValueFrom.SecretKeyRef.Name != "pgadmin-settings-db" || init.Env[0].ValueFrom.SecretKeyRef.Key != "CONFIG_DATABASE_URI" {
		t.Errorf("the URI must come from the Secret: %+v", init.Env)
	}
	for _, sample := range []string{uri, uri + "\n", "postgresql://pgadmin:p%40ss@db/pgadmin?sslmode=require&application_name=it's"} {
		literal, err := ConfigDatabaseURILiteral(sample)
		if err != nil {
			t.Fatal(err)
		}
		if want := pyString(strings.TrimSpace(sample)); literal != want {
			t.Errorf("file content %q, operator value %q", literal, want)
		}
	}
	if _, err := ConfigDatabaseURILiteral("postgresql://a\nb"); err == nil {
		t.Error("a control character in the URI must be refused")
	}

	// DATA_DIR: an emptyDir on both sides, no PVC, no backup.
	for what, pod := range map[string]corev1.PodSpec{"chart": got.Spec.Template.Spec, "operator": want.Spec.Template.Spec} {
		if v := volumeNamed(t, pod.Volumes, "settings"); v.EmptyDir == nil || v.PersistentVolumeClaim != nil {
			t.Errorf("%s settings volume %+v, want an emptyDir", what, v)
		}
		for _, c := range pod.InitContainers {
			if c.Name == "pre-upgrade-backup" {
				t.Errorf("%s renders a pre-upgrade backup with an external settings database", what)
			}
		}
	}
	if _, ok := docs["PersistentVolumeClaim/chart-pgadmin"]; ok {
		t.Error("the chart renders a PVC with an external settings database")
	}
	if *got.Spec.Replicas != 1 || got.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %d strategy %s", *got.Spec.Replicas, got.Spec.Strategy.Type)
	}
}

func TestChartPgAdminOIDCMatchesTheOperator(t *testing.T) {
	p := oauth2PgAdmin()
	// The chart refuses quotes in a value that reaches pgAdmin's Python
	// configuration (the schema); the fixture's "it's" is the operator's
	// escaping test, not a shape the chart can express.
	p.Spec.Auth.OAuth2.RequiredClaims = map[string]pgtoolboxv1alpha1.ClaimValues{"groups": {"dba"}}
	in := testInputs(p, false)
	want := buildDeployment(p, in)
	docs := renderPgAdminChart(t, chartOIDCValues)
	got := chartObject[appsv1.Deployment](t, docs, "Deployment/chart-pgadmin")
	comparePods(t, got, want, in, false)

	// Sign-in: the whole environment of the pgadmin container.
	chartEnv := envMap(containerNamed(t, got.Spec.Template.Spec.Containers, "pgadmin").Env)
	opEnv := envMap(containerNamed(t, want.Spec.Template.Spec.Containers, "pgadmin").Env)
	if !equality.Semantic.DeepEqual(chartEnv, opEnv) {
		t.Errorf("pgadmin env differs\nchart    %v\noperator %v", chartEnv, opEnv)
	}
	if v := volumeNamed(t, got.Spec.Template.Spec.Volumes, "oauth2-client"); !equality.Semantic.DeepEqual(v, volumeNamed(t, want.Spec.Template.Spec.Volumes, "oauth2-client")) {
		t.Errorf("oauth2-client volume differs: %+v", v)
	}
	compareRegistration(t, got, p, in, "oauth2")
}

func TestChartPgAdminOpenShiftMatchesTheOperator(t *testing.T) {
	p := openshiftPgAdmin()
	p.Spec.MetadataPropagation = nil
	in := testInputs(p, true)
	want := buildDeployment(p, in)
	docs := renderPgAdminChart(t, chartOpenShiftValues)
	got := chartObject[appsv1.Deployment](t, docs, "Deployment/chart-pgadmin")
	comparePods(t, got, want, in, true)

	chartEnv := envMap(containerNamed(t, got.Spec.Template.Spec.Containers, "pgadmin").Env)
	opEnv := envMap(containerNamed(t, want.Spec.Template.Spec.Containers, "pgadmin").Env)
	if !equality.Semantic.DeepEqual(chartEnv, opEnv) {
		t.Errorf("pgadmin env differs\nchart    %v\noperator %v", chartEnv, opEnv)
	}

	chartProxy := containerNamed(t, got.Spec.Template.Spec.Containers, "oauth-proxy")
	opProxy := containerNamed(t, want.Spec.Template.Spec.Containers, "oauth-proxy")
	sameContainerShape(t, "oauth-proxy", chartProxy, opProxy)
	// The SubjectAccessReview differs by design (no PgAdmin object to
	// ask about: may get secrets in the namespace); the ServiceAccount
	// name is the object's own.
	normalize := func(args []string, sa string) []string {
		out := []string{}
		for _, a := range args {
			if strings.HasPrefix(a, "--openshift-sar=") {
				continue
			}
			out = append(out, strings.ReplaceAll(a, sa, "<sa>"))
		}
		return out
	}
	if a, b := normalize(chartProxy.Args, "chart-pgadmin"), normalize(opProxy.Args, namesOf(p).base); !slices.Equal(a, b) {
		t.Errorf("oauth-proxy args differ\nchart    %v\noperator %v", a, b)
	}
	if !equality.Semantic.DeepEqual(chartProxy.ReadinessProbe, opProxy.ReadinessProbe) || !equality.Semantic.DeepEqual(chartProxy.Ports, opProxy.Ports) {
		t.Errorf("oauth-proxy probe or ports differ: %+v %+v", chartProxy.ReadinessProbe, chartProxy.Ports)
	}
	for _, name := range []string{"proxy-tls", "proxy-cookie"} {
		if a, b := volumeNamed(t, got.Spec.Template.Spec.Volumes, name).Secret.DefaultMode, volumeNamed(t, want.Spec.Template.Spec.Volumes, name).Secret.DefaultMode; *a != *b {
			t.Errorf("%s mode %o, operator %o", name, *a, *b)
		}
	}

	svc := chartObject[corev1.Service](t, docs, "Service/chart-pgadmin")
	opSvc := buildService(p)
	if !equality.Semantic.DeepEqual(svc.Spec.Ports, opSvc.Spec.Ports) {
		t.Errorf("service ports: chart %+v, operator %+v", svc.Spec.Ports, opSvc.Spec.Ports)
	}
	if svc.Annotations["service.beta.openshift.io/serving-cert-secret-name"] != "chart-pgadmin-tls" {
		t.Errorf("service annotations %v", svc.Annotations)
	}
	sa := chartObject[corev1.ServiceAccount](t, docs, "ServiceAccount/chart-pgadmin")
	opSA := buildServiceAccount(p)
	const ref = "serviceaccounts.openshift.io/oauth-redirectreference.primary"
	if strings.ReplaceAll(sa.Annotations[ref], "chart-pgadmin", "<n>") != strings.ReplaceAll(opSA.Annotations[ref], namesOf(p).base, "<n>") {
		t.Errorf("redirect reference: chart %s, operator %s", sa.Annotations[ref], opSA.Annotations[ref])
	}
	compareRegistration(t, got, p, in, "webserver")
}

// comparePods compares what both Pods share: the pod-level settings, the
// pgadmin container's probes and shape, passfile-sync and the installer,
// and the volumes that are not named after the object.
func comparePods(t *testing.T, got, want *appsv1.Deployment, in buildInputs, openshift bool) {
	t.Helper()
	gs, ws := got.Spec.Template.Spec, want.Spec.Template.Spec
	if *got.Spec.Replicas != 1 || got.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || want.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("replicas %d strategy %s (operator %s)", *got.Spec.Replicas, got.Spec.Strategy.Type, want.Spec.Strategy.Type)
	}
	if !equality.Semantic.DeepEqual(gs.SecurityContext, ws.SecurityContext) {
		t.Errorf("pod securityContext: chart %+v, operator %+v (openshift %v)", gs.SecurityContext, ws.SecurityContext, openshift)
	}
	if *gs.AutomountServiceAccountToken != *ws.AutomountServiceAccountToken || *gs.EnableServiceLinks != *ws.EnableServiceLinks ||
		*gs.TerminationGracePeriodSeconds != *ws.TerminationGracePeriodSeconds {
		t.Error("automount, service links or grace period differ")
	}

	chartPgAdmin, opPgAdmin := containerNamed(t, gs.Containers, "pgadmin"), containerNamed(t, ws.Containers, "pgadmin")
	sameContainerShape(t, "pgadmin", chartPgAdmin, opPgAdmin)
	if !equality.Semantic.DeepEqual(chartPgAdmin.StartupProbe, opPgAdmin.StartupProbe) || !equality.Semantic.DeepEqual(chartPgAdmin.ReadinessProbe, opPgAdmin.ReadinessProbe) ||
		!equality.Semantic.DeepEqual(chartPgAdmin.LivenessProbe, opPgAdmin.LivenessProbe) {
		t.Errorf("pgadmin probes differ:\nchart    %+v\noperator %+v", chartPgAdmin.StartupProbe, opPgAdmin.StartupProbe)
	}
	if !equality.Semantic.DeepEqual(chartPgAdmin.Ports, opPgAdmin.Ports) || len(chartPgAdmin.VolumeMounts) != len(opPgAdmin.VolumeMounts) {
		t.Errorf("pgadmin ports or mounts differ: %+v %+v", chartPgAdmin.Ports, chartPgAdmin.VolumeMounts)
	}

	chartSync, opSync := containerNamed(t, gs.Containers, "passfile-sync"), containerNamed(t, ws.Containers, "passfile-sync")
	sameContainerShape(t, "passfile-sync", chartSync, opSync)
	if !equality.Semantic.DeepEqual(envMap(chartSync.Env), envMap(opSync.Env)) {
		t.Errorf("passfile-sync env: chart %v, operator %v", envMap(chartSync.Env), envMap(opSync.Env))
	}
	if a, b := flagValues(chartSync.Command)["--dir"], flagValues(opSync.Command)["--dir"]; !slices.Equal(a, b) {
		t.Errorf("passfile-sync --dir: chart %v, operator %v", a, b)
	}

	chartInstall, opInstall := containerNamed(t, gs.InitContainers, "install-passfile-sync"), containerNamed(t, ws.InitContainers, "install-passfile-sync")
	sameContainerShape(t, "install-passfile-sync", chartInstall, opInstall)
	if !slices.Equal(chartInstall.Command, opInstall.Command) {
		t.Errorf("installer command: chart %v, operator %v", chartInstall.Command, opInstall.Command)
	}
	if in.OperatorImage == "" {
		t.Fatal("fixture without an operator image")
	}

	for _, name := range []string{tmpVolume, runVolume, credentialsVolume, "pgtoolbox-bin", "kube-api-access"} {
		if a, b := volumeNamed(t, gs.Volumes, name), volumeNamed(t, ws.Volumes, name); !equality.Semantic.DeepEqual(a, b) {
			t.Errorf("volume %s: chart %+v, operator %+v", name, a, b)
		}
	}
	if a, b := volumeNamed(t, gs.Volumes, bootstrapVolume).Secret, volumeNamed(t, ws.Volumes, bootstrapVolume).Secret; *a.DefaultMode != *b.DefaultMode || !equality.Semantic.DeepEqual(a.Items, b.Items) {
		t.Errorf("bootstrap volume: chart %+v, operator %+v", a, b)
	}
}

// compareRegistration: the chart's registration sidecar and pre-upgrade
// backup take the arguments the operator's Jobs take, but for the source
// of the list (--declared, not --offer), the sidecar's --keep-running, and
// its wait for pgAdmin's ping (--wait-ready, --wait): beside a pgAdmin that
// may still be creating its database, where the operator's Job starts only
// once pgAdmin runs.
func compareRegistration(t *testing.T, got *appsv1.Deployment, p *pgtoolboxv1alpha1.PgAdmin, in buildInputs, authSource string) {
	t.Helper()
	spec := got.Spec.Template.Spec
	reg := containerNamed(t, spec.InitContainers, "registration")
	job := buildRegistrationJob(p, in, "0065be49deadbeef")
	opReg := job.Spec.Template.Spec.Containers[0]
	chartFlags, opFlags := flagValues(reg.Command), flagValues(opReg.Command)
	for _, flag := range []string{"--declared", "--withheld", "--keep-running", "--wait-ready", "--wait"} {
		if _, ok := chartFlags[flag]; !ok {
			t.Errorf("registration lacks %s", flag)
		}
		delete(chartFlags, flag)
	}
	delete(opFlags, "--offer")
	if !equality.Semantic.DeepEqual(chartFlags, opFlags) {
		t.Errorf("registration flags differ\nchart    %v\noperator %v", chartFlags, opFlags)
	}
	if chartFlags["--auth-source"][0] != authSource {
		t.Errorf("auth source %v", chartFlags["--auth-source"])
	}
	if reg.RestartPolicy == nil || *reg.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("the registration must be a native sidecar")
	}
	if !equality.Semantic.DeepEqual(envMap(reg.Env), envMap(opReg.Env)) || !equality.Semantic.DeepEqual(reg.SecurityContext, opReg.SecurityContext) ||
		reg.TerminationMessagePolicy != opReg.TerminationMessagePolicy {
		t.Errorf("registration env, securityContext or termination message policy differ")
	}

	backup := containerNamed(t, spec.InitContainers, "pre-upgrade-backup")
	opBackup := buildBackupJob(p, in, in.Image, "0065be49deadbeef", 3).Spec.Template.Spec.Containers[0]
	bf, of := flagValues(backup.Command), flagValues(opBackup.Command)
	for _, flag := range []string{"--settings-db", "--dir", "--keep", "--lock"} {
		if !slices.Equal(bf[flag], of[flag]) {
			t.Errorf("backup %s: chart %v, operator %v", flag, bf[flag], of[flag])
		}
	}
	if !slices.Equal(bf["--if-image-changed"], []string{containerNamed(t, spec.Containers, "pgadmin").Image}) {
		t.Errorf("backup --if-image-changed %v is not pgAdmin's image", bf["--if-image-changed"])
	}
}
