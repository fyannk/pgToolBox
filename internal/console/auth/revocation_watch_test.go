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

package auth

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// compactedAPI serves a revocation list left unchanged longer than the API
// server keeps history: a watch from its resourceVersion is refused as
// expired, as a real API server does.
type compactedAPI struct {
	mu      sync.Mutex
	cm      *corev1.ConfigMap
	watches []*watch.RaceFreeFakeWatcher
}

func (a *compactedAPI) Get(context.Context, string, metav1.GetOptions) (*corev1.ConfigMap, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cm.DeepCopy(), nil
}

func (a *compactedAPI) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if opts.ResourceVersion != "" {
		return nil, apierrors.NewResourceExpired("too old resource version: " + opts.ResourceVersion)
	}
	w := watch.NewRaceFreeFake()
	a.watches = append(a.watches, w)
	return w, nil
}

func (a *compactedAPI) open() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.watches)
}

func TestRevocationsFollowAListOlderThanTheWatchHistory(t *testing.T) {
	api := &compactedAPI{cm: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "revocations", ResourceVersion: "474228"}}}
	r := NewRevocations(api, "revocations", nil)
	r.backoff = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(5 * time.Second)
	for api.open() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the watch never opened")
		}
		time.Sleep(time.Millisecond)
	}
	sid := strings.Repeat("ab", 16)
	revoked := api.cm.DeepCopy()
	revoked.ResourceVersion = "2180574"
	revoked.Data = map[string]string{"sessions": sid + "\n"}
	api.mu.Lock()
	api.watches[0].Modify(revoked)
	api.mu.Unlock()
	for !r.Revoked(sid, "bob", time.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("a session revoked after start-up was never refused")
		}
		time.Sleep(time.Millisecond)
	}
}

// hungOnce is a revocation list whose first read the API server never
// answers (a request queued by its flow control, a dead pooled
// connection): the read gives up after readTimeout and is retried, rather
// than leaving the list unread for good (round 21, k8s P3-3).
type hungOnce struct {
	compactedAPI
	gets atomic.Int32
}

func (h *hungOnce) Get(ctx context.Context, name string, o metav1.GetOptions) (*corev1.ConfigMap, error) {
	if h.gets.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return h.compactedAPI.Get(ctx, name, o)
}

func TestARevocationReadThatHangsIsRetried(t *testing.T) {
	api := &hungOnce{compactedAPI: compactedAPI{cm: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "revocations", ResourceVersion: "7"}}}}
	r := NewRevocations(api, "revocations", slog.New(slog.DiscardHandler))
	r.backoff = func(int) time.Duration { return time.Millisecond }
	r.readTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-r.Ready():
	case <-time.After(5 * time.Second):
		t.Fatalf("the list was never read after a read that hung (%d reads)", api.gets.Load())
	}
	if n := api.gets.Load(); n < 2 {
		t.Errorf("%d reads, want the hung one given up and retried", n)
	}
}
