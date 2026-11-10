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
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// openShiftIngressPolicyGroup labels the namespaces of the OpenShift
// routers, host-network ones included (OVN-Kubernetes matches host-network
// traffic from the routers to this label).
const openShiftIngressPolicyGroup = "policy-group.network.openshift.io/ingress"

// anyPeer is TRUSTED_PROXY_CIDRS when the NetworkPolicy admits only the
// proxies: every peer that reaches the port is one.
var anyPeer = []string{"0.0.0.0/0", "::/0"}

// exposurePolicyName is the console's ingress NetworkPolicy.
func exposurePolicyName(c *pgtoolboxv1alpha1.PgToolBoxConsole) string {
	return c.WorkloadName() + "-ingress"
}

// exposurePolicyPeers are who the ingress NetworkPolicy admits to the
// console port, nil when no policy is rendered: not exposed, turned off,
// or ingress and gateway exposure without from (the controller's namespace
// differs per installation, so no default is safe). For a Route the
// OpenShift router namespaces are always admitted, first; from adds to
// them (replacing them would cut the console off its own Route).
func exposurePolicyPeers(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) []networkingv1.NetworkPolicyPeer {
	if !exposed {
		return nil
	}
	spec := c.Spec.Exposure.NetworkPolicy
	if spec != nil && spec.Enabled != nil && !*spec.Enabled {
		return nil
	}
	var peers []networkingv1.NetworkPolicyPeer
	if c.Spec.Exposure.TypeOrDefault() == pgtoolboxv1alpha1.ExposureTypeRoute {
		peers = append(peers, routerPeer())
	}
	if spec != nil {
		for i := range spec.From {
			peer := spec.From[i].NetworkPolicyPeer()
			if slices.ContainsFunc(peers, func(p networkingv1.NetworkPolicyPeer) bool { return equality.Semantic.DeepEqual(p, peer) }) {
				continue
			}
			peers = append(peers, peer)
		}
	}
	if len(peers) == 0 {
		return nil
	}
	return peers
}

// routerPeer is every Pod of the namespaces labelled as OpenShift ingress.
func routerPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{openShiftIngressPolicyGroup: ""},
	}}
}

// broadPeer describes the first peer that admits more than a proxy can be,
// "" when none: every namespace (an empty namespaceSelector, whatever the
// podSelector: any namespace owner can label a Pod), every Pod of the
// console's namespace (an empty podSelector alone), or every address (an
// ipBlock of prefix length 0). A selector with no label and no expression
// is empty.
func broadPeer(peers []networkingv1.NetworkPolicyPeer) string {
	empty := func(s *metav1.LabelSelector) bool {
		return s != nil && len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
	}
	for i, peer := range peers {
		switch {
		case empty(peer.NamespaceSelector):
			return fmt.Sprintf("peer %d has an empty namespaceSelector: it admits Pods of every namespace", i)
		case peer.NamespaceSelector == nil && empty(peer.PodSelector):
			return fmt.Sprintf("peer %d has an empty podSelector alone: it admits every Pod of the namespace", i)
		case peer.IPBlock != nil:
			if prefix, err := netip.ParsePrefix(peer.IPBlock.CIDR); err != nil || prefix.Bits() == 0 {
				return fmt.Sprintf("peer %d has ipBlock %s: it admits every address", i, peer.IPBlock.CIDR)
			}
		}
	}
	return ""
}

// policyBypass names the first other NetworkPolicy of the namespace that
// admits, to the console port of Pods labelled podLabels, a peer the
// exposure's own policy does not list, "" when none. NetworkPolicies add
// up: any one of them that admits a peer lets it in. A peer is the
// exposure's only when it is one of ours exactly (a narrower peer written
// differently counts as another, and turns the trust off: false alarms
// are the safe side). own is the exposure policy's name, skipped.
func policyBypass(policies []networkingv1.NetworkPolicy, own string, podLabels map[string]string, ours []networkingv1.NetworkPolicyPeer) string {
	for i := range policies {
		np := &policies[i]
		if np.Name == own || !appliesToIngress(np) {
			continue
		}
		if selector, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err == nil && !selector.Matches(labels.Set(podLabels)) {
			continue
		}
		for _, rule := range np.Spec.Ingress {
			if !portsAdmitHTTP(rule.Ports) {
				continue
			}
			if len(rule.From) == 0 {
				return "NetworkPolicy " + np.Name + " admits every source to the console port"
			}
			for _, peer := range rule.From {
				if !slices.ContainsFunc(ours, func(p networkingv1.NetworkPolicyPeer) bool { return equality.Semantic.DeepEqual(p, peer) }) {
					return "NetworkPolicy " + np.Name + " admits a peer the exposure does not list to the console port"
				}
			}
		}
	}
	return ""
}

// appliesToIngress: no policyTypes means Ingress (and Egress when there
// are egress rules).
func appliesToIngress(np *networkingv1.NetworkPolicy) bool {
	return len(np.Spec.PolicyTypes) == 0 || slices.Contains(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
}

// portsAdmitHTTP: a rule without ports admits every port; else one of its
// ports is the console's (by name, number or range), over TCP.
func portsAdmitHTTP(ports []networkingv1.NetworkPolicyPort) bool {
	if len(ports) == 0 {
		return true
	}
	for _, port := range ports {
		if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP {
			continue
		}
		switch {
		case port.Port == nil:
			return true
		case port.Port.Type == intstr.String:
			if port.Port.StrVal == "http" {
				return true
			}
		case port.Port.IntVal == httpPort:
			return true
		case port.EndPort != nil && port.Port.IntVal <= httpPort && httpPort <= *port.EndPort:
			return true
		}
	}
	return false
}

// trustsAllPeers: a container of pod believes X-Forwarded-For from any
// peer (TRUSTED_PROXY_CIDRS is the any-peer list).
func trustsAllPeers(pod *corev1.Pod) bool {
	all := strings.Join(anyPeer, ",")
	for _, container := range pod.Spec.Containers {
		for _, e := range container.Env {
			if e.Name == "TRUSTED_PROXY_CIDRS" && e.Value == all {
				return true
			}
		}
	}
	return false
}

// proxyTrust is what the console believes of X-Forwarded-For.
type proxyTrust struct {
	Hops  int32
	CIDRs []string
}

// effectiveProxyTrust follows the exposure that is published, not the
// one requested: no Route without the Route API, nothing at all while the
// hostname is refused. Hops default to one in front of a published
// exposure. The trusted peers are the spec's CIDRs; without them, every
// peer when the NetworkPolicy admits only the proxies (no peer of it is
// broad, and bypass, the other NetworkPolicy that admits more, is ""),
// and none otherwise (the console then ignores the header, whatever the
// hops).
func effectiveProxyTrust(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, bypass string) proxyTrust {
	var trust proxyTrust
	if exposed {
		trust.Hops = 1
	}
	spec := c.Spec.Exposure.TrustedProxy
	if spec != nil && spec.Hops != nil {
		trust.Hops = *spec.Hops
	}
	if trust.Hops <= 0 {
		return proxyTrust{}
	}
	if spec != nil && len(spec.CIDRs) > 0 {
		for _, cidr := range spec.CIDRs {
			// CEL refused what is not a CIDR; an entry that still does
			// not parse is never trusted.
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				trust.CIDRs = append(trust.CIDRs, prefix.Masked().String())
			}
		}
		return trust
	}
	if peers := exposurePolicyPeers(c, exposed); peers != nil && broadPeer(peers) == "" && bypass == "" {
		trust.CIDRs = append(trust.CIDRs, anyPeer...)
	}
	return trust
}

// proxyEnv is TRUSTED_PROXY_HOPS and TRUSTED_PROXY_CIDRS.
func proxyEnv(trust proxyTrust) []corev1.EnvVar {
	if trust.Hops <= 0 {
		return nil
	}
	envs := []corev1.EnvVar{env("TRUSTED_PROXY_HOPS", strconv.Itoa(int(trust.Hops)))}
	if len(trust.CIDRs) > 0 {
		envs = append(envs, env("TRUSTED_PROXY_CIDRS", strings.Join(trust.CIDRs, ",")))
	}
	return envs
}

// buildExposureNetworkPolicy renders the console's ingress NetworkPolicy,
// nil when none applies. The console port admits only the exposure's
// peers; the probe port (health, and /metrics, which authenticates every
// scrape and never reads X-Forwarded-For) stays open to any source, so
// that kubelets and Prometheus are not cut off. Egress is not policed.
func buildExposureNetworkPolicy(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) *networkingv1.NetworkPolicy {
	peers := exposurePolicyPeers(c, exposed)
	if peers == nil {
		return nil
	}
	port := func(name string) []networkingv1.NetworkPolicyPort {
		protocol, value := corev1.ProtocolTCP, intstr.FromString(name)
		return []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &value}}
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: objectMeta(c, exposurePolicyName(c), pgtoolboxv1alpha1.PropagationTargetAll),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: selectorLabels(c)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{From: peers, Ports: port("http")},
				{Ports: port("probes")},
			},
		},
	}
}

// exposureConfinedFact is the advisory ExposureConfined, nil when nothing
// is published (the condition is then removed). bypass is policyBypass's
// finding.
func exposureConfinedFact(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, bypass string) *conditionFact {
	if !exposed {
		return nil
	}
	trust := effectiveProxyTrust(c, exposed, bypass)
	believed := "X-Forwarded-For is ignored (trustedProxy.hops is 0)"
	switch {
	case trust.Hops > 0 && len(trust.CIDRs) == 0:
		believed = "X-Forwarded-For is ignored: no trusted peer (set trustedProxy.cidrs, or networkPolicy.from); every client shares the proxy's address and sign-in budget"
	case trust.Hops > 0:
		believed = "X-Forwarded-For is read " + strconv.Itoa(int(trust.Hops)) + " hop(s) deep from peers in " + strings.Join(trust.CIDRs, ",")
	}
	peers := exposurePolicyPeers(c, exposed)
	if peers != nil {
		if broad := broadPeer(peers); broad != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposurePeersBroad,
				Message: "NetworkPolicy " + exposurePolicyName(c) + ": networkPolicy.from " + broad + "; " + believed,
			}
		}
		if bypass != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposurePolicyBypassed,
				Message: bypass + " (NetworkPolicies add up): NetworkPolicy " + exposurePolicyName(c) + " does not confine it; " + believed,
			}
		}
		return &conditionFact{
			Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected,
			Message: "NetworkPolicy " + exposurePolicyName(c) + " admits only the exposure's peers to the console port, and no other NetworkPolicy admits more (the CNI must enforce them); " + believed,
		}
	}
	why := "networkPolicy.enabled is false"
	if spec := c.Spec.Exposure.NetworkPolicy; spec == nil || spec.Enabled == nil || *spec.Enabled {
		why = "networkPolicy.from is not set, and the " + string(c.Spec.Exposure.TypeOrDefault()) + " controller's namespace has no safe default"
	}
	return &conditionFact{
		Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposureOpen,
		Message: "no ingress NetworkPolicy (" + why + "): any Pod can reach the console port; " + believed,
	}
}

// setExposureConfined records the advisory ExposureConfined, or removes
// it when nothing is published.
func setExposureConfined(console *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, bypass string) {
	fact := exposureConfinedFact(console, exposed, bypass)
	if fact == nil {
		meta.RemoveStatusCondition(&console.Status.Conditions, pgtoolboxv1alpha1.ConditionExposureConfined)
		return
	}
	setCondition(console, pgtoolboxv1alpha1.ConditionExposureConfined, fact.Status, fact.Reason, fact.Message)
}

// conditionFact is one condition's status, reason and message.
type conditionFact struct {
	Status  metav1.ConditionStatus
	Reason  pgtoolboxv1alpha1.Reason
	Message string
}
