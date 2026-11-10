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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
// differs per installation, so no default is safe).
func exposurePolicyPeers(c *pgtoolboxv1alpha1.PgToolBoxConsole, exposed bool) []networkingv1.NetworkPolicyPeer {
	if !exposed {
		return nil
	}
	spec := c.Spec.Exposure.NetworkPolicy
	if spec != nil && spec.Enabled != nil && !*spec.Enabled {
		return nil
	}
	if spec != nil && len(spec.From) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, len(spec.From))
		for i := range spec.From {
			spec.From[i].DeepCopyInto(&peers[i])
		}
		return peers
	}
	if c.Spec.Exposure.TypeOrDefault() == pgtoolboxv1alpha1.ExposureTypeRoute {
		return []networkingv1.NetworkPolicyPeer{{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{openShiftIngressPolicyGroup: ""},
		}}}
	}
	return nil
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
// peer when the NetworkPolicy admits only the proxies, and none otherwise
// (the console then ignores the header, whatever the hops).
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
	switch {
	case spec != nil && len(spec.CIDRs) > 0:
		for _, cidr := range spec.CIDRs {
			trust.CIDRs = append(trust.CIDRs, maskedCIDR(cidr))
		}
	case exposurePolicyPeers(c, exposed) != nil:
		trust.CIDRs = append(trust.CIDRs, anyPeer...)
	}
	return trust
}

// maskedCIDR is cidr in canonical form, host bits cleared, the way the
// console reads it; a single address is its /32 or /128. CEL has already
// refused what is not a CIDR.
func maskedCIDR(cidr string) string {
	if prefix, err := netip.ParsePrefix(cidr); err == nil {
		return prefix.Masked().String()
	}
	return canonicalCIDR(cidr)
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
		believed = "X-Forwarded-For is ignored: no trusted peer (set trustedProxy.cidrs, or networkPolicy.from); every client shares the proxy's address and sign-in budget"
	case trust.Hops > 0:
		believed = "X-Forwarded-For is read " + strconv.Itoa(int(trust.Hops)) + " hop(s) deep from peers in " + strings.Join(trust.CIDRs, ",")
	}
	if exposurePolicyPeers(c, exposed) != nil {
		return &conditionFact{
			Status: metav1.ConditionTrue, Reason: pgtoolboxv1alpha1.ReasonAsExpected,
			Message: "NetworkPolicy " + exposurePolicyName(c) + " admits only the exposure's peers to the console port (the CNI must enforce it); " + believed,
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
