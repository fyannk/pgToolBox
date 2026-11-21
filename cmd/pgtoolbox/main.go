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

// Command pgtoolbox is the offline command-line tool of pgToolBox. Its
// one subcommand today, preview, reads two versions of CloudNativePG
// Cluster manifests and says what applying the second over the first
// sets in motion (pkg/cnpgstatus/preview), for a CI or GitOps pipeline.
// It never contacts a cluster.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time (-X main.version).
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `Usage: pgtoolbox <command> [arguments]

Commands:
  preview   what applying NEW over OLD Cluster manifests sets in motion
  version   print the version

Run "pgtoolbox preview -h" for the preview options and exit codes.
`

// run dispatches a subcommand and returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return exitInvalid
	}
	switch args[0] {
	case "preview":
		return runPreview(args[1:], stdin, stdout, stderr)
	case "version", "--version":
		_, _ = fmt.Fprintln(stdout, version)
		return exitOK
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return exitOK
	default:
		_, _ = fmt.Fprintf(stderr, "pgtoolbox: unknown command %q\n\n%s", args[0], usage)
		return exitInvalid
	}
}
