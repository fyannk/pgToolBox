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

package api

import (
	"context"
	"fmt"
	"sync"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// nsReviewer allows clusters in every namespace but "ns-denied", where it
// allows one name; it counts the reviews.
type nsReviewer struct {
	mu    sync.Mutex
	calls int
}

func (r *nsReviewer) Review(_ context.Context, sar *authorizationv1.SubjectAccessReview) (*authorizationv1.SubjectAccessReview, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	ra := sar.Spec.ResourceAttributes
	sar.Status.Allowed = ra != nil && (ra.Namespace != "ns-denied" || ra.Name == "c-allowed")
	return sar, nil
}

// TestDigestFilterAsksPerNamespace: a digest of 1,600 Clusters in 16
// namespaces costs one review per namespace, and per Cluster only where
// the namespace-wide answer is no.
func TestDigestFilterAsksPerNamespace(t *testing.T) {
	r := &nsReviewer{}
	az := authz.NewAuthorizer(r, authz.Options{})
	d := viewmodel.Digest{}
	for i := range 1500 {
		d.Clusters = append(d.Clusters, viewmodel.DigestCluster{Ref: viewmodel.ClusterRef{Namespace: fmt.Sprintf("ns-%02d", i%15), Name: fmt.Sprintf("c-%d", i)}})
	}
	for i := range 99 {
		d.Clusters = append(d.Clusters, viewmodel.DigestCluster{Ref: viewmodel.ClusterRef{Namespace: "ns-denied", Name: fmt.Sprintf("c-%d", i)}})
	}
	d.Clusters = append(d.Clusters, viewmodel.DigestCluster{Ref: viewmodel.ClusterRef{Namespace: "ns-denied", Name: "c-allowed"}})
	p := &auth.Principal{User: authz.User{Name: "jane"}}
	got := filterDigests(context.Background(), az, p, []viewmodel.Digest{d, d})[0]
	if len(got.Clusters) != 1501 || got.Hidden != 99 {
		t.Fatalf("kept %d, hidden %d; want 1501, 99", len(got.Clusters), got.Hidden)
	}
	if want := 16 + 100; r.calls != want {
		t.Fatalf("%d reviews, want %d: one per namespace, then the names of the denied one", r.calls, want)
	}
}
