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

package main

// Access-request wiring of the console binary: the endpoints read
// PgAccessRequests from the informer cache (the console's Role grants
// get/list/watch on them) and write as the caller.

import (
	"context"
	"log/slog"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/cache"
	"github.com/fyannk/pgToolBox/internal/console/ops"
	"github.com/fyannk/pgToolBox/internal/console/server"
)

// accessStore adapts the console cache to api.AccessStore.
type accessStore struct{ m *cache.Manager }

func (s accessStore) Namespaces() []string { return s.m.Namespaces() }

func (s accessStore) Readable(namespace string) bool {
	return s.m.Readable(namespace, cache.SourceAccessRequests)
}

// Served is the cache's discovery answer for PgAccessRequest: false only
// when the API server said it does not serve the kind (a team install
// without the operator), so the endpoints answer 501 instead of sending
// the caller after a role that would change nothing.
func (s accessStore) Served() bool {
	served := s.m.Served(cache.SourceAccessRequests)
	return served == nil || *served
}

// AccessRequests lists from the cache; a namespace whose source is not
// readable (CRD absent, Role without the grant) has none to show, and
// Readable says why.
func (s accessStore) AccessRequests(ctx context.Context, namespace string) ([]pgtoolboxv1alpha1.PgAccessRequest, error) {
	if !s.Readable(namespace) {
		return nil, nil
	}
	var list pgtoolboxv1alpha1.PgAccessRequestList
	if err := s.m.Reader().List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// accessFactory builds the access-request endpoints; writes only when the
// console serves operations (OPERATIONS_ENABLED), each as the caller.
func accessFactory(m *cache.Manager, writable bool, log *slog.Logger) server.AccessFactory {
	return func(restConfig *rest.Config) (*api.Access, error) {
		access := &api.Access{Store: accessStore{m}, Audit: log.With("component", "access")}
		if !writable {
			return access, nil
		}
		c, err := ops.NewAccessUserClient(auth.UserConfig(restConfig))
		if err != nil {
			return nil, err
		}
		access.Writer = ops.NewAccessWriter(c, 0)
		return access, nil
	}
}
