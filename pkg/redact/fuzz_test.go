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

package redact

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// FuzzSafe drives the redaction boundary with arbitrary cause text.
// Invariant 6 is that errors carry a category and never a cause: a
// request URL, a header value, or an injected token may sit anywhere
// inside the error chain and must not reach the output. The existing
// table tests assert this for one hand-written hostile cause; this
// asserts it for any cause at all, at any wrapping depth.
func FuzzSafe(f *testing.F) {
	f.Add("GET https://10.0.0.1/api: unauthorized: sekret-canary", 0)
	f.Add("", 3)
	f.Add("Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature", 1)
	f.Add("token=abc&url=https://host/path?sig=deadbeef", 5)
	f.Add("forbidden", 2)

	categories := map[string]bool{
		string(CategoryCanceled):    true,
		string(CategoryTimeout):     true,
		string(CategoryForbidden):   true,
		string(CategoryNotFound):    true,
		string(CategoryUnavailable): true,
		string(CategoryInternal):    true,
	}

	f.Fuzz(func(t *testing.T, cause string, depth int) {
		// Bounded so the fuzzer spends its time on the cause text rather
		// than on building a very deep chain.
		depth = ((depth % 8) + 8) % 8

		err := error(errors.New(cause))
		for i := 0; i < depth; i++ {
			err = fmt.Errorf("layer %d: %w", i, err)
		}

		safe := Safe(err)

		// The output is a category, never prose. Anything else means a
		// cause reached a caller that renders it.
		if !categories[safe] {
			t.Fatalf("Safe returned %q, which is not one of the closed category set", safe)
		}

		// The cause text itself must not survive. A cause that is part of
		// the category vocabulary is exempt: "forbidden" appearing in the
		// output of an error categorized forbidden is the contract
		// working, not a leak, and the set-membership check above is what
		// actually proves nothing else got through.
		if cause != "" && !partOfVocabulary(cause) && strings.Contains(safe, cause) {
			t.Fatalf("Safe leaked cause text %q into %q", cause, safe)
		}

		// The same holds for a categorized error, which is the path a
		// handler actually takes.
		wrapped := NewError("fuzz op", CategoryForbidden, err)
		if got := Safe(wrapped); got != string(CategoryForbidden) {
			t.Fatalf("Safe on a categorized error = %q, want %q", got, CategoryForbidden)
		}
		if msg := wrapped.Error(); msg != "fuzz op: forbidden" {
			t.Fatalf("Error() = %q, want the operation and category only", msg)
		}
		if cause != "" && !partOfVocabulary(cause) && strings.Contains(wrapped.Error(), cause) {
			t.Fatalf("Error() leaked cause text %q", cause)
		}

		if again := Safe(err); again != safe {
			t.Fatalf("Safe is not deterministic: %q then %q", safe, again)
		}
	})
}

// partOfVocabulary reports whether text is a fragment of the strings the
// redaction boundary is allowed to emit — the category names and the
// operation label. A cause drawn from that vocabulary will appear in the
// output no matter how well redaction works, so a substring test cannot
// tell it apart from a leak.
func partOfVocabulary(text string) bool {
	for _, allowed := range []string{
		"fuzz op: " + string(CategoryForbidden),
		string(CategoryCanceled), string(CategoryTimeout),
		string(CategoryForbidden), string(CategoryNotFound),
		string(CategoryUnavailable), string(CategoryInternal),
	} {
		if strings.Contains(allowed, text) {
			return true
		}
	}
	return false
}
