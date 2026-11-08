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
	"strconv"
	"strings"
)

// Minor is a CloudNativePG operator minor, "1.30", "1.31".
type Minor string

// The minors this package has tables for. pgToolBox supports
// CloudNativePG 1.30 and later: 1.29 (end of life upstream on
// 2026-09-29) has no table since 2026-11-06, so a 1.29 operator is an
// unsupported minor and every per-minor answer for it is "not known".
const (
	// Minor130 was verified at tag v1.30.0, the version of
	// github.com/cloudnative-pg/api in go.mod.
	Minor130 Minor = "1.30"
	// Minor131 is PLANNED: upstream main on 2026-10-23 (21e24f6), which
	// still reports itself as 1.30.1 (pkg/versions/versions.go). Re-check
	// every 1.31 fact at the 1.31.0 tag.
	Minor131 Minor = "1.31"
)

// newestReleased is the newest minor whose tables were read at a
// released tag. An unknown or unsupported minor is evaluated against
// it, with every answer outside it reported as not known.
const newestReleased = Minor130

// SupportedMinors returns the minors that have tables, oldest first.
// 1.31 is planned (upstream main), not released.
func SupportedMinors() []Minor {
	return []Minor{Minor130, Minor131}
}

// supported reports whether m has tables.
func (m Minor) supported() bool {
	return minorIndex(m) >= 0
}

// minorIndex is m's position in SupportedMinors, or -1.
func minorIndex(m Minor) int {
	for i, s := range SupportedMinors() {
		if s == m {
			return i
		}
	}
	return -1
}

// atLeast reports whether m is a supported minor not older than floor.
func (m Minor) atLeast(floor Minor) bool {
	i, f := minorIndex(m), minorIndex(floor)
	return i >= 0 && f >= 0 && i >= f
}

// maxVersionLength bounds every version string this package parses: a
// real one is a dozen bytes, and the inputs come from Pods and from
// unauthenticated instance endpoints.
const maxVersionLength = 64

// semver is the numeric core of a CloudNativePG version: the operator
// tags are vMAJOR.MINOR.PATCH with optional -rcN / -devN suffixes.
type semver struct {
	major, minor, patch int
	pre                 string
}

// less orders two versions; a pre-release sorts before its release.
func (v semver) less(o semver) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	if v.patch != o.patch {
		return v.patch < o.patch
	}
	if (v.pre == "") != (o.pre == "") {
		return v.pre != ""
	}
	return v.pre < o.pre
}

func (v semver) minorString() Minor {
	return Minor(strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor))
}

// String renders the version as major.minor.patch, with -pre when set.
func (v semver) String() string {
	s := strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor) + "." + strconv.Itoa(v.patch)
	if v.pre != "" {
		s += "-" + v.pre
	}
	return s
}

// parseSemver reads "1.30.1", "v1.30.1", "1.30.0-rc1", "1.30.0-ubi9" or
// "1.30" (patch 0). Anything else, including a version with a build
// suffix of non-printable bytes or one longer than maxVersionLength, is
// rejected.
func parseSemver(s string) (semver, bool) {
	if s == "" || len(s) > maxVersionLength {
		return semver{}, false
	}
	s = strings.TrimPrefix(s, "v")
	core, pre, _ := strings.Cut(s, "-")
	for _, r := range pre {
		if !isVersionRune(r) {
			return semver{}, false
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return semver{}, false
	}
	nums := [3]int{}
	for i, p := range parts {
		n, ok := atoiStrict(p)
		if !ok {
			return semver{}, false
		}
		nums[i] = n
	}
	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, true
}

func isVersionRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '.' || r == '-'
}

// atoiStrict accepts only plain decimal digits (no sign, no spaces) and
// at most six of them.
func atoiStrict(s string) (int, bool) {
	if s == "" || len(s) > 6 {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// MinorOf maps a version to its supported minor: "1.30.1" and "v1.30.1"
// give "1.30", a feature floor ">=1.30" gives "1.30". A version whose
// minor has no table, or that does not parse, gives false.
func MinorOf(version string) (Minor, bool) {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), ">="))
	v, ok := parseSemver(version)
	if !ok {
		return "", false
	}
	m := v.minorString()
	if !m.supported() {
		return "", false
	}
	return m, true
}
