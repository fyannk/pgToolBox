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
	"strings"
	"testing"
)

func TestRedactorRemovesKnownAndShapedSecrets(t *testing.T) {
	t.Parallel()
	canary := "known-canary-secret"
	redactor := New([]byte(canary))
	input := "failure " + canary + " Authorization: Bearer another-canary\n" +
		"https://example.test/path?sig=signed-canary&safe=value password=assignment-canary"
	result := redactor.String(input)
	for _, forbidden := range []string{canary, "another-canary", "signed-canary", "assignment-canary"} {
		if strings.Contains(result, forbidden) {
			t.Fatalf("result contains %q: %s", forbidden, result)
		}
	}
	if count := strings.Count(result, replacement); count < 4 {
		t.Fatalf("redaction count = %d in %q", count, result)
	}
}

func TestRedactorLearnsRotatedValuesConcurrently(t *testing.T) {
	t.Parallel()
	redactor := New([]byte("old-secret-canary"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			_ = redactor.String("probe old-secret-canary")
		}
	}()
	redactor.Add([]byte("new-secret-canary"), nil, []byte("old-secret-canary"))
	<-done
	got := redactor.String("old-secret-canary then new-secret-canary")
	if strings.Contains(got, "canary") {
		t.Fatalf("rotated or previous value not redacted: %q", got)
	}
	for index := range MaxKnownValues + 5 {
		redactor.Add([]byte(strings.Repeat("x", 20) + string(rune('a'+index%26)) + strings.Repeat("y", index)))
	}
	if count := len(redactor.values); count != MaxKnownValues {
		t.Fatalf("redactor retains %d values; want bound %d", count, MaxKnownValues)
	}
	var nilRedactor *Redactor
	nilRedactor.Add([]byte("ignored"))
	if nilRedactor.String("password=hunter2") != "password=[REDACTED]" {
		t.Fatal("nil redactor must still apply the generic patterns")
	}
}
