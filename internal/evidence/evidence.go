/*
Copyright © contributors to the pgtoolbox project.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

// Package evidence computes the repository destination identity the console
// verifies against the viewer sidecar's claims.
//
// The fingerprint algorithm is deliberately not implemented here: three
// projects computing "the same" hash from prose would eventually disagree, so
// all three consume one implementation with golden vectors — the viewer's
// types-only module (contract C2). This package only maps Barman's
// destination shape onto that module's input.
package evidence

import (
	"fmt"
	"strings"

	evidencev1alpha1 "github.com/fyannk/pgObjectStoreViewer/api/evidence/v1alpha1"
)

// Destination is the credential-free identity of one S3 repository root,
// parsed from a Barman destinationPath.
type Destination struct {
	Bucket string
	Prefix string
}

// ParseDestination splits a Barman destinationPath of the form
// s3://bucket/prefix. Only the s3 scheme belongs to the contract's initial
// profile; anything else is the caller's cue to degrade rather than guess.
func ParseDestination(destinationPath string) (Destination, error) {
	remainder, found := strings.CutPrefix(destinationPath, "s3://")
	if !found {
		return Destination{}, fmt.Errorf("destination %q is not an s3:// path", destinationPath)
	}
	bucket, prefix, _ := strings.Cut(remainder, "/")
	if bucket == "" {
		return Destination{}, fmt.Errorf("destination %q names no bucket", destinationPath)
	}
	return Destination{Bucket: bucket, Prefix: prefix}, nil
}

// Fingerprint returns the sha256: destination identity for one Barman server
// in one S3 repository, through the shared canonicalization.
//
// Region is always empty: Barman models the S3 region as a Secret reference,
// and the fingerprint covers credential-free values only — the producer's
// static-files profile carries no region variable either, so both sides omit
// it by construction.
func Fingerprint(destination Destination, endpointURL, serverName string) (string, error) {
	return evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Endpoint:  endpointURL,
		Region:    "",
		Bucket:    destination.Bucket,
		Prefix:    destination.Prefix,
		Format:    "barman-cloud",
		ScopeKind: "barman-server",
		ScopeName: serverName,
	})
}
