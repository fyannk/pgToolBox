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

package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// silentAPI is one ConfigMap under the API server's optimistic
// concurrency whose watches never deliver anything: a replica sees what
// another wrote only by reading it (LoadGrants).
type silentAPI struct {
	mu sync.Mutex
	cm *corev1.ConfigMap
	rv int
}

func newSilentAPI() *silentAPI {
	return &silentAPI{cm: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "console-state", Namespace: "team-a", ResourceVersion: "1"}}, rv: 1}
}

func (a *silentAPI) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cm.DeepCopy(), nil
}

func (a *silentAPI) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	return watch.NewRaceFreeFake(), nil
}

func (a *silentAPI) Update(_ context.Context, cm *corev1.ConfigMap, _ metav1.UpdateOptions) (*corev1.ConfigMap, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cm.ResourceVersion != a.cm.ResourceVersion {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("stale"))
	}
	a.rv++
	a.cm = cm.DeepCopy()
	a.cm.ResourceVersion = strconv.Itoa(a.rv)
	return a.cm.DeepCopy(), nil
}

// sharedReplica is a harness whose confirmations are kept in the
// ConfigMap of a, as one replica of several, the MCP leader while lead
// says so.
func sharedReplica(t *testing.T, a *silentAPI, lead func() bool) (harness, *recordingOperator, *sharedstate.Store) {
	t.Helper()
	store := sharedstate.New(sharedstate.Options{
		Client: a, Name: "console-state", Now: func() time.Time { return fixtures.Now }, Logger: slog.New(slog.DiscardHandler),
	})
	store.SetLeader(lead)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = store.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for !store.MarksKnown() {
		if time.Now().After(deadline) {
			t.Fatal("the store never read its ConfigMap")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.Confirmations = store })
	return h, op, store
}

// A confirmation spent on the MCP leader is refused by the next one: the
// spend is written through before the tool runs, and the next leader
// loads it (the re-review's TestRev2ReplayOtherReplicaBeforeSync, after a
// failover).
func TestConfirmationReplayAfterAFailover(t *testing.T) {
	t.Parallel()
	shared := newSilentAPI()
	var leader atomic.Value
	leader.Store("a")
	a, opA, sa := sharedReplica(t, shared, func() bool { return leader.Load() == "a" })
	b, opB, sb := sharedReplica(t, shared, func() bool { return leader.Load() == "b" })
	if err := sa.LoadGrants(context.Background()); err != nil {
		t.Fatal(err)
	}
	orders := map[string]any{"namespace": "team-a", "name": "orders"}

	cs, sniffed := formSniffedClient(t, a.url, "tok-dba")
	if res, _ := callTool(t, cs, "backup", orders); res.IsError {
		t.Fatalf("the first call: %s", text(res))
	}
	if len(opA.requests()) != 1 {
		t.Fatalf("replica A ran %d operations", len(opA.requests()))
	}
	sniffed.mu.Lock()
	state := sniffed.state
	sniffed.mu.Unlock()
	if state == "" {
		t.Fatal("no request state was sniffed")
	}
	leader.Store("b")
	sa.DropGrants()
	if err := sb.LoadGrants(context.Background()); err != nil {
		t.Fatal(err)
	}
	csB, _ := formSniffedClient(t, b.url, "tok-dba")
	res, err := csB.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "backup", Arguments: orders, RequestState: state,
		InputResponses: mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmField: "orders"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(text(res), "already used") || len(opB.requests()) != 0 {
		t.Errorf("the next leader on A's spent confirmation: error %v %q, ran %d", res.IsError, text(res), len(opB.requests()))
	}
}

// formSniffedClient answers a form confirmation with the name it asks
// for, and keeps the request state it sends back.
func formSniffedClient(t *testing.T, url, token string) (*mcp.ClientSession, *stateSniffer) {
	t.Helper()
	sn := &stateSniffer{token: token}
	c := mcp.NewClient(&mcp.Implementation{Name: "Claude Code", Version: "v0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}},
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			props, _ := req.Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)
			field, _ := props[confirmField].(map[string]any)
			title, _ := field["title"].(string)
			typed := strings.TrimSuffix(strings.TrimPrefix(title, "Type "), " to confirm")
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmField: typed}}, nil
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: sn}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, sn
}
