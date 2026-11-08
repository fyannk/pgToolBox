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

import (
	"context"
	"errors"
	"net/http"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// cacheSyncTimeout bounds one readiness check: a probe waits at most this
// long for the informers, well under the kubelet's default 1 s probe
// timeout, and answers "not ready" rather than hanging.
const cacheSyncTimeout = 500 * time.Millisecond

// cacheSyncer is the part of the manager's cache readiness needs.
type cacheSyncer interface {
	WaitForCacheSync(ctx context.Context) bool
}

// cacheSyncedCheck is the readiness check: ready only once every informer the controllers started has synced,
// so a replica whose watches are still listing (or cannot list: RBAC, an
// unreachable API server) is held out of the metrics Service and reported
// by `kubectl get pods`. The cache runs on every replica, leader or not,
// so a standby is ready too.
func cacheSyncedCheck(cache cacheSyncer, timeout time.Duration) healthz.Checker {
	return func(request *http.Request) error {
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		if !cache.WaitForCacheSync(ctx) {
			return errors.New("the informer caches have not synced")
		}
		return nil
	}
}

// addProbes wires the probe endpoints on --health-probe-bind-address:
// /healthz is liveness (the process answers), /readyz is readiness (the
// caches have synced).
func addProbes(mgr ctrl.Manager) error {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	return mgr.AddReadyzCheck("informer-cache", cacheSyncedCheck(mgr.GetCache(), cacheSyncTimeout))
}
