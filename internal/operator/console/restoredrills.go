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
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// The labels the console puts on a drill Cluster and on its
// inheritedMetadata, so on the drill's Pods and recovery Job
// (internal/console/ops: LabelDrillOwner, LabelRestoreDrillOf; a test pins
// them). The drill-owner value is the console's namespace.
const (
	drillOwnerLabel = pgtoolboxv1alpha1.ReservedKeyPrefix + "drill-owner"
	drillOfLabel    = pgtoolboxv1alpha1.ReservedKeyPrefix + "restore-drill-of"
)

// The CloudNativePG instance's ports: the instance manager's status
// endpoint and the exporter.
const (
	instanceStatusPort  int32 = 8000
	instanceMetricsPort int32 = 9187
)

const defaultDrillOperatorNamespace = "cnpg-system"

// restoreDrillsNetworkPolicyName is the drill Pods' NetworkPolicy, deleted
// when drills or the policy are turned off.
func restoreDrillsNetworkPolicyName(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	return c.WorkloadName() + "-restore-drills"
}

func drillTCP(port int32) networkingv1.NetworkPolicyPort {
	protocol, value := corev1.ProtocolTCP, intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &value}
}

func drillUDP(port int32) networkingv1.NetworkPolicyPort {
	protocol, value := corev1.ProtocolUDP, intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &value}
}

// buildDrillNetworkPolicy is the policy around the drill Pods: a promoted drill is a live copy of a production database,
// and what the console cannot turn off (an extension, a function somebody
// calls) must not reach the network. Egress: DNS, the API server (the
// instance manager, the barman-cloud sidecar), the object store; ingress:
// the CloudNativePG operator and this console, on the instance's ports.
// Nil when it is not enabled.
func buildDrillNetworkPolicy(c *pgtoolboxv1alpha1.PgToolBoxConsole) *networkingv1.NetworkPolicy {
	if !c.RestoreDrillNetworkPolicyEnabled() {
		return nil
	}
	spec := &c.Spec.RestoreDrills.NetworkPolicy
	operatorNamespace := spec.OperatorNamespace
	if operatorNamespace == "" {
		operatorNamespace = defaultDrillOperatorNamespace
	}
	inNamespace := func(name string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": name}}
	}

	apiServer := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{drillTCP(443), drillTCP(6443)}}
	for _, cidr := range spec.APIServerCIDRs {
		apiServer.To = append(apiServer.To, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: canonicalCIDR(cidr)}})
	}
	objectStore := networkingv1.NetworkPolicyEgressRule{}
	for i := range spec.ObjectStore {
		objectStore.To = append(objectStore.To, *spec.ObjectStore[i].DeepCopy())
	}
	ports := spec.ObjectStorePorts
	if len(ports) == 0 {
		ports = []int32{443}
	}
	for _, port := range ports {
		objectStore.Ports = append(objectStore.Ports, drillTCP(port))
	}

	policy := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: objectMeta(c, restoreDrillsNetworkPolicyName(c), pgtoolboxv1alpha1.PropagationTargetAll),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels:      map[string]string{drillOwnerLabel: c.Namespace},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: drillOfLabel, Operator: metav1.LabelSelectorOpExists}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: inNamespace(operatorNamespace)}},
					Ports: []networkingv1.NetworkPolicyPort{drillTCP(instanceStatusPort)},
				},
				{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: inNamespace(c.Namespace),
						PodSelector:       &metav1.LabelSelector{MatchLabels: selectorLabels(c)},
					}},
					Ports: []networkingv1.NetworkPolicyPort{drillTCP(instanceStatusPort), drillTCP(instanceMetricsPort)},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// DNS: any Pod of the cluster on 53 (the resolver's namespace
				// and labels differ per platform), as the pgAdmin policy.
				{
					To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
					Ports: []networkingv1.NetworkPolicyPort{drillUDP(53), drillTCP(53), drillUDP(5353), drillTCP(5353)},
				},
				apiServer,
				objectStore,
			},
		},
	}
	specs.SetHash(policy, specs.Hash(policy.Spec))
	return policy
}

// canonicalCIDR leaves a CIDR the CRD pattern admitted as it is, or turns a
// bare address into a host prefix.
func canonicalCIDR(cidr string) string {
	if prefix, err := netip.ParsePrefix(cidr); err == nil {
		return prefix.String()
	}
	if addr, err := netip.ParseAddr(cidr); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()).String()
	}
	return cidr
}
