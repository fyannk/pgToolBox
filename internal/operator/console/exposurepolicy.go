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
	"k8s.io/apimachinery/pkg/util/intstr"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
)

// openShiftIngressPolicyGroup labels the namespaces of the OpenShift
// routers, host-network ones included (OVN-Kubernetes matches host-network
// traffic from the routers to this label).
const openShiftIngressPolicyGroup = "policy-group.network.openshift.io/ingress"

// The narrowest ipBlock that still counts as broad is one bit wider than
// these: a /7 (IPv4) or a /31 (IPv6). With at most 16 peers of at most a
// /8 each, the blocks together never cover every address either.
const (
	minIPv4BlockBits = 8
	minIPv6BlockBits = 32
)

// privateRanges are the IPv4 ranges a Pod network is cut from: an ipBlock
// that holds a whole one admits the Pods, not only the proxies.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// uniqueLocal is where IPv6 Pod networks are cut from (fd00:10:244::/56 on
// kind, a /48 or /56 elsewhere). It is wider than any ipBlock may be
// (minIPv6BlockBits), so the IPv6 rule is another one: a block inside it
// wider than a /64 (a single subnet, the narrowest a Pod network is) is
// broad.
var uniqueLocal = netip.MustParsePrefix("fc00::/7")

const minUniqueLocalBlockBits = 64

// sharedNamespaceKeys are namespace label keys whose values many
// namespaces share, set by the platform or by any namespace owner (Pod
// Security admission levels, OpenShift's SCC label sync and run level): a
// namespaceSelector that pins only these selects every namespace of a
// level, not the proxies'. Keys ending in "/" are prefixes. Other labels a
// namespace owner may set on their own namespace (a tenant label of
// Capsule or Rancher, say) cannot be told apart from an installer's: a
// peer that pins one is the user's to keep narrow.
var sharedNamespaceKeys = []string{
	"pod-security.kubernetes.io/",
	"security.openshift.io/",
	"openshift.io/run-level",
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
// is the default; a namespaceSelector is broad too when it pins only
// sharedNamespaceKeys (pod-security.kubernetes.io/enforce=restricted is on
// most namespaces). Broad are then a namespaceSelector that is (Pods of
// nearly every namespace, whatever the podSelector: any namespace owner
// can label a Pod), a podSelector that is, alone (nearly every Pod of the
// console's namespace), and an ipBlock wider than /8 (IPv4) or /32 (IPv6),
// holding a whole private IPv4 range (10/8, 172.16/12, 192.168/16,
// 100.64/10: where Pod networks are cut from), or inside fc00::/7 and
// wider than a /64. The check reads the spec only: an ipBlock that is
// the Pod network, or another NetworkPolicy that admits more (policies add
// up), is not seen.
func broadPeer(peers []networkingv1.NetworkPolicyPeer) string {
	for i, peer := range peers {
		switch {
		case peer.NamespaceSelector != nil && !pinsValue(peer.NamespaceSelector):
			return fmt.Sprintf("peer %d has a namespaceSelector that pins no label value: it admits Pods of (nearly) every namespace", i)
		case peer.NamespaceSelector != nil && !pinsValueOf(peer.NamespaceSelector, distinctNamespaceKey):
			return fmt.Sprintf("peer %d has a namespaceSelector that pins only labels many namespaces share (%s): it admits Pods of every namespace that carries them", i, strings.Join(sharedNamespaceKeys, ", "))
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
	return pinsValueOf(s, func(string) bool { return true })
}

// pinsValueOf: the selector pins the value of a label whose key counts.
func pinsValueOf(s *metav1.LabelSelector, counts func(key string) bool) bool {
	for key := range s.MatchLabels {
		if counts(key) {
			return true
		}
	}
	return slices.ContainsFunc(s.MatchExpressions, func(e metav1.LabelSelectorRequirement) bool {
		return e.Operator == metav1.LabelSelectorOpIn && counts(e.Key)
	})
}

// distinctNamespaceKey: key is not one of sharedNamespaceKeys.
func distinctNamespaceKey(key string) bool {
	return !slices.ContainsFunc(sharedNamespaceKeys, func(shared string) bool {
		if strings.HasSuffix(shared, "/") {
			return strings.HasPrefix(key, shared)
		}
		return key == shared
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
	if prefix.Addr().Is6() {
		if uniqueLocal.Contains(prefix.Addr()) && prefix.Bits() < minUniqueLocalBlockBits {
			return fmt.Sprintf("it is a unique-local block (%s) wider than /%d, the size IPv6 Pod networks are cut to", uniqueLocal, minUniqueLocalBlockBits)
		}
		return ""
	}
	for _, private := range privateRanges {
		if prefix.Bits() <= private.Bits() && prefix.Contains(private.Addr()) {
			return "it holds the whole private range " + private.String() + ", where Pod networks are cut from"
		}
	}
	return ""
}

// proxyTrust is what the console believes of X-Forwarded-For.
type proxyTrust struct {
	Hops  int32
	CIDRs []string
}

// effectiveProxyTrust follows the exposure that is published, not the
// one requested: no Route without the Route API, nothing at all while the
// hostname is refused. Hops default to one in front of a published
// exposure. The trusted peers are trustedProxy.cidrs only: without them
// the console ignores the header, whatever the hops, and takes the TCP
// peer (the proxy's address) as the client's.
func effectiveProxyTrust(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) proxyTrust {
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
	if spec != nil {
		for _, cidr := range spec.CIDRs {
			// CEL refused what is not a CIDR; an entry that still does
			// not parse is never trusted.
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				trust.CIDRs = append(trust.CIDRs, prefix.Masked().String())
			}
		}
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
// is published (the condition is then removed).
func exposureConfinedFact(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) *conditionFact {
	if !exposed {
		return nil
	}
	trust := effectiveProxyTrust(c, exposed)
	believed := "X-Forwarded-For is ignored (trustedProxy.hops is 0)"
	switch {
	case trust.Hops > 0 && len(trust.CIDRs) == 0:
		believed = "X-Forwarded-For is ignored: trustedProxy.cidrs is not set, so every client shares the proxy's address (sign-in budgets and the audit log see the proxy); set it to the ingress controller's addresses"
	case trust.Hops > 0:
		believed = "X-Forwarded-For is read " + strconv.Itoa(int(trust.Hops)) + " hop(s) deep from peers in " + strings.Join(trust.CIDRs, ",")
	}
	if peers := exposurePolicyPeers(c, exposed); peers != nil {
		if broad := broadPeer(peers); broad != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposurePeersBroad,
				Message: "NetworkPolicy " + exposurePolicyName(c) + ": networkPolicy.from " + broad + "; " + believed,
			}
		}
		return &conditionFact{
			Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected,
			Message: "NetworkPolicy " + exposurePolicyName(c) + " admits only the exposure's peers to the console port (the CNI must enforce it, and other NetworkPolicies of the namespace add up); " + believed,
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
func setExposureConfined(console *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) {
	fact := exposureConfinedFact(console, exposed)
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
