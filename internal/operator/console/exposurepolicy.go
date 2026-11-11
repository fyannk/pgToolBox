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
	"crypto/sha256"
	"encoding/hex"
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

// annotationTrustedBlocks sits beside annotationTrustsEveryPeer when the
// policy has ipBlock peers: the hash of the blocks (trustedBlocksHash) a
// console Pod's address showed not to be the Pod network when the grant
// was made. While no console Pod has an address (the only one is
// replaced, the workload scales from zero), a template that carries the
// grant for the same blocks keeps it: the Pod network does not change, so
// the earlier check still holds, and dropping the grant would roll the
// Pod that is coming up, which a single-replica StatefulSet would do
// forever. With no such template (creation), the blocks are not trusted.
const annotationTrustedBlocks = pgtoolboxv1alpha1.ReservedKeyPrefix + "trusted-ipblocks"

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
	// Unverified says why the peers could not be checked, or are not in
	// force yet, "" when they were: no console Pod has an address while an
	// ipBlock peer needs one, more console Pods than could be listed, or a
	// widened networkPolicy.from held back until no Pod trusts every peer.
	Unverified string
	// Note says what was not checked without withholding the trust (the
	// Nodes' podCIDRs, when the operator may not list them), "" when
	// nothing: it goes into the condition message only.
	Note string
}

// clear: nothing found keeps the policy from confining the console port
// (a Note is not a finding).
func (f confinement) clear() bool {
	return f.Broad == "" && f.Bypass == "" && f.Unverified == ""
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
// wider than a /64. An ipBlock holding a console Pod's own address is
// found by the reconciler (podNetworkBlock).
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

// podNetworkBlock names the first ipBlock peer that admits the address of
// a console Pod (its except entries aside), "" when none: such a block
// admits the Pod network, every Pod of the cluster, not the proxies. With
// an ipBlock peer and no console Pod address to check it against (the
// first Pod is Pending, the workload scales from zero), unverified says
// so: until a Pod has an address the block may be the Pod network, so no
// peer is trusted (fail closed; the Pod's address arrives on a watched
// Pod update).
func podNetworkBlock(peers []networkingv1.NetworkPolicyPeer, pods []corev1.Pod) (broad, unverified string) {
	var addrs []netip.Addr
	var names []string
	for p := range pods {
		for _, ip := range pods[p].Status.PodIPs {
			if addr, err := netip.ParseAddr(ip.IP); err == nil {
				addrs, names = append(addrs, addr), append(names, pods[p].Name)
			}
		}
	}
	for i, peer := range peers {
		if peer.IPBlock == nil {
			continue
		}
		block, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil {
			continue // broadPeer reports it
		}
		if len(addrs) == 0 {
			if unverified == "" {
				unverified = fmt.Sprintf(noPodAddress+", so ipBlock %s (peer %d) cannot be told apart from the Pod network: no peer is trusted until one has", peer.IPBlock.CIDR, i)
			}
			continue
		}
		for a, addr := range addrs {
			if block.Contains(addr) && !excepted(peer.IPBlock.Except, addr) {
				return fmt.Sprintf("peer %d has ipBlock %s, which holds the address of console Pod %s: it admits the Pod network", i, peer.IPBlock.CIDR, names[a]), ""
			}
		}
	}
	return "", unverified
}

// noPodAddress is podNetworkBlock's unverified when no console Pod has an
// address: the one case an earlier check may stand in for
// (trustedBlocksHash).
const noPodAddress = "no console Pod has an address yet"

// nodePodCIDRs are a Node's spec.podCIDRs.
type nodePodCIDRs struct {
	Name  string
	CIDRs []netip.Prefix
}

// nodeNetworkBlock names the first ipBlock peer that overlaps a Node's
// podCIDR, "" when none: such a block admits the Pods of that node,
// wherever the console Pods run (podNetworkBlock sees only their own
// addresses). An except entry that holds the whole podCIDR excepts it.
// CNIs that do not allocate from spec.podCIDRs (Calico, Cilium's cluster
// pool) leave it empty: the console Pods' own addresses are then the only
// check.
func nodeNetworkBlock(peers []networkingv1.NetworkPolicyPeer, nodes []nodePodCIDRs) string {
	for i, peer := range peers {
		if peer.IPBlock == nil {
			continue
		}
		block, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil {
			continue // broadPeer reports it
		}
		block = block.Masked()
		for _, node := range nodes {
			for _, cidr := range node.CIDRs {
				if block.Overlaps(cidr) && !exceptedPrefix(peer.IPBlock.Except, cidr) {
					return fmt.Sprintf("peer %d has ipBlock %s, which overlaps the Pod range %s of Node %s: it admits the Pods of that node", i, peer.IPBlock.CIDR, cidr, node.Name)
				}
			}
		}
	}
	return ""
}

// nodeCoherenceNote says that the Node check is not meaningful, "" when
// nothing shows that: a console Pod's address lies outside every podCIDR
// of its own Node of that address family. A CNI that allocates Pod
// addresses itself (Calico IPAM, Cilium's cluster pool) while
// kube-controller-manager still fills spec.podCIDRs leaves ranges there
// that are not the Pod network, so a block that misses them proves
// nothing; the console Pods' own addresses are then the only check. A Pod
// whose Node has no podCIDRs, or none of that family, shows nothing (the
// coverage note counts those Nodes). The Pods are taken by name, so the
// note names the same Pod from one pass to the next. It is a note, not a
// finding: the trust is not withheld for it (a Node range that overlaps a
// block still makes it broad: false alarms are the safe side).
func nodeCoherenceNote(pods []corev1.Pod, nodes []nodePodCIDRs) string {
	ranges := make(map[string][]netip.Prefix, len(nodes))
	for _, node := range nodes {
		ranges[node.Name] = node.CIDRs
	}
	sorted := slices.Clone(pods)
	slices.SortFunc(sorted, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for p := range sorted {
		pod := &sorted[p]
		cidrs, ok := ranges[pod.Spec.NodeName]
		if !ok || pod.Spec.HostNetwork {
			continue
		}
		for _, ip := range pod.Status.PodIPs {
			addr, err := netip.ParseAddr(ip.IP)
			if err != nil {
				continue
			}
			sameFamily := false
			inside := slices.ContainsFunc(cidrs, func(cidr netip.Prefix) bool {
				if cidr.Addr().Is4() != addr.Is4() {
					return false
				}
				sameFamily = true
				return cidr.Contains(addr)
			})
			if sameFamily && !inside {
				return fmt.Sprintf("the Node check is not meaningful: console Pod %s has address %s, outside the podCIDRs of its Node %s (the CNI does not allocate from spec.podCIDRs), so only the console Pods' own addresses were checked against the ipBlock peers", pod.Name, addr, pod.Spec.NodeName)
			}
		}
	}
	return ""
}

// joinNotes joins the notes that are set.
func joinNotes(notes ...string) string {
	return strings.Join(slices.DeleteFunc(notes, func(n string) bool { return n == "" }), "; ")
}

// exceptedPrefix: one of except holds the whole of cidr.
func exceptedPrefix(except []string, cidr netip.Prefix) bool {
	return slices.ContainsFunc(except, func(entry string) bool {
		prefix, err := netip.ParsePrefix(entry)
		return err == nil && prefix.Bits() <= cidr.Bits() && prefix.Masked().Contains(cidr.Addr())
	})
}

// trustedBlocksHash identifies the ipBlock peers of peers (CIDR and except
// entries, in any order), "" when there are none.
func trustedBlocksHash(peers []networkingv1.NetworkPolicyPeer) string {
	var blocks []string
	for _, peer := range peers {
		if peer.IPBlock == nil {
			continue
		}
		except := slices.Clone(peer.IPBlock.Except)
		slices.Sort(except)
		blocks = append(blocks, peer.IPBlock.CIDR+" except "+strings.Join(except, ","))
	}
	if len(blocks) == 0 {
		return ""
	}
	slices.Sort(blocks)
	sum := sha256.Sum256([]byte(strings.Join(blocks, "\n")))
	return hex.EncodeToString(sum[:16])
}

// peersWiden: the policy with peers would admit a source that the one
// with live does not (a peer live does not cover: coveredBy).
func peersWiden(peers, live []networkingv1.NetworkPolicyPeer) bool {
	return slices.ContainsFunc(peers, func(peer networkingv1.NetworkPolicyPeer) bool { return !coveredBy(peer, live) })
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
	if peers := exposurePolicyPeers(c, exposed); peers != nil && broadPeer(peers) == "" && found.clear() {
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
	fact := exposureConfinedFindings(c, exposed, found)
	if fact != nil && found.Note != "" {
		fact.Message += "; " + found.Note
	}
	return fact
}

// exposureConfinedFindings is exposureConfinedFact without the note.
func exposureConfinedFindings(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool, found confinement) *conditionFact {
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
		if found.Unverified != "" {
			return &conditionFact{
				Status: metav1.ConditionFalse, Reason: pgtoolboxv1alpha1.ReasonExposureTrustUnverified,
				Message: "NetworkPolicy " + exposurePolicyName(c) + ": " + found.Unverified + "; " + believed,
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
