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

package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatcherReloadsValidAndKeepsOldOnInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	writeFile(validOIDC)

	applied := make(chan *Config, 4)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	w := NewWatcher(path, logger, func(c *Config) { applied <- c })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Give the watcher a moment to install the directory watch, then
	// rewrite with a real change: a second user.
	time.Sleep(200 * time.Millisecond)
	updated := strings.Replace(validOIDC,
		`  - {subject: "jane@corp.example", level: dba}`,
		"  - {subject: \"jane@corp.example\", level: dba}\n  - {subject: \"new@corp.example\", level: view}", 1)
	writeFile(updated)

	select {
	case c := <-applied:
		if len(c.Users) != 2 {
			t.Fatalf("reloaded config has %d users, want 2", len(c.Users))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload applied after valid rewrite")
	}

	// An invalid rewrite must not be applied; the old config stays.
	writeFile("provider:\n  modes: [bogus]\n")
	select {
	case c := <-applied:
		t.Fatalf("invalid config was applied: %d users", len(c.Users))
	case <-time.After(3 * time.Second):
		// expected: nothing applied
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watcher exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop on cancel")
	}
}
