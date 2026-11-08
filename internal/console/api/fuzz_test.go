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
	"net/url"
	"regexp"
	"strings"
	"testing"
)

var (
	nsPattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// FuzzClusterPath: whatever a client puts in the path, the read model is
// consulted only with a namespace and a name that are valid Kubernetes
// names, and only after the authorization check passed.
func FuzzClusterPath(f *testing.F) {
	for _, s := range [][2]string{
		{"team-a", "billing"},
		{"Team_A", "x"},
		{"..", "billing"},
		{"team-a", "../../etc"},
		{"team-a", strings.Repeat("a", 300)},
		{"", ""},
		{"a%2fb", "c"},
		{"team-a", "a.b"},
	} {
		f.Add(s[0], s[1])
	}
	src := &spySource{}
	h, _ := New(Deps{Source: src, Authz: FixtureAuthorizer{}})
	f.Fuzz(func(t *testing.T, ns, name string) {
		src.mu.Lock()
		src.calls = nil
		src.mu.Unlock()
		path := "/api/v1/namespaces/" + url.PathEscape(ns) + "/clusters/" + url.PathEscape(name) + "/instances"
		w := call(t, h, FixturePrincipal(), path)
		if len(src.calls) == 0 {
			return
		}
		if len(ns) > 63 || len(name) > 253 || !nsPattern.MatchString(ns) || !namePattern.MatchString(name) {
			t.Fatalf("read model consulted for %q/%q (status %d)", ns, name, w.Code)
		}
	})
}
