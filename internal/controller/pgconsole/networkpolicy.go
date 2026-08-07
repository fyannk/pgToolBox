/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package pgconsole

import (
	"context"
	"net/url"
	"strconv"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/adminsync"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	dnsPort           int32 = 53
	openshiftDNSPort  int32 = 5353
	httpsPort         int32 = 443
	kubeAPIServerPort int32 = 6443
)

// reconcileNetworkPolicy converges the generated NetworkPolicy: it deletes
// the policy when the feature is disabled and otherwise writes only when
// the existing object no longer matches the desired one, so steady-state
// reconciles issue no API writes.
func (r *Reconciler) reconcileNetworkPolicy(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	if !networkPolicyEnabled(console) {
		return r.deleteNetworkPolicyIfOwned(ctx, console)
	}

	desired, err := r.networkPolicy(console)
	if err != nil {
		return err
	}
	var existing networkingv1.NetworkPolicy
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		// Created rather than applied: the controller-runtime fake client
		// cannot server-side-apply NetworkPolicy, and a create/update path
		// keeps the object testable without changing production semantics.
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if networkPolicyMatches(&existing, desired) {
		return nil
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

// networkPolicy builds the default-deny policy confining the console Pod:
// ingress is restricted to the proxy port and egress to DNS, whatever the
// authentication mode needs, and any user-supplied extra egress. The build
// is deterministic and every server-side default is materialized up front,
// so the object compares equal to what the API server stores and no update
// loop can start.
func (r *Reconciler) networkPolicy(console *pgtoolboxv1alpha1.PgConsole) (*networkingv1.NetworkPolicy, error) {
	spec := &console.Spec.NetworkPolicy
	policy := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{
			APIVersion: networkingv1.SchemeGroupVersion.String(),
			Kind:       "NetworkPolicy",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      application.ResourceName(console.Name, ""),
			Namespace: console.Namespace,
			Labels:    application.CommonLabels(console.Name),
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: application.SelectorLabels(console.Name)},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			// Only the proxy port admits traffic; the console's own port
			// never leaves the Pod. There are no endpoint ports — the
			// console never connects to PostgreSQL.
			Ingress: []networkingv1.NetworkPolicyIngressRule{{Ports: r.ingressPorts(console)}},

			Egress: []networkingv1.NetworkPolicyEgressRule{dnsEgressRule()},
		},
	}
	if spec.PolicyTypes == pgtoolboxv1alpha1.NetworkPolicyTypesIngress {
		// Ingress-only mode: egress stays unconstrained, for CNIs or
		// platform policies that do not support egress enforcement.
		policy.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		policy.Spec.Egress = nil
		if err := controllerutil.SetControllerReference(console, policy, r.Scheme); err != nil {
			return nil, err
		}
		return policy, nil
	}

	policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{
		Ports: authenticationEgressPorts(console),
	})

	for i := range spec.ExtraEgress {
		rule := spec.ExtraEgress[i].DeepCopy()
		// NetworkPolicy admission defaults an omitted protocol to TCP.
		// Materialize that semantic default so no-op comparisons remain
		// stable after the generated object has passed through the API server.
		for j := range rule.Ports {
			if rule.Ports[j].Protocol == nil {
				protocol := corev1.ProtocolTCP
				rule.Ports[j].Protocol = &protocol
			}
		}
		policy.Spec.Egress = append(policy.Spec.Egress, *rule)
	}

	if err := controllerutil.SetControllerReference(console, policy, r.Scheme); err != nil {
		return nil, err
	}
	return policy, nil
}

// ingressPorts admits the proxy port and, when the admin-sync sidecar is
// injected, its in-pod API port.
func (r *Reconciler) ingressPorts(console *pgtoolboxv1alpha1.PgConsole) []networkingv1.NetworkPolicyPort {
	ports := []networkingv1.NetworkPolicyPort{tcpPort(proxyPort)}
	if pgAdminEnabled(console) && r.OperatorImage != "" {
		ports = append(ports, tcpPort(adminsync.SidecarPort))
	}
	return ports
}

// networkPolicyEnabled reports whether a NetworkPolicy should exist for this
// instance; generation defaults to on when the field is left unset.
func networkPolicyEnabled(console *pgtoolboxv1alpha1.PgConsole) bool {
	spec := &console.Spec.NetworkPolicy
	return spec.Enabled == nil || *spec.Enabled
}

// authenticationEgressPorts derives what the pod must reach. Every mode
// needs the API server (6443) — the console reads its cluster through it
// and the proxy creates access requests through it. Egress policy is
// evaluated against the post-DNAT destination, so the kubernetes.default
// VIP resolves to the endpoint port 6443. The oidc mode additionally needs
// its identity provider, on whatever port the issuer URL names (443 when it
// names none); the local mode authenticates against the rendered
// configuration and needs nothing more.
func authenticationEgressPorts(console *pgtoolboxv1alpha1.PgConsole) []networkingv1.NetworkPolicyPort {
	ports := []networkingv1.NetworkPolicyPort{tcpPort(kubeAPIServerPort)}
	auth := console.Spec.Proxy.Authentication
	if auth.Mode == pgtoolboxv1alpha1.ProxyAuthenticationModeOIDC && auth.OIDC != nil {
		if identityPort := issuerPort(auth.OIDC.IssuerURL); identityPort != kubeAPIServerPort {
			ports = append(ports, tcpPort(identityPort))
		}
	}
	return ports
}

// issuerPort extracts the explicit port of an issuer URL, defaulting to 443
// — the CRD schema pins the scheme to https. An unparseable URL also falls
// back to 443 rather than failing the reconcile: admission has already
// vetted the field, and the honest default keeps the policy deny-by-default
// instead of absent.
func issuerPort(issuerURL string) int32 {
	parsed, err := url.Parse(issuerURL)
	if err != nil {
		return httpsPort
	}
	port := parsed.Port()
	if port == "" {
		return httpsPort
	}
	parsedPort, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		return httpsPort
	}
	return int32(parsedPort)
}

// dnsEgressRule admits cluster DNS on both 53 and 5353: egress policy is
// evaluated against the post-DNAT destination, and OpenShift's CoreDNS
// endpoints answer on 5353 behind the :53 Service VIP.
func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(dnsPort)
	openshiftPort := intstr.FromInt32(openshiftDNSPort)
	return networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &port},
			{Protocol: &tcp, Port: &port},
			{Protocol: &udp, Port: &openshiftPort},
			{Protocol: &tcp, Port: &openshiftPort},
		},
	}
}

// tcpPort builds a numeric TCP port entry with the protocol set explicitly,
// matching API-server defaulting so comparisons stay stable.
func tcpPort(portNumber int32) networkingv1.NetworkPolicyPort {
	protocol := corev1.ProtocolTCP
	port := intstr.FromInt32(portNumber)
	return networkingv1.NetworkPolicyPort{Protocol: &protocol, Port: &port}
}

// networkPolicyMatches reports whether the existing policy already satisfies
// the desired one (labels, controller owner, and semantically equal spec), so
// reconciliation can skip the write and no update loop forms against
// server-side defaulting.
func networkPolicyMatches(existing, desired *networkingv1.NetworkPolicy) bool {
	return shared.LabelsContain(existing.Labels, desired.Labels) &&
		shared.ControllerOwnerMatches(existing, desired) &&
		apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec)
}

// deleteNetworkPolicyIfOwned removes the generated policy when the feature
// is turned off, but only if this instance controls it, so a user-managed
// policy under the generated name is left alone. Absence is not an error.
func (r *Reconciler) deleteNetworkPolicyIfOwned(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	var existing networkingv1.NetworkPolicy
	key := client.ObjectKey{
		Namespace: console.Namespace,
		Name:      application.ResourceName(console.Name, ""),
	}
	if err := r.Get(ctx, key, &existing); err != nil {
		return client.IgnoreNotFound(err)
	}
	owner := metav1.GetControllerOf(&existing)
	if owner == nil || owner.UID != console.UID {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &existing))
}
