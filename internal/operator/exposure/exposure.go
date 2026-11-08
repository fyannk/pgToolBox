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

// Package exposure builds the objects that publish a workload's Service
// (an OpenShift Route, an Ingress, a Gateway API HTTPRoute) from a CR's
// ExposureSpec, and reads each one's status into the ExposureReady fact.
// PgAdmin and PgToolBoxConsole share it; what differs between them (the port, TLS
// re-encryption, a long-lived stream) is in Target.
package exposure

import (
	"fmt"
	"strconv"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/specs"
)

// Target is the Service an exposure object points at.
type Target struct {
	// Meta is the exposure object's metadata (name, namespace, labels,
	// annotations, owner); it is copied, never modified.
	Meta metav1.ObjectMeta
	// Service is the Service's name.
	Service string
	// PortName is the Service port a Route or an Ingress targets.
	PortName string
	// Port is the Service port number an HTTPRoute targets.
	Port int32
	// Reencrypt makes a Route re-encrypt to a backend serving the service
	// CA's certificate, instead of terminating at the edge.
	Reencrypt bool
	// StreamTimeout, when set, keeps a long-lived response (SSE) open at
	// the edge: the Route's router timeout, the HTTPRoute's request timeout,
	// ingress-nginx's read and send timeouts with buffering off. Explicit
	// annotations in Meta win.
	StreamTimeout time.Duration
	// StickyCookie, when set, keeps a person on one replica of the Service
	// with a cookie of that name: ingress-nginx's cookie affinity on an
	// Ingress, the router's cookie on a Route. A Gateway API HTTPRoute has
	// no standard field for it. Explicit annotations in Meta win.
	StickyCookie string
}

// meta is the object's metadata with the builder's default annotations
// added under the ones Meta already sets.
func (t Target) meta(annotations map[string]string) metav1.ObjectMeta {
	out := *t.Meta.DeepCopy()
	if len(annotations) == 0 {
		return out
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	for key, value := range annotations {
		if _, set := out.Annotations[key]; !set {
			out.Annotations[key] = value
		}
	}
	return out
}

func seconds(d time.Duration) string { return strconv.FormatInt(int64(d/time.Second), 10) }

// Route builds the OpenShift Route of spec for t.
func Route(spec *pgtoolboxv1alpha1.ExposureSpec, t Target) *routev1.Route {
	termination := routev1.TLSTerminationEdge
	if t.Reencrypt {
		// The backend serves TLS with the service CA's certificate, which
		// the router trusts for re-encryption without a destination CA.
		termination = routev1.TLSTerminationReencrypt
	}
	annotations := map[string]string{}
	if t.StreamTimeout > 0 {
		annotations["haproxy.router.openshift.io/timeout"] = seconds(t.StreamTimeout) + "s"
	}
	if t.StickyCookie != "" {
		// The router is sticky by default; this only names its cookie.
		annotations["router.openshift.io/cookie_name"] = t.StickyCookie
	}
	route := &routev1.Route{
		TypeMeta:   metav1.TypeMeta{APIVersion: "route.openshift.io/v1", Kind: "Route"},
		ObjectMeta: t.meta(annotations),
		Spec: routev1.RouteSpec{
			Host: spec.Hostname,
			To:   routev1.RouteTargetReference{Kind: "Service", Name: t.Service, Weight: new(int32(100))},
			Port: &routev1.RoutePort{TargetPort: intstr.FromString(t.PortName)},
			TLS: &routev1.TLSConfig{
				Termination:                   termination,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
	specs.SetHash(route, specs.Hash(route.Spec))
	return route
}

// Ingress builds the Ingress of spec for t.
func Ingress(spec *pgtoolboxv1alpha1.ExposureSpec, t Target) *networkingv1.Ingress {
	pathType := networkingv1.PathTypePrefix
	// ingress-nginx honours these; other controllers ignore them, so their
	// own go into spec.exposure.annotations.
	annotations := map[string]string{}
	if t.StreamTimeout > 0 {
		annotations["nginx.ingress.kubernetes.io/proxy-read-timeout"] = seconds(t.StreamTimeout)
		annotations["nginx.ingress.kubernetes.io/proxy-send-timeout"] = seconds(t.StreamTimeout)
		annotations["nginx.ingress.kubernetes.io/proxy-buffering"] = "off"
	}
	if t.StickyCookie != "" {
		// persistent: a person stays on their replica even when the
		// controller's endpoint list changes, until that replica goes.
		annotations["nginx.ingress.kubernetes.io/affinity"] = "cookie"
		annotations["nginx.ingress.kubernetes.io/affinity-mode"] = "persistent"
		annotations["nginx.ingress.kubernetes.io/session-cookie-name"] = t.StickyCookie
		annotations["nginx.ingress.kubernetes.io/session-cookie-path"] = "/"
		annotations["nginx.ingress.kubernetes.io/session-cookie-samesite"] = "Lax"
		if spec.TLS != nil {
			annotations["nginx.ingress.kubernetes.io/session-cookie-secure"] = "true"
		}
	}
	ingress := &networkingv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: t.meta(annotations),
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				Host: spec.Hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: t.Service, Port: networkingv1.ServiceBackendPort{Name: t.PortName},
						}},
					}},
				}},
			}},
		},
	}
	if spec.IngressClassName != "" {
		ingress.Spec.IngressClassName = new(spec.IngressClassName)
	}
	if spec.TLS != nil {
		tls := networkingv1.IngressTLS{Hosts: []string{spec.Hostname}}
		if spec.TLS.CertificateSecretRef != nil {
			tls.SecretName = spec.TLS.CertificateSecretRef.Name
		}
		ingress.Spec.TLS = []networkingv1.IngressTLS{tls}
	}
	specs.SetHash(ingress, specs.Hash(ingress.Spec))
	return ingress
}

// HTTPRoute builds the Gateway API HTTPRoute of spec for t; spec.Gateway
// must be set (the CRD requires it for type gateway).
func HTTPRoute(spec *pgtoolboxv1alpha1.ExposureSpec, t Target) *gatewayv1.HTTPRoute {
	parent := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(spec.Gateway.ParentRef.Name)}
	if ns := spec.Gateway.ParentRef.Namespace; ns != "" {
		parent.Namespace = new(gatewayv1.Namespace(ns))
	}
	if section := spec.Gateway.ParentRef.SectionName; section != "" {
		parent.SectionName = new(gatewayv1.SectionName(section))
	}
	port := gatewayv1.PortNumber(t.Port)
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(t.Service), Port: &port,
			},
		}}},
	}
	if t.StreamTimeout > 0 {
		timeout := gatewayv1.Duration(seconds(t.StreamTimeout) + "s")
		rule.Timeouts = &gatewayv1.HTTPRouteTimeouts{Request: &timeout}
	}
	route := &gatewayv1.HTTPRoute{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
		ObjectMeta: t.meta(nil),
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(spec.Hostname)},
			Rules:           []gatewayv1.HTTPRouteRule{rule},
		},
	}
	specs.SetHash(route, specs.Hash(route.Spec))
	return route
}

// URL is where an exposure of spec is reached from outside the cluster, ""
// for clusterIP.
func URL(spec *pgtoolboxv1alpha1.ExposureSpec) string {
	switch spec.TypeOrDefault() {
	case pgtoolboxv1alpha1.ExposureTypeRoute, pgtoolboxv1alpha1.ExposureTypeGateway:
		return "https://" + spec.Hostname + "/"
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		if spec.TLS != nil {
			return "https://" + spec.Hostname + "/"
		}
		return "http://" + spec.Hostname + "/"
	case pgtoolboxv1alpha1.ExposureTypeClusterIP:
	}
	return ""
}

// Fact is the ExposureReady condition read from an exposure object's
// status.
type Fact struct {
	Status  metav1.ConditionStatus
	Reason  pgtoolboxv1alpha1.Reason
	Message string
}

// RouteFact reads a Route's admission.
func RouteFact(route *routev1.Route) Fact {
	for _, ingress := range route.Status.Ingress {
		for _, condition := range ingress.Conditions {
			if condition.Type != routev1.RouteAdmitted {
				continue
			}
			if condition.Status == "True" {
				return Fact{metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "admitted by router " + ingress.RouterName}
			}
			return Fact{
				metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonNotAdmitted,
				fmt.Sprintf("router %s: %s: %s", ingress.RouterName, condition.Reason, condition.Message),
			}
		}
	}
	return Fact{metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling, "no router has reported on the Route yet"}
}

// IngressFact reads an Ingress's published address.
func IngressFact(ingress *networkingv1.Ingress) Fact {
	if len(ingress.Status.LoadBalancer.Ingress) > 0 {
		address := ingress.Status.LoadBalancer.Ingress[0]
		at := address.Hostname
		if at == "" {
			at = address.IP
		}
		return Fact{metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "served at " + at}
	}
	return Fact{metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling, "no ingress controller has published an address yet"}
}

// HTTPRouteFact reads an HTTPRoute's acceptance by its Gateway.
func HTTPRouteFact(route *gatewayv1.HTTPRoute) Fact {
	for _, parent := range route.Status.Parents {
		accepted := meta.FindStatusCondition(parent.Conditions, string(gatewayv1.RouteConditionAccepted))
		if accepted == nil {
			continue
		}
		if accepted.Status == metav1.ConditionTrue {
			resolved := meta.FindStatusCondition(parent.Conditions, string(gatewayv1.RouteConditionResolvedRefs))
			if resolved != nil && resolved.Status == metav1.ConditionFalse {
				return Fact{metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonNotAccepted, resolved.Reason + ": " + resolved.Message}
			}
			return Fact{metav1.ConditionTrue, pgtoolboxv1alpha1.ReasonAsExpected, "accepted by Gateway " + string(parent.ParentRef.Name)}
		}
		return Fact{
			metav1.ConditionFalse, pgtoolboxv1alpha1.ReasonNotAccepted,
			fmt.Sprintf("Gateway %s: %s: %s", parent.ParentRef.Name, accepted.Reason, accepted.Message),
		}
	}
	return Fact{metav1.ConditionUnknown, pgtoolboxv1alpha1.ReasonReconciling, "no Gateway has reported on the HTTPRoute yet"}
}
