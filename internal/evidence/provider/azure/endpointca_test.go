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

package azure

import (
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// TestAzureContractTrustsTheEndpointCA: an Azurite-style endpoint over TLS
// with a private CA (barman-cloud's endpointCA for Azure) is read through
// the real Blob client with CABundle, and refused without it.
func TestAzureContractTrustsTheEndpointCA(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/devstoreaccount1/backups") {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("comp") {
		case "list":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<?xml version="1.0"?><EnumerationResults><Blobs><Blob><Name>team-a/orders/base/backup.info</Name><Properties><Content-Length>5</Content-Length></Properties></Blob></Blobs><NextMarker/></EnumerationResults>`)
		case "properties":
			w.Header().Set("Content-Length", "5")
		default:
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "5")
				return
			}
			_, _ = io.WriteString(w, "plain")
		}
	}))
	t.Cleanup(server.Close)
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	open := func(t *testing.T, caBundle []byte) *Store {
		t.Helper()
		adapter, err := New(context.Background(), Options{
			Container: "backups", Prefix: "team-a", Account: "devstoreaccount1", Endpoint: server.URL,
			SASToken: []byte("sv=2024-01-01&sig=synthetic-canary"), CABundle: caBundle, RequestTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
	ctx := context.Background()
	adapter := open(t, bundle)
	page, err := adapter.List(ctx, store.ListRequest{Prefix: "orders/", Limit: 10})
	if err != nil || len(page.Objects) != 1 || page.Objects[0].Key != "orders/base/backup.info" {
		t.Fatalf("List = %#v, %v", page, err)
	}
	if object, err := adapter.Stat(ctx, store.StatRequest{Key: "orders/base/backup.info"}); err != nil || object.Size != 5 {
		t.Fatalf("Stat = %#v, %v", object, err)
	}
	body, err := adapter.Open(ctx, store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(content) != "plain" {
		t.Fatalf("Open read %q, %v", content, err)
	}

	// Without the CA the system roots do not verify the endpoint: no
	// request completes.
	before := requests.Load()
	if _, err := open(t, nil).List(ctx, store.ListRequest{Prefix: "orders/", Limit: 10}); err == nil {
		t.Fatal("an endpoint signed by an untrusted CA was read")
	}
	if requests.Load() != before {
		t.Fatal("a request reached the handler without a verified TLS handshake")
	}
}

func TestAzureRejectsAnInvalidCABundle(t *testing.T) {
	t.Parallel()
	for _, bundle := range [][]byte{[]byte("synthetic-canary"), make([]byte, 1024*1024+1)} {
		_, err := New(context.Background(), Options{
			Container: "backups", Account: "orders", SASToken: []byte("sig=x"), CABundle: bundle, RequestTimeout: time.Second,
		})
		if fault.Categorize(err) != fault.InvalidConfig || (err != nil && strings.Contains(err.Error(), "canary")) {
			t.Fatalf("New() = %v", err)
		}
	}
}
