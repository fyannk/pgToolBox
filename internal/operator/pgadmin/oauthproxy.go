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
	"fmt"
	"strings"

	imagev1 "github.com/openshift/api/image/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/operator/configuration"
)

// The cluster's own oauth-proxy: an ImageStream of the OpenShift release
// payload (cluster-samples-operator manifests, applied by the cluster version
// operator) whose tag resolves to the payload's image by digest. It follows
// the cluster's updates and a disconnected cluster's mirror serves it.
const (
	clusterOAuthProxyNamespace = "openshift"
	clusterOAuthProxyName      = "oauth-proxy"
	clusterOAuthProxyTag       = "v4.4"
)

// +kubebuilder:rbac:groups=image.openshift.io,resources=imagestreams,resourceNames=oauth-proxy,verbs=get

// oauthProxyDefault is the oauth-proxy image of a PgAdmin whose spec names
// none (read only for a PgAdmin that runs the proxy): OAUTH_PROXY_IMAGE when
// the installer set it, else on OpenShift the cluster's own
// (openshift/oauth-proxy:v4.4), else the compiled-in default.
// A cluster without that ImageStream, or one the operator may not read,
// gets the compiled-in default; any other read error is returned, so that a
// transient failure never swaps the image of a running pgAdmin.
func (r *Reconciler) oauthProxyDefault(ctx context.Context, p *pgtoolboxv1alpha1.PgAdmin) (string, error) {
	proxy := p.Spec.Auth.OpenShiftOAuthProxy
	if proxy == nil || proxy.Image != nil || r.Config.OAuthProxyImage != configuration.DefaultOAuthProxyImage || !r.Features.OpenShift {
		return r.Config.OAuthProxyImage, nil
	}
	var stream imagev1.ImageStream
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: clusterOAuthProxyNamespace, Name: clusterOAuthProxyName}, &stream)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err):
		logf.FromContext(ctx).V(1).Info("the cluster's oauth-proxy is unreadable; using the compiled-in default", "error", err.Error())
		return r.Config.OAuthProxyImage, nil
	default:
		return "", fmt.Errorf("read the ImageStream %s/%s: %w", clusterOAuthProxyNamespace, clusterOAuthProxyName, err)
	}
	if image := taggedImage(&stream, clusterOAuthProxyTag); image != "" {
		return image, nil
	}
	logf.FromContext(ctx).V(1).Info("the cluster's oauth-proxy tag is not imported; using the compiled-in default", "tag", clusterOAuthProxyTag)
	return r.Config.OAuthProxyImage, nil
}

// taggedImage is the image a tag of the stream currently points at, when it
// is pinned by digest; "" otherwise.
func taggedImage(stream *imagev1.ImageStream, tag string) string {
	for _, t := range stream.Status.Tags {
		if t.Tag != tag || len(t.Items) == 0 {
			continue
		}
		if image := t.Items[0].DockerImageReference; strings.Contains(image, "@sha256:") {
			return image
		}
	}
	return ""
}
