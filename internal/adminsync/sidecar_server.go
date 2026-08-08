/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package adminsync

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// SyncRequest is the only payload of the sidecar API: the complete desired
// state. It carries the connections pgAdmin should offer, each with the
// password of a credential the CloudNativePG cluster publishes. Passwords
// are never logged and travel only over the in-pod mTLS API.
type SyncRequest struct {
	Servers []Server `json:"servers"`
}

// SidecarOptions configures RunSidecar.
type SidecarOptions struct {
	// Addr is the listen address, defaulting to ":9600".
	Addr string
	// CertDir contains tls.crt and tls.key for serving.
	CertDir string
	// TokenFile contains the shared bearer token.
	TokenFile string
	// PythonPath and SetupPath locate pgAdmin's supported user-management
	// CLI inside the pod filesystem.
	PythonPath string
	SetupPath  string
	// PassFile is the path where the sidecar writes the combined pgpass
	// file; it defaults to DefaultPassFilePath.
	PassFile string
	// SettingsDB is pgAdmin's settings database, read to discover which
	// accounts exist. It defaults to DefaultSettingsDBPath.
	SettingsDB string
}

// RunSidecar serves the admin-sync API until the context ends. pgAdmin
// state changes are applied by this process, which already lives inside the
// pgAdmin Pod, so the operator needs no exec-style permission at all.
func (o SidecarOptions) RunSidecar(ctx context.Context) error {
	if o.Addr == "" {
		o.Addr = fmt.Sprintf(":%d", SidecarPort)
	}
	if o.PythonPath == "" {
		o.PythonPath = pythonPath
	}
	if o.SetupPath == "" {
		o.SetupPath = setupPath
	}
	if o.PassFile == "" {
		o.PassFile = DefaultPassFilePath
	}
	if o.SettingsDB == "" {
		o.SettingsDB = DefaultSettingsDBPath
	}

	token, err := os.ReadFile(o.TokenFile)
	if err != nil {
		return fmt.Errorf("read sidecar token: %w", err)
	}
	expected := strings.TrimSpace(string(token))
	if len(expected) < 32 {
		return fmt.Errorf("sidecar token in %s is too short", o.TokenFile)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request SyncRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}
		if err := o.apply(r.Context(), request); err != nil {
			// The error carries usernames and CLI output only, never
			// credential material.
			_, _ = fmt.Fprintf(os.Stderr, "admin-sync request failed: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{
		Addr:              o.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	_, _ = fmt.Fprintf(os.Stdout, "admin-sync sidecar listening on %s\n", o.Addr)
	errs := make(chan error, 1)
	go func() {
		errs <- server.ListenAndServeTLS(
			filepath.Join(o.CertDir, "tls.crt"),
			filepath.Join(o.CertDir, "tls.key"),
		)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errs:
		return err
	}
}

// apply converges pgAdmin state: every account gets the same connections,
// because the credentials belong to the cluster rather than to whoever
// signed in, and the proxy has already refused anyone below
// spec.pgAdmin.accessMinLevel.
//
// pgAdmin does not offer a way to share one list. Its shared-server feature
// strips passfile and the TLS file paths out of a server the moment it
// materializes it for anyone but the owner — deliberately, so that "each
// user should configure their own" — so a genuinely shared entry can carry
// visibility but never credentials. What it does support is a server per
// account, so that is what this writes: the same list, once per account,
// each pointing at that account's own copy of the credential file.
//
// Accounts arrive on their own. pgAdmin creates one on first sight of the
// proxy's identity header, so the set is whatever has signed in so far and
// is read back from pgAdmin rather than supplied by the operator; an
// account that appears later is served by the next sync.
func (o SidecarOptions) apply(ctx context.Context, request SyncRequest) error {
	accounts, err := o.accounts(ctx)
	if err != nil {
		return err
	}
	for _, account := range accounts {
		if err := o.writePassfile(account, request.Servers); err != nil {
			return err
		}
		if err := o.loadServers(ctx, account, request.Servers); err != nil {
			return err
		}
	}
	// One combined file outside anyone's storage, for connections made
	// without pgAdmin's own resolution — PGPASSFILE names it, so a psql in
	// the Pod finds the same credentials.
	return o.writeLegacyPassfile(request.Servers)
}

// accounts lists the pgAdmin accounts to provision, straight from its
// settings database. The bootstrap account is skipped: it exists only to
// initialize that database and nobody signs in with it.
func (o SidecarOptions) accounts(ctx context.Context) ([]string, error) {
	const query = `SELECT email FROM user WHERE auth_source = 'webserver'`
	output, err := o.querySettingsDB(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list pgAdmin accounts: %w", err)
	}
	var accounts []string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if account := strings.TrimSpace(line); account != "" {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

// querySettingsDB runs one read-only query against pgAdmin's settings
// database. It goes through the interpreter pgAdmin itself ships rather
// than a Go driver: the sidecar runs in the pgAdmin image, so sqlite3 is
// already there, and the build stays free of cgo.
func (o SidecarOptions) querySettingsDB(ctx context.Context, query string) (string, error) {
	script := "import sqlite3,sys\n" +
		"c=sqlite3.connect(sys.argv[1])\n" +
		"print('\\n'.join(str(r[0]) for r in c.execute(sys.argv[2])))\n"
	command := exec.CommandContext(ctx, o.PythonPath, "-c", script, o.SettingsDB, query) // #nosec G204
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(output))
	}
	return string(output), nil
}

// serverDocument is the JSON shape consumed by setup.py load-servers.
type serverDocument struct {
	Servers map[string]serverEntry `json:"Servers"`
}

type serverEntry struct {
	Name                 string            `json:"Name"`
	Group                string            `json:"Group"`
	Host                 string            `json:"Host"`
	Port                 int32             `json:"Port"`
	MaintenanceDB        string            `json:"MaintenanceDB"`
	Username             string            `json:"Username"`
	ConnectionParameters map[string]string `json:"ConnectionParameters"`
}

// loadServers imports the whole connection list for one account, replacing
// whatever that account had so the operation is idempotent.
//
// The passfile parameter has to be present and has to resolve, and pgAdmin
// treats those two failures very differently: absent, it prompts for a
// password; present but unresolvable, it connects with none and the server
// answers "no password supplied". It resolves the value through its file
// manager, which joins it onto the signed-in account's storage directory —
// so the path is storage-relative, and writePassfile puts the file exactly
// where that resolution lands.
func (o SidecarOptions) loadServers(ctx context.Context, account string, servers []Server) error {
	document := serverDocument{Servers: map[string]serverEntry{}}
	for i, server := range servers {
		document.Servers[strconv.Itoa(i+1)] = serverEntry{
			Name:          server.Name,
			Group:         server.Group,
			Host:          server.Host,
			Port:          server.Port,
			MaintenanceDB: server.MaintenanceDB,
			Username:      server.Username,
			ConnectionParameters: map[string]string{
				"sslmode":  server.SSLMode,
				"passfile": storageRelativePassFile,
			},
		}
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("marshal server JSON for %q: %w", account, err)
	}

	file, err := os.CreateTemp("", "pgadmin-servers-*.json")
	if err != nil {
		return fmt.Errorf("create server JSON temp file for %q: %w", account, err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write server JSON temp file for %q: %w", account, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close server JSON temp file for %q: %w", account, err)
	}

	command := exec.CommandContext(ctx, o.PythonPath, o.SetupPath, // #nosec G204
		"load-servers", file.Name(),
		"--user", account,
		"--auth-source", "webserver",
		"--replace",
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("load servers for %q: %w: %s", account, err, string(output))
	}
	return nil
}

// writePassfile renders the combined pgpass file from the current desired
// state. Each user gets one line; removed users disappear on the next sync.
// The file is created with restricted permissions and atomically replaced.
//
// The host field is the wildcard rather than the server's hostname. libpq
// matches a pgpass line against the host string it was given, not against
// the host it resolved: a connection made by address, or through any alias
// of the same Service, matches no line spelled with the name and fails with
// "fe_sendauth: no password supplied" — the credential is right there and
// simply never consulted. Matching on the username instead costs nothing
// here, because this file is pod-private, is mounted only by pgAdmin and
// the sidecar that writes it, and holds none but this console's roles for
// this one cluster.
// writePassfile writes one account's credential file, inside that
// account's own pgAdmin storage directory, because that is the only place
// pgAdmin resolves a server's passfile to. Every account gets the same
// credentials: they are the cluster's, not the reader's.
//
// The host field is the wildcard. libpq matches a line against the host
// string it was given rather than the host it resolved, so a line naming
// the Service fails for a connection made by address, and reports it as no
// password rather than as a file it declined to use.
func (o SidecarOptions) writePassfile(account string, servers []Server) error {
	directory := userStorageDirectory(account)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create storage directory for %q: %w", account, err)
	}
	var lines []string
	for _, server := range servers {
		lines = append(lines, pgpassLine(
			pgpassAnyHost,
			server.Port,
			server.MaintenanceDB,
			server.Username,
			server.Password,
		))
	}
	content := []byte(strings.Join(lines, "\n"))
	if len(content) > 0 {
		content = append(content, '\n')
	}

	target := filepath.Join(directory, "pgpass")
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		return fmt.Errorf("write passfile for %q: %w", account, err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("replace passfile for %q: %w", account, err)
	}
	return nil
}

// writeLegacyPassfile keeps the combined file the pgAdmin container still
// points PGPASSFILE at, so a connection made outside pgAdmin's own
// resolution — a psql in the Pod, a probe — still finds credentials.
func (o SidecarOptions) writeLegacyPassfile(servers []Server) error {
	var lines []string
	for _, server := range servers {
		lines = append(lines, pgpassLine(
			pgpassAnyHost,
			server.Port,
			server.MaintenanceDB,
			server.Username,
			server.Password,
		))
	}
	content := []byte(strings.Join(lines, "\n"))
	if len(content) > 0 {
		content = append(content, '\n')
	}

	if err := os.MkdirAll(filepath.Dir(o.PassFile), 0o750); err != nil {
		return fmt.Errorf("create passfile directory: %w", err)
	}

	tempFile := o.PassFile + ".tmp"
	if err := os.WriteFile(tempFile, content, 0o600); err != nil {
		return fmt.Errorf("write passfile: %w", err)
	}
	return os.Rename(tempFile, o.PassFile)
}

// pgpassAnyHost is pgpass's wildcard: it matches whatever host string the
// client connected with, by name or by address.
const pgpassAnyHost = "*"

// storageRelativePassFile is the passfile as pgAdmin's file manager sees
// it: paths are relative to the signed-in user's own storage directory.
const storageRelativePassFile = "/pgpass"

// pgAdminStorageRoot is where pgAdmin keeps those per-user directories. It
// is on the settings volume, so the file survives a restart with the rest
// of pgAdmin's state rather than living in an emptyDir.
const pgAdminStorageRoot = "/var/lib/pgadmin/storage"

// userStorageDirectory is the storage directory pgAdmin resolves for one
// account. pgAdmin derives it from the username with '@' replaced, which is
// why the subject must be an email address for pgAdmin to accept it at all.
func userStorageDirectory(subject string) string {
	return filepath.Join(pgAdminStorageRoot, strings.ReplaceAll(subject, "@", "_"))
}

// pgpassLine renders one pgpass line, escaping colon and backslash in each
// field so the file round-trips through libpq.
func pgpassLine(host string, port int32, database, username, password string) string {
	return fmt.Sprintf("%s:%d:%s:%s:%s",
		pgpassEscape(host),
		port,
		pgpassEscape(database),
		pgpassEscape(username),
		pgpassEscape(password),
	)
}

func pgpassEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `:`, `\:`)
	return s
}
