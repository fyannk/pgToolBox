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

package passfilesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"sync"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/fyannk/pgToolBox/internal/operator/adminsync"
	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/declared"
)

// CloudNativePG's kinds the declared mode reads, by name only.
var (
	clustersResource      = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "clusters"}
	databaseRolesResource = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "databaseroles"}
	poolersResource       = schema.GroupVersionResource{Group: "postgresql.cnpg.io", Version: "v1", Resource: "poolers"}
)

// DeclaredOptions configure the sidecar in the declared mode.
type DeclaredOptions struct {
	// Client reads Secrets; Dynamic reads CloudNativePG's kinds.
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	// Namespace is the Pod's: every object the list names is in it.
	Namespace string
	Document  declared.Document
	Files     Files
	// WithheldFile, when set, is where the servers the registration must
	// leave out are written (declared.Withheld), on a volume shared with
	// the registration sidecar.
	WithheldFile string
	// CADir is where the servers' caConfigMaps are mounted
	// (declared.CAConfigMapFile); required when a server names one.
	CADir string
	Log   logr.Logger
	// Debounce coalesces bursts of changes into one rewrite.
	Debounce time.Duration
	// Backoff is the wait before a failed get or watch is retried.
	Backoff time.Duration
}

// declaredState is what the sidecar has seen; guarded by mu.
type declaredState struct {
	mu      sync.Mutex
	live    Live
	changed chan struct{}
}

func (s *declaredState) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// RunDeclared follows every object the list names (get and watch by name)
// until ctx ends, resolving and rewriting the files on every change. The
// list itself is fixed for the Pod's life: the chart rolls the Pod when it
// changes.
func RunDeclared(ctx context.Context, opts DeclaredOptions) error {
	if opts.Debounce == 0 {
		opts.Debounce = 500 * time.Millisecond
	}
	if opts.Backoff == 0 {
		opts.Backoff = 5 * time.Second
	}
	s := &declaredState{
		live: Live{
			Clusters: map[string]*cnpgv1.Cluster{}, DatabaseRoles: map[string]*cnpgv1.DatabaseRole{},
			Poolers: map[string]*cnpgv1.Pooler{}, Secrets: map[string]*corev1.Secret{},
			CAConfigMaps: map[string][]byte{},
		},
		changed: make(chan struct{}, 1),
	}
	// The CA ConfigMaps are files the kubelet keeps current; their
	// directory is watched (the kubelet swaps a ..data link on each
	// update), so a renewed CA reaches libpq's files without a restart.
	var caEvents <-chan fsnotify.Event
	var caErrors <-chan error
	readCAs := func() {}
	if configMaps := opts.Document.CAConfigMaps(); len(configMaps) > 0 {
		if opts.CADir == "" {
			return errors.New("the declared servers name a caConfigMap: the CA directory is required")
		}
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return fmt.Errorf("watch the CA ConfigMaps: %w", err)
		}
		defer func() { _ = watcher.Close() }()
		if err := watcher.Add(opts.CADir); err != nil {
			return fmt.Errorf("watch the CA ConfigMaps in %s: %w", opts.CADir, err)
		}
		caEvents, caErrors = watcher.Events, watcher.Errors
		readCAs = func() {
			files := readCAConfigMaps(opts.CADir, configMaps, opts.Log)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.live.CAConfigMaps = files
			s.notify()
		}
		readCAs()
	}
	names := opts.Document.Names()
	for _, name := range names.Secrets {
		go follow(ctx, opts.Log.WithValues("secret", name), secretSource(opts.Client), opts.Namespace, name, opts.Backoff,
			func(obj any) {
				store(s, s.live.Secrets, name, obj, func(o any) *corev1.Secret { secret, _ := o.(*corev1.Secret); return secret })
			})
	}
	for _, name := range names.Clusters {
		go follow(ctx, opts.Log.WithValues("cluster", name), dynamicSource(opts.Dynamic, clustersResource), opts.Namespace, name, opts.Backoff,
			func(obj any) { store(s, s.live.Clusters, name, obj, typed[cnpgv1.Cluster](opts.Log)) })
	}
	for _, name := range names.DatabaseRoles {
		go follow(ctx, opts.Log.WithValues("databaseRole", name), dynamicSource(opts.Dynamic, databaseRolesResource), opts.Namespace, name, opts.Backoff,
			func(obj any) { store(s, s.live.DatabaseRoles, name, obj, typed[cnpgv1.DatabaseRole](opts.Log)) })
	}
	for _, name := range names.Poolers {
		go follow(ctx, opts.Log.WithValues("pooler", name), dynamicSource(opts.Dynamic, poolersResource), opts.Namespace, name, opts.Backoff,
			func(obj any) { store(s, s.live.Poolers, name, obj, typed[cnpgv1.Pooler](opts.Log)) })
	}

	// Each server's last logged reason, so the log says what changed and
	// not the same refusal at every rewrite.
	reported := map[string]string{}
	// Watch events (a Cluster's status, a Secret's resync) rewrite the files
	// often; the log says so only when what is offered changes.
	lastOffered := -1
	// The withheld file is rewritten only when its content changes: the
	// registration re-registers on every change of the file.
	var lastWithheld []byte
	timer := time.NewTimer(opts.Debounce)
	if len(opts.Document.Servers) > 0 {
		// The first rewrite waits for the first answers, so the log does
		// not report every server absent before anything was read.
		timer.Stop()
	}
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-s.changed:
			timer.Reset(opts.Debounce)
		case _, ok := <-caEvents:
			if !ok {
				return errors.New("the CA ConfigMap watch closed")
			}
			readCAs()
		case _, ok := <-caErrors:
			// An overflow loses events, not the files: read them again.
			if !ok {
				return errors.New("the CA ConfigMap watch closed")
			}
			readCAs()
		case <-timer.C:
			s.mu.Lock()
			live := Live{
				Clusters: maps.Clone(s.live.Clusters), DatabaseRoles: maps.Clone(s.live.DatabaseRoles),
				Poolers: maps.Clone(s.live.Poolers), Secrets: maps.Clone(s.live.Secrets),
				CAConfigMaps: maps.Clone(s.live.CAConfigMaps),
			}
			s.mu.Unlock()
			connections := Resolve(opts.Document, opts.Namespace, live)
			outcome, err := RenderDeclared(opts.Files, connections)
			if err != nil {
				opts.Log.Error(err, "writing the credential files failed; retrying")
				timer.Reset(opts.Backoff)
				continue
			}
			if opts.WithheldFile != "" {
				raw, err := WithheldOf(connections).Encode()
				if err == nil && !bytes.Equal(raw, lastWithheld) {
					err = adminsync.WriteFile(opts.WithheldFile, string(raw))
				}
				if err != nil {
					opts.Log.Error(err, "writing the withheld servers failed; retrying")
					timer.Reset(opts.Backoff)
					continue
				}
				lastWithheld = raw
			}
			changed := outcome.Offered != lastOffered
			for _, c := range connections {
				reason := c.Reason()
				if previous, seen := reported[c.Name]; seen && previous == reason {
					continue
				}
				changed = true
				reported[c.Name] = reason
				switch {
				case c.Withheld:
					opts.Log.Info("server not registered in pgAdmin: its Cluster cannot serve the endpoint", "server", c.Name, "reason", reason)
				case reason != "":
					opts.Log.Info("server has no password in the credential files", "server", c.Name, "reason", reason)
				case c.Password == "":
					opts.Log.Info("server declared without credentials; pgAdmin asks for the password", "server", c.Name)
				default:
					opts.Log.Info("server credential verified", "server", c.Name)
				}
			}
			if changed {
				opts.Log.Info("credential files written", "offered", outcome.Offered, "of", len(connections))
			}
			lastOffered = outcome.Offered
		}
	}
}

// readCAConfigMaps reads the ca.crt of each mounted CA ConfigMap, at most
// MaxSecretBytes+1 bytes (caPEM refuses a larger one). A missing file (an
// optional ConfigMap not created yet, or without ca.crt) is left out
// silently: the server's reason says so; another error is logged.
func readCAConfigMaps(dir string, configMaps []string, log logr.Logger) map[string][]byte {
	out := make(map[string][]byte, len(configMaps))
	for _, name := range configMaps {
		raw, err := readBounded(declared.CAConfigMapFile(dir, name), MaxSecretBytes+1)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			log.Error(err, "unreadable CA ConfigMap file", "configMap", name)
		default:
			out[name] = raw
		}
	}
	return out
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- under the mounted CA directory, named by the validated list.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit))
}

// store files obj (nil: absent or not readable) under name and asks for a
// rewrite.
func store[T any](s *declaredState, into map[string]*T, name string, obj any, convert func(any) *T) {
	value := convert(obj)
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == nil {
		delete(into, name)
	} else {
		into[name] = value
	}
	s.notify()
}

// typed converts a dynamic client's object into T; nil when obj is nil or
// does not convert (logged: a CloudNativePG version whose object this
// binary cannot read is a reason, never a guess).
func typed[T any](log logr.Logger) func(any) *T {
	return func(obj any) *T {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u == nil {
			return nil
		}
		out := new(T)
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out); err != nil {
			log.Error(err, "unreadable object", "kind", u.GetKind(), "name", u.GetName())
			return nil
		}
		return out
	}
}

func dynamicSource(client dynamic.Interface, resource schema.GroupVersionResource) source {
	return source{
		get: func(ctx context.Context, namespace, name string) (any, string, error) {
			obj, err := client.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, "", err
			}
			return obj, obj.GetResourceVersion(), nil
		},
		watch: func(ctx context.Context, namespace string, options metav1.ListOptions) (watch.Interface, error) {
			return client.Resource(resource).Namespace(namespace).Watch(ctx, options)
		},
	}
}
