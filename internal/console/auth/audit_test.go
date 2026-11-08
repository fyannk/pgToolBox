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

package auth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogSafe(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"curl/8.5.0":             "curl/8.5.0",
		"a\nb":                   `a\nb`,
		"a\r\nlevel=ERROR msg=x": `a\r\nlevel=ERROR msg=x`,
		"tab\there":              "tab?here",
		"bell\x07nul\x00":        "bell?nul?",
		"\u0085next":             "?next",
		"été":                    "été",
	} {
		if got := LogSafe(in); got != want {
			t.Errorf("LogSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// A client-chosen value in any attribute shape never starts a new line of
// a text-handler audit trail.
func TestRecordEscapesNewlines(t *testing.T) {
	var logs bytes.Buffer
	a := &Auditor{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	a.Record(context.Background(), Client{IP: "192.0.2.1", UserAgent: "ua\naudit=forged"}, AuditLoginFailure,
		"user", "alice\nforged=1",
		"hosts", []string{"a.example", "b\r\nforged=2"},
		"error", errors.New("bad\nforged=3"),
		slog.String("attr", "x\nforged=4"),
		"size", 42,
	)
	out := logs.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("want one line, got %d:\n%s", n, out)
	}
	for _, want := range []string{"size=42", "a.example", `forged=1`, `forged=4`} {
		if !strings.Contains(out, want) {
			t.Errorf("line misses %q: %s", want, out)
		}
	}
}
