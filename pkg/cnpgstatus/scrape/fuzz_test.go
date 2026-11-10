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

package scrape

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/metrics"
)

// FuzzParseCatalog feeds arbitrary expositions to the catalog parser and
// asserts what the sweep relies on: it never fails on a body of bounded
// lines, it records only the catalog's own keys, never a NaN or an
// infinity, the same body always folds to the same values, and comments
// and blank lines change nothing.
func FuzzParseCatalog(f *testing.F) {
	for _, seed := range []string{
		"cnpg_backends_total{datname=\"app\",state=\"active\"} 3\ncnpg_backends_total{datname=\"app\",state=\"idle\"} 4\n",
		"cnpg_pg_replication_lag 1.5 1700000000000\n",
		"cnpg_collector_pg_wal{value=\"count\"} 12\ncnpg_collector_pg_wal{value=\"size\"} 16777216\n",
		"cnpg_backends_total NaN\ncnpg_backends_total +Inf\n",
		"cnpg_backends_total{datname=\"a\\\"b\"} 1\n# HELP x\n\n",
		"{} 1\n}{ 2\nname{ 3\n",
	} {
		f.Add(seed)
	}
	keys := func(catalog metrics.Catalog) (series, instants map[string]bool) {
		series, instants = map[string]bool{}, map[string]bool{}
		for _, def := range catalog.Series {
			series[def.Key] = true
		}
		for _, def := range catalog.Instants {
			instants[def.Key] = true
		}
		return series, instants
	}
	seriesKeys, instantKeys := keys(metrics.Instance)
	f.Fuzz(func(t *testing.T, body string) {
		series, instants, err := ParseCatalog(metrics.Instance, strings.NewReader(body))
		if err != nil {
			if len(body) < 64*1024 {
				t.Fatalf("a body of %d bytes failed: %v", len(body), err)
			}
			return
		}
		for name, values := range map[string]map[string]float64{"series": series, "instants": instants} {
			allowed := seriesKeys
			if name == "instants" {
				allowed = instantKeys
			}
			for key, v := range values {
				if !allowed[key] {
					t.Fatalf("%s key %q is not in the catalog", name, key)
				}
				if math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("%s %q recorded %v", name, key, v)
				}
			}
		}
		again, againInstants, err := ParseCatalog(metrics.Instance, strings.NewReader(body))
		if err != nil || !reflect.DeepEqual(series, again) || !reflect.DeepEqual(instants, againInstants) {
			t.Fatalf("the same body folded differently: %v %v / %v %v (%v)", series, instants, again, againInstants, err)
		}
		padded, paddedInstants, err := ParseCatalog(metrics.Instance, strings.NewReader("# HELP padding\n\n"+body+"\n# EOF\n"))
		if err != nil || !reflect.DeepEqual(series, padded) || !reflect.DeepEqual(instants, paddedInstants) {
			t.Fatalf("comments changed the reading: %v %v / %v %v (%v)", series, instants, padded, paddedInstants, err)
		}
	})
}

// FuzzParseLabels asserts the label-block parser's bounds and that what
// it accepts is what it means: at most maxLabels pairs, no value over
// maxValue bytes, only valid label names, and a block rendered back from the result reads as
// the same map.
func FuzzParseLabels(f *testing.F) {
	for _, seed := range []string{
		`datname="app",state="active"`,
		`a="x\"y",b="line\nbreak",c="back\\slash"`,
		`value="volume_size"`,
		`a="unterminated`,
		`noequals`,
		`a=unquoted`,
		` , a="1" ,, b="2"`,
		`="empty name"`,
	} {
		f.Add(seed, 8, 32)
	}
	f.Fuzz(func(t *testing.T, labels string, maxLabels, maxValue int) {
		maxLabels, maxValue = max(0, maxLabels%64), max(0, maxValue%512)
		out, ok := ParseLabels(labels, maxLabels, maxValue)
		if !ok {
			if out != nil {
				t.Fatalf("a refused block returned %v", out)
			}
			return
		}
		if len(out) > maxLabels {
			t.Fatalf("%d labels over a bound of %d", len(out), maxLabels)
		}
		for name, value := range out {
			if !validLabelName(name) {
				t.Fatalf("label name %q is not a Prometheus label name", name)
			}
			if len(value) > maxValue {
				t.Fatalf("label %q: %d bytes over a bound of %d", name, len(value), maxValue)
			}
		}
		rendered := renderLabels(out)
		back, ok := ParseLabels(rendered, maxLabels, maxValue)
		if !ok || !reflect.DeepEqual(out, back) {
			t.Fatalf("ParseLabels(%q) = %v, but its rendering %q reads as %v (%v)", labels, out, rendered, back, ok)
		}
		for name, value := range back {
			if got, found := lookupLabel(rendered, name); !found || got != value {
				t.Fatalf("lookupLabel(%q, %q) = %q, %v; ParseLabels said %q", rendered, name, got, found, value)
			}
		}
	})
}

// renderLabels writes a label map as an exposition label block, names
// sorted, values escaped the way the parser unescapes them.
func renderLabels(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		for _, c := range []byte(labels[name]) {
			switch c {
			case '\\':
				b.WriteString(`\\`)
			case '"':
				b.WriteString(`\"`)
			case '\n':
				b.WriteString(`\n`)
			default:
				b.WriteByte(c)
			}
		}
		b.WriteByte('"')
	}
	return b.String()
}
