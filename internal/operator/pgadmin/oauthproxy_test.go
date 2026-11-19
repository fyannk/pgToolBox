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
	"context"
	"testing"

	imagev1 "github.com/openshift/api/image/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
)

const clusterProxyImage = "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func clusterProxyStream(tags ...imagev1.NamedTagEventList) *imagev1.ImageStream {
	return &imagev1.ImageStream{
		ObjectMeta: metav1.ObjectMeta{Namespace: "openshift", Name: "oauth-proxy"},
		Status:     imagev1.ImageStreamStatus{Tags: tags},
	}
}

func proxyTag(tag, image string) imagev1.NamedTagEventList {
	return imagev1.NamedTagEventList{Tag: tag, Items: []imagev1.TagEvent{{DockerImageReference: image}}}
}

// TestOAuthProxyImageFromTheCluster: on OpenShift a PgAdmin that names no
// oauth-proxy image runs the cluster's own (openshift/oauth-proxy:v4.4);
// an installer's OAUTH_PROXY_IMAGE or the spec wins over it, and a cluster
// that has none, or does not let the operator read it, gets the compiled-in
// default.
func TestOAuthProxyImageFromTheCluster(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "image.openshift.io", Resource: "imagestreams"}, "oauth-proxy", nil)
	tests := []struct {
		name         string
		stream       *imagev1.ImageStream
		getErr       error
		notOpenShift bool
		configured   string
		spec         *pgtoolboxv1alpha1.ImageSpec
		want         string
	}{
		{name: "cluster image", stream: clusterProxyStream(proxyTag("v4.4", clusterProxyImage)), want: clusterProxyImage},
		{name: "no ImageStream", want: configuration.DefaultOAuthProxyImage},
		{name: "ImageStream unreadable", stream: clusterProxyStream(proxyTag("v4.4", clusterProxyImage)), getErr: forbidden, want: configuration.DefaultOAuthProxyImage},
		{name: "tag not imported", stream: clusterProxyStream(imagev1.NamedTagEventList{Tag: "v4.4"}), want: configuration.DefaultOAuthProxyImage},
		{name: "another tag only", stream: clusterProxyStream(proxyTag("latest", clusterProxyImage)), want: configuration.DefaultOAuthProxyImage},
		{name: "tag not pinned by digest", stream: clusterProxyStream(proxyTag("v4.4", "quay.io/openshift/origin-oauth-proxy:4.18")), want: configuration.DefaultOAuthProxyImage},
		{name: "not OpenShift", stream: clusterProxyStream(proxyTag("v4.4", clusterProxyImage)), notOpenShift: true, want: configuration.DefaultOAuthProxyImage},
		{
			name: "OAUTH_PROXY_IMAGE wins", stream: clusterProxyStream(proxyTag("v4.4", clusterProxyImage)),
			configured: "registry.example.org/mirror/oauth-proxy:4.18", want: "registry.example.org/mirror/oauth-proxy:4.18",
		},
		{
			name: "spec wins", stream: clusterProxyStream(proxyTag("v4.4", clusterProxyImage)),
			spec: &pgtoolboxv1alpha1.ImageSpec{Repository: "quay.io/openshift/origin-oauth-proxy", Tag: "4.18"}, want: "quay.io/openshift/origin-oauth-proxy:4.18",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := openshiftPgAdmin()
			p.Spec.NetworkPolicy = pgtoolboxv1alpha1.PgAdminNetworkPolicySpec{}
			p.Spec.Auth.OpenShiftOAuthProxy.Image = tt.spec
			objs := baseObjects(p)
			if tt.stream != nil {
				objs = append(objs, tt.stream)
			}
			var funcs *interceptor.Funcs
			if tt.getErr != nil {
				funcs = &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*imagev1.ImageStream); ok {
						return tt.getErr
					}
					return c.Get(ctx, key, obj, opts...)
				}}
			}
			h := newHarness(t, funcs, objs...)
			h.r.Features.OpenShift = !tt.notOpenShift
			if tt.configured != "" {
				h.r.Config.OAuthProxyImage = tt.configured
			}
			if err := h.reconcile(p.Name); err != nil {
				t.Fatal(err)
			}
			var deployment appsv1.Deployment
			if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &deployment); err != nil {
				t.Fatal(err)
			}
			if got := containerNamed(t, deployment.Spec.Template.Spec.Containers, "oauth-proxy").Image; got != tt.want {
				t.Errorf("oauth-proxy image = %s, want %s", got, tt.want)
			}
		})
	}
}

// A transient failure to read the cluster's ImageStream fails the
// reconcile: it never swaps the running proxy for the compiled-in default.
func TestOAuthProxyImageReadFailureKeepsTheRunningImage(t *testing.T) {
	p := openshiftPgAdmin()
	p.Spec.NetworkPolicy = pgtoolboxv1alpha1.PgAdminNetworkPolicySpec{}
	failing := false
	h := newHarness(t, &interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*imagev1.ImageStream); ok && failing {
			return apierrors.NewServerTimeout(schema.GroupResource{Group: "image.openshift.io", Resource: "imagestreams"}, "get", 1)
		}
		return c.Get(ctx, key, obj, opts...)
	}}, append(baseObjects(p), clusterProxyStream(proxyTag("v4.4", clusterProxyImage)))...)
	h.r.Features.OpenShift = true
	if err := h.reconcile(p.Name); err != nil {
		t.Fatal(err)
	}
	failing = true
	if err := h.reconcile(p.Name); err == nil {
		t.Fatal("a failed read of the cluster's oauth-proxy must fail the reconcile")
	}
	var deployment appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: p.Name + "-pgadmin"}, &deployment); err != nil {
		t.Fatal(err)
	}
	if got := containerNamed(t, deployment.Spec.Template.Spec.Containers, "oauth-proxy").Image; got != clusterProxyImage {
		t.Errorf("oauth-proxy image = %s after a failed read, want the running %s", got, clusterProxyImage)
	}
}
