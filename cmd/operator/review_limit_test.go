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

package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// TestStartupClientBoundsTheRulesReview: round 16, K8S P2-1. One namespace
// whose rules review answers more than managerrole.ReviewResponseLimit
// (one Role of thousands of rules bound many times to the operator) is
// left out alone through the start-up client, before its body is decoded;
// the other namespaces are kept, and the start goes on.
func TestStartupClientBoundsTheRulesReview(t *testing.T) {
	codecs := serializer.NewCodecFactory(scheme)
	rule := authorizationv1.ResourceRule{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"list"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType := r.Header.Get("Content-Type")
		info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), contentType)
		body, err := io.ReadAll(r.Body)
		var review authorizationv1.SelfSubjectRulesReview
		if r.Method != http.MethodPost || !ok || err != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if _, _, err := info.Serializer.Decode(body, nil, &review); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		review.APIVersion, review.Kind = "authorization.k8s.io/v1", "SelfSubjectRulesReview"
		review.Status.ResourceRules = []authorizationv1.ResourceRule{rule}
		if review.Spec.Namespace == "team-hostile" {
			// Over ReviewResponseLimit of duplicated rules (JSON or protobuf), streamed
			// without a length.
			review.Status.ResourceRules = slices.Repeat(review.Status.ResourceRules, 250_000)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusCreated)
		_ = codecs.EncoderForVersion(info.Serializer, authorizationv1.SchemeGroupVersion).Encode(&review, w)
	}))
	defer server.Close()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(authorizationv1.SchemeGroupVersion.WithKind("SelfSubjectRulesReview"), meta.RESTScopeRoot)
	c, err := client.New(startupConfig(&rest.Config{Host: server.URL}), client.Options{Scheme: scheme, Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	required := []managerrole.Permission{{Group: "apps", Resource: "deployments", Verbs: []string{"list"}}}
	review, err := managerrole.PermittedNamespaces(t.Context(), c, required, []string{"team-a", "team-hostile", "team-b"}, 0)
	if err != nil {
		t.Fatalf("one oversized review failed the start: %v", err)
	}
	if !slices.Equal(review.Permitted, []string{"team-a", "team-b"}) || len(review.Unreviewed) != 1 || review.Unreviewed[0].Namespace != "team-hostile" {
		t.Fatalf("review = %+v, want team-a and team-b kept, team-hostile unreviewed", review)
	}
	if !errors.Is(review.Unreviewed[0].Err, managerrole.ErrReviewTooLarge) {
		t.Errorf("team-hostile's error = %v, want ErrReviewTooLarge", review.Unreviewed[0].Err)
	}
}
