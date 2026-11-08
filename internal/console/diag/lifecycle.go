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

package diag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/fyannk/pgToolBox/pkg/diagnose"
	"github.com/fyannk/pgToolBox/pkg/diagnose/catalog"
)

// maxLifecycleBytes bounds the table read: a few dozen lines of JSON.
const maxLifecycleBytes = 256 << 10

// LifecycleTable is the end-of-life table in force: the deployment's
// (LIFECYCLE_FILE, the chart's lifecycle ConfigMap mounted as a file) when
// it reads, the built-in one otherwise. It reloads when the ConfigMap
// changes, so an edit takes effect at the next diagnostics run, without a
// restart or a release.
type LifecycleTable struct {
	path   string
	logger *slog.Logger
	mu     sync.RWMutex
	cur    diagnose.Lifecycle
	digest string
}

// NewLifecycleTable loads path once; an empty path is the built-in table.
func NewLifecycleTable(path string, logger *slog.Logger) *LifecycleTable {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	t := &LifecycleTable{path: path, logger: logger}
	t.reload()
	return t
}

// Current returns the table in force.
func (t *LifecycleTable) Current() diagnose.Lifecycle {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.cur
}

func (t *LifecycleTable) reload() {
	next, problem := loadLifecycle(t.path)
	digest := next.Source + "|" + problem + "|" + fmt.Sprint(next.Reviewed, next.Lines)
	t.mu.Lock()
	changed := digest != t.digest
	t.cur, t.digest = next, digest
	t.mu.Unlock()
	switch {
	case !changed:
	case problem != "":
		t.logger.Warn("lifecycle table refused; the built-in one applies", slog.String("problem", problem))
	default:
		t.logger.Info("lifecycle table loaded", slog.String("source", next.Source), slog.String("reviewed", next.Reviewed))
	}
}

// loadLifecycle reads the file, falling back to the built-in table with
// the reason quoted in its source, so a finding says which table spoke.
func loadLifecycle(path string) (diagnose.Lifecycle, string) {
	builtIn := catalog.Lifecycle()
	if path == "" {
		return builtIn, ""
	}
	f, err := os.Open(path) //nolint:gosec // an operator-configured mount path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The ConfigMap is optional in the mount: absent means built-in.
			return builtIn, ""
		}
		builtIn.Source = "built-in; " + path + " unreadable"
		return builtIn, err.Error()
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxLifecycleBytes+1))
	if err == nil && len(data) > maxLifecycleBytes {
		err = errors.New("larger than 256 KiB")
	}
	var l diagnose.Lifecycle
	if err == nil {
		l, err = catalog.ParseLifecycle(data, "ConfigMap, "+filepath.Base(path))
	}
	if err != nil {
		builtIn.Source = "built-in; the lifecycle ConfigMap was refused"
		return builtIn, err.Error()
	}
	return l, ""
}

// Watch reloads the table on every change of its directory (a ConfigMap
// mount swaps ..data atomically) until ctx is done.
func (t *LifecycleTable) Watch(ctx context.Context) error {
	if t.path == "" {
		<-ctx.Done()
		return nil
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(filepath.Dir(t.path)); err != nil {
		return err
	}
	t.reload()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-watcher.Events:
			if !ok {
				return errors.New("lifecycle table watch closed")
			}
			t.reload()
		case werr, ok := <-watcher.Errors:
			if !ok {
				return errors.New("lifecycle table watch closed")
			}
			t.logger.Warn("lifecycle table watch error", slog.String("error", werr.Error()))
		}
	}
}
