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

package readmodel

import (
	"testing"

	"k8s.io/client-go/rest"

	"github.com/fyannk/pgToolBox/internal/console/config"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

func load(t *testing.T, env map[string]string) config.Console {
	t.Helper()
	cfg, err := config.LoadConsole(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewWiresEveryPart(t *testing.T) {
	cfg := load(t, map[string]string{config.EnvWatchNamespaces: "team-a"})
	rm, err := New(cfg, &rest.Config{Host: "https://127.0.0.1:6443"}, nil, Identity{Version: "test", Namespace: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.Ready() {
		t.Error("ready before the cache synced")
	}
	names := map[string]bool{}
	for _, r := range rm.Runners() {
		names[r.Name] = r.Core
	}
	for _, core := range []string{"informer-cache", "instance-status-sweep", "diagnostics"} {
		if !names[core] {
			t.Errorf("%s missing or not core: %v", core, names)
		}
	}
	if _, ok := names["history-journal"]; ok {
		t.Error("an in-memory history has a journal runner")
	}
	// Without a CA bundle the sweep is up but every instance is "not
	// verifiable"; the state says why.
	if st := rm.Sweep.State(); st.State != viewmodel.SourcePending {
		t.Errorf("sweep before a run = %+v", st)
	}
}

func TestDisabledParts(t *testing.T) {
	cfg := load(t, map[string]string{
		config.EnvWatchNamespaces: "team-a", config.EnvInstanceStatusEnabled: "false",
		config.EnvDiagnosticsEnabled: "false", config.EnvHistoryEnabled: "false",
	})
	rm, err := New(cfg, &rest.Config{Host: "https://127.0.0.1:6443"}, nil, Identity{Version: "test", Namespace: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.History != nil {
		t.Error("history built while disabled")
	}
	if rm.Sweep.State().State != viewmodel.SourceDisabled || rm.Diagnostics.State().State != viewmodel.SourceDisabled {
		t.Errorf("sweep %+v diagnostics %+v", rm.Sweep.State(), rm.Diagnostics.State())
	}
}

// TestPersistenceWiring: without HISTORY_PATH or STATE_CONFIGMAP nothing
// is snapshotted or shared, as before; with them, each has its runner and
// the persisted parts load from the history's directory.
func TestPersistenceWiring(t *testing.T) {
	plain := load(t, map[string]string{config.EnvWatchNamespaces: "team-a"})
	rm, err := New(plain, &rest.Config{Host: "https://127.0.0.1:6443"}, nil, Identity{Version: "test", Namespace: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.Snapshots != nil || rm.Shared != nil {
		t.Fatal("snapshots or shared state built without HISTORY_PATH and STATE_CONFIGMAP")
	}
	for _, r := range rm.Runners() {
		if r.Name == "state-snapshots" || r.Name == "shared-state" {
			t.Fatalf("runner %s without its setting", r.Name)
		}
	}

	dir := t.TempDir()
	cfg := load(t, map[string]string{
		config.EnvWatchNamespaces: "team-a", config.EnvHistoryPath: dir + "/history.db", config.EnvStateConfigMap: "pgtoolbox-console-state",
	})
	rm, err = New(cfg, &rest.Config{Host: "https://127.0.0.1:6443"}, nil, Identity{Version: "test", Namespace: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if rm.Snapshots == nil || rm.Shared == nil || !rm.Shared.Enabled() {
		t.Fatal("snapshots or shared state missing")
	}
	if got := rm.Snapshots.Loaded("sweep"); got != "none" {
		t.Errorf("sweep snapshot on an empty volume = %q", got)
	}
	names := map[string]bool{}
	for _, r := range rm.Runners() {
		names[r.Name] = r.Core
	}
	if core, ok := names["state-snapshots"]; !ok || core {
		t.Error("state-snapshots missing or core")
	}
	if core, ok := names["shared-state"]; !ok || core {
		t.Error("shared-state missing or core: a missing ConfigMap must not make the console unready")
	}

	if _, err := New(cfg, &rest.Config{Host: "https://127.0.0.1:6443"}, nil, Identity{Version: "test"}); err == nil {
		t.Error("STATE_CONFIGMAP without the console's namespace accepted")
	}
}
