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

package config

// This file is the team-namespace console's read-side configuration. The
// variable names it shares with config.go keep their meaning there.
// Listener, session and identity settings belong to the API/auth layer and
// are not here.

import (
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Variables of the console contract, as charts/pgtools sets them
// (its README, "The console binary contract"). INSTANCE_STATUS_ENABLED,
// INSTANCE_STATUS_INTERVAL, INSTANCE_STATUS_CA_FILE, LOG_STREAM_ENABLED
// (effective only with ALLOW_LOGS), LOG_MATCH_MAX_AGE, METRICS_ENABLED,
// HISTORY_ENABLED, HISTORY_PATH, HISTORY_MAX_REVISIONS, HISTORY_MAX_BYTES,
// API_REQUEST_TIMEOUT, EVENTS_MAX_AGE, ALLOW_LOGS and
// ALLOW_CLUSTER_CATALOGS are declared in config.go; the differences of
// this read side are stated on the Console fields.
const (
	// EnvWatchNamespaces is the comma-separated list of namespaces the
	// console watches: the release namespace or a short list the team
	// owns. Never empty and never "all namespaces": the console holds
	// namespaced grants only.
	EnvWatchNamespaces = "WATCH_NAMESPACES"
	// EnvPodNamespace is the console's own namespace (downward API); it
	// is the default of WATCH_NAMESPACES when that variable is unset.
	EnvPodNamespace = "POD_NAMESPACE"
	// EnvClusterSelector narrows the watched Clusters by label selector.
	EnvClusterSelector = "CLUSTER_SELECTOR"
	// EnvResyncPeriod is the informers' long safety resync. Watches are
	// event-driven; this is not polling.
	EnvResyncPeriod = "RESYNC_PERIOD"
	// EnvReprobeInterval is how often optional APIs are probed again so a
	// CRD installed after start is picked up.
	EnvReprobeInterval = "REPROBE_INTERVAL"
	// EnvEventsMaxPerCluster bounds the events served per Cluster.
	EnvEventsMaxPerCluster = "EVENTS_MAX_PER_CLUSTER"
	// EnvInstanceStatusTimeout bounds one /pg/status or metrics read.
	EnvInstanceStatusTimeout = "INSTANCE_STATUS_TIMEOUT"
	// EnvInstanceStatusConcurrency bounds the concurrent reads of a sweep.
	EnvInstanceStatusConcurrency = "INSTANCE_STATUS_CONCURRENCY"
	// EnvRingRetention is how long the in-memory rings behind "since
	// when" and the sparklines keep samples (default 1 h).
	EnvRingRetention = "RING_RETENTION"
	// EnvVolumeTrendInterval is the slot of the long volume trend behind
	// the storage forecasts: one sample per volume per slot (default 15m).
	EnvVolumeTrendInterval = "VOLUME_TREND_INTERVAL"
	// EnvVolumeTrendRetention is how long the volume trend is kept
	// (default 30 days, about 2,880 samples per volume; at least the
	// forecast's seven-day fit window).
	EnvVolumeTrendRetention = "VOLUME_TREND_RETENTION"
	// EnvVolumeTrendMaxSeries bounds the volumes trended at once; the
	// least recently observed is dropped beyond it.
	EnvVolumeTrendMaxSeries = "VOLUME_TREND_MAX_SERIES"
	// EnvDiagnosticsEnabled runs the background diagnostics.
	EnvDiagnosticsEnabled = "DIAGNOSTICS_ENABLED"
	// EnvLifecycleFile is the end-of-life table (JSON) the diagnostics
	// read, the chart's lifecycle ConfigMap mounted as a file and
	// reloaded on change; unset or absent is the built-in table.
	EnvLifecycleFile = "LIFECYCLE_FILE"
	// EnvStateConfigMap names the ConfigMap in the console's namespace
	// its replicas share sign-outs and "since" times through; unset keeps
	// both per replica.
	EnvStateConfigMap = "STATE_CONFIGMAP"
	// EnvDiagnosticsInterval is the runner's safety cadence; a run also
	// follows every change of a Cluster's inputs.
	EnvDiagnosticsInterval = "DIAGNOSTICS_INTERVAL"
	// EnvDiagnosticsDebounce coalesces bursts of changes into one run.
	EnvDiagnosticsDebounce = "DIAGNOSTICS_DEBOUNCE"
	// EnvLogLevel is debug, info, warn or error.
	EnvLogLevel = "LOG_LEVEL"
	// EnvEvidenceEnabled reads repository evidence from the namespace's
	// EvidenceAgents; default true. Off, the backups screen
	// says "disabled" instead of looking for an agent.
	EnvEvidenceEnabled = "EVIDENCE_ENABLED"
	// EnvEvidenceTokenDir is the directory of the projected ServiceAccount
	// tokens the console presents to the agents, one per watched namespace
	// N at <dir>/N/token with audience pgtoolbox-evidence-agent/N;
	// default evidencev1alpha1.DefaultTokenDirectory.
	EnvEvidenceTokenDir = "EVIDENCE_TOKEN_DIR"
	// EnvEvidenceTokenPathRetired is a single cluster-wide token file. It is
	// refused at start, so packaging that sets it fails loudly instead of
	// presenting a token every agent refuses.
	EnvEvidenceTokenPathRetired = "EVIDENCE_TOKEN_PATH"
	// EnvEvidenceInterval is how often each agent is asked again.
	EnvEvidenceInterval = "EVIDENCE_INTERVAL"
	// EnvOperationsEnabled serves the operations endpoints;
	// default true. Every write is still the caller's own, authorized by
	// the API server; false removes the endpoints' effect (501).
	EnvOperationsEnabled = "OPERATIONS_ENABLED"
)

// maxWatchNamespaces bounds the "short list the team owns". A console
// watching more than this is the cluster-scoped shape the console's
// namespaced grants rule out; the answer is another team console.
const maxWatchNamespaces = 16

// Console is the validated configuration of the console's read side.
type Console struct {
	// WatchNamespaces are the watched namespaces, deduplicated, in the
	// order given. Never empty.
	WatchNamespaces []string
	// ClusterSelector narrows the Clusters; labels.Everything() when unset.
	ClusterSelector labels.Selector
	// APIRequestTimeout bounds one API request (probes, lists).
	APIRequestTimeout time.Duration
	// ResyncPeriod is the informers' safety resync.
	ResyncPeriod time.Duration
	// ReprobeInterval is the optional-API re-probe cadence.
	ReprobeInterval time.Duration
	// EventsMaxAge and EventsMaxPerCluster bound the events served.
	EventsMaxAge        time.Duration
	EventsMaxPerCluster int
	// AllowLogs lets the API serve pods/log reads (ALLOW_LOGS, default
	// true; the chart sets false when its Role grants no pods/log).
	AllowLogs bool
	// AllowClusterCatalogs reads ClusterImageCatalogs through the opt-in
	// ClusterRole; without the binding the panel says "not readable".
	AllowClusterCatalogs bool

	// InstanceStatus is the /pg/status sweep.
	InstanceStatus InstanceStatus
	// MetricsEnabled scrapes the instances' exporters on the same sweep.
	MetricsEnabled bool
	// RingRetention is the in-memory ring window.
	RingRetention time.Duration
	// VolumeTrend is the long, coarse trend per volume behind the storage
	// forecasts; it is mirrored next to HISTORY_PATH when that is set.
	VolumeTrend VolumeTrend

	// DiagnosticsEnabled, DiagnosticsInterval and DiagnosticsDebounce
	// drive the background runner.
	DiagnosticsEnabled  bool
	DiagnosticsInterval time.Duration
	DiagnosticsDebounce time.Duration
	// LifecycleFile is the deployment's end-of-life table; empty is the
	// built-in one.
	LifecycleFile string
	// StateConfigMap is the shared state ConfigMap's name; empty shares
	// nothing (the sign-out and the clocks stay per replica).
	StateConfigMap string

	// History is the optional bbolt history.
	History History
	// Digest is the daily digest; it needs History.
	Digest Digest

	// LogLevel is debug, info, warn or error.
	LogLevel string
	// Evidence is the evidence-agent consumer.
	Evidence Evidence
	// OperationsEnabled serves the operations endpoints.
	OperationsEnabled bool
	// LogFollow follows member containers' logs for the diagnostics
	// (LOG_STREAM_ENABLED and ALLOW_LOGS); LogMatchMaxAge expires what
	// nothing has repeated.
	LogFollow      bool
	LogMatchMaxAge time.Duration
	// Prometheus is the PromQL source of the metrics charts.
	Prometheus Prometheus
	// MonitoringURL is the link-out to the site's monitoring system
	// (MONITORING_URL, https or root-relative); empty when unset.
	MonitoringURL string
	// PgAdminURL is a fixed pgAdmin link (PGADMIN_URL, https or
	// root-relative); empty when unset.
	PgAdminURL string
	// RestoreDrill is the restore-drill operation (RESTORE_DRILL_*).
	RestoreDrill RestoreDrill
	// MCP is the MCP endpoint (MCP_*).
	MCP MCP
	// Tracing is the OpenTelemetry exporter (OTEL_*, TRACING_*).
	Tracing Tracing
}

// Evidence configures the evidence-agent consumer.
type Evidence struct {
	Enabled bool
	// TokenDirectory holds one token per watched namespace (<dir>/<ns>/token);
	// empty selects evidencev1alpha1.DefaultTokenDirectory.
	TokenDirectory string
	Interval       time.Duration
	// AgentsFile is EVIDENCE_AGENTS_FILE; Agents its validated entries.
	// Set, the console reads these fixed agents (the pgtools chart's,
	// without the operator) and lists no EvidenceAgent.
	AgentsFile string
	Agents     []StaticAgent
}

// VolumeTrend bounds the long volume trend: Retention / Interval
// samples per volume, at most MaxSeries volumes.
type VolumeTrend struct {
	Interval  time.Duration
	Retention time.Duration
	MaxSeries int
}

// VolumeTrendBudgetMiB caps what the volume trend may hold once every
// series is full. Each bound alone looks harmless, but their product does
// not: one-minute samples for 90 days on 100,000 volumes would be about
// 97 GiB. The defaults (16,384 volumes, 30 days at 15 minutes) come to
// about 360 MiB and fit, as does twice the retention or the volumes.
const VolumeTrendBudgetMiB = 768

// volumeTrendSampleBytes is one sample: bytes and inodes as two float32.
const volumeTrendSampleBytes = 8

// WorstCaseMiB is the memory of the samples once every series is full.
func (v VolumeTrend) WorstCaseMiB() int64 {
	if v.Interval <= 0 {
		return 0
	}
	samples := int64(v.Retention / v.Interval)
	return samples * int64(v.MaxSeries) * volumeTrendSampleBytes >> 20
}

// InstanceStatus configures the bounded /pg/status sweep.
type InstanceStatus struct {
	Enabled     bool
	Interval    time.Duration
	Timeout     time.Duration
	Concurrency int
	// CAFile is the admin-filled CA bundle: a PEM
	// concatenation of the watched Clusters' ca.crt, mounted from a
	// ConfigMap and re-read when it changes. Empty when the chart mounts
	// none. There is no insecure mode: a Cluster whose CA is
	// missing is an error row.
	CAFile string
}

// History configures the optional persistent history.
type History struct {
	// Enabled keeps the bounded revision history at all (in memory unless
	// Path is set); HISTORY_ENABLED, default true.
	Enabled bool
	// Path is the bbolt file; empty keeps history in memory only, which
	// is what lets the console run two replicas. A non-empty path means
	// one replica with Recreate — the chart's rule.
	Path         string
	MaxRevisions int
	MaxBytes     int
}

// Persistent reports whether history survives restarts.
func (h History) Persistent() bool { return h.Path != "" }

// TimelinePath is the file of the incident timeline and the digests,
// beside the history journal on the same volume (timeline.db in its
// directory); empty when history is in memory.
func (h History) TimelinePath() string {
	if h.Path == "" {
		return ""
	}
	name := "timeline.db"
	if filepath.Base(h.Path) == name {
		name = "timeline-journal.db"
	}
	return filepath.Join(filepath.Dir(h.Path), name)
}

// LoadConsole reads the console contract through lookup. Validation is total:
// every variable is checked and the returned error joins one *VarError
// per violated constraint, naming the variable and the rule and never
// the supplied value.
func LoadConsole(lookup Lookup) (Console, error) {
	var errs []error
	fail := func(name, constraint string) {
		errs = append(errs, &VarError{Name: name, Constraint: constraint})
	}

	cfg := Console{ClusterSelector: labels.Everything()}
	cfg.WatchNamespaces = watchNamespaces(lookup, fail)
	if raw, ok := lookup(EnvClusterSelector); ok && strings.TrimSpace(raw) != "" {
		selector, err := labels.Parse(raw)
		if err != nil {
			fail(EnvClusterSelector, "must be a Kubernetes label selector")
		} else {
			cfg.ClusterSelector = selector
		}
	}
	cfg.APIRequestTimeout = durationVar(lookup, EnvAPIRequestTimeout, 10*time.Second, time.Second, 2*time.Minute, fail)
	cfg.ResyncPeriod = durationVar(lookup, EnvResyncPeriod, 10*time.Hour, time.Hour, 24*time.Hour, fail)
	cfg.ReprobeInterval = durationVar(lookup, EnvReprobeInterval, 5*time.Minute, time.Minute, time.Hour, fail)
	cfg.EventsMaxAge = durationVar(lookup, EnvEventsMaxAge, time.Hour, 5*time.Minute, 24*time.Hour, fail)
	cfg.EventsMaxPerCluster = intVar(lookup, EnvEventsMaxPerCluster, 200, 10, 2000, fail)
	cfg.AllowClusterCatalogs = boolVar(lookup, EnvAllowClusterCatalogs, false, fail)
	cfg.AllowLogs = boolVar(lookup, EnvAllowLogs, true, fail)

	cfg.InstanceStatus = instanceStatus(lookup, fail)
	cfg.MetricsEnabled = boolVar(lookup, EnvMetricsEnabled, true, fail)
	cfg.RingRetention = durationVar(lookup, EnvRingRetention, time.Hour, 5*time.Minute, 6*time.Hour, fail)
	if cfg.RingRetention < cfg.InstanceStatus.Interval*4 {
		fail(EnvRingRetention, "must hold at least four sweeps ("+EnvInstanceStatusInterval+" × 4)")
	}
	cfg.VolumeTrend = VolumeTrend{
		Interval:  durationVar(lookup, EnvVolumeTrendInterval, 15*time.Minute, time.Minute, time.Hour, fail),
		Retention: durationVar(lookup, EnvVolumeTrendRetention, 30*24*time.Hour, 7*24*time.Hour, 90*24*time.Hour, fail),
		MaxSeries: intVar(lookup, EnvVolumeTrendMaxSeries, 16384, 16, 100_000, fail),
	}
	if cfg.VolumeTrend.Interval < cfg.InstanceStatus.Interval {
		fail(EnvVolumeTrendInterval, "must not be shorter than "+EnvInstanceStatusInterval)
	}
	if mib := cfg.VolumeTrend.WorstCaseMiB(); mib > VolumeTrendBudgetMiB {
		fail(EnvVolumeTrendMaxSeries, "with this retention and interval holds up to "+strconv.FormatInt(mib, 10)+
			" MiB of samples; keep "+EnvVolumeTrendMaxSeries+" × "+EnvVolumeTrendRetention+" / "+EnvVolumeTrendInterval+
			" under "+strconv.Itoa(VolumeTrendBudgetMiB)+" MiB")
	}

	cfg.DiagnosticsEnabled = boolVar(lookup, EnvDiagnosticsEnabled, true, fail)
	cfg.DiagnosticsInterval = durationVar(lookup, EnvDiagnosticsInterval, time.Minute, 10*time.Second, 30*time.Minute, fail)
	cfg.DiagnosticsDebounce = durationVar(lookup, EnvDiagnosticsDebounce, 2*time.Second, 0, 30*time.Second, fail)
	if raw, _ := lookup(EnvLifecycleFile); raw != "" {
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(EnvLifecycleFile, "must be an absolute, clean path")
		} else {
			cfg.LifecycleFile = raw
		}
	}
	if raw, _ := lookup(EnvStateConfigMap); raw != "" {
		if len(validation.IsDNS1123Subdomain(raw)) != 0 {
			fail(EnvStateConfigMap, "must be a ConfigMap name")
		} else {
			cfg.StateConfigMap = raw
		}
	}

	cfg.History = history(lookup, fail)
	cfg.Digest = digest(lookup, cfg.History, fail)

	cfg.Evidence = Evidence{
		Enabled:  boolVar(lookup, EnvEvidenceEnabled, true, fail),
		Interval: durationVar(lookup, EnvEvidenceInterval, time.Minute, 10*time.Second, time.Hour, fail),
	}
	if _, ok := lookup(EnvEvidenceTokenPathRetired); ok {
		fail(EnvEvidenceTokenPathRetired, "is retired: the console now reads one token per watched namespace from "+
			EnvEvidenceTokenDir+" (<dir>/<namespace>/token, audience pgtoolbox-evidence-agent/<namespace>); upgrade the chart or manifests")
	}
	if raw, ok := lookup(EnvEvidenceTokenDir); ok && raw != "" {
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(EnvEvidenceTokenDir, "must be an absolute, clean path")
		} else {
			cfg.Evidence.TokenDirectory = raw
		}
	}
	if raw, ok := lookup(EnvEvidenceAgentsFile); ok && raw != "" && cfg.Evidence.Enabled {
		cfg.Evidence.AgentsFile = raw
		cfg.Evidence.Agents = loadStaticAgents(raw, cfg.WatchNamespaces, fail)
	}
	cfg.OperationsEnabled = boolVar(lookup, EnvOperationsEnabled, true, fail)
	cfg.LogFollow = boolVar(lookup, EnvLogStreamEnabled, true, fail) && cfg.AllowLogs
	cfg.LogMatchMaxAge = durationVar(lookup, EnvLogMatchMaxAge, 6*time.Hour, 10*time.Minute, 48*time.Hour, fail)
	cfg.MonitoringURL = linkVar(lookup, EnvMonitoringURL, false, fail)
	cfg.PgAdminURL = linkVar(lookup, EnvPgAdminURL, false, fail)
	cfg.Prometheus = prometheus(lookup, fail)
	cfg.RestoreDrill = restoreDrill(lookup, cfg.WatchNamespaces, fail)
	if cfg.RestoreDrill.Enabled && !cfg.OperationsEnabled {
		// A drill is an operation: its start is a write as the user.
		fail(EnvRestoreDrillEnabled, "requires "+EnvOperationsEnabled+"=true")
	}
	cfg.MCP = mcpConfig(lookup, fail)
	cfg.Tracing = tracingConfig(lookup, fail)
	cfg.LogLevel = "info"
	if raw, ok := lookup(EnvLogLevel); ok && raw != "" {
		switch raw {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = raw
		default:
			fail(EnvLogLevel, "must be debug, info, warn or error")
		}
	}

	if len(errs) > 0 {
		return Console{}, errors.Join(errs...)
	}
	return cfg, nil
}

// watchNamespaces reads WATCH_NAMESPACES, defaulting to POD_NAMESPACE
// only when the variable is absent. A variable that is present but empty
// is an error: it is the one value a careless template produces, and it
// must never come to mean "every namespace".
func watchNamespaces(lookup Lookup, fail func(name, constraint string)) []string {
	raw, set := lookup(EnvWatchNamespaces)
	if !set {
		own, ok := lookup(EnvPodNamespace)
		if !ok || own == "" {
			fail(EnvWatchNamespaces, "is required (or set "+EnvPodNamespace+" to watch the console's own namespace); it is never all namespaces")
			return nil
		}
		raw = own
	}
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		name := strings.TrimSpace(part)
		switch {
		case name == "":
			continue
		case name == "*" || strings.EqualFold(name, "all"):
			fail(EnvWatchNamespaces, "must list namespaces; there is no all-namespaces console")
			return nil
		case len(name) > 63 || !dns1123Label.MatchString(name):
			fail(EnvWatchNamespaces, "must be a comma-separated list of DNS-1123 labels")
			return nil
		case slices.Contains(out, name):
			continue
		}
		out = append(out, name)
	}
	switch {
	case len(out) == 0:
		fail(EnvWatchNamespaces, "must name at least one namespace; an empty value is not all namespaces")
		return nil
	case len(out) > maxWatchNamespaces:
		fail(EnvWatchNamespaces, "must name at most "+strconv.Itoa(maxWatchNamespaces)+" namespaces; run one console per team instead")
		return nil
	}
	return out
}

func instanceStatus(lookup Lookup, fail func(name, constraint string)) InstanceStatus {
	is := InstanceStatus{
		Enabled:     boolVar(lookup, EnvInstanceStatusEnabled, true, fail),
		Interval:    durationVar(lookup, EnvInstanceStatusInterval, 30*time.Second, 5*time.Second, 10*time.Minute, fail),
		Timeout:     durationVar(lookup, EnvInstanceStatusTimeout, 5*time.Second, time.Second, 30*time.Second, fail),
		Concurrency: intVar(lookup, EnvInstanceStatusConcurrency, 8, 1, 64, fail),
	}
	if is.Timeout >= is.Interval {
		fail(EnvInstanceStatusTimeout, "must be shorter than "+EnvInstanceStatusInterval)
	}
	if raw, ok := lookup(EnvInstanceStatusInsecure); ok && raw != "" && raw != "false" {
		fail(EnvInstanceStatusInsecure, "is not supported: a Cluster whose CA is not in the bundle is shown as not verifiable, never read unverified")
	}
	// The chart sets the CA file only when the admin named a bundle
	// ConfigMap. Without one the sweep still runs and every instance is an
	// error row "not verifiable: no CA bundle configured" — never an
	// unverified read and never a start-up failure that hides the rest of
	// the console.
	if raw, _ := lookup(EnvInstanceStatusCAFile); raw != "" {
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(EnvInstanceStatusCAFile, "must be an absolute, clean path")
		} else {
			is.CAFile = raw
		}
	}
	return is
}

func history(lookup Lookup, fail func(name, constraint string)) History {
	h := History{
		Enabled:      boolVar(lookup, EnvHistoryEnabled, true, fail),
		MaxRevisions: intVar(lookup, EnvHistoryMaxRevisions, 5000, 100, 1_000_000, fail),
		MaxBytes:     intVar(lookup, EnvHistoryMaxBytes, 64<<20, 1<<20, 1<<30, fail),
	}
	if raw, ok := lookup(EnvHistoryPath); ok && raw != "" {
		if !filepath.IsAbs(raw) || filepath.Clean(raw) != raw {
			fail(EnvHistoryPath, "must be an absolute, clean path")
		} else {
			h.Path = raw
		}
		if !h.Enabled {
			// A mounted file with history switched off is conflicting
			// intent, not a silent no-op.
			fail(EnvHistoryPath, "requires "+EnvHistoryEnabled+"=true")
		}
	}
	return h
}

// Info is the configuration as name/value pairs for the console's
// pgtoolbox_console_config_info metric and its start-up log line. Nothing here
// is secret; the CA bundle is named by path, never by content.
func (c Console) Info() map[string]string {
	info := map[string]string{
		EnvWatchNamespaces:           strings.Join(c.WatchNamespaces, ","),
		EnvClusterSelector:           c.ClusterSelector.String(),
		EnvInstanceStatusEnabled:     strconv.FormatBool(c.InstanceStatus.Enabled),
		EnvInstanceStatusInterval:    c.InstanceStatus.Interval.String(),
		EnvInstanceStatusConcurrency: strconv.Itoa(c.InstanceStatus.Concurrency),
		EnvInstanceStatusCAFile:      c.InstanceStatus.CAFile,
		EnvMetricsEnabled:            strconv.FormatBool(c.MetricsEnabled),
		EnvRingRetention:             c.RingRetention.String(),
		EnvVolumeTrendInterval:       c.VolumeTrend.Interval.String(),
		EnvVolumeTrendRetention:      c.VolumeTrend.Retention.String(),
		EnvVolumeTrendMaxSeries:      strconv.Itoa(c.VolumeTrend.MaxSeries),
		EnvDiagnosticsEnabled:        strconv.FormatBool(c.DiagnosticsEnabled),
		EnvLifecycleFile:             c.LifecycleFile,
		EnvStateConfigMap:            c.StateConfigMap,
		EnvHistoryEnabled:            strconv.FormatBool(c.History.Enabled),
		EnvHistoryPath:               c.History.Path,
		EnvAllowLogs:                 strconv.FormatBool(c.AllowLogs),
		EnvAllowClusterCatalogs:      strconv.FormatBool(c.AllowClusterCatalogs),
		EnvEvidenceEnabled:           strconv.FormatBool(c.Evidence.Enabled),
		EnvEvidenceInterval:          c.Evidence.Interval.String(),
		EnvEvidenceAgentsFile:        c.Evidence.AgentsFile,
		EnvOperationsEnabled:         strconv.FormatBool(c.OperationsEnabled),
		EnvLogStreamEnabled:          strconv.FormatBool(c.LogFollow),
		EnvMonitoringURL:             c.MonitoringURL,
		EnvPgAdminURL:                c.PgAdminURL,
	}
	c.Prometheus.info(info)
	c.Digest.info(info)
	c.RestoreDrill.info(info)
	c.MCP.info(info)
	c.Tracing.info(info)
	return info
}
