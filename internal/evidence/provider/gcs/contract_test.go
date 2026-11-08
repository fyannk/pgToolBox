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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/fyannk/pgToolBox/internal/evidence/provider/cursor"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
	"github.com/fyannk/pgToolBox/internal/evidence/store/storetest"
)

func TestGCSSharedReaderContract(t *testing.T) {
	storetest.ReaderContract(t, func(t *testing.T) store.Reader {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/storage/v1/b/bucket/o") || r.URL.Path == "/b/bucket/o" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"items":[{"name":"root/value","size":"3","updated":"2026-07-27T12:00:00Z","etag":"etag"}]}`))
				return
			}
			if r.Method == http.MethodGet && r.URL.Query().Get("alt") != "media" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"name":"root/value","size":"3","updated":"2026-07-27T12:00:00Z","etag":"etag"}`))
				return
			}
			_, _ = w.Write([]byte("abc"))
		}))
		t.Cleanup(server.Close)
		client, err := storage.NewClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		codec, err := cursor.New()
		if err != nil {
			t.Fatal(err)
		}
		return &Store{client: client, bucket: "bucket", prefix: "root/", timeout: time.Second, cursor: codec}
	})
}
