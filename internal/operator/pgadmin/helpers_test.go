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
	"os/exec"
	"strings"
	"testing"

	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

func TestPyString(t *testing.T) {
	cases := map[string]string{
		"plain":       `'plain'`,
		"it's":        `'it\'s'`,
		`back\slash`:  `'back\\slash'`,
		"line\nbreak": `'line\u000abreak'`,
		"é":           `'\u00e9'`,
		"🐘":           `'\U0001f418'`,
	}
	for in, want := range cases {
		if got := pyString(in); got != want {
			t.Errorf("pyString(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestPyStringEvaluatesToItself checks the literals with a real Python
// when one is on PATH (CI images have one); skipped otherwise.
func TestPyStringEvaluatesToItself(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	for _, in := range []string{"it's", `a\b`, "x\ny\x00z", "é🐘", "'; import os; os.system('id'); '"} {
		out, err := exec.CommandContext(t.Context(), python, "-c", "import sys; sys.stdout.write("+pyString(in)+")").Output() // #nosec G204 -- test
		if err != nil || string(out) != in {
			t.Errorf("python evaluated %s to %q (%v), want %q", pyString(in), out, err, in)
		}
	}
}

func TestOAuth2ConfigReadsTheSecretFromAFile(t *testing.T) {
	config := oauth2Config(oauth2PgAdmin().Spec.Auth.OAuth2)
	for _, want := range []string{
		`'OAUTH2_CLIENT_SECRET': open('/run/pgadmin/oauth2/client-secret').read().strip()`,
		`'OAUTH2_SERVER_METADATA_URL': 'https://idp.example.com/realms/corp/.well-known/openid-configuration'`,
		`'OAUTH2_ADDITIONAL_CLAIMS': {'groups': ['dba', 'it\'s']}`,
		`'OAUTH2_USERNAME_CLAIM': 'email'`,
		`'OAUTH2_SCOPE': 'openid email profile'`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("OAUTH2_CONFIG lacks %s:\n%s", want, config)
		}
	}
}

func TestEgressStatement(t *testing.T) {
	p := openshiftPgAdmin()
	in := testInputs(p, true)
	confined, open := egressStatement(buildNetworkPolicy(p, in))
	if confined {
		t.Fatal("the extraEgress 0.0.0.0/0 rule and the open identity-provider rule must be reported")
	}
	joined := strings.Join(open, "; ")
	for _, want := range []string{"identity-provider allows port TCP/443 to any address", "extraEgress[0] allows every port to 0.0.0.0/0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("open rules %q lack %q", joined, want)
		}
	}
	if strings.Contains(joined, "kubernetes-api") || strings.Contains(joined, "dns") || strings.Contains(joined, "postgresql") {
		t.Errorf("named rules reported open: %q", joined)
	}

	p.Spec.NetworkPolicy.ExtraEgress = nil
	p.Spec.NetworkPolicy.IdentityProviderCIDRs = []string{"10.1.2.0/24"}
	if confined, open := egressStatement(buildNetworkPolicy(p, in)); !confined {
		t.Fatalf("named destinations only, still open: %v", open)
	}
	in.APIServer = nil
	if _, open := egressStatement(buildNetworkPolicy(p, in)); !strings.Contains(strings.Join(open, ";"), "kubernetes-api allows port TCP/443,TCP/6443 to any address") {
		t.Fatalf("unknown API server endpoints must be reported open: %v", open)
	}
	p.Spec.NetworkPolicy.Enabled = nil
	if confined, open := egressStatement(buildNetworkPolicy(p, in)); confined || !strings.Contains(open[0], "not enabled") {
		t.Fatalf("no policy: %v %v", confined, open)
	}
}

func podWith(hash string, mutate func(*corev1.Pod)) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pga-0", Annotations: map[string]string{pgtoolboxv1alpha1.AnnotationSpecHash: hash}}}
	if mutate != nil {
		mutate(&pod)
	}
	return pod
}

func TestWorkloadFactCopiesTheChildsFact(t *testing.T) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{pgtoolboxv1alpha1.AnnotationSpecHash: "h1"}}}
	cases := []struct {
		name string
		pods []corev1.Pod
		want pgtoolboxv1alpha1.Reason
	}{
		{"no pod", nil, pgtoolboxv1alpha1.ReasonRolloutInProgress},
		{"old template only", []corev1.Pod{podWith("h0", nil)}, pgtoolboxv1alpha1.ReasonRolloutInProgress},
		{"unschedulable", []corev1.Pod{podWith("h1", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "0/3 nodes"}}
		})}, pgtoolboxv1alpha1.ReasonPodUnschedulable},
		{"image pull", []corev1.Pod{podWith("h1", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "pgadmin", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
		})}, pgtoolboxv1alpha1.ReasonImagePullFailed},
		{"init crash", []corev1.Pod{podWith("h1", func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "install-passfile-sync", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
		})}, pgtoolboxv1alpha1.ReasonCrashLoopBackOff},
		{"not ready", []corev1.Pod{podWith("h1", nil)}, pgtoolboxv1alpha1.ReasonPodNotReady},
		{"ready", []corev1.Pod{podWith("h1", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		})}, pgtoolboxv1alpha1.ReasonAsExpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workloadFact(deployment, nil, &corev1.PodList{Items: tc.pods}); got.Reason != tc.want {
				t.Fatalf("reason %s (%s), want %s", got.Reason, got.Message, tc.want)
			}
		})
	}
}

// TestWorkloadFactCountsEveryDesiredReplica: with a configDatabase pgAdmin
// runs several replicas; WorkloadReady is True only when each of them runs
// the current template and is Ready, and a Pod that cannot start is named
// whichever replica it is.
func TestWorkloadFactCountsEveryDesiredReplica(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{pgtoolboxv1alpha1.AnnotationSpecHash: "h1"}},
		Spec:       appsv1.DeploymentSpec{Replicas: new(int32(3))},
	}
	readyPod := func(name string) corev1.Pod {
		return podWith("h1", func(p *corev1.Pod) {
			p.Name = name
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		})
	}
	crashing := podWith("h1", func(p *corev1.Pod) {
		p.Name = "pga-2"
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "pgadmin", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	})
	quota := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "team-pgadmin"}, Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: `pods "team-pgadmin-x" is forbidden: exceeded quota: pods`,
	}}}}
	cases := []struct {
		name     string
		pods     []corev1.Pod
		observed *appsv1.Deployment
		want     pgtoolboxv1alpha1.Reason
	}{
		{"one of three, quota exceeded", []corev1.Pod{readyPod("pga-0")}, quota, pgtoolboxv1alpha1.ReasonReplicaFailure},
		{"three of three", []corev1.Pod{readyPod("pga-0"), readyPod("pga-1"), readyPod("pga-2")}, nil, pgtoolboxv1alpha1.ReasonAsExpected},
		{"one of three", []corev1.Pod{readyPod("pga-0")}, nil, pgtoolboxv1alpha1.ReasonRolloutInProgress},
		{"the third crash-loops", []corev1.Pod{readyPod("pga-0"), readyPod("pga-1"), crashing}, nil, pgtoolboxv1alpha1.ReasonCrashLoopBackOff},
		{"an old-template Pod does not count", []corev1.Pod{readyPod("pga-0"), readyPod("pga-1"), podWith("h0", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		})}, nil, pgtoolboxv1alpha1.ReasonRolloutInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workloadFact(deployment, tc.observed, &corev1.PodList{Items: tc.pods}); got.Reason != tc.want {
				t.Fatalf("reason %s (%s), want %s", got.Reason, got.Message, tc.want)
			}
		})
	}
}

func TestJobOutcomeAndReport(t *testing.T) {
	running := &batchv1.Job{}
	if finished, failed, _ := jobOutcome(running); finished || failed {
		t.Fatal("running job reported finished")
	}
	failedJob := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "3 failures"}}}}
	if finished, failed, message := jobOutcome(failedJob); !finished || !failed || message != "BackoffLimitExceeded: 3 failures" {
		t.Fatalf("failed job: %v %v %q", finished, failed, message)
	}
	if r := parseReport(`{"pgAdminVersion":"9.18","servers":4}`); r.PgAdminVersion != "9.18" || r.Servers != 4 {
		t.Fatalf("report %+v", r)
	}
	if r := parseReport(`{"pgAdminVersion":"9.18\nInjected: x"}`); r.PgAdminVersion != "" {
		t.Fatalf("a version with a line break must be dropped: %+v", r)
	}
	if r := parseReport(`{"pgAdminVersion":"9.18\t","servers":-3}`); r.PgAdminVersion != "" || r.Servers != 0 {
		t.Fatalf("a tab or a negative count reached status: %+v", r)
	}
	if r := parseReport("not json"); r != (registrationReport{}) {
		t.Fatalf("garbage: %+v", r)
	}
}

func TestExposureFacts(t *testing.T) {
	admitted := &routev1.Route{Status: routev1.RouteStatus{Ingress: []routev1.RouteIngress{{
		RouterName: "default",
		Conditions: []routev1.RouteIngressCondition{{Type: routev1.RouteAdmitted, Status: corev1.ConditionTrue}},
	}}}}
	if f := routeFact(admitted); f.Status != metav1.ConditionTrue {
		t.Fatalf("admitted route: %+v", f)
	}
	rejected := &routev1.Route{Status: routev1.RouteStatus{Ingress: []routev1.RouteIngress{{
		RouterName: "default",
		Conditions: []routev1.RouteIngressCondition{{Type: routev1.RouteAdmitted, Status: corev1.ConditionFalse, Reason: "HostAlreadyClaimed"}},
	}}}}
	if f := routeFact(rejected); f.Reason != pgtoolboxv1alpha1.ReasonNotAdmitted || !strings.Contains(f.Message, "HostAlreadyClaimed") {
		t.Fatalf("rejected route: %+v", f)
	}
	if f := ingressFact(&networkingv1.Ingress{}); f.Status != metav1.ConditionUnknown {
		t.Fatalf("ingress without address: %+v", f)
	}
	route := &gatewayv1.HTTPRoute{Status: gatewayv1.HTTPRouteStatus{RouteStatus: gatewayv1.RouteStatus{Parents: []gatewayv1.RouteParentStatus{{
		ParentRef:  gatewayv1.ParentReference{Name: "gw"},
		Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionFalse, Reason: "NotAllowedByListeners"}},
	}}}}}
	if f := httpRouteFact(route); f.Reason != pgtoolboxv1alpha1.ReasonNotAccepted {
		t.Fatalf("http route: %+v", f)
	}
}

func TestExposureURL(t *testing.T) {
	if got := exposureURL(oauth2PgAdmin()); got != "http://team-pgadmin.team-a.svc/" {
		t.Errorf("clusterIP: %s", got)
	}
	if got := exposureURL(openshiftPgAdmin()); got != "https://pgadmin.apps.example.com/" {
		t.Errorf("route: %s", got)
	}
	if got := exposureURL(ingressPgAdmin()); got != "https://pgadmin.example.com/" {
		t.Errorf("ingress tls: %s", got)
	}
}
