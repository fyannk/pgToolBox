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
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// The optional NetworkPolicy. NetworkPolicy is never a product default or
// precondition (the admin decides whether the cluster enforces one), so it
// is off unless spec.networkPolicy.enabled, and what it does to egress is
// reported, not enforced, by PgAdminEgressConfined.

// apiServerEndpoint is one address of the default/kubernetes Service's
// endpoints: egress policy is evaluated after the Service DNAT, so the
// rule names these, not the Service IP.
type apiServerEndpoint struct {
	IP    string
	Ports []int32
}

// apiServerEndpoints are the API server's endpoints, or none when they
// could not be read (the rule then opens the usual ports to any address).
type apiServerEndpoints []apiServerEndpoint

// Annotation on the NetworkPolicy naming each generated egress rule, in
// order, so the egress statement names what it reports.
const annotationEgressRules = pgtoolboxv1alpha1.ReservedKeyPrefix + "egress-rules"

func tcp(port int32) networkingv1.NetworkPolicyPort {
	protocol := corev1.ProtocolTCP
	value := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &value}
}

func udp(port int32) networkingv1.NetworkPolicyPort {
	protocol := corev1.ProtocolUDP
	value := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &value}
}

// buildNetworkPolicy renders the policy, or nil when it is not enabled.
func buildNetworkPolicy(p *pgtoolboxv1alpha1.PgAdmin, in buildInputs) *networkingv1.NetworkPolicy {
	spec := &p.Spec.NetworkPolicy
	if !spec.IsEnabled() {
		return nil
	}
	ingressPort := pgAdminPort
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		ingressPort = proxyPort
	}
	var ruleNames []string
	var egress []networkingv1.NetworkPolicyEgressRule
	add := func(name string, rule networkingv1.NetworkPolicyEgressRule) {
		ruleNames = append(ruleNames, name)
		egress = append(egress, rule)
	}

	// DNS: any Pod of the cluster on 53 (the resolver's namespace and
	// labels differ per platform; in-cluster Pods only, no address).
	add("dns", networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{}}},
		Ports: []networkingv1.NetworkPolicyPort{udp(53), tcp(53), udp(5353), tcp(5353)},
	})

	// PostgreSQL: the registered Clusters' instances and the Poolers the
	// endpoints use, in this namespace and in the namespaces of granted
	// Clusters (and the configDatabase Cluster, among in.Clusters).
	var peers []networkingv1.NetworkPolicyPeer
	if len(in.Clusters) > 0 {
		peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "cnpg.io/cluster", Operator: metav1.LabelSelectorOpIn, Values: in.Clusters}},
		}})
	}
	if len(in.Poolers) > 0 {
		peers = append(peers, networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "cnpg.io/poolerName", Operator: metav1.LabelSelectorOpIn, Values: in.Poolers}},
		}})
	}
	// Granted Clusters of other namespaces (PgAdminAccess), by namespace.
	for _, namespace := range slices.Sorted(maps.Keys(in.RemoteClusters)) {
		names := slices.Sorted(slices.Values(in.RemoteClusters[namespace]))
		inNamespace := &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}
		peers = append(peers, networkingv1.NetworkPolicyPeer{NamespaceSelector: inNamespace, PodSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "cnpg.io/cluster", Operator: metav1.LabelSelectorOpIn, Values: names}},
		}})
		if len(in.Poolers) > 0 {
			peers = append(peers, networkingv1.NetworkPolicyPeer{NamespaceSelector: inNamespace.DeepCopy(), PodSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "cnpg.io/poolerName", Operator: metav1.LabelSelectorOpIn, Values: in.Poolers}},
			}})
		}
	}
	if len(peers) > 0 {
		add("postgresql", networkingv1.NetworkPolicyEgressRule{To: peers, Ports: []networkingv1.NetworkPolicyPort{tcp(postgresPort)}})
	}

	// The API server, for passfile-sync (and the oauth-proxy).
	if len(in.APIServer) > 0 {
		var apiPeers []networkingv1.NetworkPolicyPeer
		var apiPorts []networkingv1.NetworkPolicyPort
		seen := map[int32]bool{}
		for _, endpoint := range in.APIServer {
			apiPeers = append(apiPeers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: hostCIDR(endpoint.IP)}})
			for _, port := range endpoint.Ports {
				if !seen[port] {
					seen[port] = true
					apiPorts = append(apiPorts, tcp(port))
				}
			}
		}
		add("kubernetes-api", networkingv1.NetworkPolicyEgressRule{To: apiPeers, Ports: apiPorts})
	} else {
		add("kubernetes-api", networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(443), tcp(6443)}})
	}

	// The identity provider: pgAdmin's server-side OAuth2 calls, or the
	// oauth-proxy's calls to the OpenShift OAuth server.
	idp := networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(443)}}
	for _, cidr := range spec.IdentityProviderCIDRs {
		idp.To = append(idp.To, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	add("identity-provider", idp)

	for i, rule := range spec.ExtraEgress {
		add(fmt.Sprintf("extraEgress[%d]", i), *rule.DeepCopy())
	}

	ingress := networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{tcp(ingressPort)}}
	for i := range spec.IngressFrom {
		ingress.From = append(ingress.From, *spec.IngressFrom[i].DeepCopy())
	}

	meta := objectMeta(p, namesOf(p).base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetAll)
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[annotationEgressRules] = strings.Join(ruleNames, ",")
	policy := &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: meta,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: identity(p).SelectorLabels(componentPgAdmin)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{ingress},
			Egress:      egress,
		},
	}
	specs.SetHash(policy, specs.Hash(policy.Spec))
	return policy
}

func hostCIDR(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip + "/32"
	}
	return netip.PrefixFrom(addr, addr.BitLen()).String()
}

// egressStatement says whether policy confines pgAdmin's egress to named
// destinations, and names every rule that does not: a rule with no
// destination, or with an address block of prefix length 0. A nil policy
// confines nothing.
func egressStatement(policy *networkingv1.NetworkPolicy) (confined bool, open []string) {
	if policy == nil {
		return false, []string{"spec.networkPolicy is not enabled: egress is whatever the namespace allows"}
	}
	names := strings.Split(policy.Annotations[annotationEgressRules], ",")
	for i, rule := range policy.Spec.Egress {
		name := fmt.Sprintf("egress[%d]", i)
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		if why := openRule(rule); why != "" {
			open = append(open, name+" "+why)
		}
	}
	return len(open) == 0, open
}

// openRule returns why a rule allows unnamed destinations, or "".
func openRule(rule networkingv1.NetworkPolicyEgressRule) string {
	ports := describePorts(rule.Ports)
	if len(rule.To) == 0 {
		return "allows " + ports + " to any address"
	}
	for _, peer := range rule.To {
		if peer.IPBlock == nil {
			continue
		}
		prefix, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil || prefix.Bits() == 0 {
			return "allows " + ports + " to " + peer.IPBlock.CIDR
		}
	}
	return ""
}

func describePorts(ports []networkingv1.NetworkPolicyPort) string {
	if len(ports) == 0 {
		return "every port"
	}
	var out []string
	for _, port := range ports {
		protocol := "TCP"
		if port.Protocol != nil {
			protocol = string(*port.Protocol)
		}
		value := "*"
		if port.Port != nil {
			value = port.Port.String()
		}
		out = append(out, protocol+"/"+value)
	}
	slices.Sort(out)
	return "port " + strings.Join(out, ",")
}
