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
	"time"

	"github.com/fyannk/pgToolBox/internal/operator/managerrole"
)

// reReviewInterval is the pause between two background reviews of the
// namespaces whose review failed at start.
const reReviewInterval = time.Minute

// errRestartToWatch is the cause the operator's context is cancelled with
// when a namespace left out for a failed review can now be watched: main
// exits 1 once the manager has stopped, and the restart watches it.
var errRestartToWatch = errors.New("a watched namespace left out at start can now be watched: restarting to watch it")

// reReviewFailed reviews again, every interval until ctx ends, the
// namespaces dropped because their review failed (a connection lost past
// the retries, no answer in time, a response too large): the informers'
// namespaces are fixed when the manager starts, so one that becomes
// reviewable and grants every permission is only watched after a restart,
// and restart is called once (with errRestartToWatch, naming the
// namespaces) as soon as one does. A namespace still failing, or now
// lacking a permission, is reviewed again at the next interval; nothing is
// written anywhere. Each round is one managerrole.PermittedNamespaces of
// those namespaces only, bounded like the start's.
func reReviewFailed(ctx context.Context, api managerrole.Creator, dropped []droppedNamespace, interval time.Duration, restart context.CancelCauseFunc) {
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
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		review, err := managerrole.PermittedNamespaces(ctx, api, required, namespaces, namespaceReviewTimeout)
		if err != nil {
			return
		}
		if len(review.Permitted) > 0 {
			setupLog.Info("restarting: a watched namespace whose permission review failed at start now grants every permission the operator needs",
				"namespaces", review.Permitted)
			restart(fmt.Errorf("%w: %v", errRestartToWatch, review.Permitted))
			return
		}
	}
}
