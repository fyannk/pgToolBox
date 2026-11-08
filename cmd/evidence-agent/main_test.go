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

package main

import (
	"strings"
	"testing"
)

func TestReleaseVersionAcceptsOnlyBoundedReleaseIdentifiers(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"v0.2.0":                   "v0.2.0",
		"v0.2.0-rc.1":              "v0.2.0-rc.1",
		"v0.1.2-12-g0da6f5d-dirty": "v0.1.2-12-g0da6f5d-dirty",
		"0da6f5d":                  "0da6f5d",
		"1.0.0+build_7":            "1.0.0+build_7",
		"development":              "development",
		"":                         "development",
		"-v1":                      "development",
		"v1 with space":            "development",
		"v1\nforged-log-line":      "development",
		strings.Repeat("v", 65):    "development",
		"v1/../../etc":             "development",
		"v1é":                      "development",
	} {
		if got := releaseVersion(raw); got != want {
			t.Fatalf("releaseVersion(%q) = %q; want %q", raw, got, want)
		}
	}
}
