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
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// errRestartToWatch is the cause the operator's context is cancelled with
// when a namespace left out for a failed review can now be watched: main
// exits once the manager has stopped, and the restart watches it.
var errRestartToWatch = errors.New("a watched namespace left out at start can now be watched: restarting to watch it")

// reReviewPolicy is when reReviewFailed reviews again and when it may
// restart the operator.
type reReviewPolicy struct {
	// interval is the pause between two background reviews.
	interval time.Duration
	// passes is how many reviews in a row a namespace must pass before
	// the operator restarts for it: one passing review between two
	// failing ones restarts nothing.
	passes int
	// minUptime is how long the process must have run before it restarts
	// to watch: past ten minutes kubelet resets the container's restart
	// back-off, so a tenant who makes a namespace pass and fail in turn
	// cannot hold the operator in CrashLoopBackOff.
	minUptime time.Duration
	// started is when the process started.
	started time.Time
	// now is the clock.
	now func() time.Time
	// jitter is the pause before the restart, so that the replicas, which
	// review on the same schedule, do not stop together.
	jitter func() time.Duration
}

const (
	// reReviewInterval is the pause between two background reviews of the
	// namespaces whose review failed at start.
	reReviewInterval = time.Minute
	// reReviewPasses is reReviewPolicy.passes.
	reReviewPasses = 2
	// restartMinUptime is reReviewPolicy.minUptime: kubelet resets a
	// container's restart back-off once it ran ten minutes.
	restartMinUptime = 10 * time.Minute
	// restartMaxJitter bounds reReviewPolicy.jitter.
	restartMaxJitter = time.Minute
)

// defaultReReviewPolicy is the operator's reReviewPolicy for a process
// started at started.
func defaultReReviewPolicy(started time.Time) reReviewPolicy {
	return reReviewPolicy{
		interval: reReviewInterval, passes: reReviewPasses, minUptime: restartMinUptime,
		started: started, now: time.Now, jitter: restartJitter,
	}
}

// restartJitter is a pause in [0, restartMaxJitter): not a secret, only
// spread between the replicas.
func restartJitter() time.Duration {
	return rand.N(restartMaxJitter) //nolint:gosec // G404: spreads restarts, nothing to guess.
}

// reReviewFailed reviews again, every policy.interval until ctx ends, the
// namespaces dropped because their review failed for a reason that may pass
// (an API server error or a connection lost, past the retries): the
// informers' namespaces are fixed when the manager starts, so one that
// becomes reviewable and grants every permission is only watched after a
// restart. A namespace whose review was too large or too slow
// (contentCause) is never reviewed here: its RoleBindings decide that, and
// whoever administers them could make it fail at start and pass later at
// will; it is reviewed at the next start, whatever causes it. For the same
// reason a namespace whose review here turns too large or too slow leaves
// the loop, and gauge moves it to that cause. restart is called once per
// process (with errRestartToWatch, naming the namespaces) when a namespace
// passed policy.passes reviews in a row, the process has run
// policy.minUptime, and a pause of policy.jitter has passed; a namespace
// that fails a review again starts its count over. Nothing is written
// anywhere. Each round is one managerrole.PermittedNamespaces of those
// namespaces only, bounded like the start's.
func reReviewFailed(ctx context.Context, api managerrole.Creator, dropped []droppedNamespace, gauge droppedGauge, policy reReviewPolicy, restart context.CancelCauseFunc) {
	var namespaces []string
	var required []managerrole.Permission
	for _, d := range dropped {
		if d.Cause == dropReviewFailed && len(d.required) > 0 {
			namespaces = append(namespaces, d.Namespace)
			required = d.required
		}
	}
	if len(namespaces) == 0 {
		return
	}
	passed := make(map[string]int, len(namespaces))
	waitLogged := false
	ticker := time.NewTicker(policy.interval)
	defer ticker.Stop()
	for len(namespaces) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		review, err := managerrole.PermittedNamespaces(ctx, api, required, namespaces, namespaceReviewTimeout)
		if err != nil {
			return
		}
		for _, u := range review.Unreviewed {
			cause := contentCause(u)
			if cause == "" {
				continue
			}
			setupLog.Error(u.Err, "watched namespace no longer reviewed in the background: its permission review is too large or too slow (its RoleBindings decide that); it is reviewed again at the next start",
				"namespace", u.Namespace, "reason", string(cause))
			namespaces = slices.DeleteFunc(namespaces, func(n string) bool { return n == u.Namespace })
			gauge.move(dropReviewFailed, cause)
		}
		var ready []string
		for _, namespace := range namespaces {
			if !slices.Contains(review.Permitted, namespace) {
				passed[namespace] = 0
				continue
			}
			passed[namespace]++
			if passed[namespace] >= policy.passes {
				ready = append(ready, namespace)
			}
		}
		if len(ready) == 0 {
			continue
		}
		if uptime := policy.now().Sub(policy.started); uptime < policy.minUptime {
			if !waitLogged {
				setupLog.Info("a watched namespace whose permission review failed at start now grants every permission: the operator restarts to watch it once it has run long enough",
					"namespaces", ready, "minUptime", policy.minUptime.String(), "uptime", uptime.Round(time.Second).String())
				waitLogged = true
			}
			continue
		}
		pause := policy.jitter()
		setupLog.Info("restarting to watch: a watched namespace whose permission review failed at start now grants every permission the operator needs (an intentional exit, not a crash)",
			"namespaces", ready, "in", pause.Round(time.Second).String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
		restart(fmt.Errorf("%w: %v", errRestartToWatch, ready))
		return
	}
}
