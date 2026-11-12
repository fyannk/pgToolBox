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

package managerrole

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// oversized answers every request with a body one byte over the limit,
// without a length.
type oversized struct{}

func (oversized) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusCreated, ContentLength: -1,
		Body: io.NopCloser(strings.NewReader(strings.Repeat("x", ReviewResponseLimit+1))),
	}, nil
}

// TestLimitReviewResponsesPaths: round 17, K8S P3-2. A review's response
// is bounded whatever prefix the server URL carries (a proxy such as
// Rancher's /k8s/clusters/<id>); other requests pass unchanged.
func TestLimitReviewResponsesPaths(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		bounded      bool
	}{
		{http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", true},
		{http.MethodPost, "/k8s/clusters/c-m-abc12/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", true},
		{http.MethodPost, "/proxy/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", true},
		{http.MethodGet, "/apis/apps/v1/namespaces/team-a/deployments", false},
		{http.MethodPost, "/k8s/clusters/c-m-abc12/api/v1/namespaces/team-a/events", false},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, "https://rancher.example.com"+tc.path, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := LimitReviewResponses(oversized{}).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, err = io.ReadAll(resp.Body)
			if got := errors.Is(err, ErrReviewTooLarge); got != tc.bounded {
				t.Errorf("read error = %v; bounded %t, want %t", err, got, tc.bounded)
			}
		})
	}
}
