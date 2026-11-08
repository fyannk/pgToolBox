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

package gcs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

func TestSafeErrorRedactsGoogleAPIDetails(t *testing.T) {
	t.Parallel()
	canary := "gcs-token-canary"
	for status, want := range map[int]fault.Category{401: fault.Authentication, 403: fault.Authorization, 404: fault.NotFound, 429: fault.Throttled, 500: fault.Unavailable} {
		err := safeError(context.Background(), &googleapi.Error{Code: status, Message: canary})
		if fault.Categorize(err) != want || strings.Contains(err.Error(), canary) {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}

func TestListUsesOpaquePrefixBoundProviderPagination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("pageToken") == "native-token-canary" {
			_, _ = writer.Write([]byte(`{"items":[{"name":"root/alpha/two","size":"2","updated":"2026-07-27T12:00:00Z"}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"items":[{"name":"root/alpha/one","size":"1","updated":"2026-07-27T12:00:00Z"}],"nextPageToken":"native-token-canary"}`))
	}))
	defer server.Close()
	reader, err := New(context.Background(), Options{Bucket: "bucket", Prefix: "root", Endpoint: server.URL + "/storage/v1/", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	first, err := reader.List(context.Background(), store.ListRequest{Prefix: "alpha/", Limit: 1})
	if err != nil || len(first.Objects) != 1 || first.Objects[0].Key != "alpha/one" || first.NextCursor == "" || strings.Contains(first.NextCursor, "native-token-canary") {
		t.Fatalf("first List() = %#v, %v", first, err)
	}
	second, err := reader.List(context.Background(), store.ListRequest{Prefix: "alpha/", Cursor: first.NextCursor, Limit: 1})
	if err != nil || len(second.Objects) != 1 || second.Objects[0].Key != "alpha/two" || second.NextCursor != "" {
		t.Fatalf("second List() = %#v, %v", second, err)
	}
	if _, err := reader.List(context.Background(), store.ListRequest{Prefix: "beta/", Cursor: first.NextCursor, Limit: 1}); !errors.Is(err, store.ErrInvalidRequest) {
		t.Fatalf("cross-prefix cursor error = %v", err)
	}
}

// TestOpenKeepsTheFailureCategory: an Open that fails is reported by its
// category, not as the cancellation of its own request context.
func TestOpenKeepsTheFailureCategory(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"error":{"code":404,"message":"No such object"}}`))
	}))
	defer server.Close()
	reader, err := New(context.Background(), Options{Bucket: "bucket", Endpoint: server.URL + "/storage/v1/", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.Open(context.Background(), store.OpenRequest{Key: "missing", MaxBytes: 64})
	if errors.Is(err, context.Canceled) || fault.Categorize(err) != fault.NotFound {
		t.Fatalf("Open = %v (category %s), want not found", err, fault.Categorize(err))
	}
}
