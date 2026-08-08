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
	"strings"
	"time"
)

// SyncRequest is the only payload of the sidecar API: the complete desired
// state. It carries one entry per PgToolBoxUser, including the postgres
// password; the password is never logged and only travels over the in-pod
// mTLS API.
type SyncRequest struct {
	Users []User `json:"users"`
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

// apply converges pgAdmin state: it writes the combined passfile, then for
// each user ensures the external account exists with the right role and the
// shared server definition is loaded. It stops at the first failure and the
// operator retries the whole request; the mapping is absolute, so replays
// converge instead of compounding.
func (o SidecarOptions) apply(ctx context.Context, request SyncRequest) error {
	if err := o.writePassfile(request.Users); err != nil {
		return fmt.Errorf("write pgpass file: %w", err)
	}
	for _, user := range request.Users {
		if err := o.addUser(ctx, user.Subject, user.PgAdminRole); err != nil {
			return err
		}
		if err := o.updateRole(ctx, user.Subject, user.PgAdminRole); err != nil {
			return err
		}
		if err := o.loadServers(ctx, user.Subject, user.Server); err != nil {
			return err
		}
	}
	return nil
}

// addUser creates the webserver-authenticated external user. setup.py keeps
// creation and modification in separate subcommands: update-external-user
// only updates, so without this the account never existed and load-servers
// failed with "The specified user ID could not be found".
//
// It is idempotent by tolerance rather than by flag — pgAdmin has no
// create-or-update — so an "already exists" outcome is success. Any other
// failure surfaces, and the following updateRole is what converges the role
// of a user that already existed.
func (o SidecarOptions) addUser(ctx context.Context, username, role string) error {
	command := exec.CommandContext(ctx, o.PythonPath, o.SetupPath, // #nosec G204
		"add-external-user", username,
		"--auth-source", "webserver",
		"--role", role,
		"--email", username,
	)
	output, err := command.CombinedOutput()
	if err == nil || strings.Contains(string(output), "already exists") {
		return nil
	}
	return fmt.Errorf("add user %q: %w: %s", username, err, string(output))
}

// updateRole assigns a pgAdmin role to one webserver-authenticated external
// user through the supported setup.py CLI, the only sanctioned way to change
// roles without touching pgAdmin's database directly. The returned error
// embeds the CLI output, which carries no credential material.
func (o SidecarOptions) updateRole(ctx context.Context, username, role string) error {
	command := exec.CommandContext(ctx, o.PythonPath, o.SetupPath, // #nosec G204
		"update-external-user", username,
		"--auth-source", "webserver",
		"--role", role,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("update role of %q: %w: %s", username, err, string(output))
	}
	return nil
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

// loadServers imports one shared server definition for a single user,
// replacing any existing servers owned by that user so the operation is
// idempotent.
func (o SidecarOptions) loadServers(ctx context.Context, username string, server Server) error {
	document := serverDocument{
		Servers: map[string]serverEntry{
			"1": {
				Name:          server.Name,
				Group:         server.Group,
				Host:          server.Host,
				Port:          server.Port,
				MaintenanceDB: server.MaintenanceDB,
				Username:      server.Username,
				ConnectionParameters: map[string]string{
					"sslmode":  server.SSLMode,
					"passfile": server.PassFile,
				},
			},
		},
	}
	payload, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("marshal server JSON for %q: %w", username, err)
	}

	file, err := os.CreateTemp("", "pgadmin-servers-*.json")
	if err != nil {
		return fmt.Errorf("create server JSON temp file for %q: %w", username, err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return fmt.Errorf("write server JSON temp file for %q: %w", username, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close server JSON temp file for %q: %w", username, err)
	}

	command := exec.CommandContext(ctx, o.PythonPath, o.SetupPath, // #nosec G204
		"load-servers", file.Name(),
		"--user", username,
		"--auth-source", "webserver",
		"--replace",
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("load servers for %q: %w: %s", username, err, string(output))
	}
	return nil
}

// writePassfile renders the combined pgpass file from the current desired
// state. Each user gets one line; removed users disappear on the next sync.
// The file is created with restricted permissions and atomically replaced.
func (o SidecarOptions) writePassfile(users []User) error {
	var lines []string
	for _, user := range users {
		lines = append(lines, pgpassLine(
			user.Server.Host,
			user.Server.Port,
			user.Server.MaintenanceDB,
			user.Server.Username,
			user.Password,
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
