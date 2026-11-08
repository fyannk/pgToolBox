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
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/fyannk/pgToolBox/internal/operator/pgadmin/offer"
)

// Options configure the sidecar.
type Options struct {
	Client    kubernetes.Interface
	Namespace string
	// OfferSecret is the offer Secret's name.
	OfferSecret string
	Files       Files
	Log         logr.Logger
	// Debounce coalesces bursts of changes into one rewrite.
	Debounce time.Duration
	// Backoff is the wait before a failed get or watch is retried.
	Backoff time.Duration
}

// state is what the sidecar has seen; guarded by mu.
type state struct {
	mu      sync.Mutex
	doc     *offer.Document
	secrets map[string]*corev1.Secret
	watches map[string]context.CancelFunc
	changed chan struct{}
}

func (s *state) notify() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Run watches the offer and its Secrets until ctx ends, rewriting the files
// on every change.
func Run(ctx context.Context, opts Options) error {
	if opts.Debounce == 0 {
		opts.Debounce = 500 * time.Millisecond
	}
	if opts.Backoff == 0 {
		opts.Backoff = 5 * time.Second
	}
	s := &state{secrets: map[string]*corev1.Secret{}, watches: map[string]context.CancelFunc{}, changed: make(chan struct{}, 1)}
	go watchByName(ctx, opts, offer.SecretRef{Name: opts.OfferSecret}, func(obj any) {
		holder, _ := obj.(*corev1.Secret)
		s.mu.Lock()
		defer s.mu.Unlock()
		if holder == nil {
			s.doc = nil
		} else if doc, err := offer.Parse(holder.Data[offer.SecretKey]); err != nil {
			opts.Log.Error(err, "offer rejected; the credential files keep their last content", "secret", opts.OfferSecret)
			return
		} else {
			s.doc = &doc
		}
		s.resubscribe(ctx, opts)
		s.notify()
	})

	timer := time.NewTimer(opts.Debounce)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.changed:
			timer.Reset(opts.Debounce)
		case <-timer.C:
			s.mu.Lock()
			if s.doc == nil {
				s.mu.Unlock()
				continue
			}
			doc := *s.doc
			secrets := maps.Clone(s.secrets)
			s.mu.Unlock()
			outcome, err := Render(opts.Files, doc, secrets)
			if err != nil {
				opts.Log.Error(err, "writing the credential files failed; retrying")
				timer.Reset(opts.Backoff)
				continue
			}
			opts.Log.Info("credential files written", "offered", outcome.Offered, "of", len(doc.Servers), "refused", outcome.Refused)
		}
	}
}

// resubscribe starts a watch for every Secret the offer names, in the
// PgAdmin's namespace and in the namespaces a PgAdminAccess grants, and
// stops the others. Called with mu held.
func (s *state) resubscribe(ctx context.Context, opts Options) {
	var wanted []offer.SecretRef
	if s.doc != nil {
		wanted = s.doc.SecretRefs()
	}
	keys := make([]string, 0, len(wanted))
	for _, ref := range wanted {
		keys = append(keys, ref.Key())
	}
	for key, cancel := range s.watches {
		if !slices.Contains(keys, key) {
			cancel()
			delete(s.watches, key)
			delete(s.secrets, key)
		}
	}
	for _, ref := range wanted {
		name := ref.Key()
		if _, ok := s.watches[name]; ok {
			continue
		}
		watchCtx, cancel := context.WithCancel(ctx)
		s.watches[name] = cancel
		go watchByName(watchCtx, opts, ref, func(obj any) {
			secret, _ := obj.(*corev1.Secret)
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, still := s.watches[name]; !still {
				return
			}
			if secret == nil {
				delete(s.secrets, name)
			} else {
				s.secrets[name] = secret
			}
			s.notify()
		})
	}
}

// watchByName follows one Secret by name (follow).
func watchByName(ctx context.Context, opts Options, ref offer.SecretRef, onChange func(any)) {
	namespace := ref.Namespace
	if namespace == "" {
		namespace = opts.Namespace
	}
	follow(ctx, opts.Log.WithValues("secret", ref.Key()), secretSource(opts.Client), namespace, ref.Name, opts.Backoff, onChange)
}

// source reads one kind of object by name: get, and a watch the caller
// scopes to that name with a field selector.
type source struct {
	get   func(ctx context.Context, namespace, name string) (any, string, error)
	watch func(ctx context.Context, namespace string, options metav1.ListOptions) (watch.Interface, error)
}

func secretSource(client kubernetes.Interface) source {
	return source{
		get: func(ctx context.Context, namespace, name string) (any, string, error) {
			secret, err := client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, "", err
			}
			return secret, secret.ResourceVersion, nil
		},
		watch: func(ctx context.Context, namespace string, options metav1.ListOptions) (watch.Interface, error) {
			return client.CoreV1().Secrets(namespace).Watch(ctx, options)
		},
	}
}

// follow follows one object by name with get + watch (the only verbs a
// name-scoped Role allows: the API server authorizes a watch against
// resourceNames when its field selector is metadata.name=<name>), calling
// onChange with the object, or nil when it is absent or not readable. It
// re-gets after every closed watch, so a missed event costs at most one
// round trip.
func follow(ctx context.Context, log logr.Logger, src source, namespace, name string, backoff time.Duration, onChange func(any)) {
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	for ctx.Err() == nil {
		obj, version, err := src.get(ctx, namespace, name)
		switch {
		case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
			log.Info("not readable", "reason", reasonOf(err))
			onChange(nil)
		case err != nil:
			log.Error(err, "get failed; retrying")
			sleep(ctx, backoff)
			continue
		default:
			onChange(obj)
		}
		options := metav1.ListOptions{FieldSelector: selector, ResourceVersion: version, AllowWatchBookmarks: true}
		w, err := src.watch(ctx, namespace, options)
		if err != nil {
			log.Info("watch failed; retrying", "reason", reasonOf(err))
			sleep(ctx, backoff)
			continue
		}
		drain(ctx, w, name, onChange)
		w.Stop()
	}
}

// drain delivers the watch's events for name until it closes. The field
// selector already scopes the watch to that name; an event for another
// object (from a server, proxy or fake that ignores the selector) is never
// filed under it, and neither is another object's deletion.
func drain(ctx context.Context, w watch.Interface, name string, onChange func(any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.ResultChan():
			if !ok {
				return
			}
			if named, isNamed := event.Object.(interface{ GetName() string }); isNamed && named.GetName() != name {
				continue
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				onChange(event.Object)
			case watch.Deleted:
				onChange(nil)
			case watch.Error:
				return
			case watch.Bookmark:
				// Progress marker only: nothing changed.
			}
		}
	}
}

// reasonOf names an API error without its body (which could echo a
// request).
func reasonOf(err error) string {
	if status, ok := err.(apierrors.APIStatus); ok {
		return fmt.Sprintf("%s (%d)", status.Status().Reason, status.Status().Code)
	}
	return "transport error"
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
