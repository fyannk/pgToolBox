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

package cnpgstatus

import (
	"sort"
	"strings"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Version confidences, best first.
const (
	// ConfidenceReported: an instance manager said so on /pg/status.
	ConfidenceReported = "reported"
	// ConfidenceImage: read from the operator image tag in a Pod.
	ConfidenceImage = "image"
	// ConfidenceInferred: a commit-hash match or a feature floor.
	ConfidenceInferred = "inferred"
	// ConfidenceUnknown: no evidence.
	ConfidenceUnknown = "unknown"
)

// Init container names (pkg/specs/pods.go). bootstrap-controller copies
// the instance manager from the OPERATOR image into every instance Pod
// (v1.30.0:82, main:82). bootstrap-instance is PLANNED 1.31
// (main:86, 8179beb05 "use an init container instead of a separate Job
// for new instances") and runs the PostgreSQL image: its presence is a
// 1.31 marker, its tag is not an operator version.
const (
	bootstrapControllerContainer = "bootstrap-controller"
	bootstrapInstanceContainer   = "bootstrap-instance"
)

// releaseCommits maps operator release commits to their version. The
// operator writes its build commit into status.cloudNativePGCommitHash
// (internal/controller/cluster_status.go v1.30.0:303); release builds
// set it to `git rev-parse --short` of the tagged commit
// (.github/workflows/release-publish.yml v1.30.0:158-165). Full hashes
// from `git rev-parse <tag>^{commit}` in the upstream checkout
// (2026-11-06). The 1.29 hashes stay although 1.29 is below the support
// floor: an operator that old is then named as such (its minor has no
// table, every per-minor answer is "not known") rather than guessed.
var releaseCommits = []struct{ commit, version string }{
	{"23eae00cd7aad82978397798dd27b600eb25ae3d", "1.29.0"},
	{"a4060c152630c9e8958e17d3d23f26b4eb30b69f", "1.29.1"},
	{"94ee3117858765c77d0dbfde381a037944a48a64", "1.29.2"},
	{"6887f9aeb7ec159ed4a7e5fce28441aafd26317c", "1.29.3"},
	{"4b5e244a7d031f67e025c83c1555e7726ecbbfa1", "1.30.0"},
	{"2a35abb4628f209d149825ef3c38011e0701ff2f", "1.30.1"},
}

// VersionEvidence is everything DetectVersion may use. Never a Pod or
// Cluster label: app.kubernetes.io/version is the PostgreSQL major. The
// console reads no ConfigMap, so the cnpg.io/operatorVersion annotation
// (set only on the cnpg-default-monitoring ConfigMap, the CA Secrets and
// major-upgrade Jobs: internal/controller/cluster_create.go v1.30.0:865-972,
// pkg/reconciler/majorupgrade/reconciler.go:249) is not an input.
type VersionEvidence struct {
	// InstanceManagerVersions are /pg/status instanceManagerVersion values
	// of the instances read (pkg/management/postgres/probes.go v1.30.0:45:
	// versions.Version).
	InstanceManagerVersions []string
	// InstancePods are the instance Pods, for the bootstrap-controller
	// init container image tag and the 1.31 bootstrap-instance marker.
	InstancePods []*corev1.Pod
	// Cluster is probed for status.cloudNativePGCommitHash,
	// status.operatorCertificateFingerprint and condition types.
	Cluster *cnpgv1.Cluster
	// LeaseServed: true when the Cluster's primary Lease (named after the
	// Cluster, internal/controller/primary_lease.go v1.30.0:49-55) was
	// observed. DatabaseRolesServed / FailoverQuorumsServed: the
	// databaseroles / failoverquorums CRDs are served. nil = not probed;
	// false is never evidence (RBAC can hide either).
	LeaseServed, DatabaseRolesServed, FailoverQuorumsServed *bool
}

// Version is the detected operator version.
type Version struct {
	// Version is a semantic version ("1.30.0"), a floor (">=1.30"), or
	// "" when unknown.
	Version string `json:"version"`
	// Minor is the supported minor whose tables apply, or "" when the
	// version is unknown or has no tables.
	Minor Minor `json:"minor"`
	// Confidence is reported, image, inferred or unknown.
	Confidence string `json:"confidence"`
	// DetectedBy names the evidence, for the UI's provenance line.
	DetectedBy string `json:"detectedBy,omitempty"`
}

// featureFloor is the oldest minor that can produce one observation.
type featureFloor struct {
	minor Minor
	name  string
}

// DetectVersion answers "which CNPG version?" by ranking the evidence:
//
//  1. the highest instanceManagerVersion reported;
//  2. the highest bootstrap-controller init image tag (lags after an
//     in-place operator upgrade until Pods are recreated);
//  3. status.cloudNativePGCommitHash against the release-commit table;
//  4. feature floors: operatorCertificateFingerprint, condition
//     Initialized, the primary Lease, the databaseroles CRD (>=1.30,
//     cluster_types.go v1.30.0:1086/1185);
//     ConsistentSystemID and failoverquorums (>=1.27); PLANNED 1.31:
//     a bootstrap-instance init container or the
//     SyncReplicationTopologySatisfied condition.
//
// A feature floor above the ranked answer's minor overrides it, with
// confidence inferred: the operator (which writes phases and
// conditions) is newer than the lagging instance manager or image, and
// upstream main builds still report 1.30.1 (pkg/versions/versions.go
// main:26), so a 1.31 marker beside a reported 1.30 means main.
func DetectVersion(e VersionEvidence) Version {
	floor := highestFloor(e)
	best := rankedVersion(e)
	if best.Confidence == ConfidenceUnknown {
		if floor.minor == "" {
			return best
		}
		return Version{
			Version:    ">=" + string(floor.minor),
			Minor:      minorOrEmpty(string(floor.minor)),
			Confidence: ConfidenceInferred,
			DetectedBy: "feature:" + floor.name,
		}
	}
	if bv, ok := parseSemver(best.Version); ok && floor.minor != "" && minorGreater(floor.minor, bv.minorString()) {
		return Version{
			Version:    ">=" + string(floor.minor),
			Minor:      minorOrEmpty(string(floor.minor)),
			Confidence: ConfidenceInferred,
			DetectedBy: "feature:" + floor.name + " overrides " + best.DetectedBy + " " + best.Version,
		}
	}
	return best
}

func minorOrEmpty(version string) Minor {
	m, _ := MinorOf(version)
	return m
}

// rankedVersion applies steps 1-3.
func rankedVersion(e VersionEvidence) Version {
	if v, detail, ok := highest(e.InstanceManagerVersions); ok {
		return Version{Version: v.String(), Minor: minorOrEmpty(v.String()), Confidence: ConfidenceReported, DetectedBy: "instanceManagerVersion" + detail}
	}
	var tags []string
	for _, pod := range e.InstancePods {
		if pod == nil {
			continue
		}
		for _, container := range pod.Spec.InitContainers {
			if container.Name == bootstrapControllerContainer {
				tags = append(tags, imageTag(container.Image))
			}
		}
	}
	if v, detail, ok := highest(tags); ok {
		return Version{Version: v.String(), Minor: minorOrEmpty(v.String()), Confidence: ConfidenceImage, DetectedBy: "bootstrap-controller image" + detail}
	}
	if e.Cluster != nil {
		if version, ok := commitVersion(e.Cluster.Status.CommitHash); ok {
			return Version{Version: version, Minor: minorOrEmpty(version), Confidence: ConfidenceInferred, DetectedBy: "status.cloudNativePGCommitHash"}
		}
	}
	return Version{Confidence: ConfidenceUnknown}
}

// highest returns the highest parseable version among values, and a
// " (mixed: a, b)" detail when the parseable ones differ (a rolling
// upgrade in flight). Unparseable values are ignored.
func highest(values []string) (semver, string, bool) {
	seen := map[string]semver{}
	for _, raw := range values {
		v, ok := parseSemver(strings.TrimSpace(raw))
		if ok {
			seen[v.String()] = v
		}
	}
	if len(seen) == 0 {
		return semver{}, "", false
	}
	all := make([]semver, 0, len(seen))
	for _, v := range seen {
		all = append(all, v)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].less(all[j]) })
	best := all[len(all)-1]
	if len(all) == 1 {
		return best, "", true
	}
	names := make([]string, 0, len(all))
	for _, v := range all {
		names = append(names, v.String())
	}
	return best, " (mixed: " + strings.Join(names, ", ") + ")", true
}

// imageTag returns the tag of an image reference: the part after the
// last ':' of the last path element, without any @digest. A digest-only
// reference has no tag.
func imageTag(image string) string {
	if len(image) > 512 {
		return ""
	}
	image, _, _ = strings.Cut(image, "@")
	last := image[strings.LastIndex(image, "/")+1:]
	_, tag, found := strings.Cut(last, ":")
	if !found {
		return ""
	}
	return tag
}

// commitVersion matches a short or full commit hash against the
// release table; at least seven hex digits are required.
func commitVersion(hash string) (string, bool) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) < 7 || len(hash) > 40 {
		return "", false
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	for _, release := range releaseCommits {
		if strings.HasPrefix(release.commit, hash) {
			return release.version, true
		}
	}
	return "", false
}

// highestFloor applies step 4. Floors below the supported minors are returned too (as
// "1.27"), so that a cluster with only 1.27-era evidence still reads as
// ">=1.27" instead of nothing.
func highestFloor(e VersionEvidence) featureFloor {
	var floors []featureFloor
	add := func(minor Minor, name string) { floors = append(floors, featureFloor{minor: minor, name: name}) }
	for _, pod := range e.InstancePods {
		if pod == nil {
			continue
		}
		for _, container := range pod.Spec.InitContainers {
			if container.Name == bootstrapInstanceContainer {
				add(Minor131, "bootstrap-instance init container (planned 1.31)")
			}
		}
	}
	if c := e.Cluster; c != nil {
		if meta.FindStatusCondition(c.Status.Conditions, ConditionSyncReplicationTopologySatisfied) != nil {
			add(Minor131, "condition SyncReplicationTopologySatisfied (planned 1.31)")
		}
		if c.Status.OperatorCertificateFingerprint != "" {
			add(Minor130, "status.operatorCertificateFingerprint")
		}
		if meta.FindStatusCondition(c.Status.Conditions, ConditionInitialized) != nil {
			add(Minor130, "condition Initialized")
		}
		if meta.FindStatusCondition(c.Status.Conditions, ConditionConsistentSystemID) != nil {
			add("1.27", "condition ConsistentSystemID")
		}
	}
	if isTrue(e.LeaseServed) {
		add(Minor130, "primary Lease")
	}
	if isTrue(e.DatabaseRolesServed) {
		add(Minor130, "databaseroles CRD")
	}
	if isTrue(e.FailoverQuorumsServed) {
		add("1.27", "failoverquorums CRD")
	}
	best := featureFloor{}
	for _, f := range floors {
		if best.minor == "" || minorGreater(f.minor, best.minor) {
			best = f
		}
	}
	return best
}

func isTrue(b *bool) bool { return b != nil && *b }

// minorGreater compares two "X.Y" minors numerically.
func minorGreater(a, b Minor) bool {
	va, okA := parseSemver(string(a))
	vb, okB := parseSemver(string(b))
	return okA && okB && vb.less(va)
}
