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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/ops"
)

func drillConsole(mutate func(*pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec)) *pgtoolboxv1alpha1.PgToolBoxConsole {
	c := &pgtoolboxv1alpha1.PgToolBoxConsole{ObjectMeta: metav1.ObjectMeta{Name: "console", Namespace: "team-a", UID: "u"}}
	c.Spec.RestoreDrills = &pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec{Enabled: true}
	if mutate != nil {
		mutate(c.Spec.RestoreDrills)
	}
	return c
}

// The NetworkPolicy selects the labels the console puts on a drill: if they
// drift apart, the policy fences nothing.
func TestDrillLabelsAreTheConsoles(t *testing.T) {
	if drillOwnerLabel != ops.LabelDrillOwner || drillOfLabel != ops.LabelRestoreDrillOf {
		t.Fatalf("operator drill labels %q, %q; console %q, %q", drillOwnerLabel, drillOfLabel, ops.LabelDrillOwner, ops.LabelRestoreDrillOf)
	}
}

func TestDrillNetworkPolicy(t *testing.T) {
	t.Run("none without drills, or when turned off", func(t *testing.T) {
		off := drillConsole(nil)
		off.Spec.RestoreDrills.Enabled = false
		if buildDrillNetworkPolicy(off) != nil {
			t.Fatal("a policy without drills")
		}
		if buildDrillNetworkPolicy(drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) {
			d.NetworkPolicy.Enabled = new(false)
		})) != nil {
			t.Fatal("a policy turned off")
		}
	})

	t.Run("by default: the drill Pods, DNS, the API server and the object store on 443", func(t *testing.T) {
		policy := buildDrillNetworkPolicy(drillConsole(nil))
		if policy == nil {
			t.Fatal("no policy with drills on")
		}
		if policy.Name != "console-restore-drills" || policy.Namespace != "team-a" {
			t.Fatalf("policy %s/%s", policy.Namespace, policy.Name)
		}
		selector := policy.Spec.PodSelector
		if selector.MatchLabels[drillOwnerLabel] != "team-a" || len(selector.MatchExpressions) != 1 ||
			selector.MatchExpressions[0].Key != drillOfLabel || selector.MatchExpressions[0].Operator != metav1.LabelSelectorOpExists {
			t.Fatalf("pod selector %+v", selector)
		}
		if !slices.Equal(policy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}) {
			t.Fatalf("policy types %v", policy.Spec.PolicyTypes)
		}
		if len(policy.Spec.Egress) != 3 {
			t.Fatalf("egress rules %d, want DNS, API server, object store", len(policy.Spec.Egress))
		}
		if api := policy.Spec.Egress[1]; len(api.To) != 0 || len(api.Ports) != 2 {
			t.Fatalf("API server rule %+v", api)
		}
		if store := policy.Spec.Egress[2]; len(store.To) != 0 || len(store.Ports) != 1 || store.Ports[0].Port.IntVal != 443 {
			t.Fatalf("object store rule %+v", store)
		}
		operator := policy.Spec.Ingress[0]
		if operator.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "cnpg-system" ||
			len(operator.Ports) != 1 || operator.Ports[0].Port.IntVal != instanceStatusPort {
			t.Fatalf("operator ingress %+v", operator)
		}
		console := policy.Spec.Ingress[1]
		if console.From[0].PodSelector.MatchLabels["app.kubernetes.io/instance"] != "console" || len(console.Ports) != 2 {
			t.Fatalf("console ingress %+v", console)
		}
		for _, rule := range policy.Spec.Ingress {
			for _, port := range rule.Ports {
				if *port.Protocol != corev1.ProtocolTCP {
					t.Fatalf("ingress port %+v", port)
				}
			}
		}
	})

	t.Run("closed to the given addresses and peers", func(t *testing.T) {
		store := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.10/32"}}
		policy := buildDrillNetworkPolicy(drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) {
			d.NetworkPolicy = pgtoolboxv1alpha1.PgToolBoxConsoleDrillNetworkPolicy{
				OperatorNamespace: "cnpg", APIServerCIDRs: []string{"10.0.0.1/32", "fd00::1/128"},
				ObjectStore: []networkingv1.NetworkPolicyPeer{store}, ObjectStorePorts: []int32{9000},
			}
		}))
		api := policy.Spec.Egress[1]
		if len(api.To) != 2 || api.To[0].IPBlock.CIDR != "10.0.0.1/32" || api.To[1].IPBlock.CIDR != "fd00::1/128" {
			t.Fatalf("API server rule %+v", api)
		}
		objectStore := policy.Spec.Egress[2]
		if len(objectStore.To) != 1 || objectStore.To[0].IPBlock.CIDR != "192.0.2.10/32" || objectStore.Ports[0].Port.IntVal != 9000 {
			t.Fatalf("object store rule %+v", objectStore)
		}
		if policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "cnpg" {
			t.Fatalf("operator namespace %+v", policy.Spec.Ingress[0])
		}
	})
}

func TestRestoreDrillEnvCap(t *testing.T) {
	value := func(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
		for _, e := range restoreDrillEnv(c) {
			if e.Name == "RESTORE_DRILL_MAX_CONCURRENT" {
				return e.Value
			}
		}
		return ""
	}
	if got := value(drillConsole(nil)); got != "3" {
		t.Fatalf("default cap %q, want 3 (the chart's)", got)
	}
	if got := value(drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) { d.MaxConcurrent = new(int32(7)) })); got != "7" {
		t.Fatalf("cap %q, want 7", got)
	}
}

// The drill's server CA follows who keeps the instance CA bundle: the
// operator's mirror follows a drill's own CA; a bundle the user keeps was
// filled before the drill, so the drill is signed by the source's CA.
func TestRestoreDrillEnvServerCA(t *testing.T) {
	value := func(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
		for _, e := range restoreDrillEnv(c) {
			if e.Name == "RESTORE_DRILL_SERVER_CA" {
				return e.Value
			}
		}
		return ""
	}
	if got := value(drillConsole(nil)); got != "own" {
		t.Fatalf("mirrored bundle: %q, want own", got)
	}
	c := drillConsole(nil)
	c.Spec.InstanceCABundleRef = &pgtoolboxv1alpha1.LocalObjectReference{Name: "team-ca"}
	if got := value(c); got != "source" {
		t.Fatalf("user-kept bundle: %q, want source", got)
	}
}

// The drill seal's own keys: a Secret named in sealKeySecretRef is mounted
// read-only as a directory and RESTORE_DRILL_SEAL_KEY_FILE points at its
// key; unset, or with drills off, neither is rendered and the console
// derives the keys from the session secret.
func TestRestoreDrillSealKey(t *testing.T) {
	seal := &pgtoolboxv1alpha1.SecretKeyReference{Name: "drill-seal", Key: "keys"}
	cases := map[string]struct {
		console  *pgtoolboxv1alpha1.PgToolBoxConsole
		wantFile string
	}{
		"unset": {console: drillConsole(nil)},
		"set": {
			console:  drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) { d.SealKeySecretRef = seal }),
			wantFile: "/var/run/console/restore-drill-seal/keys",
		},
		"set with drills off": {console: drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) {
			d.Enabled, d.SealKeySecretRef = false, seal
		})},
		"set with operations off": {console: func() *pgtoolboxv1alpha1.PgToolBoxConsole {
			c := drillConsole(func(d *pgtoolboxv1alpha1.PgToolBoxConsoleRestoreDrillsSpec) { d.SealKeySecretRef = seal })
			c.Spec.Operations = new(false)
			return c
		}()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var file string
			for _, e := range restoreDrillEnv(tc.console) {
				if e.Name == "RESTORE_DRILL_SEAL_KEY_FILE" {
					file = e.Value
				}
			}
			if file != tc.wantFile {
				t.Fatalf("RESTORE_DRILL_SEAL_KEY_FILE %q, want %q", file, tc.wantFile)
			}
			mounts, volumes := consoleVolumes(tc.console)
			var mount *corev1.VolumeMount
			for i := range mounts {
				if mounts[i].Name == "restore-drill-seal" {
					mount = &mounts[i]
				}
			}
			var volume *corev1.Volume
			for i := range volumes {
				if volumes[i].Name == "restore-drill-seal" {
					volume = &volumes[i]
				}
			}
			if tc.wantFile == "" {
				if mount != nil || volume != nil {
					t.Fatalf("seal key mounted without a seal key file: %+v %+v", mount, volume)
				}
				return
			}
			if mount == nil || !mount.ReadOnly || mount.MountPath != "/var/run/console/restore-drill-seal" || mount.SubPath != "" {
				t.Fatalf("seal key mount %+v: want a read-only directory mount", mount)
			}
			if volume == nil || volume.Secret == nil || volume.Secret.SecretName != "drill-seal" ||
				len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Key != "keys" || volume.Secret.Items[0].Path != "keys" {
				t.Fatalf("seal key volume %+v", volume)
			}
		})
	}
}
