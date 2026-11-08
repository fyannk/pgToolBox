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

// Command gen writes the console API's committed artifacts: openapi.yaml
// and the MSW fixtures under testdata/fixtures (make openapi). With
// -check it writes nothing and fails when a committed file differs
// (make verify-openapi, the CI drift check).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/fyannk/pgToolBox/internal/console/api"
)

func main() {
	dir := flag.String("dir", "internal/console/api", "the api package directory")
	check := flag.Bool("check", false, "compare instead of writing")
	flag.Parse()
	files, err := api.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	drift := 0
	for _, n := range names {
		p := filepath.Join(*dir, n)
		if *check {
			got, err := os.ReadFile(p) // #nosec G304 -- a path inside the repository
			if err != nil || !bytes.Equal(got, files[n]) {
				fmt.Fprintf(os.Stderr, "%s is stale: run make openapi\n", p)
				drift++
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(p, files[n], 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if drift > 0 {
		os.Exit(1)
	}
}
