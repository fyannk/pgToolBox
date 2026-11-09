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

package diagnose

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
)

// Component names one versioned part of the stack a catalog rule can pin
// itself to. A rule's claim — "this log line means this thing" — is only
// ever true of particular releases, so the pin is part of the claim, not
// metadata on it.
type Component string

const (
	// ComponentCNPG is the CloudNativePG operator.
	ComponentCNPG Component = "CloudNativePG"
	// ComponentPostgreSQL is the database itself. Only the major version
	// is operator-reported, so pins against it compare majors.
	ComponentPostgreSQL Component = "PostgreSQL"
	// ComponentBarman is the Barman Cloud plugin, observed through the
	// sidecar it ships into each instance pod.
	ComponentBarman Component = "Barman Cloud plugin"
	// ComponentPgBackRest is either pgBackRest CNPG-I plugin (Dalibo's or
	// Opera's). No rule is pinned to it: the console observes no version
	// of either, and its rules read the repository evidence only.
	ComponentPgBackRest Component = "pgBackRest plugin"
	// ComponentKubernetes is the cluster the console runs against.
	ComponentKubernetes Component = "Kubernetes"
)

// ComponentVersion is one observed version fact together with its
// provenance, so a finding gated on it can quote where the number came
// from rather than asserting it.
type ComponentVersion struct {
	// Version is the observed version as dotted numerals, such as "17"
	// or "0.6.0".
	Version string
	// Origin, Object and Detail carry the provenance in Evidence form.
	Origin string
	Object string
	Detail string
}

// evidence quotes the version fact beneath a finding that rests on it.
func (v ComponentVersion) evidence() Evidence {
	return Evidence{Origin: v.Origin, Object: v.Object, Detail: v.Detail}
}

// VersionFacts is every component version the console has observed. A
// component absent from the map is unknown, and unknown is a first-class
// answer: a rule pinned to it reports that it could not be evaluated,
// never that it is clear and never that it matched.
type VersionFacts map[Component]ComponentVersion

// versionFacts derives the observed versions from the published
// snapshots. It is a pure function of the input, like everything else in
// this package: versions are observed, never configured, because an
// injected version the cluster does not actually run would silently gate
// every pinned rule wrong.
func versionFacts(in Input) VersionFacts {
	facts := VersionFacts{}

	if in.HasKubeVersion {
		if fields, ok := parseVersion(in.KubeVersion.GitVersion); ok {
			detail := fmt.Sprintf("/version reports %q", in.KubeVersion.GitVersion)
			if in.KubeVersion.Stale {
				detail += " (stale: contact has since been lost, this is the retained last-good report)"
			}
			facts[ComponentKubernetes] = ComponentVersion{
				Version: joinVersion(fields),
				Origin:  "Kubernetes-reported",
				Object:  "API server",
				Detail:  detail,
			}
		}
	}

	if in.HasCluster && in.Cluster.Cluster.Present && in.Cluster.Cluster.PostgresMajorVersion != nil {
		major := *in.Cluster.Cluster.PostgresMajorVersion
		facts[ComponentPostgreSQL] = ComponentVersion{
			Version: strconv.Itoa(major),
			Origin:  "operator-reported",
			Object:  "Cluster",
			Detail:  fmt.Sprintf("status reports PostgreSQL major version %d", major),
		}
	}

	// The console's version detector, when it has an exact answer, is
	// the CloudNativePG version: it ranks the instance managers' own
	// report and the operator's Pod annotation above the init image,
	// which lags an in-place operator upgrade. The image, Job and
	// retained-image readings below are the fallbacks for an Input that
	// carries no detector answer.
	if v, ok := detectedCNPG(in); ok {
		facts[ComponentCNPG] = v
	}

	if in.HasPods {
		if pod, container, version, ok := memberContainerVersion(in, barmanSidecarContainer); ok {
			facts[ComponentBarman] = ComponentVersion{
				Version: version,
				Origin:  "console-parsed from Kubernetes-observed image",
				Object:  fmt.Sprintf("Pod/%s container %s", pod, container.Name),
				Detail:  fmt.Sprintf("sidecar image %q", container.Image),
			}
		}
		// The operator injects its own image into every instance pod as
		// the bootstrap init container that copies the instance manager
		// in, so that image's tag is the operator version — observed on
		// the workload itself, not on the operator's Deployment, which
		// lives outside this console's namespaced authority.
		if _, known := facts[ComponentCNPG]; !known {
			if pod, container, version, ok := memberContainerVersion(in, bootstrapControllerContainer); ok {
				facts[ComponentCNPG] = ComponentVersion{
					Version: version,
					Origin:  "console-parsed from Kubernetes-observed image",
					Object:  fmt.Sprintf("Pod/%s init container %s", pod, container.Name),
					Detail:  fmt.Sprintf("operator bootstrap image %q", container.Image),
				}
			}
		}
	}

	// A cluster that broke before its first instance pod — a bootstrap
	// job failing over and over — has no pod to read the version from,
	// which is precisely when the operator's own verdicts matter most.
	// The bootstrap Jobs carry the same injected image, so they are the
	// source of last resort.
	if _, known := facts[ComponentCNPG]; !known && in.HasInfrastructure {
		for _, child := range in.Infrastructure.Children {
			if child.Kind != "Job" || child.BootstrapImage == "" {
				continue
			}
			if version, ok := imageTagVersion(child.BootstrapImage); ok {
				facts[ComponentCNPG] = ComponentVersion{
					Version: version,
					Origin:  "console-parsed from Kubernetes-observed image",
					Object:  fmt.Sprintf("Job/%s init container %s", child.Name, bootstrapControllerContainer),
					Detail:  fmt.Sprintf("operator bootstrap image %q", child.BootstrapImage),
				}
				break
			}
		}
	}

	// No pod and no job — a hibernated cluster, or one whose instances
	// are all gone — leaves the last image an instance pod carried. It
	// is retained by the pod store and labelled as such: the version is
	// what it was, not what a pod says now.
	// It is used only for the Cluster it was seen on: a recreated
	// Cluster of the same name is another cluster, and inherits nothing.
	if _, known := facts[ComponentCNPG]; !known && in.HasPods && in.Pods.LastOperatorImage != "" &&
		in.HasCluster && in.Cluster.Cluster.Present && in.Cluster.Cluster.UID != "" &&
		in.Pods.LastOperatorImageOwner == in.Cluster.Cluster.UID {
		if version, ok := imageTagVersion(in.Pods.LastOperatorImage); ok {
			facts[ComponentCNPG] = ComponentVersion{
				Version: version,
				Origin:  "console-retained from a Kubernetes-observed image",
				Object:  fmt.Sprintf("Pod/%s init container %s", in.Pods.LastOperatorImagePod, bootstrapControllerContainer),
				Detail: fmt.Sprintf("operator bootstrap image %q, last seen %s; no instance pod carries it now",
					in.Pods.LastOperatorImage, in.Pods.LastOperatorImageSeenAt.UTC().Format(time.RFC3339)),
			}
		}
	}

	return facts
}

// detectedCNPG turns the detector's answer into a version fact, when it
// is an exact version (a floor such as ">=1.30" gates nothing: a pinned
// rule needs a number).
func detectedCNPG(in Input) (ComponentVersion, bool) {
	if !in.HasCNPGVersion {
		return ComponentVersion{}, false
	}
	v := in.CNPGVersion
	fields, ok := parseVersion(v.Version)
	if !ok || len(fields) < 3 {
		return ComponentVersion{}, false
	}
	origin := "console-inferred"
	switch v.Confidence {
	case cnpgstatus.ConfidenceReported:
		origin = "instance-reported"
	case cnpgstatus.ConfidenceImage:
		origin = "operator-written on the instance Pods"
	}
	return ComponentVersion{
		Version: joinVersion(fields),
		Origin:  origin,
		Object:  "Cluster instances",
		Detail:  "CloudNativePG " + v.Version + " from " + v.DetectedBy,
	}, true
}

// barmanSidecarContainer is the container name the Barman Cloud plugin
// ships its sidecar under.
const barmanSidecarContainer = "plugin-barman-cloud"

// bootstrapControllerContainer is the init container the operator adds
// to every instance pod, running the operator's own image.
const bootstrapControllerContainer = observe.BootstrapControllerContainer

// memberContainerVersion finds the named container on any instance pod
// and parses its image tag. The first pod carrying one wins: the
// operator rolls these containers with the cluster, so a mixed set is
// transient and any member is as good an answer as another.
func memberContainerVersion(in Input, name string) (pod string, container ContainerRef, version string, ok bool) {
	for _, p := range in.Pods.Pods {
		for _, c := range p.Containers {
			if c.Name != name {
				continue
			}
			if v, parsed := imageTagVersion(c.Image); parsed {
				return p.Name, ContainerRef{Name: c.Name, Image: c.Image}, v, true
			}
		}
	}
	return "", ContainerRef{}, "", false
}

// ContainerRef names a container and its image for provenance.
type ContainerRef struct {
	Name  string
	Image string
}

// imageTagVersion extracts a dotted numeric version from an image tag:
// "ghcr.io/x/y:v0.6.0" yields "0.6.0", a digest-only or tagless image
// yields nothing. Only the leading numeric part of the tag counts —
// "17.5-bookworm" yields "17.5" — because everything after it names a
// build variant, not a version.
func imageTagVersion(image string) (string, bool) {
	// A digest pins content, not a version.
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	colon := strings.LastIndexByte(image, ':')
	if colon < 0 || strings.ContainsRune(image[colon:], '/') {
		return "", false
	}
	tag := strings.TrimPrefix(image[colon+1:], "v")
	if fields, ok := parseVersion(tag); ok {
		return joinVersion(fields), true
	}
	return "", false
}

// parseVersion reads the leading dotted numerals of a version string:
// "1.26.1" is [1 26 1], "17.5-bookworm" is [17 5], "latest" is nothing.
func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(s, "v")
	var fields []int
	for {
		digits := 0
		for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return nil, false
		}
		n, err := strconv.Atoi(s[:digits])
		if err != nil {
			return nil, false
		}
		fields = append(fields, n)
		s = s[digits:]
		if !strings.HasPrefix(s, ".") {
			return fields, true
		}
		s = s[1:]
	}
}

// joinVersion renders parsed fields back to dotted numerals.
func joinVersion(fields []int) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = strconv.Itoa(f)
	}
	return strings.Join(parts, ".")
}

// compareVersions orders two parsed versions numerically, treating a
// missing field as zero so "1.26" and "1.26.0" are equal.
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// satisfies reports whether a version meets a constraint.
//
// A constraint is space-separated clauses that must all hold, each an
// operator and a version: ">=1.24", "<1.26", "=17", "!=1.25.0". The
// empty constraint holds for every version. A malformed clause or an
// unparseable version fails closed — the rule does not apply — because a
// version-pinned claim evaluated against a version it cannot read would
// be asserting an applicability nobody established.
func satisfies(version, constraint string) bool {
	v, ok := parseVersion(version)
	if !ok {
		return false
	}
	for _, clause := range strings.Fields(constraint) {
		op := "="
		rest := clause
		for _, candidate := range [...]string{">=", "<=", "!=", ">", "<", "="} {
			if strings.HasPrefix(clause, candidate) {
				op, rest = candidate, clause[len(candidate):]
				break
			}
		}
		bound, ok := parseVersion(rest)
		if !ok {
			return false
		}
		cmp := compareVersions(v, bound)
		holds := false
		switch op {
		case ">=":
			holds = cmp >= 0
		case "<=":
			holds = cmp <= 0
		case ">":
			holds = cmp > 0
		case "<":
			holds = cmp < 0
		case "=":
			holds = cmp == 0
		case "!=":
			holds = cmp != 0
		}
		if !holds {
			return false
		}
	}
	return true
}

// Versions is every component version the input shows, with where each
// came from: what the rules pin against, for a screen to state.
func Versions(in Input) VersionFacts { return versionFacts(in) }

// ImageTagVersion reads the version in an image's tag ("…/postgresql:18.6-system-trixie" is "18.6").
func ImageTagVersion(image string) (string, bool) { return imageTagVersion(image) }
