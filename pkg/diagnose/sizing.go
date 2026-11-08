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
	"sort"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/observe"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/pgconf"
)

// The configuration advisor: the declared PostgreSQL memory settings
// weighed against the postgres container's resources. PostgreSQL sizes
// itself from its parameters and cannot see the cgroup; the kernel
// OOM-kills the container when the cgroup is exceeded, and CloudNativePG
// then restarts the instance (a primary's restart is a failover). Every
// number here is declared intent (the Cluster spec), not what PostgreSQL
// applied: /pg/status reports no setting.

// declaredOrigin is the origin of every spec-backed claim.
const declaredOrigin = "declared in the Cluster spec"

// sizingInput is the parsed sizing, or the reason it cannot be judged.
func sizingInput(in Input, needed ...string) (observe.PostgresSizing, pgconf.Memory, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return observe.PostgresSizing{}, pgconf.Memory{}, reason
	}
	sizing := in.Cluster.Cluster.Sizing
	if !sizing.Read {
		return sizing, pgconf.Memory{}, "the Cluster's spec was not read for its PostgreSQL parameters and resources"
	}
	memory := readMemory(sizing)
	var bad []string
	for _, u := range memory.Unparsable {
		for _, n := range needed {
			if u.Name == n {
				bad = append(bad, fmt.Sprintf("%s = %q", u.Name, u.Value))
			}
		}
	}
	if len(bad) > 0 {
		return sizing, memory, "the estimate needs " + strings.Join(bad, ", ") + ", which PostgreSQL would not accept either"
	}
	return sizing, memory, ""
}

// readMemory parses the declared parameters; one the console did not
// keep in full is unparsable, never its default nor a prefix.
func readMemory(sizing observe.PostgresSizing) pgconf.Memory {
	m := pgconf.ReadMemory(sizing.Parameters, sizing.WALSegmentBytes)
	for _, name := range sizing.TooLong {
		m.Unparsable = append(m.Unparsable, pgconf.Unparsable{
			Name: strings.ToLower(name), Value: "…",
			Reason: "the declared value is longer than the console keeps, so it was not read",
		})
	}
	sort.Slice(m.Unparsable, func(i, j int) bool { return m.Unparsable[i].Name < m.Unparsable[j].Name })
	return m
}

// iec renders bytes as PostgreSQL's units read them (1 MB = 1024 kB),
// spelled the Kubernetes way to avoid the ambiguity: 1.5 GiB.
func iec(b int64) string {
	const unit = 1024
	if b < unit && b > -unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit || n <= -unit; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	s := fmt.Sprintf("%.1f", v)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + string("KMGTPE"[exp]) + "iB"
}

// setting renders one parameter for evidence: its value and where it
// came from.
func setting(s pgconf.Setting, render string) string {
	switch {
	case s.Default && s.Auto:
		return fmt.Sprintf("%s %s (not set: automatic)", s.Name, render)
	case s.Default:
		return fmt.Sprintf("%s %s (not set: PostgreSQL's default)", s.Name, render)
	case s.Auto:
		return fmt.Sprintf("%s %s (%s: automatic)", s.Name, render, s.Raw)
	default:
		return fmt.Sprintf("%s %s (declared %q)", s.Name, render, s.Raw)
	}
}

// containerLimit is the memory limit the postgres container runs with,
// and where it was read.
type containerLimit struct {
	bytes    int64
	evidence Evidence
}

// memoryLimit is the postgres container's memory limit. The Cluster's
// declared limit wins: a LimitRange only defaults a missing limit or
// refuses a Pod, it never changes a declared one. Without one, the
// instance Pods say whether a LimitRange set a limit; the smallest is
// the one that bites first. nil with no reason when the Pods carry no
// limit either; a reason when no Pod was observed to tell.
func memoryLimit(in Input, sizing observe.PostgresSizing) (*containerLimit, string) {
	if sizing.MemoryLimit != nil {
		return &containerLimit{bytes: *sizing.MemoryLimit, evidence: Evidence{
			Origin: declaredOrigin, Object: "Cluster",
			Detail: fmt.Sprintf("resources.limits.memory %s (%d bytes), applied to the postgres container of every instance",
				iec(*sizing.MemoryLimit), *sizing.MemoryLimit),
		}}, ""
	}
	if !in.HasPods || in.Pods.Stale {
		return nil, "the Cluster declares no memory limit and its Pods are not observed, so a limit a LimitRange set on them cannot be ruled out"
	}
	var found *containerLimit
	seen := 0
	for _, pod := range in.Pods.Pods {
		if pod.Deleting {
			continue
		}
		seen++
		if pod.MemoryLimit != nil && (found == nil || *pod.MemoryLimit < found.bytes) {
			found = &containerLimit{bytes: *pod.MemoryLimit, evidence: Evidence{
				Origin: "observed on the Pod", Object: "Pod " + pod.Name,
				Detail: fmt.Sprintf("postgres container memory limit %s (%d bytes), not declared in the Cluster: set by a LimitRange or an admission webhook",
					iec(*pod.MemoryLimit), *pod.MemoryLimit),
			}}
		}
	}
	if seen == 0 {
		return nil, "the Cluster declares no memory limit and no instance Pod exists to show whether a LimitRange sets one"
	}
	return found, ""
}

// hugePagesBytes is the total huge page reservation.
func hugePagesBytes(sizing observe.PostgresSizing) int64 {
	var total int64
	for _, v := range sizing.HugePages {
		total += v
	}
	return total
}

// sharedBytes is the part of the estimate PostgreSQL allocates as one
// shared segment at start-up. The real segment is somewhat larger (lock
// tables, the other shared structures); PostgreSQL 15+ reports it as
// shared_memory_size, which /pg/status does not carry.
func sharedBytes(m pgconf.Memory) int64 {
	return m.SharedBuffers.Value + m.WALBuffers.Value
}

// segmentMargin is how far above shared_buffers + wal_buffers the real
// shared segment may reach: the lock table, the process array and the
// other shared structures, tens of MiB at common settings, more with
// many connections. A reservation within the margin may or may not hold
// the segment; the console cannot tell (shared_memory_size is not
// reported).
func segmentMargin(m pgconf.Memory) int64 {
	return max(32<<20, sharedBytes(m)/10)
}

// sharedInHugePages reports that the shared segment comes from the
// container's huge page reservation, which Kubernetes accounts apart
// from the memory limit. Only huge_pages = on proves it: a running
// instance then has its segment in huge pages (it does not start
// otherwise). With try it may have fallen back to normal pages, charged
// to the memory limit, so the segment is not subtracted.
func sharedInHugePages(sizing observe.PostgresSizing, m pgconf.Memory) bool {
	reserved := hugePagesBytes(sizing)
	return reserved > 0 && reserved >= sharedBytes(m) && m.HugePages.Value == pgconf.HugePagesOn
}

func majorOf(in Input) int {
	if v := in.Cluster.Cluster.PostgresMajorVersion; v != nil {
		return *v
	}
	return 0
}

// MemoryOverLimit matches a Cluster whose conventional worst-case
// PostgreSQL memory exceeds the postgres container's memory limit.
type MemoryOverLimit struct{}

func (MemoryOverLimit) describe() string {
	return "shared_buffers + wal_buffers + max_connections × work_mem + autovacuum_max_workers × " +
		"autovacuum memory above the postgres container's memory limit"
}

func (MemoryOverLimit) evaluate(_ string, in Input) ([]conditionMatch, string) {
	sizing, m, reason := sizingInput(in, "shared_buffers", "wal_buffers", "work_mem", "max_connections",
		"maintenance_work_mem", "autovacuum_work_mem", "autovacuum_max_workers", "huge_pages")
	if reason != "" {
		return nil, reason
	}
	lim, why := memoryLimit(in, sizing)
	if lim == nil {
		return nil, why
	}
	major := majorOf(in)
	estimate := m.WorstCase(major)
	charged := estimate
	inHuge := sharedInHugePages(sizing, m)
	if inHuge {
		charged -= sharedBytes(m)
	}
	limit := lim.bytes
	if charged <= limit {
		return nil, ""
	}
	avName := "maintenance_work_mem"
	if !m.AutovacuumWorkMem.Auto {
		avName = "autovacuum_work_mem"
	}
	terms := []string{
		setting(m.SharedBuffers, iec(m.SharedBuffers.Value)),
		setting(m.WALBuffers, iec(m.WALBuffers.Value)),
		fmt.Sprintf("%s × %s = %s", setting(m.MaxConnections, strconv.FormatInt(m.MaxConnections.Value, 10)),
			setting(m.WorkMem, iec(m.WorkMem.Value)), iec(m.MaxConnections.Value*m.WorkMem.Value)),
		fmt.Sprintf("%s × %s %s = %s", setting(m.AutovacuumMaxWorkers, strconv.FormatInt(m.AutovacuumMaxWorkers.Value, 10)),
			avName, iec(m.AutovacuumPerWorker(major)), iec(m.AutovacuumMaxWorkers.Value*m.AutovacuumPerWorker(major))),
	}
	derived := fmt.Sprintf("estimate %s against a limit of %s: %.1f× the limit", iec(charged), iec(limit), float64(charged)/float64(limit))
	if inHuge {
		derived += fmt.Sprintf("; the %s shared segment is left out, since it comes from the huge page reservation", iec(sharedBytes(m)))
	}
	if major > 0 && major < 17 && m.AutovacuumWorkMem.Value > m.AutovacuumPerWorker(major) {
		derived += fmt.Sprintf("; autovacuum memory capped at 1 GiB per worker on PostgreSQL %d", major)
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: fmt.Sprintf("PostgreSQL may use up to about %s, more than the %s memory limit: the container risks an OOM kill under load.",
			iec(charged), iec(limit)),
		evidence: []Evidence{
			{Origin: declaredOrigin, Object: "Cluster", Detail: "spec.postgresql.parameters: " + strings.Join(terms, "; ")},
			lim.evidence,
			{Origin: "console-derived", Object: "memory estimate", Detail: derived},
		},
	}}, ""
}

// SharedBuffersOverLimit matches a Cluster whose shared segment alone
// (shared_buffers + wal_buffers) reaches the memory limit.
type SharedBuffersOverLimit struct{}

func (SharedBuffersOverLimit) describe() string {
	return "shared_buffers + wal_buffers at or above the postgres container's memory limit"
}

func (SharedBuffersOverLimit) evaluate(_ string, in Input) ([]conditionMatch, string) {
	sizing, m, reason := sizingInput(in, "shared_buffers", "wal_buffers", "huge_pages")
	if reason != "" {
		return nil, reason
	}
	lim, why := memoryLimit(in, sizing)
	if lim == nil {
		return nil, why
	}
	if sharedInHugePages(sizing, m) || sharedBytes(m) < lim.bytes {
		return nil, ""
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: fmt.Sprintf("shared_buffers and wal_buffers (%s) do not fit the %s memory limit.", iec(sharedBytes(m)), iec(lim.bytes)),
		evidence: []Evidence{
			{Origin: declaredOrigin, Object: "Cluster", Detail: "spec.postgresql.parameters: " +
				setting(m.SharedBuffers, iec(m.SharedBuffers.Value)) + "; " + setting(m.WALBuffers, iec(m.WALBuffers.Value))},
			lim.evidence,
		},
	}}, ""
}

// EffectiveCacheSizeOverLimit matches an effective_cache_size larger
// than the memory limit.
type EffectiveCacheSizeOverLimit struct{}

func (EffectiveCacheSizeOverLimit) describe() string {
	return "effective_cache_size above the postgres container's memory limit"
}

func (EffectiveCacheSizeOverLimit) evaluate(_ string, in Input) ([]conditionMatch, string) {
	sizing, m, reason := sizingInput(in, "effective_cache_size")
	if reason != "" {
		return nil, reason
	}
	lim, why := memoryLimit(in, sizing)
	if lim == nil {
		return nil, why
	}
	if m.EffectiveCacheSize.Value <= lim.bytes {
		return nil, ""
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: fmt.Sprintf("effective_cache_size (%s) is larger than the %s memory limit.",
			iec(m.EffectiveCacheSize.Value), iec(lim.bytes)),
		evidence: []Evidence{
			{Origin: declaredOrigin, Object: "Cluster", Detail: "spec.postgresql.parameters: " + setting(m.EffectiveCacheSize, iec(m.EffectiveCacheSize.Value))},
			lim.evidence,
		},
	}}, ""
}

// HugePagesProblem is the kind of huge page mismatch a HugePages
// condition looks for.
type HugePagesProblem int

// The huge page mismatches.
const (
	// HugePagesMissing: huge_pages = on, and the reservation is absent
	// or smaller than the shared segment: PostgreSQL refuses to start.
	HugePagesMissing HugePagesProblem = iota
	// HugePagesUnused: a reservation PostgreSQL will not use, because
	// huge_pages is off or the reservation is too small for try.
	HugePagesUnused
)

// HugePages matches a mismatch between huge_pages and the container's
// huge page reservation. Whether the node has free huge pages is not
// observable here (the console reads no Node); a pod whose reservation
// cannot be met stays Pending, which the scheduling checks report.
type HugePages struct{ Problem HugePagesProblem }

func (c HugePages) describe() string {
	if c.Problem == HugePagesMissing {
		return "huge_pages = on without a hugepages-* reservation large enough for shared_buffers + wal_buffers"
	}
	return "a hugepages-* reservation PostgreSQL will not use (huge_pages off, or try with too small a reservation)"
}

func (c HugePages) evaluate(_ string, in Input) ([]conditionMatch, string) {
	sizing, m, reason := sizingInput(in, "shared_buffers", "wal_buffers", "huge_pages")
	if reason != "" {
		return nil, reason
	}
	reserved, need := hugePagesBytes(sizing), sharedBytes(m)
	// need is a floor: the real segment is larger by an amount the
	// console cannot read. Within the margin, whether it fits is unknown.
	inBand := reserved >= need && reserved < need+segmentMargin(m)
	var summary string
	switch {
	case inBand && c.Problem == HugePagesMissing && m.HugePages.Value == pgconf.HugePagesOn,
		inBand && c.Problem == HugePagesUnused && m.HugePages.Value == pgconf.HugePagesTry:
		return nil, fmt.Sprintf("the %s huge page reservation is barely above shared_buffers + wal_buffers (%s); the full shared segment is larger and not reported, so whether it fits cannot be told",
			iec(reserved), iec(need))
	case c.Problem == HugePagesMissing && m.HugePages.Value == pgconf.HugePagesOn && reserved < need:
		summary = fmt.Sprintf("huge_pages is on but the container reserves %s of huge pages for a shared segment of at least %s: PostgreSQL will refuse to start.",
			iec(reserved), iec(need))
	case c.Problem == HugePagesUnused && reserved > 0 && m.HugePages.Value == pgconf.HugePagesOff:
		summary = fmt.Sprintf("The container reserves %s of huge pages, but huge_pages is off: the reservation is unused.", iec(reserved))
	case c.Problem == HugePagesUnused && reserved > 0 && m.HugePages.Value == pgconf.HugePagesTry && reserved < need:
		summary = fmt.Sprintf("The container reserves %s of huge pages, less than the shared segment of at least %s: PostgreSQL falls back to normal pages and the reservation is unused.",
			iec(reserved), iec(need))
	default:
		return nil, ""
	}
	return []conditionMatch{{
		subject: clusterSubject,
		summary: summary,
		evidence: []Evidence{
			{Origin: declaredOrigin, Object: "Cluster", Detail: "spec.postgresql.parameters: " +
				setting(m.HugePages, pgconf.HugePagesWord(m.HugePages.Value)) + "; " +
				setting(m.SharedBuffers, iec(m.SharedBuffers.Value)) + "; " + setting(m.WALBuffers, iec(m.WALBuffers.Value))},
			{Origin: declaredOrigin, Object: "Cluster", Detail: "resources.limits: " + hugePagesText(sizing)},
		},
	}}, ""
}

func hugePagesText(sizing observe.PostgresSizing) string {
	if len(sizing.HugePages) == 0 {
		return "no hugepages-* resource"
	}
	names := make([]string, 0, len(sizing.HugePages))
	for n := range sizing.HugePages {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + " " + iec(sizing.HugePages[n])
	}
	return strings.Join(parts, ", ")
}

// ParametersUnparsable matches memory parameters PostgreSQL would not
// accept: the instance would refuse the configuration (at start, or as
// a reload error), and the console cannot size what it cannot read.
type ParametersUnparsable struct{}

func (ParametersUnparsable) describe() string {
	return "a memory parameter in spec.postgresql.parameters that does not parse as PostgreSQL reads it"
}

func (ParametersUnparsable) evaluate(_ string, in Input) ([]conditionMatch, string) {
	if reason := clusterUnavailable(in); reason != "" {
		return nil, reason
	}
	sizing := in.Cluster.Cluster.Sizing
	if !sizing.Read {
		return nil, "the Cluster's spec was not read for its PostgreSQL parameters"
	}
	m := readMemory(sizing)
	if len(m.Unparsable) == 0 {
		return nil, ""
	}
	evidence := make([]Evidence, 0, len(m.Unparsable))
	names := make([]string, 0, len(m.Unparsable))
	for _, u := range m.Unparsable {
		names = append(names, u.Name)
		evidence = append(evidence, Evidence{Origin: declaredOrigin, Object: "Cluster", Detail: "spec.postgresql.parameters." + u.Name + ": " + u.Reason})
	}
	return []conditionMatch{{
		subject:  clusterSubject,
		summary:  "PostgreSQL would not accept " + strings.Join(names, ", ") + " as declared.",
		evidence: evidence,
	}}, ""
}
