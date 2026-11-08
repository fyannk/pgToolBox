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

// Package preview says what a change to a CloudNativePG Cluster spec
// sets in motion: which parameters apply on a reload and which wait for
// a restart, whether the operator rolls the instance Pods, how the
// primary is handled (switchover, in-place restart, or waiting for a
// person) and in which order the instances restart.
//
// It is a pure function of two Cluster definitions — the one in force
// and the one proposed or recorded — so the history screen uses it on a
// past revision and a future editor can use it on a proposed spec. It
// predicts from the operator's rules, never from the instances: the
// instance's own pg_settings.pending_restart (pendingRestart in
// /pg/status) remains the fact once the change is applied, and this
// package says so rather than claiming it.
//
// The rules were read from CloudNativePG v1.30.0
// (internal/controller/cluster_upgrade.go: rolloutRequiredInstances,
// updatePrimaryPod, isPodNeedingRollout and its checkers;
// pkg/postgres/configuration.go: FixedConfigurationParameters;
// pkg/management/postgres/probes.go: the hot-standby-sensitive
// decrease), and the parameter contexts from PostgreSQL (gucs.go).
package preview

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
)

// Effect is what a change does to the running instances, in increasing
// order of disruption. Refused and Unknown are outside the order: the
// first changes nothing (the spec cannot carry it), the second is a
// change the classifier does not know, and an unknown change makes the
// whole preview unknown (with the known effects as a floor, AtLeast).
type Effect string

// The effects.
const (
	// EffectNone changes nothing on the running instances (an
	// operator-side setting, a scale, a volume resize).
	EffectNone Effect = "none"
	// EffectReload is applied by a configuration reload, without a
	// restart (sighup, superuser and user parameters; pg_hba; new TLS
	// certificates).
	EffectReload Effect = "reload"
	// EffectStart starts PostgreSQL on instances that were stopped: a
	// fence lifted (the instance manager starts PostgreSQL again) or
	// hibernation switched off (the operator recreates the Pods).
	EffectStart Effect = "start"
	// EffectRestart needs PostgreSQL restarted (pg_settings
	// pending_restart). The operator does not restart a replica in
	// place: it deletes the replica's Pod, which is recreated on the same
	// volumes (rolloutRequiredInstances → upgradePod). Only the primary,
	// under primaryUpdateMethod restart, restarts in place
	// (updatePrimaryPod → updateRestartAnnotation); a lower
	// hot-standby-sensitive value has the primary's own instance manager
	// restart it first.
	EffectRestart Effect = "restart"
	// EffectRollout recreates the instance Pods one by one (the operator's
	// rolling update).
	EffectRollout Effect = "rollout"
	// EffectShutdown stops PostgreSQL: fencing (the listed instances, or
	// all with "*") or hibernation (every instance, Pods deleted).
	EffectShutdown Effect = "shutdown"
	// EffectMajorUpgrade is an offline major version upgrade: every
	// instance stops, pg_upgrade runs, and the replicas are cloned again.
	EffectMajorUpgrade Effect = "majorUpgrade"
	// EffectRefused is a value the operator's admission webhook refuses
	// (a parameter it fixes or blocks): the spec cannot carry it.
	EffectRefused Effect = "refused"
	// EffectUnknown is a change this package cannot classify, or a
	// parameter the Cluster's PostgreSQL major does not have (context
	// absent: PostgreSQL rejects the configuration file, so neither a
	// reload nor a restart can be predicted).
	EffectUnknown Effect = "unknown"
)

var rank = map[Effect]int{
	EffectNone: 0, EffectReload: 1, EffectStart: 2, EffectRestart: 3, EffectRollout: 4, EffectShutdown: 5, EffectMajorUpgrade: 6,
}

// worst returns the more disruptive of two ordered effects; Refused and
// Unknown rank as None and are counted apart.
func worst(a, b Effect) Effect {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// The parameter contexts reported.
const (
	// ContextPostmaster is read at server start only.
	ContextPostmaster = "postmaster"
	// ContextReload is sighup, superuser, user or backend: a reload (or a
	// new session) applies it.
	ContextReload = "reload"
	// ContextFixed is managed by the operator.
	ContextFixed = "fixed"
	// ContextBlocked must keep its default for the operator to work.
	ContextBlocked = "blocked"
	// ContextAbsent is a parameter the tables know from another
	// PostgreSQL major but which the Cluster's major does not have.
	ContextAbsent = "absent"
)

// ParameterChange is one changed postgresql.parameters entry.
type ParameterChange struct {
	Name string `json:"name"`
	// Before and After are the values; empty for an added or removed
	// (reset to default) parameter.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	// Effect is reload, restart, refused or unknown.
	Effect Effect `json:"effect"`
	// Context is postmaster, reload, fixed, blocked or absent; empty when
	// unknown.
	Context string `json:"context,omitempty"`
	// Decrease is a lower value of a hot-standby-sensitive parameter: the
	// primary restarts first.
	Decrease bool `json:"decrease,omitempty"`
}

// Reason codes of a field change, the closed vocabulary the UI words.
const (
	ReasonImage             = "image"
	ReasonMajorVersion      = "majorVersion"
	ReasonPodTemplate       = "podTemplate"
	ReasonNewVolume         = "newVolume"
	ReasonVolumeResize      = "volumeResize"
	ReasonScale             = "scale"
	ReasonRestartAnnotation = "restartAnnotation"
	ReasonHibernation       = "hibernation"
	ReasonFencing           = "fencing"
	ReasonSharedLibraries   = "sharedPreloadLibraries"
	ReasonManagedExtension  = "managedExtension"
	ReasonCertificates      = "certificates"
	ReasonHBA               = "hba"
	ReasonSynchronous       = "synchronous"
	ReasonReplicaRole       = "replicaRole"
	ReasonOperatorOnly      = "operatorOnly"
	ReasonUnknownField      = "unknownField"
)

// FieldChange is one changed spec field (or annotation) outside the
// parameters.
type FieldChange struct {
	// Path is the field, such as "spec.resources" or
	// "metadata.annotations.cnpg.io/hibernation".
	Path   string `json:"path"`
	Effect Effect `json:"effect"`
	// Reason is one of the Reason codes.
	Reason string `json:"reason"`
}

// Primary actions: what the operator does with the primary once the
// replicas are done (or first, for a decrease).
const (
	PrimaryNone           = "none"
	PrimaryReload         = "reload"
	PrimaryRestartInPlace = "restartInPlace"
	PrimaryRecreate       = "recreate"
	PrimarySwitchover     = "switchover"
	PrimaryWaitForUser    = "waitForUser"
	PrimaryRestartFirst   = "restartFirst"
	PrimaryMajorUpgrade   = "majorUpgrade"
	PrimaryShutdown       = "shutdown"
	PrimaryStart          = "start"
	// PrimaryUnknown: a change could not be classified, so what happens
	// to the primary cannot be said (the steps are then a floor).
	PrimaryUnknown = "unknown"
)

// Step is one stage of the order in which instances are touched.
type Step struct {
	// Role is "replicas", "primary" or "all".
	Role string `json:"role"`
	// Instances are the instance names known from the Cluster status;
	// empty when it reports none. Replicas are listed by name: the
	// operator takes them most-lagged first, which is a fact of the
	// moment the update runs, not of the spec.
	Instances []string `json:"instances"`
	// Action is reload, recreate, start, shutdown, or a Primary* action.
	Action string `json:"action"`
}

// Preview is what applying After over Before sets in motion.
type Preview struct {
	// Effect is the most disruptive ordered effect of all changes, or
	// unknown as soon as one change could not be classified.
	Effect Effect `json:"effect"`
	// AtLeast is set when Effect is unknown: the most disruptive effect of
	// the changes that were classified, which the change does at least.
	AtLeast Effect `json:"atLeast,omitempty"`
	// Parameters are the changed postgresql.parameters, by name.
	Parameters []ParameterChange `json:"parameters"`
	// Fields are the other changed fields, by path.
	Fields []FieldChange `json:"fields"`
	// Strategy and Method are the primaryUpdateStrategy and
	// primaryUpdateMethod in force after the change (defaults applied).
	Strategy string `json:"strategy" enum:"unsupervised,supervised"`
	Method   string `json:"method" enum:"restart,switchover"`
	// Primary is what happens to the primary; unknown when a change could
	// not be classified (unless every instance is shut down anyway).
	Primary string `json:"primary"`
	// Steps is the order.
	Steps []Step `json:"steps"`
	// PostgresMajor is the major the parameter table was read for; 0 when
	// unknown, in which case only parameters with the same context in
	// every covered major are classified.
	PostgresMajor int `json:"postgresMajor,omitempty"`
	// Verified reports that both the PostgreSQL major and the
	// CloudNativePG minor are covered by the tables and that every change
	// was classified.
	Verified bool `json:"verified"`
	// Unknown counts the changes classified unknown.
	Unknown int `json:"unknown,omitempty"`
}

// Input is the two definitions and what is known of the target.
type Input struct {
	// Before is the definition in force; nil when there is none (a first
	// observation), in which case only After's own facts are used.
	Before []byte
	// After is the recorded or proposed Cluster, as JSON.
	After []byte
	// PostgresMajor overrides the major read from the definitions.
	PostgresMajor int
	// CNPGMinor is the operator minor; empty when unknown.
	CNPGMinor cnpgstatus.Minor
}

// maxFields bounds the reported field changes.
const maxFields = 128

// cluster is the part of a Cluster definition the preview reads; the
// rest of the spec is compared generically.
type cluster struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec   map[string]json.RawMessage `json:"spec"`
	Status struct {
		CurrentPrimary  string   `json:"currentPrimary"`
		InstanceNames   []string `json:"instanceNames"`
		PGDataImageInfo *struct {
			MajorVersion int `json:"majorVersion"`
		} `json:"pgDataImageInfo"`
	} `json:"status"`
}

type postgresSpec struct {
	Parameters map[string]string `json:"parameters"`
}

// ErrNoDefinition is returned when After is empty.
var ErrNoDefinition = errors.New("preview: no definition to preview")

// Compute is the preview of After over Before.
func Compute(in Input) (Preview, error) {
	if len(in.After) == 0 {
		return Preview{}, ErrNoDefinition
	}
	var before, after cluster
	if len(in.Before) > 0 {
		if err := json.Unmarshal(in.Before, &before); err != nil {
			return Preview{}, fmt.Errorf("preview: reading the previous definition: %w", err)
		}
	}
	if err := json.Unmarshal(in.After, &after); err != nil {
		return Preview{}, fmt.Errorf("preview: reading the definition: %w", err)
	}
	p := Preview{Effect: EffectNone, Parameters: []ParameterChange{}, Fields: []FieldChange{}, Steps: []Step{}}
	p.PostgresMajor = postgresMajor(in.PostgresMajor, &after)
	p.Verified = p.PostgresMajor >= minMajor && p.PostgresMajor <= maxMajor && minorCovered(in.CNPGMinor)

	p.Strategy = specString(after.Spec, "primaryUpdateStrategy", "unsupervised")
	p.Method = specString(after.Spec, "primaryUpdateMethod", "restart")

	decrease := p.parameters(&before, &after, in.CNPGMinor)
	p.fields(&before, &after)
	p.annotations(&before, &after)
	// known is every classified change; operational leaves out fencing
	// and hibernation, which the plan handles as their own steps.
	known, operational := EffectNone, EffectNone
	for _, c := range p.Parameters {
		if c.Effect == EffectUnknown {
			p.Unknown++
			continue
		}
		known, operational = worst(known, c.Effect), worst(operational, c.Effect)
	}
	for _, f := range p.Fields {
		if f.Effect == EffectUnknown {
			p.Unknown++
			continue
		}
		known = worst(known, f.Effect)
		if f.Reason != ReasonFencing && f.Reason != ReasonHibernation {
			operational = worst(operational, f.Effect)
		}
	}
	p.Effect = known
	p.plan(&before, &after, decrease, operational)
	if p.Unknown > 0 {
		// An unclassified change may be anything up to a rollout: the
		// preview never claims less than it knows, nor that the known part
		// is all there is. The steps stay as the floor.
		p.Effect, p.AtLeast, p.Verified = EffectUnknown, known, false
		if p.Primary != PrimaryShutdown {
			p.Primary = PrimaryUnknown
		}
	}
	return p, nil
}

// minorCovered reports whether the operator rules were read for m.
func minorCovered(m cnpgstatus.Minor) bool {
	return m == cnpgstatus.Minor130 || m == cnpgstatus.Minor131
}

var imageMajor = regexp.MustCompile(`:(\d{2})(?:[.\-_]|$)`)

// postgresMajor is the override, the status' PGDATA major, or the
// image tag's leading number.
func postgresMajor(override int, c *cluster) int {
	if override > 0 {
		return override
	}
	if c.Status.PGDataImageInfo != nil && c.Status.PGDataImageInfo.MajorVersion > 0 {
		return c.Status.PGDataImageInfo.MajorVersion
	}
	return tagMajor(specString(c.Spec, "imageName", ""))
}

func tagMajor(image string) int {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	m := imageMajor.FindStringSubmatch(image)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func specString(spec map[string]json.RawMessage, key, def string) string {
	var s string
	if raw, ok := spec[key]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	return def
}

// fixed returns the operator's class of a parameter it manages, for the
// given operator minor: CloudNativePG's FixedConfigurationParameters at
// v1.30.0, less ssl_ecdh_curve on main (planned
// 1.31).
func fixed(name string, minor cnpgstatus.Minor) string {
	if name == "ssl_ecdh_curve" {
		if minor == cnpgstatus.Minor131 {
			return ""
		}
		return ContextFixed
	}
	return fixedParameters[name]
}

var fixedParameters = map[string]string{
	"allow_system_table_mods": ContextBlocked, "archive_mode": ContextFixed, "bonjour": ContextBlocked,
	"bonjour_name": ContextBlocked, "cluster_name": ContextFixed, "config_file": ContextBlocked,
	"data_directory": ContextBlocked, "data_sync_retry": ContextBlocked, "event_source": ContextBlocked,
	"external_pid_file": ContextBlocked, "hba_file": ContextBlocked, "hot_standby": ContextBlocked,
	"ident_file": ContextBlocked, "jit_provider": ContextBlocked, "listen_addresses": ContextBlocked,
	"logging_collector": ContextBlocked, "port": ContextFixed, "primary_conninfo": ContextFixed,
	"primary_slot_name": ContextFixed, "recovery_target": ContextFixed, "recovery_target_action": ContextFixed,
	"recovery_target_inclusive": ContextFixed, "recovery_target_lsn": ContextFixed, "recovery_target_name": ContextFixed,
	"recovery_target_time": ContextFixed, "recovery_target_timeline": ContextFixed, "recovery_target_xid": ContextFixed,
	"restore_command": ContextFixed, "shared_preload_libraries": ContextFixed, "temp_tablespaces": ContextFixed,
	"unix_socket_directories": ContextBlocked, "unix_socket_group": ContextBlocked, "unix_socket_permissions": ContextBlocked,
	"archive_cleanup_command": ContextBlocked, "archive_command": ContextFixed, "log_destination": ContextBlocked,
	"log_directory": ContextBlocked, "log_file_mode": ContextBlocked, "log_filename": ContextBlocked,
	"log_rotation_age": ContextBlocked, "log_rotation_size": ContextBlocked, "log_truncate_on_rotation": ContextBlocked,
	"pg_failover_slots.primary_dsn": ContextFixed, "pg_failover_slots.standby_slot_names": ContextFixed,
	"promote_trigger_file": ContextBlocked, "recovery_end_command": ContextBlocked, "recovery_min_apply_delay": ContextBlocked,
	"restart_after_crash": ContextBlocked, "ssl": ContextFixed, "ssl_ca_file": ContextFixed, "ssl_cert_file": ContextFixed,
	"ssl_crl_file": ContextFixed, "ssl_dh_params_file": ContextFixed, "ssl_key_file": ContextFixed,
	"ssl_passphrase_command": ContextFixed, "ssl_passphrase_command_supports_reload": ContextFixed,
	"ssl_prefer_server_ciphers": ContextFixed, "stats_temp_directory": ContextBlocked,
	"synchronous_standby_names": ContextFixed, "synchronized_standby_slots": ContextFixed,
	"syslog_facility": ContextBlocked, "syslog_ident": ContextBlocked, "syslog_sequence_numbers": ContextBlocked,
	"syslog_split_messages": ContextBlocked,
}

// parameters classifies the changed postgresql.parameters and reports
// whether a hot-standby-sensitive parameter decreased.
func (p *Preview) parameters(before, after *cluster, minor cnpgstatus.Minor) bool {
	b, a := params(before), params(after)
	names := map[string]bool{}
	for k := range b {
		names[k] = true
	}
	for k := range a {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		if b[k] != a[k] || hasKey(b, k) != hasKey(a, k) {
			sorted = append(sorted, k)
		}
	}
	sort.Strings(sorted)
	decrease := false
	for _, name := range sorted {
		c := ParameterChange{Name: name, Before: b[name], After: a[name]}
		if class := fixed(strings.ToLower(name), minor); class != "" {
			c.Effect, c.Context = EffectRefused, class
		} else {
			c.Effect, c.Context = guc(name, p.PostgresMajor)
		}
		if def, ok := hotStandbySensitive[strings.ToLower(name)]; ok && lower(valueOr(b, name, def), valueOr(a, name, def)) {
			c.Decrease, decrease = true, true
		}
		p.Parameters = append(p.Parameters, c)
	}
	p.managedExtensions(before, after, b, a)
	return decrease
}

// managedExtensions: CloudNativePG adds an extension's library to
// shared_preload_libraries as soon as one parameter of its namespace is
// set (pkg/postgres/configuration.go, ManagedExtensions,
// ManagedExtension.IsUsed and setManagedSharedPreloadLibraries,
// v1.30.0). Adding the first or removing the last such parameter
// changes shared_preload_libraries, a postmaster parameter: a restart.
// A library the user also lists in spec.postgresql.shared_preload_libraries
// before and after stays loaded, and nothing changes.
func (p *Preview) managedExtensions(before, after *cluster, b, a map[string]string) {
	userB, userA := userLibraries(before), userLibraries(after)
	for _, ext := range managedExtensions {
		if used(b, ext) == used(a, ext) || (userB[ext] && userA[ext]) {
			continue
		}
		p.Fields = append(p.Fields, FieldChange{
			Path:   "spec.postgresql.parameters." + ext + ".*",
			Effect: EffectRestart, Reason: ReasonManagedExtension,
		})
	}
}

// managedExtensions are the namespaces of CloudNativePG's
// ManagedExtensions, each loading the library of the same name.
var managedExtensions = []string{"pgaudit", "pg_stat_statements", "auto_explain", "pg_failover_slots"}

// used is ManagedExtension.IsUsed: a parameter name starting with the
// namespace and a dot (case-sensitive, as the operator compares).
func used(params map[string]string, namespace string) bool {
	for k := range params {
		if strings.HasPrefix(k, namespace+".") {
			return true
		}
	}
	return false
}

func userLibraries(c *cluster) map[string]bool {
	out := map[string]bool{}
	raw, ok := c.Spec["postgresql"]
	if !ok {
		return out
	}
	var pg struct {
		Libraries []string `json:"shared_preload_libraries"`
	}
	if json.Unmarshal(raw, &pg) != nil {
		return out
	}
	for _, l := range pg.Libraries {
		out[strings.TrimSpace(l)] = true
	}
	return out
}

// valueOr is the value set in the spec, or the default the instance runs
// with when it is not set.
func valueOr(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

// lower reports a numeric decrease; a value that does not parse is not
// claimed to be one. A removed parameter is compared through its default
// (valueOr), so a removal that lowers the value is a decrease too.
func lower(before, after string) bool {
	b, errB := strconv.ParseInt(strings.TrimSpace(before), 10, 64)
	a, errA := strconv.ParseInt(strings.TrimSpace(after), 10, 64)
	return errB == nil && errA == nil && a < b
}

func params(c *cluster) map[string]string {
	raw, ok := c.Spec["postgresql"]
	if !ok {
		return map[string]string{}
	}
	var pg postgresSpec
	if json.Unmarshal(raw, &pg) != nil || pg.Parameters == nil {
		return map[string]string{}
	}
	return pg.Parameters
}

// specRules classifies the top-level spec fields. A field not listed is
// unknown: a newer operator may roll Pods for it, and saying "nothing
// happens" would be a guess. The pod-template fields are the ones
// pkg/specs/pods.go and containers.go read into the instance Pod
// (v1.30.0): any change there makes isPodNeedingRollout
// report "PodSpec is outdated".
var specRules = map[string]FieldChange{
	"imageName":                 {Effect: EffectRollout, Reason: ReasonImage},
	"imageCatalogRef":           {Effect: EffectRollout, Reason: ReasonImage},
	"imagePullPolicy":           {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"resources":                 {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"affinity":                  {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"topologySpreadConstraints": {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"schedulerName":             {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"env":                       {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"envFrom":                   {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"priorityClassName":         {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"postgresUID":               {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"postgresGID":               {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"seccompProfile":            {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"podSecurityContext":        {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"securityContext":           {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"ephemeralVolumeSource":     {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"ephemeralVolumesSizeLimit": {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"projectedVolumeTemplate":   {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"probes":                    {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"livenessProbeTimeout":      {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"startDelay":                {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"stopDelay":                 {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"logLevel":                  {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"plugins":                   {Effect: EffectRollout, Reason: ReasonPodTemplate},
	"instances":                 {Effect: EffectNone, Reason: ReasonScale},
	"primaryUpdateStrategy":     {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"primaryUpdateMethod":       {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"switchoverDelay":           {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"failoverDelay":             {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"minSyncReplicas":           {Effect: EffectReload, Reason: ReasonSynchronous},
	"maxSyncReplicas":           {Effect: EffectReload, Reason: ReasonSynchronous},
	"backup":                    {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"monitoring":                {Effect: EffectNone, Reason: ReasonOperatorOnly},
	// New server or CA certificates: the instance manager refreshes the
	// secrets and reloads PostgreSQL (instance_controller.go,
	// certificateReconciler.RefreshSecrets → instance.Reload,
	// v1.30.0).
	"certificates":           {Effect: EffectReload, Reason: ReasonCertificates},
	"managed":                {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"superuserSecret":        {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"enableSuperuserAccess":  {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"bootstrap":              {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"externalClusters":       {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"inheritedMetadata":      {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"nodeMaintenanceWindow":  {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"replicationSlots":       {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"enablePDB":              {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"description":            {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"serviceAccountTemplate": {Effect: EffectNone, Reason: ReasonOperatorOnly},
	// A replica-cluster switch (promotion, demotion, a new source) runs
	// its own procedure; this package does not predict it.
	"replica": {Effect: EffectUnknown, Reason: ReasonReplicaRole},
	"storage": {Effect: EffectNone, Reason: ReasonVolumeResize},
}

// postgresqlRules classifies the spec.postgresql sub-fields other than
// parameters.
var postgresqlRules = map[string]FieldChange{
	"shared_preload_libraries":      {Effect: EffectRestart, Reason: ReasonSharedLibraries},
	"pg_hba":                        {Effect: EffectReload, Reason: ReasonHBA},
	"pg_ident":                      {Effect: EffectReload, Reason: ReasonHBA},
	"ldap":                          {Effect: EffectReload, Reason: ReasonHBA},
	"synchronous":                   {Effect: EffectReload, Reason: ReasonSynchronous},
	"syncReplicaElectionConstraint": {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"promotionTimeout":              {Effect: EffectNone, Reason: ReasonOperatorOnly},
	"enableAlterSystem":             {Effect: EffectReload, Reason: ReasonOperatorOnly},
	"extensions":                    {Effect: EffectRollout, Reason: ReasonPodTemplate},
}

// fields compares the spec outside the parameters.
func (p *Preview) fields(before, after *cluster) {
	keys := changedKeys(before.Spec, after.Spec)
	for _, key := range keys {
		if len(p.Fields) >= maxFields {
			return
		}
		switch key {
		case "postgresql":
			p.postgresql(before.Spec[key], after.Spec[key])
			continue
		case "walStorage", "tablespaces":
			p.volumes(key, before.Spec[key], after.Spec[key])
			continue
		case "imageName", "imageCatalogRef":
			p.image(key, before, after)
			continue
		}
		rule, ok := specRules[key]
		if !ok {
			rule = FieldChange{Effect: EffectUnknown, Reason: ReasonUnknownField}
		}
		rule.Path = "spec." + key
		p.Fields = append(p.Fields, rule)
	}
}

func (p *Preview) postgresql(before, after json.RawMessage) {
	var b, a map[string]json.RawMessage
	_ = json.Unmarshal(before, &b)
	_ = json.Unmarshal(after, &a)
	for _, key := range changedKeys(b, a) {
		if key == "parameters" {
			continue
		}
		rule, ok := postgresqlRules[key]
		if !ok {
			rule = FieldChange{Effect: EffectUnknown, Reason: ReasonUnknownField}
		}
		rule.Path = "spec.postgresql." + key
		p.Fields = append(p.Fields, rule)
	}
}

// volumes: a new WAL volume or tablespace attaches a PVC, which rolls the
// Pods ("pod has missing PVCs"); a size change of an existing one is a
// PVC expansion.
func (p *Preview) volumes(key string, before, after json.RawMessage) {
	added := false
	switch key {
	case "walStorage":
		added = len(before) == 0 || string(before) == "null"
	case "tablespaces":
		var b, a []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(before, &b)
		_ = json.Unmarshal(after, &a)
		known := map[string]bool{}
		for _, t := range b {
			known[t.Name] = true
		}
		for _, t := range a {
			if !known[t.Name] {
				added = true
			}
		}
	}
	change := FieldChange{Path: "spec." + key, Effect: EffectNone, Reason: ReasonVolumeResize}
	if added {
		change.Effect, change.Reason = EffectRollout, ReasonNewVolume
	}
	p.Fields = append(p.Fields, change)
}

// image is a rolling update, or a major upgrade when the major moves.
func (p *Preview) image(key string, before, after *cluster) {
	change := FieldChange{Path: "spec." + key, Effect: EffectRollout, Reason: ReasonImage}
	if key == "imageName" {
		b, a := tagMajor(specString(before.Spec, key, "")), tagMajor(specString(after.Spec, key, ""))
		if b > 0 && a > 0 && a != b {
			change.Effect, change.Reason = EffectMajorUpgrade, ReasonMajorVersion
		}
	} else {
		var b, a struct {
			Major int `json:"major"`
		}
		_ = json.Unmarshal(before.Spec[key], &b)
		_ = json.Unmarshal(after.Spec[key], &a)
		if b.Major > 0 && a.Major > 0 && a.Major != b.Major {
			change.Effect, change.Reason = EffectMajorUpgrade, ReasonMajorVersion
		}
	}
	p.Fields = append(p.Fields, change)
}

// The annotations that act on instances (pkg/utils/labels_annotations.go).
const (
	annotationRestart     = "kubectl.kubernetes.io/restartedAt"
	annotationHibernation = "cnpg.io/hibernation"
	annotationFencing     = "cnpg.io/fencedInstances"
)

func (p *Preview) annotations(before, after *cluster) {
	b, a := before.Metadata.Annotations, after.Metadata.Annotations
	if b[annotationRestart] != a[annotationRestart] {
		// checkClusterHasDifferentRestartAnnotation: a rollout that
		// cannot be done in place, so every Pod is recreated.
		p.Fields = append(p.Fields, FieldChange{Path: "metadata.annotations." + annotationRestart, Effect: EffectRollout, Reason: ReasonRestartAnnotation})
	}
	if b[annotationHibernation] != a[annotationHibernation] {
		// "on" deletes every instance Pod (pkg/reconciler/hibernation);
		// leaving "on" has the operator recreate them on their volumes.
		change := FieldChange{Path: "metadata.annotations." + annotationHibernation, Effect: EffectNone, Reason: ReasonHibernation}
		switch hb, ha := hibernated(before), hibernated(after); {
		case ha && !hb:
			change.Effect = EffectShutdown
		case hb && !ha:
			change.Effect = EffectStart
		}
		p.Fields = append(p.Fields, change)
	}
	if b[annotationFencing] != a[annotationFencing] {
		// The instance manager of a fenced instance stops PostgreSQL and
		// starts it again when the fence is lifted
		// (internal/management/controller/instance_controller.go,
		// reconcileFencing: RequestFencingOn / RequestAndWaitFencingOff,
		// v1.30.0).
		change := FieldChange{Path: "metadata.annotations." + annotationFencing, Effect: EffectNone, Reason: ReasonFencing}
		fb, okB := parseFence(b[annotationFencing])
		fa, okA := parseFence(a[annotationFencing])
		newly, lifted := fenceDelta(fb, fa, after.Status.InstanceNames)
		switch {
		case !okB || !okA:
			change.Effect = EffectUnknown
		case len(newly) > 0:
			change.Effect = EffectShutdown
		case len(lifted) > 0:
			change.Effect = EffectStart
		}
		p.Fields = append(p.Fields, change)
	}
}

// fence is the cnpg.io/fencedInstances annotation: a JSON list of
// instance names, "*" for every instance (pkg/utils/fencing.go,
// FenceAllInstances).
type fence struct {
	all   bool
	names map[string]bool
}

func (f fence) has(name string) bool { return f.all || f.names[name] }

// parseFence reads the annotation; false when it does not parse (the
// operator then reports an error rather than fencing).
func parseFence(v string) (fence, bool) {
	f := fence{names: map[string]bool{}}
	if v == "" {
		return f, true
	}
	var list []string
	if json.Unmarshal([]byte(v), &list) != nil {
		return f, false
	}
	for _, n := range list {
		if n == "*" {
			f.all = true
		}
		f.names[n] = true
	}
	return f, true
}

// fenceDelta lists the instances the change fences and those whose fence
// it lifts. Without instance names from the status, the annotation's own
// names (or "*") are what is known.
func fenceDelta(before, after fence, instances []string) (newly, lifted []string) {
	if len(instances) == 0 {
		instances = []string{}
		for _, f := range []fence{before, after} {
			for n := range f.names {
				if !slices.Contains(instances, n) {
					instances = append(instances, n)
				}
			}
		}
		sort.Strings(instances)
	}
	for _, n := range instances {
		switch {
		case after.has(n) && !before.has(n):
			newly = append(newly, n)
		case before.has(n) && !after.has(n):
			lifted = append(lifted, n)
		}
	}
	return newly, lifted
}

// changedKeys lists the keys whose raw JSON differs, sorted.
func changedKeys(b, a map[string]json.RawMessage) []string {
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		if !equalJSON(b[k], a[k]) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// equalJSON compares two values structurally (canonical JSON from the
// history store is already ordered; a proposed spec may not be).
func equalJSON(a, b json.RawMessage) bool {
	if string(a) == string(b) {
		return true
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

// plan derives the primary's handling and the order, following
// rolloutRequiredInstances and updatePrimaryPod
// (internal/controller/cluster_upgrade.go) and the instance manager's
// handling of a hot-standby-sensitive decrease
// (internal/management/controller/instance_controller.go,
// triggerRestartForDecrease), v1.30.0. effect is the
// operational effect: every classified change but fencing and
// hibernation, which come first as their own steps.
func (p *Preview) plan(before, after *cluster, decrease bool, effect Effect) {
	primary := after.Status.CurrentPrimary
	names := after.Status.InstanceNames
	byRole := len(names) == 0
	switch hb, ha := hibernated(before), hibernated(after); {
	case ha && !hb:
		p.Primary = PrimaryShutdown
		p.Steps = append(p.Steps, Step{Role: "all", Instances: orEmpty(names), Action: PrimaryShutdown})
		return
	case hb && !ha:
		// The Pods are recreated from the definition in force: every other
		// change applies as they start.
		p.Primary = PrimaryStart
		p.Steps = append(p.Steps, Step{Role: "all", Instances: orEmpty(names), Action: PrimaryStart})
		return
	case ha:
		// Still hibernated: nothing runs, the change applies at wake-up.
		p.Primary = PrimaryNone
		return
	}

	fb, _ := parseFence(before.Metadata.Annotations[annotationFencing])
	fa, _ := parseFence(after.Metadata.Annotations[annotationFencing])
	newly, lifted := fenceDelta(fb, fa, names)
	p.Primary = PrimaryNone
	for _, group := range []struct {
		names  []string
		action string
	}{{newly, PrimaryShutdown}, {lifted, PrimaryStart}} {
		if len(group.names) == 0 {
			continue
		}
		p.Steps = append(p.Steps, fenceSteps(group.names, names, primary, group.action)...)
		if slices.Contains(group.names, "*") || (primary != "" && slices.Contains(group.names, primary)) {
			p.Primary = group.action
		}
	}

	// The operator skips fenced replicas, and a fenced primary stops the
	// rollout at the primary (rolloutRequiredInstances: IsInstanceFenced).
	replicas := []string{}
	for _, name := range names {
		if name != primary && !fa.has(name) {
			replicas = append(replicas, name)
		}
	}
	sort.Strings(replicas)
	primaries := []string{}
	primaryFenced := false
	if primary != "" {
		primaryFenced = fa.has(primary)
		if !primaryFenced {
			primaries = []string{primary}
		}
	} else {
		primaryFenced = fa.all
	}
	active := append(append([]string{}, primaries...), replicas...)
	if fa.all {
		return
	}

	replicaStep := func(steps []Step) []Step {
		if len(replicas) > 0 || byRole {
			steps = append(steps, Step{Role: "replicas", Instances: replicas, Action: "recreate"})
		}
		return steps
	}
	primaryStep := func(steps []Step, action string) []Step {
		if !primaryFenced {
			steps = append(steps, Step{Role: "primary", Instances: primaries, Action: action})
		}
		return steps
	}
	action := PrimaryNone
	var steps []Step
	switch effect {
	case EffectReload:
		action = PrimaryReload
		steps = append(steps, Step{Role: "all", Instances: active, Action: "reload"})
	case EffectMajorUpgrade:
		action = PrimaryMajorUpgrade
		steps = append(steps, Step{Role: "all", Instances: active, Action: PrimaryMajorUpgrade})
	case EffectRestart, EffectRollout:
		// A replica needing a restart or a new Pod is deleted and recreated
		// on its volumes (upgradePod); the primary goes last, as
		// updatePrimaryPod decides.
		switch {
		case decrease && p.Strategy == "supervised":
			// The primary's instance manager waits for a person
			// (PhaseWaitingForUser). The replicas hide their pending restart
			// until pg_controldata carries the lower value, so with a restart
			// only they wait too; a Pod change still rolls them first.
			action = PrimaryWaitForUser
			if effect == EffectRestart {
				steps = replicaStep(primaryStep(steps, action))
			} else {
				steps = primaryStep(replicaStep(steps), action)
			}
		case decrease:
			// The primary's instance manager restarts it in place on its own,
			// before anything else; the replicas follow once pg_controldata
			// carries the value. A Pod change then still rolls the primary.
			action = PrimaryRestartFirst
			steps = replicaStep(primaryStep(steps, action))
			if effect == EffectRollout {
				steps = primaryStep(steps, p.primaryAction(effect, len(names)))
			}
		default:
			action = p.primaryAction(effect, len(names))
			steps = primaryStep(replicaStep(steps), action)
		}
	case EffectNone, EffectStart, EffectShutdown, EffectRefused, EffectUnknown:
		// No rollout step: fencing and hibernation (start, shutdown) are
		// planned above; none restarts nothing, and a refused or unknown
		// change has no predictable effect.
	}
	p.Steps = append(p.Steps, steps...)
	if primaryFenced || (p.Primary != PrimaryNone && action == PrimaryNone) {
		return
	}
	p.Primary = action
}

// fenceSteps groups fenced or unfenced instances by role: every
// instance is one step.
func fenceSteps(names, instances []string, primary, action string) []Step {
	if slices.Contains(names, "*") || primary == "" || (len(instances) > 0 && len(names) == len(instances)) {
		return []Step{{Role: "all", Instances: names, Action: action}}
	}
	var steps []Step
	others := []string{}
	for _, n := range names {
		if n != primary {
			others = append(others, n)
		}
	}
	if len(others) > 0 {
		steps = append(steps, Step{Role: "replicas", Instances: others, Action: action})
	}
	if slices.Contains(names, primary) {
		steps = append(steps, Step{Role: "primary", Instances: []string{primary}, Action: action})
	}
	return steps
}

func orEmpty(names []string) []string {
	if names == nil {
		return []string{}
	}
	return append([]string{}, names...)
}

// hibernated reports that the definition hibernates the Cluster: every
// instance is shut down, whatever else changed.
func hibernated(c *cluster) bool {
	return c.Metadata.Annotations[annotationHibernation] == "on"
}

// primaryAction is updatePrimaryPod: supervised waits for a person; the
// restart method restarts in place when only PostgreSQL must restart and
// recreates the Pod otherwise; the switchover method promotes the most
// advanced replica first, unless the Cluster has a single instance.
func (p *Preview) primaryAction(effect Effect, instances int) string {
	switch {
	case p.Strategy == "supervised":
		return PrimaryWaitForUser
	case p.Method == "switchover" && instances != 1:
		return PrimarySwitchover
	case effect == EffectRestart:
		return PrimaryRestartInPlace
	default:
		return PrimaryRecreate
	}
}
