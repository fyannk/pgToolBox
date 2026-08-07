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
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceDelay collapses the burst of events a Secret volume swap
// produces into a single reload.
const debounceDelay = time.Second

// Watcher hot-reloads the configuration file. Kubernetes Secret volumes
// update by swapping a "..data" symlink, so the watcher observes the
// containing directory rather than the file itself and debounces events.
// A reload that fails validation keeps the previous configuration.
type Watcher struct {
	path   string
	logger *slog.Logger
	// apply is called with each successfully reloaded configuration.
	apply func(*Config)
}

// NewWatcher builds a Watcher for path; apply receives validated configs.
func NewWatcher(path string, logger *slog.Logger, apply func(*Config)) *Watcher {
	return &Watcher{path: path, logger: logger, apply: apply}
}

// Run watches until ctx is canceled or watching fails fatally.
func (w *Watcher) Run(ctx context.Context) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating fs watcher: %w", err)
	}
	defer func() { _ = fw.Close() }()

	dir := filepath.Dir(w.path)
	if err := fw.Add(dir); err != nil {
		return fmt.Errorf("watching config directory %s: %w", dir, err)
	}

	var timer *time.Timer
	var timerC <-chan time.Time
	schedule := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(debounceDelay)
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fw.Events:
			if !ok {
				return fmt.Errorf("fs watcher events channel closed")
			}
			if w.relevant(ev) {
				schedule()
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return fmt.Errorf("fs watcher errors channel closed")
			}
			w.logger.Warn("config watcher error", "error", err)
		case <-timerC:
			timerC = nil
			w.reload()
		}
	}
}

// relevant reports whether the event may concern the config file. Secret
// volume swaps surface as "..data" symlink changes; plain writes surface
// under the file's own name.
func (w *Watcher) relevant(ev fsnotify.Event) bool {
	base := filepath.Base(ev.Name)
	return base == filepath.Base(w.path) || base == "..data"
}

func (w *Watcher) reload() {
	cfg, warns, err := Load(w.path)
	if err != nil {
		// Never log the file contents; Load errors reference fields only.
		w.logger.Error("config reload failed, keeping previous configuration", "error", err)
		return
	}
	for _, warn := range warns {
		w.logger.Warn("config warning", "warning", warn)
	}
	w.apply(cfg)
	w.logger.Info("configuration reloaded",
		"cookieSecrets", len(cfg.Session.CookieSecrets),
		"users", len(cfg.Users),
		"routes", len(cfg.Routes))
}
