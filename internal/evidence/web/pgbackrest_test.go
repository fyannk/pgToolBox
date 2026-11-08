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

package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest"
	"github.com/fyannk/pgToolBox/internal/evidence/formats/pgbackrest/pgbackrestfixture"
	"github.com/fyannk/pgToolBox/internal/evidence/inventory"
	"github.com/fyannk/pgToolBox/internal/evidence/readiness"
)

func newPGBackRestHandler(t *testing.T, repository *pgbackrestfixture.Repository, passphrase []byte) http.Handler {
	t.Helper()
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	format := pgbackrest.New()
	cache, err := inventory.NewCache(inventory.Initial(format.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := inventory.NewScanner(inventory.ScannerOptions{
		Store: repository.Reader(t), Format: format, Cache: cache,
		Readiness: readiness.New(true, time.Hour, func() time.Time { return now }), RefreshInterval: time.Hour,
		MaxObjects: 100_000, PageSize: 50, RecentLimit: 5, Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler),
		PGBackRest: &inventory.PGBackRestOptions{Passphrase: passphrase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scanner.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	handler, err := New(Options{
		Logger: slog.New(slog.DiscardHandler), Provider: "s3", Format: format.Descriptor(),
		Inventory: cache.Load, Readiness: func() readiness.Result { return readiness.Result{Ready: true} },
		RequestID: func() string { return "test-request-id" }, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler.Routes()
}

func get(t *testing.T, handler http.Handler, target string) (int, string) {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

func TestHandlerRendersPGBackRestStanzas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		variant    string
		passphrase []byte
		want       []string
	}{
		{
			name: "plain", variant: "plain",
			want: []string{
				"<h2>pgBackRest stanzas</h2>", "<h3>Stanza demo</h3>", "20261107-081046F_20261107-081101D",
				"fixture-repo1-diff", "every backup is usable", "<h4>Recovery window</h4>", "000000020000000000000014",
				`<a href="/wals?server=demo%2f17-1">demo/17-1</a>`, "the repository is not encrypted",
			},
		},
		{
			name: "encrypted", variant: "encrypted", passphrase: []byte(pgbackrestfixture.Passphrase),
			want: []string{"encrypted info files and manifests opened", "no: off-target-timeline"},
		},
		{
			name: "encrypted without a passphrase", variant: "encrypted",
			want: []string{"the repository is encrypted and no passphrase is configured"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			status, body := get(t, newPGBackRestHandler(t, pgbackrestfixture.Load(t, test.variant), test.passphrase), "/")
			if status != http.StatusOK {
				t.Fatalf("status %d", status)
			}
			for _, want := range test.want {
				if !strings.Contains(body, want) {
					t.Errorf("the page lacks %q", want)
				}
			}
			for _, absent := range []string{"Barman Cloud backups", "Barman WAL continuity", pgbackrestfixture.Passphrase} {
				if strings.Contains(body, absent) {
					t.Errorf("the page shows %q", absent)
				}
			}
		})
	}
}

func TestHandlerBrowsesPGBackRestWAL(t *testing.T) {
	t.Parallel()
	handler := newPGBackRestHandler(t, pgbackrestfixture.Load(t, "plain"), nil)
	status, body := get(t, handler, "/wals?server="+url.QueryEscape("demo/17-1")+"&timeline=2")
	if status != http.StatusOK || !strings.Contains(body, "<h1>pgBackRest WAL evidence</h1>") || !strings.Contains(body, "00000002000000000000000E") || strings.Contains(body, "000000010000000000000001") {
		t.Fatalf("status %d body %s", status, body)
	}
	if status, _ := get(t, handler, "/wals?server=other/17-1"); status != http.StatusBadRequest {
		t.Fatalf("an unknown archive answered %d", status)
	}
}
