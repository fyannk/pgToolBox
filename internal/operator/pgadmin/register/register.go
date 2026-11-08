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

// Package register is what the PgAdmin Jobs run inside pgAdmin's image:
// the registration of the offered set as shared servers (dump-servers →
// keep everything outside the managed group → load-servers), the pgAdmin
// role of the administrators, and the pre-upgrade backup of the settings
// database. setup.py stays the only writer of pgAdmin's database; this
// package only prepares its input and reads its output, under a file lock
// that serializes every Job on the settings volume.
package register

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"github.com/fyannk/pgToolBox/internal/operator/cnpgcred"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// Bounds of what setup.py may hand back.
const (
	maxDumpBytes   = 4 << 20
	maxOutputBytes = 4 << 10
	maxUsersBytes  = 1 << 20
)

// Runner runs one command and returns its combined output. Tests replace
// it; the default is os/exec with the context's deadline.
type Runner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

// ExecRunner runs a process.
func ExecRunner(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed program, arguments are not shell-interpreted.
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

// Options of a registration.
type Options struct {
	OfferFile string
	// DeclaredFile, instead of OfferFile, is the pgtools chart's declared
	// server list (package declared): pgAdmin without the operator. Its
	// servers are registered as shared servers like an offer's; it has no
	// personal servers and no settings database on a Cluster. Namespace is
	// the Pod's, where its endpoints' Services are.
	DeclaredFile string
	// WithheldFile, with DeclaredFile, is the passfile-sync sidecar's list
	// of declared servers whose Cluster cannot serve the endpoint
	// (declared.Withheld): they are left out. The registration waits for
	// it (within WaitForDB) after pgAdmin answers.
	WithheldFile string
	Namespace    string
	Owner        string
	AuthSource   string
	Admins       []string
	SettingsDB   string
	LockFile     string
	TempDir      string
	Python       string
	Setup        string
	PgAdminDir   string
	// WaitForDB bounds the wait for pgAdmin to create its database (and,
	// with ReadyURL, to answer).
	WaitForDB time.Duration
	// ReadyURL, when set, is pgAdmin's ping endpoint in the same Pod: the
	// registration waits for it to answer before its first setup.py run.
	// The database file appears as soon as pgAdmin starts creating it, and
	// a setup.py run that opens it before pgAdmin has added its initial
	// user leaves a database pgAdmin cannot start on again (it then asks
	// for that user on a terminal it does not have). The pgtools chart's
	// sidecar needs it; the operator's Job starts only once pgAdmin runs.
	ReadyURL string
	Run      Runner
	// ForgetOwners are pgAdmin users whose managed personal servers the
	// last registration wrote and the offer no longer has: their managed
	// personal group is emptied.
	ForgetOwners []string
	// ConfigDatabaseURI is pgAdmin's CONFIG_DATABASE_URI when its settings
	// database is on a Cluster (spec.configDatabase); SettingsDB is then
	// unused. setup.py reads it from a config_local.py written under
	// TempDir, and libpq the password from the credential files written
	// into CredentialsDir from the Secrets the offer names.
	ConfigDatabaseURI string
	CredentialsDir    string
	// ConfigDatabaseSelfContained says the URI carries everything libpq
	// needs (the pgtools chart reads it from a Secret the team names):
	// no offer, no credential files, no API client.
	ConfigDatabaseSelfContained bool
	// Secrets reads one Secret (namespace "" is the Pod's own) for the
	// settings database credentials; nil without ConfigDatabaseURI.
	Secrets func(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	// Env is added to setup.py's environment (by the default Runner).
	Env []string

	runIsDefault bool
}

func (o *Options) defaults() {
	if o.Python == "" {
		o.Python = "/venv/bin/python3"
	}
	if o.Setup == "" {
		o.Setup = "/pgadmin4/setup.py"
	}
	if o.PgAdminDir == "" {
		o.PgAdminDir = "/pgadmin4"
	}
	if o.TempDir == "" {
		o.TempDir = os.TempDir()
	}
	if o.WaitForDB == 0 {
		o.WaitForDB = 2 * time.Minute
	}
	if o.Run == nil {
		o.Run, o.runIsDefault = EnvRunner(o.Env), true
	}
}

// EnvRunner is ExecRunner with env added to the process environment.
func EnvRunner(env []string) Runner {
	return func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- fixed program, arguments are not shell-interpreted.
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		cmd.WaitDelay = 10 * time.Second
		return cmd.CombinedOutput()
	}
}

// Report is the registration's outcome, written as the Job's termination
// message for the reconciler.
type Report struct {
	PgAdminVersion string `json:"pgAdminVersion"`
	Servers        int    `json:"servers"`
	// Withheld are the declared servers left out (WithheldFile).
	Withheld []declared.WithheldServer `json:"withheld,omitempty"`
}

// serverEntry is one managed server in setup.py's import format: a shared
// server owned by the internal account, visible to every pgAdmin user,
// carrying no password — libpq finds it in the service file the section
// name points to.
type serverEntry struct {
	Name          string `json:"Name"`
	Group         string `json:"Group"`
	Host          string `json:"Host"`
	Port          int32  `json:"Port"`
	MaintenanceDB string `json:"MaintenanceDB"`
	// Username is omitted when the declared list leaves the user to the
	// Cluster or the DatabaseRole: libpq then takes it from the service
	// section the sidecar writes (pgAdmin needs no Username for a server
	// with a Service). Never "": libpq would replace an empty user with
	// the operating system's.
	Username             string            `json:"Username,omitempty"`
	Shared               bool              `json:"Shared"`
	SharedUsername       string            `json:"SharedUsername,omitempty"`
	Service              string            `json:"Service"`
	ConnectionParameters map[string]string `json:"ConnectionParameters"`
}

// PersonalGroup is the pgAdmin server group of a user's personal managed
// servers (per-person DatabaseRoles); the registration replaces that group
// in the user's tree and nothing else.
const PersonalGroup = offer.ManagedGroup + " (personal)"

// managedEntries renders the offer's shared servers (owner "") or one
// user's personal servers as setup.py entries. A personal server is not
// shared: it is that user's own entry, in PersonalGroup.
func managedEntries(doc offer.Document, owner string) []serverEntry {
	var entries []serverEntry
	for _, s := range doc.Servers {
		if s.Owner != owner {
			continue
		}
		entry := serverEntry{
			Name: s.Name, Group: offer.ManagedGroup, Host: s.Host, Port: s.Port,
			MaintenanceDB: s.DBName, Username: s.User, Shared: true, SharedUsername: s.User,
			Service:              s.Service,
			ConnectionParameters: map[string]string{"sslmode": offer.SSLMode},
		}
		if owner != "" {
			entry.Group, entry.Shared, entry.SharedUsername = PersonalGroup, false, ""
		}
		entries = append(entries, entry)
	}
	return entries
}

// Register runs one registration.
func Register(ctx context.Context, o Options) (Report, error) {
	o.defaults()
	unlock, err := lock(ctx, o.LockFile)
	if err != nil {
		return Report{}, err
	}
	defer unlock()
	var doc offer.Document
	var shared []serverEntry
	if o.DeclaredFile != "" {
		list, err := declared.Load(o.DeclaredFile)
		if err != nil {
			return Report{}, err
		}
		shared = declaredEntries(list, o.Namespace)
	} else {
		raw, err := readBounded(o.OfferFile, offer.MaxDocumentBytes)
		if err != nil {
			return Report{}, err
		}
		if doc, err = offer.Parse(raw); err != nil {
			return Report{}, err
		}
		shared = managedEntries(doc, "")
	}
	if o.ConfigDatabaseURI != "" {
		if err := o.prepareConfigDatabase(ctx, doc); err != nil {
			return Report{}, err
		}
	} else if err := waitForFile(ctx, o.SettingsDB, o.WaitForDB, "pgAdmin"); err != nil {
		return Report{}, err
	}
	if o.ReadyURL != "" {
		if err := waitForReady(ctx, o.ReadyURL, o.WaitForDB); err != nil {
			return Report{}, err
		}
	}

	var withheld []declared.WithheldServer
	if o.DeclaredFile != "" && o.WithheldFile != "" {
		// Read last, after the waits: the sidecar has had pgAdmin's start
		// to read the Clusters.
		if err := waitForFile(ctx, o.WithheldFile, o.WaitForDB, "the passfile-sync sidecar"); err != nil {
			return Report{}, err
		}
		list, err := declared.LoadWithheld(o.WithheldFile)
		if err != nil {
			return Report{}, err
		}
		names := list.Names()
		shared = slices.DeleteFunc(shared, func(e serverEntry) bool { return names[e.Name] })
		withheld = list.Servers
	}

	// The shared servers, owned by the internal account.
	if err := o.replaceManaged(ctx, o.Owner, "internal", offer.ManagedGroup, shared); err != nil {
		return Report{}, err
	}
	users, err := o.users(ctx)
	if err != nil {
		return Report{}, err
	}
	if err := o.syncAdmins(ctx, users); err != nil {
		return Report{}, err
	}
	// Personal servers: each owner's own tree, created ahead of the
	// user's first sign-in when needed; then the owners the offer dropped.
	var problems []error
	for _, owner := range doc.Owners() {
		if _, ok := users[owner]; !ok {
			if err := o.setup(ctx, "add-external-user", owner, "--auth-source", o.AuthSource, "--role", "User"); err != nil {
				problems = append(problems, fmt.Errorf("add user %q for personal servers: %w", owner, err))
				continue
			}
		}
		if err := o.replaceManaged(ctx, owner, o.AuthSource, PersonalGroup, managedEntries(doc, owner)); err != nil {
			problems = append(problems, fmt.Errorf("personal servers of %q: %w", owner, err))
		}
	}
	for _, owner := range o.ForgetOwners {
		if _, ok := users[owner]; !ok || slices.Contains(doc.Owners(), owner) {
			continue
		}
		if err := o.replaceManaged(ctx, owner, o.AuthSource, PersonalGroup, nil); err != nil {
			problems = append(problems, fmt.Errorf("remove the personal servers of %q: %w", owner, err))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return Report{}, err
	}
	servers := len(doc.Servers) // shared and personal
	if o.DeclaredFile != "" {
		servers = len(shared)
	}
	return Report{PgAdminVersion: o.version(ctx), Servers: servers, Withheld: withheld}, nil
}

// declaredEntries renders a declared list as shared servers. The display
// name, host, port and service section come from the list; the user and
// the maintenance database only when declared (the user is otherwise the
// service section's, which the sidecar derives from live state; the
// maintenance database falls back to postgres, which every Cluster has).
// Registration reads nothing live: the same list always registers the
// same servers.
func declaredEntries(list declared.Document, namespace string) []serverEntry {
	services := list.Services()
	entries := make([]serverEntry, 0, len(list.Servers))
	for i, s := range list.Servers {
		database := s.Database
		if database == "" {
			database = cnpgcred.DefaultDatabase
		}
		entries = append(entries, serverEntry{
			Name: s.DisplayName(), Group: offer.ManagedGroup, Host: s.Host(namespace), Port: declared.Port,
			MaintenanceDB: database, Username: s.User, Shared: true, SharedUsername: s.User,
			Service:              services[i],
			ConnectionParameters: map[string]string{"sslmode": offer.SSLMode},
		})
	}
	return entries
}

// replaceManaged replaces the servers of group in user's tree with
// entries, keeping every other server of that user verbatim: dump-servers,
// merge, load-servers --replace.
func (o *Options) replaceManaged(ctx context.Context, user, authSource, group string, entries []serverEntry) error {
	dumpPath := filepath.Join(o.TempDir, "pgtoolbox-dump.json")
	loadPath := filepath.Join(o.TempDir, "pgtoolbox-load.json")
	defer func() { _ = os.Remove(dumpPath); _ = os.Remove(loadPath) }()
	_ = os.Remove(dumpPath)
	if err := o.setup(ctx, "dump-servers", dumpPath, "--user", user, "--auth-source", authSource); err != nil {
		return fmt.Errorf("dump-servers: %w", err)
	}
	dumped, err := readBounded(dumpPath, maxDumpBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// An empty document is valid: --replace then clears the user's servers.
	merged, err := mergeServers(dumped, group, entries)
	if err != nil {
		return err
	}
	if err := os.WriteFile(loadPath, merged, 0o600); err != nil {
		return fmt.Errorf("write load document: %w", err)
	}
	if err := o.setup(ctx, "load-servers", loadPath, "--user", user, "--auth-source", authSource, "--replace"); err != nil {
		return fmt.Errorf("load-servers: %w", err)
	}
	return nil
}

func (o *Options) setup(ctx context.Context, args ...string) error {
	output, err := o.Run(ctx, o.PgAdminDir, o.Python, append([]string{o.Setup}, args...)...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, bounded(output))
	}
	return nil
}

// pgAdminUser is one row of setup.py get-users --json.
type pgAdminUser struct {
	Username   string `json:"username"`
	Role       string `json:"role"`
	AuthSource string `json:"auth_source"`
}

// users returns the pgAdmin users of the signed-in users' authentication
// source, by username.
func (o *Options) users(ctx context.Context) (map[string]pgAdminUser, error) {
	output, err := o.Run(ctx, o.PgAdminDir, o.Python, o.Setup, "get-users", "--auth-source", o.AuthSource, "--json")
	if err != nil {
		return nil, fmt.Errorf("get-users: %w: %s", err, bounded(output))
	}
	users, err := parseUsers(output)
	if err != nil {
		return nil, err
	}
	existing := map[string]pgAdminUser{}
	for _, u := range users {
		if u.AuthSource == o.AuthSource {
			existing[u.Username] = u
		}
	}
	return existing, nil
}

// syncAdmins gives spec.admins pgAdmin's Administrator role (creating the
// account ahead of its first sign-in), and demotes to User every other
// Administrator of the same authentication source. The internal account
// that owns the shared servers is never touched. A created administrator
// joins existing, for the personal servers that follow.
func (o *Options) syncAdmins(ctx context.Context, existing map[string]pgAdminUser) error {
	var problems []error
	for _, admin := range o.Admins {
		user, ok := existing[admin]
		switch {
		case !ok:
			if err := o.setup(ctx, "add-external-user", admin, "--auth-source", o.AuthSource, "--role", "Administrator"); err != nil {
				problems = append(problems, fmt.Errorf("add administrator %q: %w", admin, err))
				continue
			}
			existing[admin] = pgAdminUser{Username: admin, Role: "Administrator", AuthSource: o.AuthSource}
		case user.Role != "Administrator":
			if err := o.setup(ctx, "update-external-user", admin, "--auth-source", o.AuthSource, "--role", "Administrator"); err != nil {
				problems = append(problems, fmt.Errorf("promote %q: %w", admin, err))
			}
		}
	}
	for _, user := range existing {
		if user.Role == "Administrator" && !slices.Contains(o.Admins, user.Username) {
			if err := o.setup(ctx, "update-external-user", user.Username, "--auth-source", o.AuthSource, "--role", "User"); err != nil {
				problems = append(problems, fmt.Errorf("demote %q: %w", user.Username, err))
			}
		}
	}
	return errors.Join(problems...)
}

// parseUsers reads setup.py get-users --json, which prints the JSON array
// among other console output.
func parseUsers(output []byte) ([]pgAdminUser, error) {
	if len(output) > maxUsersBytes {
		return nil, fmt.Errorf("get-users printed %d bytes, over %d", len(output), maxUsersBytes)
	}
	text := string(output)
	start, end := strings.Index(text, "["), strings.LastIndex(text, "]")
	switch {
	case start < 0:
		return nil, nil // no user yet
	case end < start:
		return nil, errors.New("parse get-users output: unterminated JSON array")
	}
	var users []pgAdminUser
	if err := json.Unmarshal([]byte(text[start:end+1]), &users); err != nil {
		return nil, fmt.Errorf("parse get-users output: %w", err)
	}
	return users, nil
}

// version asks pgAdmin's own configuration for its version; "" when it
// cannot tell.
func (o *Options) version(ctx context.Context) string {
	output, err := o.Run(ctx, o.PgAdminDir, o.Python, "-c", "import config; print(config.APP_VERSION)")
	if err != nil {
		return ""
	}
	lines := strings.Fields(string(output))
	if len(lines) == 0 {
		return ""
	}
	v := lines[len(lines)-1]
	if len(v) > 32 {
		return ""
	}
	return v
}

// mergeServers returns the document to load: every entry the account
// already has outside group, verbatim and in its original order, followed
// by the managed entries.
func mergeServers(dumped []byte, group string, managed []serverEntry) ([]byte, error) {
	var current struct {
		Servers map[string]json.RawMessage `json:"Servers"`
	}
	if len(strings.TrimSpace(string(dumped))) > 0 {
		if err := json.Unmarshal(dumped, &current); err != nil {
			return nil, fmt.Errorf("parse dumped servers: %w", err)
		}
	}
	keys := make([]string, 0, len(current.Servers))
	for key := range current.Servers {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		x, errX := strconv.Atoi(keys[i])
		y, errY := strconv.Atoi(keys[j])
		switch {
		case errX == nil && errY == nil:
			return x < y
		case errX == nil:
			return true
		case errY == nil:
			return false
		}
		return keys[i] < keys[j]
	})
	merged := map[string]json.RawMessage{}
	next := 1
	for _, key := range keys {
		var entry struct {
			Group string `json:"Group"`
		}
		if err := json.Unmarshal(current.Servers[key], &entry); err != nil {
			return nil, fmt.Errorf("parse dumped server %s: %w", key, err)
		}
		if entry.Group == group {
			continue
		}
		merged[strconv.Itoa(next)] = current.Servers[key]
		next++
	}
	for _, entry := range managed {
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		merged[strconv.Itoa(next)] = raw
		next++
	}
	return json.Marshal(map[string]any{"Servers": merged})
}

// BackupOptions of a pre-upgrade backup.
type BackupOptions struct {
	SettingsDB, Dir, LockFile, Label string
	Keep                             int
	Python                           string
	Now                              func() time.Time
	Run                              Runner
}

// backupScript copies the database with SQLite's online backup API, which
// is consistent while pgAdmin keeps running. Paths come as arguments.
const backupScript = `import sqlite3, sys
src = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[2])
with dst:
    src.backup(dst)
dst.close(); src.close()`

// Backup copies the settings database into Dir and keeps the newest Keep
// backups. It returns the backup's path.
func Backup(ctx context.Context, o BackupOptions) (string, error) {
	if o.Python == "" {
		o.Python = "/venv/bin/python3"
	}
	if o.Run == nil {
		o.Run = ExecRunner
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	unlock, err := lock(ctx, o.LockFile)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := os.Stat(o.SettingsDB); err != nil {
		return "", fmt.Errorf("settings database: %w", err)
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(o.Dir, fmt.Sprintf("pgadmin4-%s-%s.db", o.Now().UTC().Format("20060102T150405Z"), o.Label))
	partial := target + ".partial"
	if output, err := o.Run(ctx, "", o.Python, "-c", backupScript, o.SettingsDB, partial); err != nil {
		_ = os.Remove(partial)
		return "", fmt.Errorf("backup: %w: %s", err, bounded(output))
	}
	if err := os.Rename(partial, target); err != nil {
		return "", err
	}
	return target, prune(o.Dir, o.Keep)
}

// BackupOnImageChange is the pgtools chart's pre-upgrade backup, run by an
// init container of the pgAdmin Pod before pgAdmin starts (and migrates
// its settings database): when the image recorded in marker differs from
// image and the settings database exists, it copies the database aside
// (Backup, keeping o.Keep copies), then records image. A first start (no
// database yet) only records the image. It returns the backup's path, ""
// when none was needed.
func BackupOnImageChange(ctx context.Context, o BackupOptions, image, marker string) (string, error) {
	if image == "" || marker == "" {
		return "", errors.New("the image and the marker file are required")
	}
	recorded, err := os.ReadFile(marker) // #nosec G304 -- path from the container's arguments.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read the recorded image: %w", err)
	}
	if strings.TrimSpace(string(recorded)) == image {
		return "", nil
	}
	path := ""
	if _, err := os.Stat(o.SettingsDB); err == nil {
		if path, err = Backup(ctx, o); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("settings database: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return path, err
	}
	// Recorded only after the copy: a failed backup is retried at the
	// next start, before pgAdmin ever opens the database with the new
	// image.
	return path, os.WriteFile(marker, []byte(image+"\n"), 0o600)
}

// prune keeps the newest keep backups (names sort by time).
func prune(dir string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var backups []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pgadmin4-") && strings.HasSuffix(entry.Name(), ".db") {
			backups = append(backups, entry.Name())
		}
	}
	slices.Sort(backups)
	var problems []error
	for len(backups) > keep {
		if err := os.Remove(filepath.Join(dir, backups[0])); err != nil {
			problems = append(problems, err)
		}
		backups = backups[1:]
	}
	return errors.Join(problems...)
}

// lock takes an exclusive flock on path, waiting for ctx.
func lock(ctx context.Context, path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- path from the Job's arguments.
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", path, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func waitForFile(ctx context.Context, path string, limit time.Duration, writer string) error {
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s has not created %s after %s", writer, path, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForReady polls url until it answers 200, for at most limit.
func waitForReady(ctx context.Context, url string, limit time.Duration) error {
	client := &http.Client{Timeout: 4 * time.Second}
	deadline := time.Now().Add(limit)
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request) // #nosec G704 -- url from the Pod's arguments, the loopback.
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pgAdmin has not answered %s after %s", url, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- path from the Job's arguments.
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes, over %d", filepath.Base(path), info.Size(), limit)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s grew over %d bytes", filepath.Base(path), limit)
	}
	return raw, nil
}

// bounded keeps the tail of a command's output (setup.py prints the error
// last). No password ever reaches setup.py, so nothing needs redacting.
func bounded(output []byte) string {
	text := strings.TrimSpace(string(output))
	if len(text) <= maxOutputBytes {
		return text
	}
	cut := len(text) - maxOutputBytes
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return "…" + text[cut:]
}
