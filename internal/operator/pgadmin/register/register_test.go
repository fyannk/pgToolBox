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

package register

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

const setupPath = "/pgadmin4/setup.py"

// fakeSetup plays setup.py: it records every invocation, writes a dump,
// captures the load document, and answers get-users and the version.
type fakeSetup struct {
	mu       sync.Mutex
	calls    [][]string
	dump     string
	users    string
	loaded   []byte
	failOn   string
	loadArgs []string
}

func (f *fakeSetup) run(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if len(args) > 1 && args[1] == f.failOn {
		return []byte("Traceback ...\nsqlite3.OperationalError: database is locked"), errors.New("exit status 1")
	}
	if args[0] == "-c" {
		return []byte("9.18\n"), nil
	}
	switch args[1] {
	case "dump-servers":
		if f.dump != "" {
			if err := os.WriteFile(args[2], []byte(f.dump), 0o600); err != nil {
				return nil, err
			}
		}
	case "load-servers":
		raw, err := os.ReadFile(args[2])
		if err != nil {
			return nil, err
		}
		f.loaded, f.loadArgs = raw, args
	case "get-users":
		return []byte(f.users), nil
	}
	return []byte("ok"), nil
}

func (f *fakeSetup) commands() []string {
	var out []string
	for _, c := range f.calls {
		if c[0] == setupPath {
			out = append(out, strings.Join(c[1:], " "))
		}
	}
	return out
}

func testOffer(t *testing.T, dir string) string {
	t.Helper()
	raw, err := offer.Encode(offer.Document{Servers: []offer.Server{{
		Name: "one rw (app)", Cluster: "one", Endpoint: "rw", Host: "one-rw.team.svc", Port: 5432,
		DBName: "app", User: "app", Service: "pgtoolbox-one-rw-app", SecretName: "one-app", CASecretName: "one-ca",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "offer.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func options(t *testing.T, fake *fakeSetup, admins ...string) Options {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "pgadmin4.db")
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		OfferFile: testOffer(t, dir), Owner: "pgtoolbox@pgtoolbox.fyannk.dev", AuthSource: "oauth2",
		Admins: admins, SettingsDB: db, LockFile: filepath.Join(dir, ".pgtoolbox", "setup.lock"),
		TempDir: dir, Setup: setupPath, WaitForDB: time.Second, Run: fake.run,
	}
}

func TestRegisterMergesManagedServers(t *testing.T) {
	fake := &fakeSetup{
		dump:  `{"Servers":{"2":{"Name":"mine-b","Group":"Mine"},"1":{"Name":"mine-a","Group":"Mine"},"3":{"Name":"stale","Group":"PgToolBox"}}}`,
		users: "[]",
	}
	report, err := Register(context.Background(), options(t, fake))
	if err != nil {
		t.Fatal(err)
	}
	if report.Servers != 1 || report.PgAdminVersion != "9.18" {
		t.Fatalf("report = %+v", report)
	}
	cmds := fake.commands()
	if len(cmds) < 2 || !strings.HasPrefix(cmds[0], "dump-servers ") || !strings.HasPrefix(cmds[1], "load-servers ") {
		t.Fatalf("commands = %v", cmds)
	}
	if !slices.Contains(fake.loadArgs, "--replace") || !slices.Contains(fake.loadArgs, "pgtoolbox@pgtoolbox.fyannk.dev") {
		t.Fatalf("load-servers args = %v", fake.loadArgs)
	}
	var doc struct {
		Servers map[string]map[string]any `json:"Servers"`
	}
	if err := json.Unmarshal(fake.loaded, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 3 || doc.Servers["1"]["Name"] != "mine-a" || doc.Servers["2"]["Name"] != "mine-b" {
		t.Fatalf("own servers not kept in order: %v", doc.Servers)
	}
	managed := doc.Servers["3"]
	if managed["Name"] != "one rw (app)" || managed["Group"] != offer.ManagedGroup || managed["Shared"] != true ||
		managed["SharedUsername"] != "app" || managed["Service"] != "pgtoolbox-one-rw-app" {
		t.Fatalf("managed entry = %v", managed)
	}
	if params, _ := managed["ConnectionParameters"].(map[string]any); params["sslmode"] != "verify-full" {
		t.Fatalf("sslmode = %v", managed["ConnectionParameters"])
	}
	for key := range managed {
		if strings.Contains(strings.ToLower(key), "pass") {
			t.Fatalf("managed entry carries %s", key)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(fake.loadArgs[1]), "pgtoolbox-load.json")); !os.IsNotExist(err) {
		t.Fatal("load document left behind")
	}
}

func TestRegisterWithoutDump(t *testing.T) {
	fake := &fakeSetup{users: ""}
	if _, err := Register(context.Background(), options(t, fake)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fake.loaded), "one rw (app)") {
		t.Fatalf("loaded = %s", fake.loaded)
	}
}

func TestRegisterSyncsAdministrators(t *testing.T) {
	fake := &fakeSetup{users: `Loading...
[
{"username": "alice@corp", "role": "User", "auth_source": "oauth2"},
{"username": "carol@corp", "role": "Administrator", "auth_source": "oauth2"},
{"username": "dave@corp", "role": "Administrator", "auth_source": "webserver"},
{"username": "pgtoolbox@pgtoolbox.fyannk.dev", "role": "Administrator", "auth_source": "internal"}
]
done`}
	if _, err := Register(context.Background(), options(t, fake, "alice@corp", "bob@corp")); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(fake.commands(), "\n")
	for _, want := range []string{
		"update-external-user alice@corp --auth-source oauth2 --role Administrator",
		"add-external-user bob@corp --auth-source oauth2 --role Administrator",
		"update-external-user carol@corp --auth-source oauth2 --role User",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in\n%s", want, cmds)
		}
	}
	for _, untouched := range []string{"dave@corp", "update-external-user pgtoolbox@"} {
		if strings.Contains(cmds, untouched) {
			t.Errorf("%q was touched:\n%s", untouched, cmds)
		}
	}
}

func TestRegisterSurfacesSetupFailure(t *testing.T) {
	for _, step := range []string{"dump-servers", "load-servers", "get-users"} {
		t.Run(step, func(t *testing.T) {
			fake := &fakeSetup{failOn: step, users: "[]"}
			_, err := Register(context.Background(), options(t, fake))
			if err == nil || !strings.Contains(err.Error(), step) || !strings.Contains(err.Error(), "database is locked") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestRegisterRefusesInvalidOffer(t *testing.T) {
	fake := &fakeSetup{}
	o := options(t, fake)
	if err := os.WriteFile(o.OfferFile, []byte(`{"version":9,"servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(context.Background(), o); err == nil {
		t.Fatal("an invalid offer was registered")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("setup.py ran: %v", fake.calls)
	}
}

func TestRegisterWaitsForDatabase(t *testing.T) {
	fake := &fakeSetup{}
	o := options(t, fake)
	o.SettingsDB = filepath.Join(t.TempDir(), "absent.db")
	o.WaitForDB = 50 * time.Millisecond
	if _, err := Register(context.Background(), o); err == nil || !strings.Contains(err.Error(), "has not created") {
		t.Fatalf("error = %v", err)
	}
}

func TestRegisterWaitsForPgAdminToAnswer(t *testing.T) {
	var pings atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Still creating its database on the first ping.
		if pings.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(server.Close)
	fake := &fakeSetup{}
	o := options(t, fake)
	o.ReadyURL = server.URL
	if _, err := Register(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if pings.Load() < 2 || len(fake.calls) == 0 {
		t.Fatalf("pings = %d, setup.py calls = %d", pings.Load(), len(fake.calls))
	}
}

func TestRegisterNeverTouchesTheDatabaseBeforePgAdminAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	fake := &fakeSetup{}
	o := options(t, fake)
	o.ReadyURL = server.URL
	o.WaitForDB = 50 * time.Millisecond
	if _, err := Register(context.Background(), o); err == nil || !strings.Contains(err.Error(), "has not answered") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("setup.py ran before pgAdmin answered: %v", fake.calls)
	}
}

func TestParseUsers(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		want    int
		wantErr bool
	}{
		{"noise around", "header\n[{\"username\":\"a\",\"role\":\"User\",\"auth_source\":\"oauth2\"}]\nfooter", 1, false},
		{"empty output", "", 0, false},
		{"no user", "No users found", 0, false},
		{"broken", "[{\"username\":", 0, true},
		{"oversize", "[" + strings.Repeat(" ", maxUsersBytes) + "]", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			users, err := parseUsers([]byte(tc.output))
			if (err != nil) != tc.wantErr || len(users) != tc.want {
				t.Fatalf("users = %v, err = %v", users, err)
			}
		})
	}
}

func TestBounded(t *testing.T) {
	long := strings.Repeat("é", maxOutputBytes) + "LAST LINE"
	got := bounded([]byte(long))
	if !strings.HasSuffix(got, "LAST LINE") || len(got) > maxOutputBytes+len("…") {
		t.Fatalf("bounded kept %d bytes", len(got))
	}
}

func TestBackupKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "pgadmin4.db")
	if err := os.WriteFile(db, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(dir, "backups")
	copyRunner := func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
		// python3 -c <script> <src> <dst>
		return nil, os.WriteFile(args[len(args)-1], []byte("copy"), 0o600)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		path, err := Backup(context.Background(), BackupOptions{
			SettingsDB: db, Dir: backups, LockFile: filepath.Join(dir, "lock"), Label: "abcd1234", Keep: 2,
			Now: func() time.Time { return now.Add(time.Duration(i) * time.Minute) }, Run: copyRunner,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(path, "-abcd1234.db") || !strings.HasPrefix(filepath.Base(path), "pgadmin4-20261029T12") {
			t.Fatalf("backup path %s", path)
		}
	}
	entries, _ := os.ReadDir(backups)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"pgadmin4-20261029T120200Z-abcd1234.db", "pgadmin4-20261029T120300Z-abcd1234.db"}
	if !slices.Equal(names, want) {
		t.Fatalf("kept %v, want %v", names, want)
	}
}

func TestBackupFailureLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "pgadmin4.db")
	_ = os.WriteFile(db, []byte("sqlite"), 0o600)
	backups := filepath.Join(dir, "backups")
	failing := func(_ context.Context, _ string, _ string, args ...string) ([]byte, error) {
		_ = os.WriteFile(args[len(args)-1], []byte("half"), 0o600)
		return []byte("disk full"), errors.New("exit status 1")
	}
	_, err := Backup(context.Background(), BackupOptions{SettingsDB: db, Dir: backups, LockFile: filepath.Join(dir, "lock"), Label: "x", Keep: 3, Run: failing})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v", err)
	}
	entries, _ := os.ReadDir(backups)
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestBackupNeedsTheDatabase(t *testing.T) {
	dir := t.TempDir()
	_, err := Backup(context.Background(), BackupOptions{
		SettingsDB: filepath.Join(dir, "none.db"), Dir: dir, LockFile: filepath.Join(dir, "lock"), Keep: 1,
		Run: func(context.Context, string, string, ...string) ([]byte, error) { return nil, nil },
	})
	if err == nil {
		t.Fatal("backed up an absent database")
	}
}

func TestLockSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "setup.lock")
	unlock, err := lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := lock(short, path); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock while held: %v", err)
	}
	acquired := make(chan func(), 1)
	go func() {
		second, err := lock(context.Background(), path)
		if err == nil {
			acquired <- second
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first is held")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case second := <-acquired:
		second()
	case <-time.After(5 * time.Second):
		t.Fatal("second lock not acquired after unlock")
	}
}

func TestWaitForFileTimesOut(t *testing.T) {
	err := waitForFile(context.Background(), filepath.Join(t.TempDir(), "absent"), 10*time.Millisecond, "pgAdmin")
	if err == nil {
		t.Fatal("waited for an absent file without failing")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForFile(ctx, filepath.Join(t.TempDir(), "absent"), time.Minute, "pgAdmin"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
}

func TestMergeServersRejectsGarbage(t *testing.T) {
	if _, err := mergeServers([]byte("{not json"), offer.ManagedGroup, nil); err == nil {
		t.Fatal("garbage dump accepted")
	}
	if _, err := mergeServers([]byte(`{"Servers":{"1":"oops"}}`), offer.ManagedGroup, nil); err == nil {
		t.Fatal("malformed entry accepted")
	}
}
