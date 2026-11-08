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

// Package backupresolve resolves a CloudNativePG Cluster's backup repository
// (Cluster → barman-cloud plugin parameters → ObjectStore, or the Cluster's
// in-tree spec.backup.barmanObjectStore, or a pgBackRest plugin's Stanza or
// Archive → credential references) into the plain composition the evidence
// agent reads. The
// operator (EvidenceAgent) and the agent itself (AGENT_RESOLVE=cluster, the
// pgtools chart without the operator) both use it, so the two install paths
// support exactly the same repository shapes. It imports no
// other internal/ package: the operator and the agent must not share code
// with each other beyond this one place (hack/check-layering.sh).
//
// This file computes the repository destination identity the console
// verifies against the producer's claims. Neither the destination grammar
// nor the fingerprint algorithm is implemented here: the operator, the agent
// and the console computing "the same" hash from prose would eventually
// disagree, so all three consume one implementation with golden vectors, the
// types-only pkg/evidence module. This file only maps Barman's destination
// shape onto that module's input, and derives the S3 signing region where
// the configuration implies one.
package backupresolve

import (
	"net/url"
	"regexp"
	"strings"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// ErrInvalidDestination is returned for any destination outside the
// contract's s3://bucket/prefix grammar. It never embeds the rejected value,
// which may carry credentials.
var ErrInvalidDestination = evidencev1alpha1.ErrInvalidS3Destination

// Destination is the credential-free identity of one S3 repository root,
// parsed from a Barman destinationPath.
type Destination struct {
	Bucket string
	// Prefix is percent-decoded once and slash-trimmed, exactly as the
	// producer derives it.
	Prefix string
}

// ParseDestination parses a Barman destinationPath through the shared
// contract parser, so the bucket and prefix hashed here are byte-identical
// to the ones the viewer hashes. Only the s3 scheme belongs to the
// contract's initial profile; anything else is the caller's cue to degrade
// rather than guess.
func ParseDestination(destinationPath string) (Destination, error) {
	bucket, prefix, err := evidencev1alpha1.ParseS3Destination(destinationPath)
	if err != nil {
		return Destination{}, ErrInvalidDestination
	}
	return Destination{Bucket: bucket, Prefix: prefix}, nil
}

// Fingerprint returns the sha256: destination identity for one Barman server
// in one S3 repository, through the shared canonicalization. The fingerprint domain
// hashes no region: the region is a signing coordinate, not an identity.
func Fingerprint(destination Destination, endpointURL, serverName string) (string, error) {
	return evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Endpoint:  endpointURL,
		Bucket:    destination.Bucket,
		Prefix:    destination.Prefix,
		Format:    "barman-cloud",
		ScopeKind: "barman-server",
		ScopeName: serverName,
	})
}

// awsRegionLabel matches one AWS region code (eu-west-1, us-gov-west-1,
// us-isob-east-1, cn-north-1, eusc-de-east-1, ...).
var awsRegionLabel = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+){1,2}-[0-9]+$`)

// awsPartitionSuffixes are the S3 endpoint DNS suffixes whose region can be
// read from the hostname. Only the commercial partition has a global
// (region-less) S3 endpoint.
var awsPartitionSuffixes = []struct {
	suffix string
	global bool
}{
	{".amazonaws.com", true},
	{".amazonaws.com.cn", false},
	{".amazonaws.eu", false},
}

// awsPartitionHost reports whether host (a lower-case hostname) belongs to
// an AWS partition this package derives regions for.
func awsPartitionHost(host string) bool {
	for _, partition := range awsPartitionSuffixes {
		if strings.HasSuffix(host, partition.suffix) {
			return true
		}
	}
	return false
}

// isS3ServiceLabel reports whether a DNS label names the S3 service:
// "s3" or one of its dashed variants (s3-fips, s3-external-1, the legacy
// s3-<region>).
func isS3ServiceLabel(label string) bool {
	return label == "s3" || strings.HasPrefix(label, "s3-")
}

// RegionFromEndpoint derives the S3 signing region from an AWS endpoint
// hostname. The region is read only from the slot that follows the
// right-most S3 service label (after an optional "dualstack"), or from a
// legacy s3-<region> service label: s3.eu-west-1.amazonaws.com,
// s3-eu-west-1.amazonaws.com, s3.dualstack.eu-west-1.amazonaws.com,
// bucket.vpce-….s3.eu-west-1.vpce.amazonaws.com, and the .amazonaws.com.cn
// and .amazonaws.eu partitions. Labels to the left of the service label are
// a virtual-hosted bucket name and never count, so a bucket named like a
// region on the global endpoint (eu-west-1.s3.amazonaws.com) still signs as
// us-east-1, the global endpoint's region. Any other host — another AWS
// service, an S3-compatible store — yields "", leaving the viewer to apply
// its compatible default; an empty endpoint also yields "".
func RegionFromEndpoint(endpointURL string) string {
	if endpointURL == "" {
		return ""
	}
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	for _, partition := range awsPartitionSuffixes {
		remainder, found := strings.CutSuffix(host, partition.suffix)
		if !found {
			continue
		}
		return regionFromLabels(strings.Split(remainder, "."), partition.global)
	}
	return ""
}

// regionFromLabels reads the region slot of one AWS S3 hostname, its
// partition suffix already removed.
func regionFromLabels(labels []string, global bool) string {
	service := -1
	for i := len(labels) - 1; i >= 0; i-- {
		if isS3ServiceLabel(labels[i]) {
			service = i
			break
		}
	}
	if service < 0 {
		return ""
	}
	if legacy := strings.TrimPrefix(labels[service], "s3-"); legacy != labels[service] && awsRegionLabel.MatchString(legacy) {
		return legacy
	}
	slot := service + 1
	if slot < len(labels) && labels[slot] == "dualstack" {
		slot++
	}
	if slot < len(labels) && awsRegionLabel.MatchString(labels[slot]) {
		return labels[slot]
	}
	if global && service == len(labels)-1 && (labels[service] == "s3" || labels[service] == "s3-external-1") {
		return "us-east-1"
	}
	return ""
}
