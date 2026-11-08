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
	"flag"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type churnOpts struct {
	kube     kubeFlags
	rate     float64
	duration time.Duration
	mode     string
	workers  int
	restore  bool
}

const (
	churnTimestamp = "timestamp"
	churnCondition = "condition"
	churnPhase     = "phase"
)

func runChurn(ctx context.Context, args []string) error {
	var o churnOpts
	fs := flag.NewFlagSet("churn", flag.ContinueOnError)
	o.kube.register(fs, 500, 1000)
	fs.Float64Var(&o.rate, "rate", 10, "Cluster status updates per second, spread over all harness Clusters")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "how long to run")
	fs.StringVar(&o.mode, "mode", churnTimestamp, "what changes: timestamp (status.currentPrimaryTimestamp), condition (the Ready condition) or phase (flips the phase away from healthy and back)")
	fs.IntVar(&o.workers, "workers", 32, "concurrent patches")
	fs.BoolVar(&o.restore, "restore", true, "at the end, put every touched Cluster back to the healthy status")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch o.mode {
	case churnTimestamp, churnCondition, churnPhase:
	default:
		return fmt.Errorf("--mode %q: want %s, %s or %s", o.mode, churnTimestamp, churnCondition, churnPhase)
	}
	if o.rate <= 0 || o.duration <= 0 || o.workers < 1 {
		return errors.New("--rate, --duration and --workers must be positive")
	}
	k, err := o.kube.connect()
	if err != nil {
		return err
	}
	return churn(ctx, k, &o)
}

// churnTarget is one Cluster and the revision the next write carries.
type churnTarget struct {
	cluster   cnpgv1.Cluster
	revision  atomic.Int64
	unhealthy atomic.Bool
}

// ref is a private object to patch into: a patch writes the server's answer
// back into the object it is given, and workers share the target.
func (t *churnTarget) ref() *cnpgv1.Cluster {
	c := &cnpgv1.Cluster{}
	c.Namespace, c.Name = t.cluster.Namespace, t.cluster.Name
	return c
}

func churn(ctx context.Context, k *kube, o *churnOpts) error {
	var list cnpgv1.ClusterList
	if err := k.client.List(ctx, &list, client.MatchingLabels{harnessLabel: "true"}); err != nil {
		return fmt.Errorf("list Clusters: %w", err)
	}
	if len(list.Items) == 0 {
		return errors.New("no harness Cluster: run populate first")
	}
	targets := make([]*churnTarget, len(list.Items))
	for i := range list.Items {
		targets[i] = &churnTarget{cluster: list.Items[i]}
	}
	fmt.Printf("churn: %.1f updates/s (%s) over %d Clusters for %s\n", o.rate, o.mode, len(targets), o.duration)

	jobs := make(chan *churnTarget, 4096)
	var issued, behind atomic.Int64
	var firstErr atomic.Value
	var mu sync.Mutex
	stats := &epStats{}
	var wg sync.WaitGroup
	for range o.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := &epStats{}
			for t := range jobs {
				smp, err := applyChurn(ctx, k.client, t, o.mode)
				if err != nil {
					firstErr.CompareAndSwap(nil, err.Error())
				}
				local.add(smp)
			}
			mu.Lock()
			stats.merge(local)
			mu.Unlock()
		}()
	}

	started := time.Now()
	deadline := started.Add(o.duration)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case now := <-tick.C:
			if now.After(deadline) {
				break loop
			}
			// Issue whatever the schedule owes, so a slow tick catches up.
			for float64(issued.Load()) < now.Sub(started).Seconds()*o.rate {
				t := targets[rand.IntN(len(targets))] //nolint:gosec // spread, not security
				select {
				case jobs <- t:
					issued.Add(1)
				default:
					// The workers cannot keep up: count it, skip it.
					behind.Add(1)
					issued.Add(1)
				}
			}
		}
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(started)

	ok := stats.count() - stats.errors
	fmt.Printf("churn: target %.1f/s, achieved %.1f/s (%d ok, %d errors, %d dropped because the workers were behind) in %s\n",
		o.rate, float64(ok)/elapsed.Seconds(), ok, stats.errors, behind.Load(), elapsed.Round(time.Millisecond))
	fmt.Printf("churn: patch latency p50 %sms p95 %sms p99 %sms\n",
		ms(percentile(stats.durations, 50)), ms(percentile(stats.durations, 95)), ms(percentile(stats.durations, 99)))
	if msg, ok := firstErr.Load().(string); ok {
		fmt.Println("churn: first error:", msg)
	}
	if o.restore {
		restored := restoreHealthy(context.WithoutCancel(ctx), k, targets, o.workers)
		fmt.Printf("churn: restored %d Clusters to the healthy status\n", restored)
	}
	return nil
}

// applyChurn writes one change to a Cluster's status and times it.
func applyChurn(ctx context.Context, cl client.Client, t *churnTarget, mode string) (sample, error) {
	rev := int(t.revision.Add(1))
	now := time.Now().UTC()
	status := clusterStatus(&t.cluster, t.cluster.Spec.Instances, now, rev)
	var patch any
	switch mode {
	case churnCondition:
		patch = map[string]any{"conditions": status.Conditions}
	case churnPhase:
		// Alternate: away from healthy, then back. Written by whoever gets
		// the Cluster next, so two workers may flip the same one twice;
		// the end state is restored at the end of the run.
		if t.unhealthy.Swap(false) {
			patch = map[string]any{"phase": status.Phase, "phaseReason": ""}
		} else {
			t.unhealthy.Store(true)
			patch = map[string]any{"phase": "Applying configuration", "phaseReason": fmt.Sprintf("churn revision %d", rev)}
		}
	default:
		patch = map[string]any{"currentPrimaryTimestamp": now.Format(time.RFC3339Nano)}
	}
	start := time.Now()
	err := patchStatus(ctx, cl, t.ref(), patch)
	d := time.Since(start)
	if err != nil {
		return sample{d: d, status: 0}, err
	}
	return sample{d: d, status: 200}, nil
}

func restoreHealthy(ctx context.Context, k *kube, targets []*churnTarget, workers int) int {
	var wg sync.WaitGroup
	var done atomic.Int64
	ch := make(chan *churnTarget)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range ch {
				status := clusterStatus(&t.cluster, t.cluster.Spec.Instances, time.Now().UTC(), int(t.revision.Load())+1)
				if patchStatus(ctx, k.client, t.ref(), status) == nil {
					done.Add(1)
				}
			}
		}()
	}
	for _, t := range targets {
		ch <- t
	}
	close(ch)
	wg.Wait()
	return int(done.Load())
}
