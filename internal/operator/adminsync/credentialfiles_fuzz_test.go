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

import (
	"strconv"
	"strings"
	"testing"
)

// parseServiceSection reads one section of a service file the way libpq's
// parseServiceFile does: trailing whitespace is stripped from each line,
// then leading whitespace; '#' lines and blank lines are skipped; a line
// "[name]" opens a section; inside it, the key is everything before the
// first '=' and the value everything after it, verbatim.
func parseServiceSection(file, service string) (map[string]string, bool) {
	values := map[string]string{}
	found := false
	for _, line := range strings.Split(file, "\n") {
		line = strings.TrimRight(line, " \t\n\v\f\r")
		line = strings.TrimLeft(line, " \t\n\v\f\r")
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			if found {
				return values, true
			}
			found = strings.HasPrefix(line[1:], service) && len(line) > len(service)+1 && line[len(service)+1] == ']'
			continue
		}
		if !found {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false
		}
		if _, set := values[key]; !set {
			values[key] = value
		}
	}
	return values, found
}

// Whatever representable accepts, the service file hands back to libpq
// exactly; whatever it refuses never reaches the file.
func FuzzServiceFile(f *testing.F) {
	f.Add("c-rw.ns.svc", "app", "app", "p@ss=w0rd", "verify-full")
	f.Add("h", "db", "user", "trailing ", "prefer")
	f.Add("h", "db", "us\ner", "pw", "prefer")
	f.Add("h", " lead", "u", "a#b[c]=d", "")
	f.Fuzz(func(t *testing.T, host, database, username, password, sslmode string) {
		servers := []Server{{
			Name: "role (" + username + ")", Host: host, Port: 5432,
			MaintenanceDB: database, Username: username, Password: password, SSLMode: sslmode,
		}}
		usable, _ := representable(servers)
		file := renderServiceFile(usable)
		if len(usable) == 0 {
			if strings.Contains(file, "password=") {
				t.Fatalf("a refused server reached the file: %q", file)
			}
			return
		}
		values, ok := parseServiceSection(file, serviceNames(usable)[0])
		if !ok {
			t.Fatalf("section not found in %q", file)
		}
		want := map[string]string{"host": host, "port": "5432", "dbname": database, "user": username, "password": password}
		if sslmode != "" {
			want["sslmode"] = sslmode
		}
		if password == "" {
			// No password: the section stays, without the key, so libpq
			// asks for one rather than sending an empty password.
			delete(want, "password")
		}
		for key, value := range want {
			if values[key] != value {
				t.Fatalf("%s = %q after the round trip, want %q (file %q)", key, values[key], value, file)
			}
		}
		if len(values) != len(want) {
			t.Fatalf("section has %v, want %v", values, want)
		}
	})
}

// libpqPassword looks a password up in a pgpass file the way libpq's
// passwordFromFile does (fe-connect.c), as an oracle independent of the
// renderer: a line whose first byte is '#' is a comment; trailing CR and LF
// are stripped and an empty line skipped, nothing else is trimmed; host,
// port, database and username are matched in turn by pwdfMatchesString, the
// first matching line wins, and its remainder is de-escaped up to the first
// unescaped ':'.
func libpqPassword(file, host, port, database, username string) (string, bool) {
	for _, line := range strings.Split(file, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		rest, ok := line, true
		for _, token := range []string{host, port, database, username} {
			if rest, ok = pwdfMatchesString(rest, token); !ok {
				break
			}
		}
		if !ok {
			continue
		}
		var password []byte
		for i := 0; i < len(rest) && rest[i] != ':'; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
			}
			password = append(password, rest[i])
		}
		return string(password), true
	}
	return "", false
}

// pwdfMatchesString is libpq's field matcher, transcribed: a field that is
// exactly "*" matches anything; otherwise a backslash makes the next byte
// literal, and the field must equal token up to an unescaped ':'. It
// returns what follows that ':'.
func pwdfMatchesString(buf, token string) (string, bool) {
	if strings.HasPrefix(buf, "*:") {
		return buf[2:], true
	}
	b, t := 0, 0
	escaped := false
	for b < len(buf) {
		if buf[b] == '\\' && !escaped {
			b++
			escaped = true
			if b == len(buf) {
				return "", false
			}
		}
		if buf[b] == ':' && t == len(token) && !escaped {
			return buf[b+1:], true
		}
		escaped = false
		if t == len(token) || buf[b] != token[t] {
			return "", false
		}
		b++
		t++
	}
	return "", false
}

// The oracles are checked against answers libpq gives, not against the
// renderer: comments, escaping, the wildcard and its escaped form, and
// what each format does with surrounding whitespace.
func TestLibpqPasswordKnownAnswers(t *testing.T) {
	const file = "# a comment:5432:*:u:never\n" +
		" #not a comment:5432:*:u:leading-space-host\r\n" +
		`\#h:5432:*:u:escaped-hash` + "\n" +
		`h\:1:5432:*:u\\v:p\:w\\d` + "\n" +
		`h:5432:*:\*:literal-star` + "\n" +
		"h:5432:*:spaced:trailing  \r\n" +
		"\n" +
		"h:5432:*:*:wildcard\n" +
		"h:5432:*:*:shadowed\n"
	cases := []struct {
		name                           string
		host, port, database, username string
		want                           string
		found                          bool
	}{
		{"a # line is a comment", "# a comment", "5432", "db", "u", "", false},
		{"only the first byte makes a comment", " #not a comment", "5432", "db", "u", "leading-space-host", true},
		{"an escaped # is a host", "#h", "5432", "db", "u", "escaped-hash", true},
		{"backslash and colon de-escape", "h:1", "5432", "db", `u\v`, `p:w\d`, true},
		{"an escaped star is literal", "h", "5432", "db", "*", "literal-star", true},
		{"trailing whitespace is part of the password, CRLF is not", "h", "5432", "db", "spaced", "trailing  ", true},
		{"a bare star matches any user, first line wins", "h", "5432", "db", "someone", "wildcard", true},
		{"the port must match", "h", "5433", "db", "someone", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := libpqPassword(file, tc.host, tc.port, tc.database, tc.username)
			if got != tc.want || found != tc.found {
				t.Fatalf("libpqPassword() = %q, %v; want %q, %v", got, found, tc.want, tc.found)
			}
		})
	}
}

func TestParseServiceSectionKnownAnswers(t *testing.T) {
	const file = "# comment\n" +
		"[other]\n" +
		"password=wrong\n" +
		"[svc-longer]\n" +
		"password=wrong-too\n" +
		"   # an indented comment\n" +
		"[svc]\n" +
		"  host=h \t\n" +
		"password= a=b#c\n" +
		"password=second\n" +
		"\n" +
		"[next]\n" +
		"user=nope\n"
	values, found := parseServiceSection(file, "svc")
	if !found {
		t.Fatal("section [svc] not found")
	}
	// Leading and trailing whitespace of the line go, the value after the
	// first '=' is verbatim, and an earlier setting is not overridden.
	want := map[string]string{"host": "h", "password": " a=b#c"}
	if len(values) != len(want) || values["host"] != want["host"] || values["password"] != want["password"] {
		t.Fatalf("section [svc] = %q, want %q", values, want)
	}
	if _, found := parseServiceSection(file, "sv"); found {
		t.Fatal("a section name prefix matched")
	}
}

// A rendered pgpass line hands libpq exactly its password for the server's
// host, port and user, under any database, and nothing to any other user:
// a role named "*" is not a wildcard.
func FuzzPgpassLine(f *testing.F) {
	f.Add("c-rw.ns.svc", "app", `p:a\ss`, "other")
	f.Add("*", "*", "", "app")
	f.Add(`h\`, `\:`, "*", "*")
	f.Add("#h", "#u", "#p", "u")
	f.Fuzz(func(t *testing.T, host, username, password, other string) {
		if !lineSafe(host) || !lineSafe(username) || !lineSafe(password) || !lineSafe(other) {
			return
		}
		file := renderPassfile([]Server{{Host: host, Port: 5432, Username: username, Password: password}})
		got, found := libpqPassword(file, host, "5432", other, username)
		if password == "" {
			// A server without a password has no line: libpq finds
			// nothing and pgAdmin asks.
			if found || file != "" {
				t.Fatalf("a server without a password wrote %q", file)
			}
			return
		}
		if !found || got != password {
			t.Fatalf("libpq reads %q, %v from %q; want %q", got, found, file, password)
		}
		if other != username {
			if got, found := libpqPassword(file, host, "5432", "db", other); found {
				t.Fatalf("user %q matched the line for %q in %q (password %q)", other, username, file, got)
			}
		}
		if other != host {
			if got, found := libpqPassword(file, other, "5432", "db", username); found {
				t.Fatalf("host %q matched the line for %q in %q (password %q)", other, host, file, got)
			}
		}
		// The migration reads lines back with SplitPgpassLine; it must
		// agree with the rendering field for field.
		fields := SplitPgpassLine(strings.TrimSuffix(file, "\n"))
		want := []string{host, strconv.Itoa(5432), PgpassWildcard, username, password}
		if len(fields) != len(want) {
			t.Fatalf("%q splits into %d fields, want 5", file, len(fields))
		}
		for i := range want {
			if fields[i] != want[i] {
				t.Fatalf("field %d of %q = %q, want %q", i, file, fields[i], want[i])
			}
		}
	})
}
