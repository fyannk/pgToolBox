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

// openShiftLegacyIngressLabel and its value label the same router
// namespaces the older way; the OpenShift project template's
// allow-from-openshift-ingress policy selects them by it.
const (
	openShiftLegacyIngressLabel = "network.openshift.io/policy-group"
	openShiftLegacyIngressValue = "ingress"
)

// anyPeer is TRUSTED_PROXY_CIDRS when the NetworkPolicy admits only the
// proxies: every peer that reaches the port is one.
var anyPeer = []string{"0.0.0.0/0", "::/0"}

// annotationTrustsEveryPeer marks the Pod template of a workload whose
// any-peer trust the operator granted from its NetworkPolicy: the policy
// is withdrawn only once no console Pod carries it. The same
// TRUSTED_PROXY_CIDRS written in trustedProxy.cidrs is the user's own
// trust and holds the policy back from nothing.
const annotationTrustsEveryPeer = pgtoolboxv1alpha1.ReservedKeyPrefix + "trusts-every-peer"

// The narrowest ipBlock that still counts as broad is one bit wider than
// these: a /7 (IPv4) or a /31 (IPv6). With at most 16 peers of at most a
// /8 each, the blocks together never cover every address either.
const (
	minIPv4BlockBits = 8
	minIPv6BlockBits = 32
)

// privateRanges are the address ranges a Pod network is cut from: an
// ipBlock that holds a whole one admits the Pods, not only the proxies.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fc00::/7"),
}

// confinement is what the reconciler found, beyond the spec, that keeps
// the exposure's NetworkPolicy from confining the console port; the zero
// value is nothing.
type confinement struct {
	// Broad names a networkPolicy.from peer that the live Pods show to be
	// broad (an ipBlock that holds a console Pod's address), "" when none.
	Broad string
	// Bypass names another NetworkPolicy that admits more (policyBypass),
	// "" when none.
	Bypass string
}

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
// "" when none. The rule: a selector is broad when it pins no label value
// (no matchLabels and no In expression: only Exists, NotIn, DoesNotExist,
// or nothing), because a key alone is on nearly everything
// (kubernetes.io/metadata.name is on every namespace) and an absent value
// is the default. Broad are then a namespaceSelector that is (Pods of
// nearly every namespace, whatever the podSelector: any namespace owner
// can label a Pod), a podSelector that is, alone (nearly every Pod of the
// console's namespace), and an ipBlock wider than /8 (IPv4) or /32 (IPv6)
// or holding a whole private range (10/8, 172.16/12, 192.168/16,
// 100.64/10, fc00::/7: where Pod networks are cut from). An ipBlock
// holding a console Pod's own address is found by the reconciler
// (podNetworkBlock).
func broadPeer(peers []networkingv1.NetworkPolicyPeer) string {
	for i, peer := range peers {
		switch {
		case peer.NamespaceSelector != nil && !pinsValue(peer.NamespaceSelector):
			return fmt.Sprintf("peer %d has a namespaceSelector that pins no label value: it admits Pods of (nearly) every namespace", i)
		case peer.NamespaceSelector == nil && peer.PodSelector != nil && !pinsValue(peer.PodSelector):
			return fmt.Sprintf("peer %d has a podSelector alone that pins no label value: it admits (nearly) every Pod of the namespace", i)
		case peer.IPBlock != nil:
			if why := broadBlock(peer.IPBlock); why != "" {
				return fmt.Sprintf("peer %d has ipBlock %s: %s", i, peer.IPBlock.CIDR, why)
			}
		}
	}
	return ""
}

// pinsValue: the selector requires a label to hold one of named values
// (a matchLabels entry, or an In expression).
func pinsValue(s *metav1.LabelSelector) bool {
	if len(s.MatchLabels) > 0 {
		return true
	}
	return slices.ContainsFunc(s.MatchExpressions, func(e metav1.LabelSelectorRequirement) bool {
		return e.Operator == metav1.LabelSelectorOpIn
	})
}

// broadBlock says why an ipBlock admits more than proxies, "" when it
// does not (its except entries are not counted: false alarms are the safe
// side).
func broadBlock(block *networkingv1.IPBlock) string {
	prefix, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		return "it does not parse, so it is not trusted"
	}
	prefix = prefix.Masked()
	limit := minIPv4BlockBits
	if prefix.Addr().Is6() {
		limit = minIPv6BlockBits
	}
	if prefix.Bits() < limit {
		return fmt.Sprintf("it is wider than /%d and admits (nearly) every address", limit)
	}
	for _, private := range privateRanges {
		if private.Addr().Is4() == prefix.Addr().Is4() && prefix.Bits() <= private.Bits() && prefix.Contains(private.Addr()) {
			return "it holds the whole private range " + private.String() + ", where Pod networks are cut from"
		}
	}
	return ""
}

// podNetworkBlock names the first ipBlock peer that admits the address of
// a console Pod (its except entries aside), "" when none: such a block
// admits the Pod network, every Pod of the cluster, not the proxies.
func podNetworkBlock(peers []networkingv1.NetworkPolicyPeer, pods []corev1.Pod) string {
	for i, peer := range peers {
		if peer.IPBlock == nil {
			continue
		}
		block, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil {
			continue // broadPeer reports it
		}
		for p := range pods {
			for _, ip := range pods[p].Status.PodIPs {
				addr, err := netip.ParseAddr(ip.IP)
				if err != nil || !block.Contains(addr) || excepted(peer.IPBlock.Except, addr) {
					continue
				}
				return fmt.Sprintf("peer %d has ipBlock %s, which holds the address of console Pod %s: it admits the Pod network", i, peer.IPBlock.CIDR, pods[p].Name)
			}
		}
	}
	return ""
}

func excepted(except []string, addr netip.Addr) bool {
	return slices.ContainsFunc(except, func(cidr string) bool {
		prefix, err := netip.ParsePrefix(cidr)
		return err == nil && prefix.Contains(addr)
	})
}

// policyBypass names the first other NetworkPolicy of the namespace that
// admits, to the console port of a Pod labelled as one of podLabels (the
// template's, and each live console Pod's: pod-template-hash and the
// StatefulSet's labels included), a peer the exposure's own policy does
// not cover, "" when none. NetworkPolicies add up: any one of them that
// admits a peer lets it in. A peer is covered when coveredBy says so;
// any other, narrower or not, turns the trust off (false alarms are the
// safe side). own is the exposure policy's name, skipped.
func policyBypass(policies []networkingv1.NetworkPolicy, own string, podLabels []map[string]string, ours []networkingv1.NetworkPolicyPeer) string {
	for i := range policies {
		np := &policies[i]
		if np.Name == own || !appliesToIngress(np) {
			continue
		}
		if selector, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector); err == nil && !slices.ContainsFunc(podLabels, func(l map[string]string) bool {
			return selector.Matches(labels.Set(l))
		}) {
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
				if !coveredBy(peer, ours) {
					return "NetworkPolicy " + np.Name + " admits a peer the exposure does not list to the console port"
				}
			}
		}
	}
	return ""
}

// coveredBy: every source peer admits, one of ours admits too. It is one
// of ours exactly, or it lies inside one of our namespace-only peers that
// has only matchLabels: its namespaceSelector carries every one of those
// labels (more labels or expressions narrow it further), whatever its
// podSelector (some Pods of those namespaces). An ipBlock is covered only
// by itself.
func coveredBy(peer networkingv1.NetworkPolicyPeer, ours []networkingv1.NetworkPolicyPeer) bool {
	return slices.ContainsFunc(ours, func(p networkingv1.NetworkPolicyPeer) bool {
		if equality.Semantic.DeepEqual(p, peer) {
			return true
		}
		if p.IPBlock != nil || p.PodSelector != nil || p.NamespaceSelector == nil ||
			len(p.NamespaceSelector.MatchLabels) == 0 || len(p.NamespaceSelector.MatchExpressions) > 0 ||
			peer.IPBlock != nil || peer.NamespaceSelector == nil {
			return false
		}
		for k, v := range p.NamespaceSelector.MatchLabels {
			if got, ok := peer.NamespaceSelector.MatchLabels[k]; !ok || got != v {
				return false
			}
		}
		return true
	})
}

// legacyRouterPeer is the OpenShift router namespaces by their older label:
// for a Route, a policy that admits them (the project template's
// allow-from-openshift-ingress) admits nothing routerPeer does not.
func legacyRouterPeer() networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
		MatchLabels: map[string]string{openShiftLegacyIngressLabel: openShiftLegacyIngressValue},
	}}
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

// trustsAllPeers: pod believes X-Forwarded-For from any peer its
// NetworkPolicy admits, on the operator's grant (the template annotation;
// trustedProxy.cidrs of 0.0.0.0/0,::/0 is the user's own choice and needs
// no policy kept for it).
func trustsAllPeers(pod *corev1.Pod) bool {
	return pod.Annotations[annotationTrustsEveryPeer] == "true"
}

// proxyTrust is what the console believes of X-Forwarded-For.
type proxyTrust struct {
	Hops  int32
	CIDRs []string
	// EveryPeer: CIDRs is anyPeer on the operator's grant, because the
	// NetworkPolicy admits only the proxies (not trustedProxy.cidrs).
	EveryPeer bool
}

// effectiveProxyTrust follows the exposure that is published, not the
// one requested: no Route without the Route API, nothing at all while the
// hostname is refused. Hops default to one in front of a published
// exposure. The trusted peers are the spec's CIDRs; without them, every
// peer when the NetworkPolicy admits only the proxies (no peer of it is
// broad, by the spec or by the live Pods, and no other NetworkPolicy
// admits more: found is the zero confinement),
// and none otherwise (the console then ignores the header, whatever the
// hops).
func effectiveProxyTrust(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, found confinement) proxyTrust {
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
	if peers := exposurePolicyPeers(c, exposed); peers != nil && broadPeer(peers) == "" && found == (confinement{}) {
		trust.CIDRs = append(trust.CIDRs, anyPeer...)
		trust.EveryPeer = true
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
// is published (the condition is then removed). found is what the
// reconciler saw of the live Pods and the other NetworkPolicies.
func exposureConfinedFact(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, found confinement) *conditionFact {
	if !exposed {
		return nil
	}
	trust := effectiveProxyTrust(c, exposed, found)
	believed := "X-Forwarded-For is ignored (trustedProxy.hops is 0)"
	switch {
	case trust.Hops > 0 && len(trust.CIDRs) == 0:
		believed = "X-Forwarded-For is ignored: no trusted peer (set trustedProxy.cidrs, or networkPolicy.from); every client shares the proxy's address, so no per-address sign-in budget applies (only the per-flow, per-token and per-replica bounds)"
	case trust.Hops > 0:
		believed = "X-Forwarded-For is read " + strconv.Itoa(int(trust.Hops)) + " hop(s) deep from peers in " + strings.Join(trust.CIDRs, ",")
	}
	peers := exposurePolicyPeers(c, exposed)
	if peers != nil {
		broad := broadPeer(peers)
		if broad == "" {
			broad = found.Broad
		}
		if broad != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposurePeersBroad,
				Message: "NetworkPolicy " + exposurePolicyName(c) + ": networkPolicy.from " + broad + "; " + believed,
			}
		}
		if found.Bypass != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposurePolicyBypassed,
				Message: found.Bypass + " (NetworkPolicies add up): NetworkPolicy " + exposurePolicyName(c) + " does not confine it; " + believed,
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
func setExposureConfined(console *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, found confinement) {
	fact := exposureConfinedFact(console, exposed, found)
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
