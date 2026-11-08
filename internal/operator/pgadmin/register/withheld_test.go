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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

const withheldList = `{"version":1,"servers":[
 {"cluster":"orders","endpoint":"rw","credentials":{"application":{"secret":"orders-app"}}},
 {"cluster":"orders","endpoint":"ro","credentials":{"application":{"secret":"orders-app"}}}
]}`

const roName = "orders ro (application)"

// declaredOptions is a registration of withheldList, with the withheld
// file in its own directory (the shared volume).
func declaredOptions(t *testing.T, fake *fakeSetup) Options {
	t.Helper()
	o := options(t, fake)
	o.OfferFile, o.DeclaredFile, o.Namespace = "", filepath.Join(o.TempDir, "servers.json"), "team"
	o.WithheldFile = filepath.Join(t.TempDir(), declared.WithheldFileName)
	if err := os.WriteFile(o.DeclaredFile, []byte(withheldList), 0o600); err != nil {
		t.Fatal(err)
	}
	return o
}

func writeWithheld(t *testing.T, path string, servers ...declared.WithheldServer) {
	t.Helper()
	raw, err := declared.Withheld{Servers: servers}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// As the sidecar writes it: a rename into place.
	if err := adminsync.WriteFile(path, string(raw)); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeSetup) lastLoaded() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.loaded)
}

var noReplica = declared.WithheldServer{Name: roName, Reason: "no replica: the Cluster has one instance"}

// TestRegisterLeavesOutWithheldServers: a declared server the sidecar
// withholds is not registered, and the report names it with its reason.
func TestRegisterLeavesOutWithheldServers(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := declaredOptions(t, fake)
	writeWithheld(t, o.WithheldFile, noReplica)
	report, err := Register(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if report.Servers != 1 || len(report.Withheld) != 1 || report.Withheld[0] != noReplica {
		t.Fatalf("report %+v", report)
	}
	if loaded := fake.lastLoaded(); !strings.Contains(loaded, "orders rw (application)") || strings.Contains(loaded, roName) {
		t.Fatalf("loaded %s", loaded)
	}
}

// TestRegisterWaitsForTheWithheldFile: without the sidecar's file the
// registration registers nothing and says which file it waited for.
func TestRegisterWaitsForTheWithheldFile(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := declaredOptions(t, fake)
	o.WaitForDB = 10 * time.Millisecond
	if _, err := Register(context.Background(), o); err == nil || !strings.Contains(err.Error(), "passfile-sync sidecar has not created") {
		t.Fatalf("error = %v", err)
	}
	if fake.lastLoaded() != "" {
		t.Fatal("registered without the withheld file")
	}
}

// TestRegisterRefusesAnUnreadableWithheldFile: a file this binary cannot
// read is an error, never a guess at what to register.
func TestRegisterRefusesAnUnreadableWithheldFile(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := declaredOptions(t, fake)
	if err := os.WriteFile(o.WithheldFile, []byte(`{"version":2,"servers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(context.Background(), o); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("error = %v", err)
	}
}

// TestFollowWithheldRegistersOnChange: the Cluster gets a replica (the
// sidecar empties the file): the ro server is registered; it loses it
// again: the server is removed. A rewrite of the same set registers
// nothing.
func TestFollowWithheldRegistersOnChange(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := declaredOptions(t, fake)
	writeWithheld(t, o.WithheldFile, noReplica)
	first, err := Register(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var reports []Report
	done := make(chan error, 1)
	go func() {
		done <- FollowWithheld(ctx, o, first, func(r Report) {
			mu.Lock()
			defer mu.Unlock()
			reports = append(reports, r)
		})
	}()
	registrations := func() []Report {
		mu.Lock()
		defer mu.Unlock()
		return append([]Report(nil), reports...)
	}

	// The same set rewritten, then the replica: exactly one registration.
	writeWithheld(t, o.WithheldFile, noReplica)
	writeWithheld(t, o.WithheldFile)
	eventuallyTrue(t, "the ro server registered", func() bool {
		r := registrations()
		return len(r) == 1 && r[0].Servers == 2 && len(r[0].Withheld) == 0 && strings.Contains(fake.lastLoaded(), roName)
	})

	// Scaled back to one instance: the server is removed.
	writeWithheld(t, o.WithheldFile, noReplica)
	eventuallyTrue(t, "the ro server removed", func() bool {
		r := registrations()
		return len(r) == 2 && r[1].Servers == 1 && !strings.Contains(fake.lastLoaded(), roName)
	})

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("follow ended with %v", err)
	}
	if n := len(registrations()); n != 2 {
		t.Fatalf("%d registrations, want 2", n)
	}
}

// TestFollowWithheldEndsOnAFailedRegistration: the kubelet restarts the
// sidecar, which registers from scratch.
func TestFollowWithheldEndsOnAFailedRegistration(t *testing.T) {
	fake := &fakeSetup{users: "[]"}
	o := declaredOptions(t, fake)
	writeWithheld(t, o.WithheldFile, noReplica)
	first, err := Register(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.failOn = "load-servers"
	fake.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- FollowWithheld(context.Background(), o, first, func(Report) {}) }()
	writeWithheld(t, o.WithheldFile)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "load-servers") {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a failed registration did not end the follow")
	}
}

func eventuallyTrue(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
