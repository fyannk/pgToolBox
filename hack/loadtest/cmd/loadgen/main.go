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

// Command loadgen drives the pgToolBox load-test harness (hack/loadtest):
// it fills a throwaway kind cluster with fake CloudNativePG Clusters (their
// Pods live on KWOK nodes), keeps them changing, hammers the console API and
// its event stream, reads the components' own metrics, fills an S3 bucket
// with a Barman layout and, in the cluster, answers the console's status
// sweep for the fake Pods.
//
// It refuses to talk to any cluster but the one named by --context: it never
// falls back to the default kubeconfig.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
)

type command struct {
	summary string
	run     func(ctx context.Context, args []string) error
}

func commands() map[string]command {
	return map[string]command{
		"populate":     {"create Clusters, member Pods, Services, Events and Backups", runPopulate},
		"churn":        {"patch Cluster status at a fixed rate", runChurn},
		"http":         {"log in virtual users, hammer the console API and open SSE streams", runHTTP},
		"scrape":       {"print RSS, goroutines, cache and SSE counters of the console and the operator", runScrape},
		"s3fill":       {"fill an S3 bucket with a Barman layout (base backups and WAL)", runS3Fill},
		"instance-sim": {"answer /pg/status and /metrics for the fake Pods (runs in the cluster)", runInstanceSim},
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: loadgen <command> [flags]\n\ncommands:")
	cmds := commands()
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(os.Stderr, "  %-13s %s\n", name, cmds[name].summary)
	}
	fmt.Fprintln(os.Stderr, "\nrun 'loadgen <command> -h' for its flags")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, ok := commands()[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := cmd.run(ctx, os.Args[2:])
	stop()
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	default:
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}
