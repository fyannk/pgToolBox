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
	"fmt"
	"reflect"
	"strings"

	pgtoolboxv1alpha1 "github.com/fyannk/pgtoolbox/api/v1alpha1"
	"github.com/fyannk/pgtoolbox/internal/conditions"
	"github.com/fyannk/pgtoolbox/internal/controller/shared"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// validateExposure rejects exposure settings that cannot be honored: with
// Gateway exposure, TLS termination and certificates belong to the
// referenced Gateway, so any TLS settings on the spec are an error. A nil
// return means the exposure configuration is reconcilable.
func validateExposure(console *pgtoolboxv1alpha1.PgConsole) error {
	exposure := &console.Spec.Exposure
	if exposure.Type != pgtoolboxv1alpha1.ExposureTypeGateway || exposure.TLS == nil {
		return nil
	}
	if exposure.TLS.Termination != "" || exposure.TLS.CertificateSecretRef != nil {
		return fmt.Errorf("gateway exposure TLS is configured on the referenced Gateway")
	}
	return nil
}

// reconcileExposure drives the user-facing exposure toward the requested
// type. Exactly one exposure object (Ingress, Route, or HTTPRoute) may
// exist at a time, so the other kinds are always deleted, and the
// RouteReady condition and status URL are updated to reflect what was
// observed. When the required platform API is not served the exposure is
// reported as not admitted instead of failing the reconcile.
func (r *Reconciler) reconcileExposure(ctx context.Context, console *pgtoolboxv1alpha1.PgConsole) error {
	exposure := &console.Spec.Exposure
	if exposure.Type == pgtoolboxv1alpha1.ExposureTypeClusterIP {
		console.Status.URL = ""
	} else {
		console.Status.URL = "https://" + exposure.Hostname
	}

	switch exposure.Type {
	case pgtoolboxv1alpha1.ExposureTypeClusterIP:
		if err := r.deleteOtherExposures(ctx, console, ""); err != nil {
			return err
		}
		conditions.MarkTrue(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
			pgtoolboxv1alpha1.ReasonNotRequested,
			"external exposure is not requested",
		)
	case pgtoolboxv1alpha1.ExposureTypeIngress:
		ingress, err := r.reconcileIngress(ctx, console)
		if err != nil {
			return err
		}
		if err := r.deleteOtherExposures(ctx, console, "ingress"); err != nil {
			return err
		}
		setIngressCondition(console, ingress)
	case pgtoolboxv1alpha1.ExposureTypeRoute:
		if !r.RouteAPIAvailable {
			if err := r.deleteOtherExposures(ctx, console, "route"); err != nil {
				return err
			}
			conditions.MarkFalse(
				console,
				pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
				pgtoolboxv1alpha1.ReasonNotAdmitted,
				"OpenShift Route API is not served",
			)
			return nil
		}
		route, err := r.reconcileRoute(ctx, console)
		if err != nil {
			return err
		}
		if err := r.deleteOtherExposures(ctx, console, "route"); err != nil {
			return err
		}
		setRouteCondition(console, route)
	case pgtoolboxv1alpha1.ExposureTypeGateway:
		if !r.GatewayAPIAvailable {
			if err := r.deleteOtherExposures(ctx, console, "gateway"); err != nil {
				return err
			}
			conditions.MarkFalse(
				console,
				pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
				pgtoolboxv1alpha1.ReasonGatewayNotFound,
				"Gateway API is not served",
			)
			return nil
		}
		httpRoute, err := r.reconcileHTTPRoute(ctx, console)
		if err != nil {
			return err
		}
		if err := r.deleteOtherExposures(ctx, console, "gateway"); err != nil {
			return err
		}
		if err := r.setHTTPRouteCondition(ctx, console, httpRoute); err != nil {
			return err
		}
	}
	return nil
}

// reconcileIngress ensures the Ingress matches the desired state, writing
// only when metadata or spec actually differ so steady-state reconciles do
// not touch the API server. It returns the live object when it already
// matches, so callers can read the published load balancer status.
func (r *Reconciler) reconcileIngress(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*networkingv1.Ingress, error) {
	desired, err := r.ingress(console)
	if err != nil {
		return nil, err
	}
	var existing networkingv1.Ingress
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && exposureObjectMatches(&existing, desired) &&
		apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return &existing, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.ApplyObject(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

// ingress builds the desired Ingress routing the exposure hostname to the
// console's Service, owned by the instance. It must stay deterministic: the
// reconcile loop compares its output against the live object to decide
// whether an update is needed.
func (r *Reconciler) ingress(console *pgtoolboxv1alpha1.PgConsole) (*networkingv1.Ingress, error) {
	exposure := &console.Spec.Exposure
	name := application.ResourceName(console.Name, "")
	pathType := networkingv1.PathTypePrefix
	ingress := &networkingv1.Ingress{
		TypeMeta: metav1.TypeMeta{APIVersion: networkingv1.SchemeGroupVersion.String(), Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   console.Namespace,
			Labels:      application.CommonLabels(console.Name),
			Annotations: exposureAnnotations(exposure),
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: stringPtrOrNil(exposure.IngressClassName),
			Rules: []networkingv1.IngressRule{{
				Host: exposure.Hostname,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: name,
							Port: networkingv1.ServiceBackendPort{Number: 80},
						}},
					}},
				}},
			}},
		},
	}
	if tls := exposure.TLS; tls != nil && tls.CertificateSecretRef != nil {
		ingress.Spec.TLS = []networkingv1.IngressTLS{{
			Hosts:      []string{exposure.Hostname},
			SecretName: tls.CertificateSecretRef.Name,
		}}
	}
	if err := controllerutil.SetControllerReference(console, ingress, r.Scheme); err != nil {
		return nil, err
	}
	return ingress, nil
}

// setIngressCondition translates the observed Ingress state into the
// RouteReady condition: admitted once a load balancer address has been
// published, not admitted until then.
func setIngressCondition(console *pgtoolboxv1alpha1.PgConsole, ingress *networkingv1.Ingress) {
	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
			pgtoolboxv1alpha1.ReasonNotAdmitted,
			"Ingress has not published a load balancer address",
		)
		return
	}
	conditions.MarkTrue(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
		pgtoolboxv1alpha1.ReasonAsExpected,
		"Ingress is admitted for %s",
		console.Spec.Exposure.Hostname,
	)
}

// reconcileRoute ensures the OpenShift Route matches the desired state,
// writing only when metadata or spec actually differ. It returns the live
// object when it already matches, so callers can inspect the router's
// admission status.
func (r *Reconciler) reconcileRoute(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*routev1.Route, error) {
	desired, err := r.route(console)
	if err != nil {
		return nil, err
	}
	var existing routev1.Route
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && exposureObjectMatches(&existing, desired) &&
		apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return &existing, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.ApplyObject(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

// route builds the desired OpenShift Route for the exposure hostname, owned
// by the instance. Reencrypt termination targets the Service's https port
// and insecure traffic is always redirected. It must stay deterministic:
// the reconcile loop compares its output against the live object to decide
// whether an update is needed.
func (r *Reconciler) route(console *pgtoolboxv1alpha1.PgConsole) (*routev1.Route, error) {
	exposure := &console.Spec.Exposure
	name := application.ResourceName(console.Name, "")
	port := "http"
	weight := int32(100)
	var routeTLS *routev1.TLSConfig
	if exposureTLS := exposure.TLS; exposureTLS != nil && exposureTLS.Termination != "" {
		termination := routev1.TLSTerminationType(exposureTLS.Termination)
		routeTLS = &routev1.TLSConfig{
			Termination:                   termination,
			InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
		}
		if termination == routev1.TLSTerminationReencrypt {
			port = "https"
		}
	}
	route := &routev1.Route{
		TypeMeta: metav1.TypeMeta{APIVersion: routev1.GroupVersion.String(), Kind: "Route"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   console.Namespace,
			Labels:      application.CommonLabels(console.Name),
			Annotations: exposureAnnotations(exposure),
		},
		Spec: routev1.RouteSpec{
			Host: exposure.Hostname,
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   name,
				Weight: &weight,
			},
			Port:           &routev1.RoutePort{TargetPort: intstr.FromString(port)},
			TLS:            routeTLS,
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
	if err := controllerutil.SetControllerReference(console, route, r.Scheme); err != nil {
		return nil, err
	}
	return route, nil
}

// setRouteCondition translates the router's admission verdict for the
// exposure hostname into the RouteReady condition, distinguishing a hostname
// already claimed by another Route from a Route that simply has not been
// admitted yet.
func setRouteCondition(console *pgtoolboxv1alpha1.PgConsole, route *routev1.Route) {
	hostname := console.Spec.Exposure.Hostname
	for _, ingress := range route.Status.Ingress {
		if ingress.Host != hostname {
			continue
		}
		for _, condition := range ingress.Conditions {
			if condition.Type != routev1.RouteAdmitted {
				continue
			}
			if condition.Status == corev1.ConditionTrue {
				conditions.MarkTrue(
					console,
					pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
					pgtoolboxv1alpha1.ReasonAsExpected,
					"Route is admitted for %s",
					hostname,
				)
				return
			}
			if routeConditionReportsHostnameConflict(condition) {
				conditions.MarkFalse(
					console,
					pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
					pgtoolboxv1alpha1.ReasonHostnameConflict,
					"Route hostname is already claimed",
				)
				return
			}
		}
	}
	conditions.MarkFalse(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
		pgtoolboxv1alpha1.ReasonNotAdmitted,
		"Route has not been admitted for %s",
		hostname,
	)
}

// routeConditionReportsHostnameConflict heuristically detects a hostname
// ownership rejection in a Route ingress condition. Routers report this only
// through free-form reason and message text, so a keyword match is the best
// signal available to surface a dedicated HostnameConflict status reason.
func routeConditionReportsHostnameConflict(condition routev1.RouteIngressCondition) bool {
	text := strings.ToLower(condition.Reason + " " + condition.Message)
	return strings.Contains(text, "host") &&
		(strings.Contains(text, "claim") || strings.Contains(text, "conflict") || strings.Contains(text, "duplicate"))
}

// reconcileHTTPRoute ensures the Gateway API HTTPRoute matches the desired
// state, writing only when metadata or spec actually differ. It returns the
// live object when it already matches, so callers can inspect the parent
// Gateway's acceptance status.
func (r *Reconciler) reconcileHTTPRoute(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*gatewayv1.HTTPRoute, error) {
	desired, err := r.httpRoute(console)
	if err != nil {
		return nil, err
	}
	var existing gatewayv1.HTTPRoute
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if err == nil && exposureObjectMatches(&existing, desired) &&
		apiequality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return &existing, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err := r.ApplyObject(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

// httpRoute builds the desired HTTPRoute attaching the exposure hostname to
// the Gateway named in spec.exposure.gateway.parentRef and backing it with
// the console's Service, owned by the instance. It must stay deterministic:
// the reconcile loop compares its output against the live object to decide
// whether an update is needed.
func (r *Reconciler) httpRoute(console *pgtoolboxv1alpha1.PgConsole) (*gatewayv1.HTTPRoute, error) {
	exposure := &console.Spec.Exposure
	name := application.ResourceName(console.Name, "")
	gatewayGroup := gatewayv1.Group(gatewayv1.GroupName)
	gatewayKind := gatewayv1.Kind("Gateway")
	serviceGroup := gatewayv1.Group("")
	serviceKind := gatewayv1.Kind("Service")
	parent := exposure.Gateway.ParentRef
	parentRef := gatewayv1.ParentReference{
		Group: &gatewayGroup,
		Kind:  &gatewayKind,
		Name:  gatewayv1.ObjectName(parent.Name),
	}
	if parent.Namespace != "" {
		namespace := gatewayv1.Namespace(parent.Namespace)
		parentRef.Namespace = &namespace
	}
	if parent.SectionName != "" {
		sectionName := gatewayv1.SectionName(parent.SectionName)
		parentRef.SectionName = &sectionName
	}
	port := gatewayv1.PortNumber(443)
	weight := int32(1)
	httpRoute := &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{APIVersion: gatewayv1.GroupVersion.String(), Kind: "HTTPRoute"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   console.Namespace,
			Labels:      application.CommonLabels(console.Name),
			Annotations: exposureAnnotations(exposure),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parentRef}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(exposure.Hostname)},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Group: &serviceGroup,
							Kind:  &serviceKind,
							Name:  gatewayv1.ObjectName(name),
							Port:  &port,
						},
						Weight: &weight,
					},
				}},
			}},
		},
	}
	if err := controllerutil.SetControllerReference(console, httpRoute, r.Scheme); err != nil {
		return nil, err
	}
	return httpRoute, nil
}

// setHTTPRouteCondition derives the RouteReady condition for Gateway
// exposure. It first checks that the referenced Gateway exists and offers a
// compatible listener — giving the user an actionable reason before any
// route status appears — and only then requires the HTTPRoute to be Accepted
// with ResolvedRefs by that parent at the current generation. A missing
// Gateway marks the condition false rather than returning an error.
func (r *Reconciler) setHTTPRouteCondition(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	httpRoute *gatewayv1.HTTPRoute,
) error {
	gateway, err := r.referencedGateway(ctx, console)
	if apierrors.IsNotFound(err) {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
			pgtoolboxv1alpha1.ReasonGatewayNotFound,
			"referenced Gateway was not found",
		)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read referenced Gateway: %w", err)
	}
	compatible, err := r.gatewayHasCompatibleListener(ctx, console, gateway)
	if err != nil {
		return fmt.Errorf("validate referenced Gateway listener: %w", err)
	}
	if !compatible {
		conditions.MarkFalse(
			console,
			pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
			pgtoolboxv1alpha1.ReasonNotAccepted,
			"referenced Gateway has no compatible listener",
		)
		return nil
	}

	for _, parentStatus := range httpRoute.Status.Parents {
		if !parentRefMatches(console, parentStatus.ParentRef) {
			continue
		}
		accepted := routeConditionIsTrue(
			parentStatus.Conditions, string(gatewayv1.RouteConditionAccepted), httpRoute.Generation,
		)
		resolved := routeConditionIsTrue(
			parentStatus.Conditions, string(gatewayv1.RouteConditionResolvedRefs), httpRoute.Generation,
		)
		if accepted && resolved {
			conditions.MarkTrue(
				console,
				pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
				pgtoolboxv1alpha1.ReasonAsExpected,
				"HTTPRoute is accepted by the referenced Gateway",
			)
			return nil
		}
	}
	conditions.MarkFalse(
		console,
		pgtoolboxv1alpha1.PgConsoleConditionRouteReady,
		pgtoolboxv1alpha1.ReasonNotAccepted,
		"HTTPRoute has not been accepted with resolved references",
	)
	return nil
}

// referencedGateway fetches the Gateway named by the exposure parentRef,
// defaulting to the instance's own namespace when the reference omits one, as
// Gateway API attachment semantics require.
func (r *Reconciler) referencedGateway(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
) (*gatewayv1.Gateway, error) {
	parent := console.Spec.Exposure.Gateway.ParentRef
	namespace := parent.Namespace
	if namespace == "" {
		namespace = console.Namespace
	}
	var gateway gatewayv1.Gateway
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: parent.Name}, &gateway)
	return &gateway, err
}

// gatewayHasCompatibleListener reports whether at least one listener on the
// Gateway would accept our HTTPRoute under Gateway API attachment semantics:
// matching the requested sectionName if set, speaking HTTP or HTTPS,
// covering the exposure hostname, and allowing HTTPRoutes from the instance's
// namespace. False means the route cannot attach and RouteReady should
// report NotAccepted with a cause the Gateway owner must fix.
func (r *Reconciler) gatewayHasCompatibleListener(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	gateway *gatewayv1.Gateway,
) (bool, error) {
	exposure := &console.Spec.Exposure
	parent := exposure.Gateway.ParentRef
	for _, listener := range gateway.Spec.Listeners {
		if parent.SectionName != "" && string(listener.Name) != parent.SectionName {
			continue
		}
		if listener.Protocol != gatewayv1.HTTPProtocolType && listener.Protocol != gatewayv1.HTTPSProtocolType {
			continue
		}
		if listener.Hostname != nil && !gatewayHostnameMatches(string(*listener.Hostname), exposure.Hostname) {
			continue
		}
		if !listenerAllowsHTTPRoute(listener) {
			continue
		}
		allowed, err := r.listenerAllowsNamespace(ctx, listener, gateway.Namespace, console.Namespace)
		if err != nil {
			return false, err
		}
		if allowed {
			return true, nil
		}
	}
	return false, nil
}

// gatewayHostnameMatches implements Gateway API hostname intersection for a
// single listener: an exact match, or a "*." wildcard covering exactly one
// extra leading label (the bare suffix itself does not match).
func gatewayHostnameMatches(listenerHostname, routeHostname string) bool {
	if listenerHostname == routeHostname {
		return true
	}
	if !strings.HasPrefix(listenerHostname, "*.") {
		return false
	}
	suffix := listenerHostname[1:]
	return strings.HasSuffix(routeHostname, suffix) && len(routeHostname) > len(suffix)
}

// listenerAllowsHTTPRoute reports whether the listener's allowedRoutes kinds
// admit HTTPRoute. An absent or empty kind list means the listener accepts
// its protocol's default route kinds, which includes HTTPRoute; false means
// the route will never attach to this listener.
func listenerAllowsHTTPRoute(listener gatewayv1.Listener) bool {
	if listener.AllowedRoutes == nil || len(listener.AllowedRoutes.Kinds) == 0 {
		return true
	}
	for _, kind := range listener.AllowedRoutes.Kinds {
		group := gatewayv1.GroupName
		if kind.Group != nil {
			group = string(*kind.Group)
		}
		if group == gatewayv1.GroupName && kind.Kind == gatewayv1.Kind("HTTPRoute") {
			return true
		}
	}
	return false
}

// listenerAllowsNamespace applies the listener's allowedRoutes namespace
// policy to the route's namespace. Per Gateway API defaults, an unset policy
// means Same, so only routes in the Gateway's namespace attach; the Selector
// policy requires reading the route namespace's labels, which is why this
// helper needs the client and can return an error.
func (r *Reconciler) listenerAllowsNamespace(
	ctx context.Context,
	listener gatewayv1.Listener,
	gatewayNamespace, routeNamespace string,
) (bool, error) {
	if listener.AllowedRoutes == nil || listener.AllowedRoutes.Namespaces == nil ||
		listener.AllowedRoutes.Namespaces.From == nil ||
		*listener.AllowedRoutes.Namespaces.From == gatewayv1.NamespacesFromSame {
		return gatewayNamespace == routeNamespace, nil
	}
	namespaces := listener.AllowedRoutes.Namespaces
	switch *namespaces.From {
	case gatewayv1.NamespacesFromAll:
		return true, nil
	case gatewayv1.NamespacesFromNone:
		return false, nil
	case gatewayv1.NamespacesFromSelector:
		if namespaces.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(namespaces.Selector)
		if err != nil {
			return false, err
		}
		var namespace corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: routeNamespace}, &namespace); err != nil {
			return false, err
		}
		return selector.Matches(labels.Set(namespace.Labels)), nil
	default:
		return false, nil
	}
}

// parentRefMatches reports whether a parentRef found in HTTPRoute status
// refers to the Gateway configured in the spec, so that status conditions
// written by other parents are ignored. Both sides default an omitted
// namespace to the instance's namespace before comparing.
func parentRefMatches(console *pgtoolboxv1alpha1.PgConsole, parent gatewayv1.ParentReference) bool {
	desired := console.Spec.Exposure.Gateway.ParentRef
	if string(parent.Name) != desired.Name {
		return false
	}
	namespace := console.Namespace
	if desired.Namespace != "" {
		namespace = desired.Namespace
	}
	actualNamespace := console.Namespace
	if parent.Namespace != nil {
		actualNamespace = string(*parent.Namespace)
	}
	if actualNamespace != namespace {
		return false
	}
	actualSection := ""
	if parent.SectionName != nil {
		actualSection = string(*parent.SectionName)
	}
	return actualSection == desired.SectionName
}

// routeConditionIsTrue reports whether the named condition is True for at
// least the given generation, so stale acceptance from a previous spec is
// not mistaken for current acceptance.
func routeConditionIsTrue(routeConditions []metav1.Condition, conditionType string, generation int64) bool {
	for _, condition := range routeConditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue &&
			condition.ObservedGeneration >= generation {
			return true
		}
	}
	return false
}

// exposureObjectMatches reports whether an existing exposure object's
// metadata still matches the desired build — labels present, annotations
// identical, and the same controller owner — so the reconcilers can skip
// writes when only the spec comparison remains.
func exposureObjectMatches(existing, desired client.Object) bool {
	return shared.LabelsContain(existing.GetLabels(), desired.GetLabels()) &&
		reflect.DeepEqual(existing.GetAnnotations(), desired.GetAnnotations()) &&
		shared.ControllerOwnerMatches(existing, desired)
}

// stringPtrOrNil maps an empty string to nil so optional API fields stay
// unset instead of pointing at "".
func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// exposureAnnotations returns the user-supplied exposure annotations with
// operator-reserved keys stripped, normalizing an empty result to nil so it
// compares equal to the annotations of an object created without any.
func exposureAnnotations(exposure *pgtoolboxv1alpha1.ExposureSpec) map[string]string {
	filtered := shared.FilteredOverlay(exposure.Annotations)
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// deleteOtherExposures enforces the single-exposure invariant by deleting
// the exposure objects of every kind except keep ("ingress", "route",
// "gateway", or "" to remove all). Kinds whose API is not served are skipped
// so the operator never queries an absent platform API.
func (r *Reconciler) deleteOtherExposures(
	ctx context.Context,
	console *pgtoolboxv1alpha1.PgConsole,
	keep string,
) error {
	key := client.ObjectKey{
		Namespace: console.Namespace,
		Name:      application.ResourceName(console.Name, ""),
	}
	if keep != "ingress" {
		if err := r.deleteExposureIfOwned(ctx, key, &networkingv1.Ingress{}, console); err != nil {
			return err
		}
	}
	if keep != "route" && r.RouteAPIAvailable {
		if err := r.deleteExposureIfOwned(ctx, key, &routev1.Route{}, console); err != nil {
			return err
		}
	}
	if keep != "gateway" && r.GatewayAPIAvailable {
		if err := r.deleteExposureIfOwned(ctx, key, &gatewayv1.HTTPRoute{}, console); err != nil {
			return err
		}
	}
	return nil
}

// deleteExposureIfOwned deletes the object at key only when this instance is
// its controller owner, so switching exposure types never removes an
// unrelated object that happens to share the generated name. Absent objects
// and delete races are not errors.
func (r *Reconciler) deleteExposureIfOwned(
	ctx context.Context,
	key client.ObjectKey,
	target client.Object,
	console *pgtoolboxv1alpha1.PgConsole,
) error {
	if err := r.Get(ctx, key, target); err != nil {
		return client.IgnoreNotFound(err)
	}
	owner := metav1.GetControllerOf(target)
	if owner == nil || owner.UID != console.UID {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, target))
}
