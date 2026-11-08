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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The cluster's passwords reach libpq through two files on a memory-backed
// emptyDir outside pgAdmin's storage directory, so no account can browse or
// download them through pgAdmin's file manager:
//
//   - the connection service file (PGSERVICEFILE), one section per managed
//     server carrying host, port, dbname, user, password and sslmode. Each
//     server definition names its section in pgAdmin's Service field, and a
//     server with a service is connected without a password prompt — which
//     a server with neither a saved password nor a passfile parameter is
//     not, and the passfile parameter is resolved inside the browsable
//     storage directory in server mode;
//   - the password file (PGPASSFILE), the same credentials for any libpq
//     client in the Pod that does not name a service (a psql, a probe).
//
// What the files buy is that passwords stay out of pgAdmin's settings
// database, its file manager, its backups and its server exports. They are
// not a boundary against pgAdmin's own users: a service section's values
// are only defaults, so a server a user registers that names a managed
// section with a host and sslmode of their own receives its password, and
// the pgpass host pin does not apply to a connection made by hostaddr. Only
// dba reaches pgAdmin, for that reason.

// serviceNamePrefix starts every section the sidecar writes, so a section
// name says where it came from.
const serviceNamePrefix = "pgtoolbox-"

// serviceNames returns one distinct section name per server, in order,
// derived from the server's display name: lower-case letters and digits,
// every other run of characters collapsed to one dash, and a numeric suffix
// when two servers would collide. The name is all libpq matches a service
// on, and it is also what pgAdmin shows in the server's Service field.
func serviceNames(servers []Server) []string {
	names := make([]string, len(servers))
	used := map[string]bool{}
	for i, server := range servers {
		if server.Service != "" {
			names[i] = server.Service
			used[server.Service] = true
			continue
		}
		var b strings.Builder
		dash := false
		for _, r := range strings.ToLower(server.Name) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
				dash = false
			} else if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
		base := serviceNamePrefix + strings.TrimSuffix(b.String(), "-")
		if base == serviceNamePrefix {
			base += "server"
		}
		name := base
		for n := 2; used[name]; n++ {
			name = base + "-" + strconv.Itoa(n)
		}
		used[name] = true
		names[i] = name
	}
	return names
}

// lineSafe reports whether a value survives a line-oriented libpq file.
// Neither format can carry a line break or a NUL, and the service file
// strips trailing whitespace from every line, so a value ending in one
// would silently change.
func lineSafe(value string) bool {
	if strings.ContainsAny(value, "\n\r\x00") {
		return false
	}
	return value == strings.TrimRight(value, " \t\v\f")
}

// representable splits servers into those both files can carry exactly and
// an error naming each one they cannot (by server name and field, never by
// value). A server left out is offered nowhere, rather than offered with a
// credential libpq would read differently.
func representable(servers []Server) ([]Server, error) {
	var usable []Server
	var refused []error
	for _, server := range servers {
		var bad []string
		for _, field := range []struct{ name, value string }{
			{"host", server.Host},
			{"maintenance database", server.MaintenanceDB},
			{"username", server.Username},
			{"password", server.Password},
			{"sslmode", server.SSLMode},
			{"service", server.Service},
			{"sslrootcert", server.SSLRootCert},
		} {
			if !lineSafe(field.value) {
				bad = append(bad, field.name)
			}
		}
		if len(bad) > 0 {
			refused = append(refused, fmt.Errorf(
				"server %q not offered: its %s cannot be written to a libpq file (line break, NUL or trailing whitespace)",
				server.Name, strings.Join(bad, ", ")))
			continue
		}
		usable = append(usable, server)
	}
	return usable, errors.Join(refused...)
}

// renderServiceFile renders the connection service file. Values are
// written verbatim: libpq takes everything after the first '=' as the
// value, with no quoting or escaping, which is why representable refuses
// the few values that cannot survive that.
func renderServiceFile(servers []Server) string {
	names := serviceNames(servers)
	var b strings.Builder
	b.WriteString("# Written by the pgToolBox admin-sync sidecar; rewritten on every sync.\n")
	for i, server := range servers {
		fmt.Fprintf(&b, "\n[%s]\n", names[i])
		fmt.Fprintf(&b, "host=%s\n", server.Host)
		fmt.Fprintf(&b, "port=%d\n", server.Port)
		fmt.Fprintf(&b, "dbname=%s\n", server.MaintenanceDB)
		fmt.Fprintf(&b, "user=%s\n", server.Username)
		if server.Password != "" {
			// A server without a password keeps its section, so pgAdmin's
			// Service still resolves and pgAdmin asks for the password
			// (the team chart's credentials: none, or a credential the
			// sidecar could not verify yet).
			fmt.Fprintf(&b, "password=%s\n", server.Password)
		}
		if server.SSLMode != "" {
			fmt.Fprintf(&b, "sslmode=%s\n", server.SSLMode)
		}
		if server.SSLRootCert != "" {
			fmt.Fprintf(&b, "sslrootcert=%s\n", server.SSLRootCert)
		}
	}
	return b.String()
}

// renderPassfile renders one pgpass line per connection.
//
// Each line names the exact host the server definition names, the
// cluster's read-write Service, so a libpq client in the Pod that connects
// to the cluster without naming a service finds its password. The host is
// a lookup key, not a restriction: libpq matches it against the host
// parameter even when hostaddr sends the connection elsewhere. The database
// field is the wildcard, so expanding any database in pgAdmin's tree
// authenticates.
func renderPassfile(servers []Server) string {
	var lines []string
	for _, server := range servers {
		if server.Password == "" {
			continue
		}
		lines = append(lines, pgpassLine(server.Host, server.Port, server.Username, server.Password))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// PgpassWildcard is pgpass's wildcard: a field that is exactly "*" matches
// any value.
const PgpassWildcard = "*"

// pgpassLine renders one pgpass line for any database. Host and username
// are literals (see pgpassLiteral); the database field is the one
// wildcard; the password is escaped so it de-escapes to itself.
func pgpassLine(host string, port int32, username, password string) string {
	return fmt.Sprintf("%s:%d:%s:%s:%s",
		pgpassLiteral(host),
		port,
		PgpassWildcard,
		pgpassLiteral(username),
		pgpassEscape(password),
	)
}

// pgpassLiteral renders a field libpq compares against a connection
// parameter so that it matches that exact value and nothing else. On top of
// pgpassEscape: a field that is exactly "*" would be the wildcard, so a
// role literally named "*" is written "\*", which libpq de-escapes and
// compares as a plain asterisk; and a leading "#" would make libpq skip the
// whole line as a comment, so it is written "\#".
func pgpassLiteral(s string) string {
	s = pgpassEscape(s)
	switch {
	case s == PgpassWildcard:
		return `\*`
	case strings.HasPrefix(s, "#"):
		return `\` + s
	}
	return s
}

// pgpassEscape escapes the two characters libpq de-escapes in every field:
// backslash, and the colon that would otherwise end the field.
func pgpassEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}

// writeCredentialFile replaces path atomically with mode 0600: libpq
// ignores a password file that is group- or world-readable, and the
// service file carries the same passwords.
func writeCredentialFile(path, content string) error {
	return writeFileAtomic(path, content, 0o750)
}

// writeFileAtomic writes content to a fresh temporary file beside path —
// created exclusively (os.CreateTemp opens with O_EXCL), mode 0600, so no
// pre-existing file or link under that name is ever written through — and
// renames it over path. The temporary file is removed on any failure.
func writeFileAtomic(path, content string, directoryMode os.FileMode) (err error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return fmt.Errorf("create directory for %s: %w", filepath.Base(path), err)
	}
	file, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict %s: %w", filepath.Base(path), err)
	}
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// ServiceNames returns the section name of every server, in order (see
// serviceNames).
func ServiceNames(servers []Server) []string { return serviceNames(servers) }

// WriteCredentialFiles renders the service file and the pgpass file for
// servers and replaces both atomically with mode 0600. A server whose
// values a libpq file cannot carry exactly is left out and named in
// refused; when the files of the rest would exceed MaxCredentialFilesBytes,
// or more than MaxServers are given, nothing beyond the bound is offered
// and refused says so. offered is what the files now carry. err is a write
// failure, after which the previous files may remain: callers retry.
func WriteCredentialFiles(serviceFile, passFile string, servers []Server) (offered []Server, refused, err error) {
	var problems []error
	if len(servers) > MaxServers {
		problems = append(problems, fmt.Errorf("%d servers offered, over the limit of %d; the rest are left out", len(servers), MaxServers))
		servers = servers[:MaxServers]
	}
	usable, unrepresentable := representable(servers)
	problems = append(problems, unrepresentable)
	serviceContent, passContent := renderServiceFile(usable), renderPassfile(usable)
	if size := len(serviceContent) + len(passContent); size > MaxCredentialFilesBytes {
		problems = append(problems, fmt.Errorf(
			"the credential files for %d server(s) would take %d bytes, over the %d-byte limit; no connection is offered",
			len(usable), size, MaxCredentialFilesBytes))
		usable = nil
		serviceContent, passContent = renderServiceFile(nil), renderPassfile(nil)
	}
	refused = errors.Join(problems...)
	if err := writeCredentialFile(serviceFile, serviceContent); err != nil {
		return nil, refused, err
	}
	if err := writeCredentialFile(passFile, passContent); err != nil {
		return nil, refused, err
	}
	return usable, refused, nil
}

// WriteFile replaces path atomically with content, mode 0600 (directories
// 0700): the CA files beside the credential files.
func WriteFile(path, content string) error {
	return writeFileAtomic(path, content, 0o700)
}

// SplitPgpassLine splits a pgpass line into its unescaped fields the way
// libpq reads it: ':' separates, '\' escapes the next character.
func SplitPgpassLine(line string) []string {
	var fields []string
	var field []byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case escaped:
			field = append(field, c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == ':':
			fields = append(fields, string(field))
			field = field[:0]
		default:
			field = append(field, c)
		}
	}
	return append(fields, string(field))
}
