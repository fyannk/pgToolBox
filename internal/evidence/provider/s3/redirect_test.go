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

package s3

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// TestS3FollowsNoRedirect: an endpoint that
// redirects a read elsewhere must not get the SSE-C key, the signature or
// anything else sent there, with the endpoint CA option or without it
// (the SDK's own client follows 307 and 308 to any host).
func TestS3FollowsNoRedirect(t *testing.T) {
	t.Parallel()
	var stolen, asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { stolen.Add(1) }))
	t.Cleanup(elsewhere.Close)
	redirecting := func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.Redirect(w, r, elsewhere.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(redirecting))
	t.Cleanup(tlsServer.Close)
	plainServer := httptest.NewServer(http.HandlerFunc(redirecting))
	t.Cleanup(plainServer.Close)
	key := bytes.Repeat([]byte{0x5a}, sseCustomerKeyBytes)
	for name, server := range map[string]*httptest.Server{"with an endpoint CA": tlsServer, "with the default client": plainServer} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			endpoint, _ := url.Parse(server.URL)
			options := Options{
				Bucket: "synthetic-bucket", Prefix: "repository", Region: "us-east-1", RequestTimeout: 5 * time.Second, Endpoint: endpoint,
				AccessKeyID: []byte("synthetic-access"), SecretAccessKey: []byte("synthetic-secret-canary"), SSECustomerKey: key,
			}
			if server == tlsServer {
				options.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			}
			adapter, err := New(context.Background(), options)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			_, err = adapter.Open(ctx, store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64})
			if !refusedRedirect(err) {
				t.Errorf("Open = %v, want the redirect refused", err)
			}
			if _, err := adapter.Stat(ctx, store.StatRequest{Key: "orders/base/backup.info"}); !refusedRedirect(err) {
				t.Errorf("Stat = %v, want the redirect refused", err)
			}
			if _, err := adapter.List(ctx, store.ListRequest{Prefix: "orders/", Limit: 10}); !refusedRedirect(err) {
				t.Errorf("List = %v, want the redirect refused", err)
			}
		})
	}
	t.Cleanup(func() {
		if stolen.Load() != 0 {
			t.Errorf("the redirect target was reached %d times", stolen.Load())
		}
		// Three operations in each of two cells, each asked once: a
		// refused redirect is not retried.
		if asked.Load() != 6 {
			t.Errorf("the endpoint was asked %d times, want 6", asked.Load())
		}
	})
}

// refusedRedirect: the adapter's error says the redirect was refused, as a
// configuration problem, with no URL in its cause.
func refusedRedirect(err error) bool {
	var adapterError *Error
	return errors.As(err, &adapterError) && adapterError.Category() == fault.InvalidConfig && adapterError.RootCause() == nofollow.ErrRedirect.Error()
}
