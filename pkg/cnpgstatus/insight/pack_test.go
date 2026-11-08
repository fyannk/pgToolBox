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

package insight

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// packFile is the pack's single source: the pgtools chart renders it,
// and deploy/insight-pack is generated from it (make insight-pack).
const packFile = "../../../charts/pgtools/files/insight-pack/queries.yaml"

type packQuery struct {
	Query           string                       `json:"query"`
	PredicateQuery  string                       `json:"predicate_query"`
	Primary         bool                         `json:"primary"`
	RunOnServer     string                       `json:"runonserver"`
	TargetDatabases []string                     `json:"target_databases"`
	Metrics         []map[string]packMetricUsage `json:"metrics"`
}

type packMetricUsage struct {
	Usage       string `json:"usage"`
	Description string `json:"description"`
}

func loadPack(t *testing.T) (map[string]packQuery, string) {
	t.Helper()
	raw, err := os.ReadFile(packFile)
	if err != nil {
		t.Fatal(err)
	}
	var pack map[string]packQuery
	if err := yaml.UnmarshalStrict(raw, &pack); err != nil {
		t.Fatal(err)
	}
	return pack, string(raw)
}

func TestPackVersionAndQueries(t *testing.T) {
	pack, raw := loadPack(t)
	if !strings.HasPrefix(raw, "# pgToolBox insight pack, version "+strconv.Itoa(PackVersion)+".") {
		t.Errorf("the pack's header must name version %d", PackVersion)
	}
	if got := pack["pgtoolbox_pack"].Query; !strings.Contains(got, "'"+strconv.Itoa(PackVersion)+"' AS version") {
		t.Errorf("pgtoolbox_pack reports another version: %s", got)
	}
	names := make([]string, 0, len(pack))
	for name := range pack {
		names = append(names, name)
	}
	sort.Strings(names)
	want := append([]string(nil), Queries...)
	sort.Strings(want)
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("pack queries %v, Queries %v", names, want)
	}
}

// TestPackQueriesAreBounded holds the rules the documentation promises
// for every query.
func TestPackQueriesAreBounded(t *testing.T) {
	pack, _ := loadPack(t)
	unqualified := regexp.MustCompile(`(?i)(^|[^._a-z])(age|mxid_age|count|max|min|sum|now|current_setting|set_config|unnest|string_agg|pg_blocking_pids|pg_get_expr|pg_stat_get_[a-z_]+|first_value|current_database|clock_timestamp|md5|regexp_replace|to_jsonb|concat_ws|array_length|bool_or|bool_and|pg_backend_pid|pg_options_to_table|string_to_array|replace)\(`)
	for name, q := range pack {
		t.Run(name, func(t *testing.T) {
			if name != "pgtoolbox_pack" {
				for _, setting := range []string{"'statement_timeout', '1000', true", "'lock_timeout', '100', true", "'max_parallel_workers_per_gather', '0', true"} {
					if !strings.Contains(q.PredicateQuery, setting) {
						t.Errorf("predicate does not set %s", setting)
					}
				}
				if q.RunOnServer != ">=14.0.0" {
					t.Errorf("runonserver = %q", q.RunOnServer)
				}
			}
			single := map[string]bool{"pgtoolbox_pack": true, "pgtoolbox_xid": true, "pgtoolbox_backends": true, "pgtoolbox_statements_info": true, "pgtoolbox_xmin": true}
			if !single[name] && !strings.Contains(q.Query, "LIMIT") {
				t.Error("a row set without LIMIT")
			}
			for _, sql := range []string{q.Query, q.PredicateQuery} {
				if m := unqualified.FindString(sql); m != "" {
					t.Errorf("function not schema-qualified: %q", m)
				}
			}
			for _, m := range q.Metrics {
				for column, usage := range m {
					if column == "query" || strings.Contains(column, "text") {
						t.Errorf("column %s: statement text never leaves the database", column)
					}
					switch usage.Usage {
					case "LABEL", "GAUGE", "COUNTER":
					default:
						t.Errorf("column %s: usage %s", column, usage.Usage)
					}
					if usage.Description == "" {
						t.Errorf("column %s has no description", column)
					}
				}
			}
		})
	}
}

// TestPackOperatorsParenthesized guards against the precedence trap of
// OPERATOR(): whatever the operator, OPERATOR(schema.op) takes the "any
// other operator" precedence, left-associative, so
// a OPERATOR(pg_catalog.+) b OPERATOR(pg_catalog.*) c reads (a + b) * c.
// Two OPERATOR() calls on one parenthesis level must be separated by
// AND, OR, a comma or a clause keyword.
func TestPackOperatorsParenthesized(t *testing.T) {
	// The expression the first pack shipped with: it must be caught.
	buggy := "SELECT COALESCE(o.t, s.t)\n  OPERATOR(pg_catalog.+) COALESCE(o.s, s.s)\n  OPERATOR(pg_catalog.*) GREATEST(c.reltuples, 0) AS x"
	if chained := operatorChains(buggy); len(chained) == 0 {
		t.Fatal("the check misses the original threshold expression")
	}
	fixed := "SELECT (COALESCE(o.t, s.t) OPERATOR(pg_catalog.+) (COALESCE(o.s, s.s) OPERATOR(pg_catalog.*) c.reltuples)) AS x"
	if chained := operatorChains(fixed); len(chained) != 0 {
		t.Fatalf("the check flags a parenthesized expression: %v", chained)
	}
	pack, _ := loadPack(t)
	for name, q := range pack {
		for _, sql := range []string{q.Query, q.PredicateQuery} {
			for _, c := range operatorChains(sql) {
				t.Errorf("%s: OPERATOR() chained without parentheses: %s", name, c)
			}
		}
	}
}

var (
	sqlComment   = regexp.MustCompile(`--[^\n]*`)
	sqlString    = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlSeparator = regexp.MustCompile(`(?i)^(AND|OR|NOT|WHEN|THEN|ELSE|END|CASE|ON|WHERE|SELECT|FROM|AS|BY|IS|IN|ANY|ALL|FILTER|OVER|UNION|JOIN|LIMIT|DESC|ASC|NULLS)$`)
	sqlWord      = regexp.MustCompile(`^[A-Za-z_]+`)
)

// operatorChains returns the fragments where two OPERATOR() calls share a
// parenthesis level with no separator between them.
func operatorChains(sql string) []string {
	sql = sqlString.ReplaceAllString(sqlComment.ReplaceAllString(sql, ""), "''")
	type level struct{ lastOp int }
	stack := []level{{lastOp: -1}}
	var out []string
	for i := 0; i < len(sql); i++ {
		top := &stack[len(stack)-1]
		switch c := sql[i]; {
		case strings.HasPrefix(sql[i:], "OPERATOR("):
			if top.lastOp >= 0 {
				out = append(out, strings.Join(strings.Fields(sql[top.lastOp:min(len(sql), i+24)]), " "))
			}
			top.lastOp = i
			// Skip the operator's own parentheses.
			if j := strings.IndexByte(sql[i:], ')'); j > 0 {
				i += j
			}
		case c == '(':
			stack = append(stack, level{lastOp: -1})
		case c == ')':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case c == ',' || c == ';':
			top.lastOp = -1
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_':
			if i > 0 && (sql[i-1] == '_' || sql[i-1] == '.' || (sql[i-1]|0x20 >= 'a' && sql[i-1]|0x20 <= 'z')) {
				continue
			}
			w := sqlWord.FindString(sql[i:])
			if sqlSeparator.MatchString(w) {
				top.lastOp = -1
			}
			i += len(w) - 1
		}
	}
	return out
}

// TestParserReadsEveryColumn emits one line per declared value column
// and checks the parser keeps it, so a column added to the pack without
// a parser case fails here.
func TestParserReadsEveryColumn(t *testing.T) {
	pack, _ := loadPack(t)
	labelValue := map[string]string{"pid": "12", "queryid": "42", "version": "1", "name": "max_connections"}
	for name, q := range pack {
		var labels []string
		for _, m := range q.Metrics {
			for column, usage := range m {
				if usage.Usage == "LABEL" {
					v, ok := labelValue[column]
					if !ok {
						v = "x"
					}
					labels = append(labels, fmt.Sprintf("%s=%q", column, v))
				}
			}
		}
		block := ""
		if len(labels) > 0 {
			block = "{" + strings.Join(labels, ",") + "}"
		}
		for _, m := range q.Metrics {
			for column, usage := range m {
				if usage.Usage == "LABEL" {
					continue
				}
				line := "cnpg_" + name + "_" + column + block + " 1"
				bogus := "cnpg_" + name + "_not_a_column" + block + " 1"
				if reflect.DeepEqual(mustParse(t, line), mustParse(t, bogus)) {
					t.Errorf("%s: the parser ignores %s", name, line)
				}
			}
		}
	}
}

func mustParse(t *testing.T, body string) Reading {
	t.Helper()
	r, err := Parse(strings.NewReader(body + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}
