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

//go:build integration

package gcs

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/providertest"
)

func TestFakeGCSJourney(t *testing.T) {
	endpoint := os.Getenv("FAKE_GCS_ENDPOINT")
	if endpoint == "" {
		t.Skip("FAKE_GCS_ENDPOINT is required")
	}
	for _, object := range providertest.BarmanFixture(t) {
		uploadURL := endpoint + "/upload/storage/v1/b/evidence-agent-proof/o?uploadType=media&name=" + url.QueryEscape("repository/"+object.Key)
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, uploadURL, bytes.NewReader(object.Data))
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			t.Fatalf("fixture upload %s status = %d", object.Key, response.StatusCode)
		}
	}
	upstream, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// A bare ReverseProxy: NewSingleHostReverseProxy sets Director, and a proxy
	// with both Director and Rewrite refuses every request.
	proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		request := pr.Out
		request.URL.Scheme, request.URL.Host, request.Host = upstream.Scheme, upstream.Host, upstream.Host
		if strings.HasPrefix(request.URL.Path, "/storage/v1/download/") {
			request.URL.Path = strings.TrimPrefix(request.URL.Path, "/storage/v1")
		}
	}}
	server := httptest.NewServer(proxy)
	defer server.Close()
	reader, err := New(context.Background(), Options{Bucket: "evidence-agent-proof", Prefix: "repository", Endpoint: server.URL + "/storage/v1/", RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	normalized := providertest.BarmanCatalogJourney(t, reader)
	providertest.WriteJourneyResult(t, normalized)
}
