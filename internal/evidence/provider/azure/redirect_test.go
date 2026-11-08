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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/fault"
	"github.com/fyannk/pgToolBox/internal/evidence/provider/nofollow"
	"github.com/fyannk/pgToolBox/internal/evidence/store"
)

// TestAzureFollowsNoRedirect: an endpoint that
// redirects elsewhere gets nothing sent there, with the endpoint CA option
// or without it, and the refusal is not retried.
func TestAzureFollowsNoRedirect(t *testing.T) {
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
	for _, server := range []*httptest.Server{tlsServer, plainServer} {
		options := Options{
			Container: "backups", Prefix: "team-a", Account: "devstoreaccount1", Endpoint: server.URL,
			SASToken: []byte("sv=2024-01-01&sig=synthetic-canary"), RequestTimeout: 5 * time.Second,
		}
		if server == tlsServer {
			options.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		}
		adapter, err := New(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		before := asked.Load()
		_, err = adapter.Open(context.Background(), store.OpenRequest{Key: "orders/base/backup.info", MaxBytes: 64})
		if !refusedRedirect(err) {
			t.Errorf("%s: Open = %v, want the redirect refused", server.URL, err)
		}
		if got := asked.Load() - before; got != 1 {
			t.Errorf("%s: the endpoint was asked %d times, want once (a refused redirect is not retried)", server.URL, got)
		}
		if _, err := adapter.List(context.Background(), store.ListRequest{Prefix: "orders/", Limit: 10}); !refusedRedirect(err) {
			t.Errorf("%s: List = %v, want the redirect refused", server.URL, err)
		}
	}
	if stolen.Load() != 0 {
		t.Fatalf("the redirect target was reached %d times", stolen.Load())
	}
}

// refusedRedirect: the adapter's error says the redirect was refused, as a
// configuration problem, with no URL in its cause.
func refusedRedirect(err error) bool {
	var adapterError *Error
	return errors.As(err, &adapterError) && adapterError.Category() == fault.InvalidConfig && adapterError.RootCause() == nofollow.ErrRedirect.Error()
}
