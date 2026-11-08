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

// Command evidence-fingerprint prints the repository destination
// fingerprint for an S3 backup destination.
//
// It exists for local development. In a real deployment pgtoolbox
// computes this value and sets REPOSITORY_EXPECTED_FINGERPRINT from it;
// the console then refuses any evidence response that does not carry
// the same one, which is what stops a sidecar pointed at the wrong
// repository from being rendered as this cluster's.
//
// The computation is not reimplemented here. It comes from the shared
// api module — the same code the evidence agent runs when it stamps a
// response — because a fingerprint the two sides compute differently
// would be worse than no fingerprint at all: it would fail closed
// forever with nothing to point at.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

func main() {
	var input evidencev1alpha1.S3FingerprintInput
	var destination string
	flag.StringVar(&destination, "destination", "", "Barman destinationPath (s3://bucket/prefix), parsed exactly as the viewer parses it")
	flag.StringVar(&input.Endpoint, "endpoint", "", "credential-free S3 endpoint origin, empty for the AWS default")
	flag.StringVar(&input.Bucket, "bucket", "", "destination bucket (alternative to -destination)")
	flag.StringVar(&input.Prefix, "prefix", "", "decoded destination prefix inside the bucket (alternative to -destination)")
	flag.StringVar(&input.ScopeName, "server", "", "the Barman server name")
	flag.Parse()

	fingerprint, err := compute(input, destination)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evidence-fingerprint:", err)
		os.Exit(1)
	}
	fmt.Println(fingerprint)
}

// compute fills the fixed profile fields and, when a destination is given,
// splits it with the contract parser so the prefix is decoded exactly once,
// as the viewer and pgtoolbox decode it. The region is not an input: the
// fingerprint domain deliberately excludes it.
func compute(input evidencev1alpha1.S3FingerprintInput, destination string) (string, error) {
	if destination != "" {
		if input.Bucket != "" || input.Prefix != "" {
			return "", errors.New("-destination cannot be combined with -bucket or -prefix")
		}
		bucket, prefix, err := evidencev1alpha1.ParseS3Destination(destination)
		if err != nil {
			return "", err
		}
		input.Bucket, input.Prefix = bucket, prefix
	}
	input.Format = "barman-cloud"
	input.ScopeKind = "barman-server"
	return evidencev1alpha1.FingerprintS3(input)
}
