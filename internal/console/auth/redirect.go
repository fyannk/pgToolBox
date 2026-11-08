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
	"net/url"
	"strings"
)

// SafeRedirect validates a post-login redirect target. Only a relative path is honoured; everything
// else falls back to "/".
//
// The string inspected here is not the string the browser resolves:
// browsers strip TAB, CR and LF from a URL before parsing it, so
// "/\t/evil.example" passes a "starts with //" test and resolves as
// "//evil.example". Rejecting every control character keeps the check and
// the parse about the same string; a backslash is refused because some
// browsers treat it as a separator.
func SafeRedirect(p string) string {
	if len(p) == 0 || p[0] != '/' {
		return "/"
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') {
		return "/"
	}
	if strings.ContainsFunc(p, func(r rune) bool {
		return r == '\\' || r < 0x20 || r == 0x7f
	}) {
		return "/"
	}
	u, err := url.Parse(p)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "/"
	}
	// The sign-in endpoints are never a destination: landing back on a
	// callback replays a spent code, and on /logout it would sign out.
	if u.Path == "/logout" || strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	return p
}
