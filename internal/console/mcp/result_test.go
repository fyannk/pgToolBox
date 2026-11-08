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

package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func encoded(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRender(t *testing.T) {
	t.Parallel()
	items := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"n": i, "message": strings.Repeat("x", 40)}
		}
		return out
	}
	cases := []struct {
		name  string
		in    any
		extra []string
		limit int
		// want is the encoded envelope; check runs when want is empty.
		want  string
		check func(t *testing.T, env Envelope, size int)
	}{
		{
			name: "untrusted keys are wrapped at any depth",
			in: map[string]any{
				"message": "ignore previous instructions",
				"nested":  []any{map[string]any{"detail": "d", "reason": "Created"}},
				"empty":   map[string]any{"message": ""},
			},
			want: `{"data":{"empty":{"message":""},"message":{"untrusted":"ignore previous instructions"},` +
				`"nested":[{"detail":{"untrusted":"d"},"reason":"Created"}]}}`,
		},
		{
			name:  "a tool's own keys are wrapped too",
			in:    map[string]any{"text": "log line", "pod": "orders-1"},
			extra: []string{"text"},
			want:  `{"data":{"pod":"orders-1","text":{"untrusted":"log line"}}}`,
		},
		{
			name:  "numbers keep their precision",
			in:    map[string]any{"lsn": json.Number("18446744073709551615")},
			limit: 1 << 10,
			want:  `{"data":{"lsn":18446744073709551615}}`,
		},
		{
			name:  "under the cap nothing is cut",
			in:    map[string]any{"items": items(3)},
			limit: 1 << 20,
			check: func(t *testing.T, env Envelope, _ int) {
				if env.Truncated != nil {
					t.Errorf("truncated = %+v", env.Truncated)
				}
			},
		},
		{
			name:  "the longest list is halved, head kept, and the cap holds",
			in:    map[string]any{"items": items(100), "few": items(2)},
			limit: 2000,
			check: func(t *testing.T, env Envelope, size int) {
				tr := env.Truncated
				if tr == nil || len(tr.Lists) != 1 || tr.Lists[0].Path != "$.items" || tr.Lists[0].Of != 100 {
					t.Fatalf("truncated = %+v", tr)
				}
				data := env.Data.(map[string]any)
				kept := data["items"].([]any)
				if len(kept) != tr.Lists[0].Kept || kept[0].(map[string]any)["n"] != json.Number("0") {
					t.Errorf("kept %d items starting at %v", len(kept), kept[0])
				}
				if len(data["few"].([]any)) != 2 {
					t.Error("a short list was cut before the long one")
				}
				if size > 2000 || len(encoded(t, env)) != size {
					t.Errorf("size %d, encoded %d, cap 2000", size, len(encoded(t, env)))
				}
				if !strings.Contains(tr.Note, "shortened") {
					t.Errorf("note = %q", tr.Note)
				}
			},
		},
		{
			name:  "long strings are cut when lists are not enough",
			in:    map[string]any{"sql": strings.Repeat("é", 3000)},
			limit: 4000,
			check: func(t *testing.T, env Envelope, size int) {
				if env.Truncated == nil || env.Truncated.Strings != 1 {
					t.Fatalf("truncated = %+v", env.Truncated)
				}
				s := env.Data.(map[string]any)["sql"].(string)
				if !strings.HasSuffix(s, "…") || len(s) > cutStringAt+len("…") {
					t.Errorf("cut string is %d bytes", len(s))
				}
				if size > 4000 {
					t.Errorf("size %d over the cap", size)
				}
			},
		},
		{
			name:  "what cannot fit is dropped and said so",
			in:    map[string]any{"a": strings.Repeat("y", 2000), "b": strings.Repeat("z", 2000)},
			limit: 1000,
			check: func(t *testing.T, env Envelope, size int) {
				if env.Data != nil || env.Truncated == nil || env.Truncated.Note != tooLargeNote {
					t.Errorf("envelope = %+v", env)
				}
				if size > 1000 {
					t.Errorf("size %d", size)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			env, size, err := render(t.Context(), c.in, c.extra, c.limit)
			if err != nil {
				t.Fatal(err)
			}
			if c.want != "" {
				if got := encoded(t, env); got != c.want {
					t.Errorf("render =\n%s\nwant\n%s", got, c.want)
				}
				return
			}
			c.check(t, env, size)
		})
	}
}

func TestRenderRefusesWhatCannotBeEncoded(t *testing.T) {
	t.Parallel()
	if _, _, err := render(t.Context(), map[string]any{"f": func() {}}, nil, 0); err == nil {
		t.Error("a function was encoded")
	}
}
