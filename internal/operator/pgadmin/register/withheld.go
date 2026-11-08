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
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

// FollowWithheld keeps the pgtools chart's registration in step with the
// passfile-sync sidecar's withheld list (declared.Withheld):
// each time the set of withheld servers differs from the one the last
// registration left out, it registers the declared list again, so a
// Cluster scaled to two instances gets its ro server back and one scaled
// to a single instance loses it. It watches the file's directory (the
// sidecar replaces the file by a rename) until ctx ends; registered is
// called after each registration. A failed registration or an unreadable
// file ends it with the error: the kubelet restarts the sidecar, which
// registers from scratch.
func FollowWithheld(ctx context.Context, o Options, last Report, registered func(Report)) error {
	if o.DeclaredFile == "" || o.WithheldFile == "" {
		return errors.New("following the withheld servers needs the declared list and the withheld file")
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()
	if err := watcher.Add(filepath.Dir(o.WithheldFile)); err != nil {
		return fmt.Errorf("watch the withheld servers: %w", err)
	}
	current := declared.Withheld{Servers: last.Withheld}.Names()
	check := func() error {
		list, err := declared.LoadWithheld(o.WithheldFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Between a rename's two events; the next event reads it.
			return nil
		case err != nil:
			return err
		case maps.Equal(list.Names(), current):
			// A rewrite of the same set (or another file's event).
			return nil
		}
		report, err := Register(ctx, o)
		if err != nil {
			return err
		}
		current = declared.Withheld{Servers: report.Withheld}.Names()
		registered(report)
		return nil
	}
	// Close the gap between the first registration and the watch.
	if err := check(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-watcher.Events:
			if !ok {
				return errors.New("withheld servers watch closed")
			}
		case _, ok := <-watcher.Errors:
			// An overflow loses events, not the file: read it again.
			if !ok {
				return errors.New("withheld servers watch closed")
			}
		}
		if err := check(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
