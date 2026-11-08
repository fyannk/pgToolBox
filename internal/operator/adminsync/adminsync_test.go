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

package adminsync

import "testing"

func TestPgpassLine(t *testing.T) {
	cases := []struct {
		name                     string
		host, username, password string
		want                     string
	}{
		{"escapes colon and backslash", "h:ost", `us\er`, `p:a\ss`, `h\:ost:5432:*:us\\er:p\:a\\ss`},
		// The database is the one wildcard; a role literally named "*"
		// must not become a second one.
		{"a role named * is literal", "h", "*", "pw", `h:5432:*:\*:pw`},
		{"a host named * is literal", "*", "app", "pw", `\*:5432:*:app:pw`},
		{"an asterisk inside a name is literal", "h", "a*b", "*", `h:5432:*:a*b:*`},
		// A line starting with '#' is a comment to libpq.
		{"a leading # is escaped", "#h", "#u", "#p", `\#h:5432:*:\#u:#p`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgpassLine(tc.host, 5432, tc.username, tc.password); got != tc.want {
				t.Errorf("pgpassLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The server JSON reaches pgAdmin's settings database. It must carry no
// password and no passfile parameter — pgAdmin resolves passfile inside the
// account's browsable storage directory — and names the service section.
