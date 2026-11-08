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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// fleetRows are n list rows over 16 namespaces, alternating the healthy
// and the failing-over fixture, each with its own name and labels: the
// shape of the 2026-11-03 load test (1,600 Clusters).
func fleetRows(n int) viewmodel.ClusterList {
	list := fixtures.ClusterList()
	list.Namespaces = nil
	for i := range 16 {
		list.Namespaces = append(list.Namespaces, viewmodel.NamespaceState{
			Name: fmt.Sprintf("team-%02d", i), Sources: []viewmodel.SourceState{{Source: "clusters", State: viewmodel.SourceLive}},
		})
	}
	list.Items = make([]viewmodel.ClusterRow, 0, n)
	for i := range n {
		row := fixtures.HealthyRow()
		if i%2 == 1 {
			row = fixtures.FailingOverRow()
		}
		row.Ref.Namespace = fmt.Sprintf("team-%02d", i%16)
		row.Ref.Name = fmt.Sprintf("pg-%04d", i)
		row.Ref.UID = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		row.Labels = map[string]string{"app": row.Ref.Name, "tier": "gold", "cnpg.io/cluster": row.Ref.Name}
		list.Items = append(list.Items, row)
	}
	return list
}

// fleetSource serves one fixed list.
type fleetSource struct {
	FixtureSource
	list viewmodel.ClusterList
}

func (s fleetSource) ClusterList(context.Context) (viewmodel.ClusterList, error) {
	l := s.list
	l.Items = append([]viewmodel.ClusterRow(nil), s.list.Items...)
	return l, nil
}

// discardResponse is a connection that counts what it is sent: the
// benchmark measures the handler, not a recorder's buffer growth.
type discardResponse struct {
	header http.Header
	code   int
	n      int
}

func (d *discardResponse) Header() http.Header { return d.header }
func (d *discardResponse) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}

func (d *discardResponse) Write(p []byte) (int, error) {
	if d.code == 0 {
		d.code = http.StatusOK
	}
	d.n += len(p)
	return len(p), nil
}

// BenchmarkClusterListHandler is GET /clusters at 1,600 rows, everything
// visible: authorization filtering plus the JSON encoding the 2026-11-03
// profile put at 29 % of the console's CPU. "memoized" is the read
// model's steady state (rows carry their encoding, status.rowMemo);
// "unmemoized" is a list whose every row is encoded on the request.
func BenchmarkClusterListHandler(b *testing.B) {
	for name, list := range map[string]viewmodel.ClusterList{
		"unmemoized": fleetRows(1600),
		"memoized":   sealed(fleetRows(1600)),
	} {
		b.Run(name, func(b *testing.B) {
			h, _ := New(Deps{Source: fleetSource{list: list}, Authz: FixtureAuthorizer{}})
			ctx := auth.WithPrincipal(context.Background(), FixturePrincipal())
			b.ReportAllocs()
			var size int
			for b.Loop() {
				r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/clusters", nil)
				w := &discardResponse{header: http.Header{}}
				h.ServeHTTP(w, r)
				if w.code != http.StatusOK {
					b.Fatalf("GET /clusters = %d", w.code)
				}
				size = w.n
			}
			b.ReportMetric(float64(size), "bytes/response")
		})
	}
}

// BenchmarkClusterListFormat is the encoding alone, to io.Discard: the
// default format against the list writer, on the same 1,600 rows.
func BenchmarkClusterListFormat(b *testing.B) {
	plain := ClusterListView{ClusterList: fleetRows(1600)}
	memo := ClusterListView{ClusterList: sealed(fleetRows(1600))}
	for _, bc := range []struct {
		name string
		f    huma.Format
		v    ClusterListView
	}{
		{"default", huma.DefaultJSONFormat, plain},
		{"writer-unmemoized", jsonFormat, plain},
		{"writer-memoized", jsonFormat, memo},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if err := bc.f.Marshal(io.Discard, bc.v); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
