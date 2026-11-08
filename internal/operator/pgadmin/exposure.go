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

	routev1 "github.com/openshift/api/route/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/exposure"
	"github.com/fyannk/pgToolBox/internal/operator/metadata"
)

// exposureMeta is the exposure object's metadata: the propagation rules,
// then the exposure's own labels and annotations, then the operator's
// identity labels, which always win.
func exposureMeta(p *pgtoolboxv1alpha1.PgAdmin) metav1.ObjectMeta {
	meta := objectMeta(p, namesOf(p).base, componentPgAdmin, pgtoolboxv1alpha1.PropagationTargetExposure)
	labels := metadata.FilteredOverlay(p.Spec.Exposure.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, meta.Labels)
	maps.Copy(labels, identity(p).Labels(componentPgAdmin))
	meta.Labels = labels
	if extra := metadata.FilteredOverlay(p.Spec.Exposure.Annotations); len(extra) > 0 {
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		maps.Copy(meta.Annotations, extra)
	}
	return meta
}

func servicePortName(p *pgtoolboxv1alpha1.PgAdmin) string {
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		return "https"
	}
	return "http"
}

// exposureTarget is pgAdmin's Service as the exposure objects see it.
func exposureTarget(p *pgtoolboxv1alpha1.PgAdmin) exposure.Target {
	port := int32(80)
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		port = 443
	}
	return exposure.Target{
		Meta: exposureMeta(p), Service: namesOf(p).base, PortName: servicePortName(p), Port: port,
		// The proxy serves TLS with the service CA's certificate.
		Reencrypt: p.Spec.Auth.OpenShiftOAuthProxy != nil,
	}
}

func buildRoute(p *pgtoolboxv1alpha1.PgAdmin) *routev1.Route {
	return exposure.Route(&p.Spec.Exposure, exposureTarget(p))
}

func buildIngress(p *pgtoolboxv1alpha1.PgAdmin) *networkingv1.Ingress {
	return exposure.Ingress(&p.Spec.Exposure, exposureTarget(p))
}

func buildHTTPRoute(p *pgtoolboxv1alpha1.PgAdmin) *gatewayv1.HTTPRoute {
	return exposure.HTTPRoute(&p.Spec.Exposure, exposureTarget(p))
}

// exposureURL is where pgAdmin is reached.
func exposureURL(p *pgtoolboxv1alpha1.PgAdmin) string {
	if url := exposure.URL(&p.Spec.Exposure); url != "" {
		return url
	}
	if p.Spec.Auth.OpenShiftOAuthProxy != nil {
		return fmt.Sprintf("https://%s.%s.svc/", namesOf(p).base, p.Namespace)
	}
	return fmt.Sprintf("http://%s.%s.svc/", namesOf(p).base, p.Namespace)
}

// exposureFact is the ExposureReady condition read from the exposure
// object's status.
type exposureFact = exposure.Fact

var (
	routeFact     = exposure.RouteFact
	ingressFact   = exposure.IngressFact
	httpRouteFact = exposure.HTTPRouteFact
)
