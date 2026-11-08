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

package preview

import (
	"slices"
	"strings"
)

// The PostgreSQL majors the parameter table covers. A Cluster on another
// major is classified against the whole span (a parameter is "restart"
// only when it is postmaster in every covered major) and says so.
const (
	minMajor = 13
	maxMajor = 18
)

// span is an inclusive range of PostgreSQL majors.
type span struct{ from, to int }

func (s span) has(major int) bool { return major >= s.from && major <= s.to }

var all = span{minMajor, maxMajor}

// postmasterGUCs are the parameters of context postmaster: PostgreSQL
// reads them only at server start, so a change waits for a restart
// (pg_settings.pending_restart). Major 18 is read from
// `postgres --describe-config` of PostgreSQL 18.6; the older majors
// apply the context changes of the release notes backwards from it
// (restore_command became sighup in 14, autovacuum_max_workers in 18,
// old_snapshot_threshold was removed in 17; the SLRU *_buffers and
// max_notify_queue_pages arrived in 17, reserved_connections in 16,
// wal_decode_buffer_size in 15, huge_page_size and
// min_dynamic_shared_memory in 14). The parameters the operator fixes
// (shared_preload_libraries, port, ...) are listed too: the classifier
// reports them as refused before it looks here.
var postmasterGUCs = map[string]span{
	"archive_mode":                        all,
	"autovacuum_freeze_max_age":           all,
	"autovacuum_max_workers":              {13, 17},
	"autovacuum_multixact_freeze_max_age": all,
	"autovacuum_worker_slots":             {18, 18},
	"bonjour":                             all,
	"bonjour_name":                        all,
	"cluster_name":                        all,
	"commit_timestamp_buffers":            {17, 18},
	"config_file":                         all,
	"data_directory":                      all,
	"data_sync_retry":                     all,
	"debug_io_direct":                     {16, 18},
	"dynamic_shared_memory_type":          all,
	"event_source":                        all,
	"external_pid_file":                   all,
	"hba_file":                            all,
	"hot_standby":                         all,
	"huge_page_size":                      {14, 18},
	"huge_pages":                          all,
	"ident_file":                          all,
	"ignore_invalid_pages":                all,
	"io_max_combine_limit":                {18, 18},
	"io_max_concurrency":                  {18, 18},
	"io_method":                           {18, 18},
	"jit_provider":                        all,
	"listen_addresses":                    all,
	"logging_collector":                   all,
	"max_active_replication_origins":      {18, 18},
	"max_connections":                     all,
	"max_files_per_process":               all,
	"max_locks_per_transaction":           all,
	"max_logical_replication_workers":     all,
	"max_notify_queue_pages":              {17, 18},
	"max_pred_locks_per_transaction":      all,
	"max_prepared_transactions":           all,
	"max_replication_slots":               all,
	"max_wal_senders":                     all,
	"max_worker_processes":                all,
	"min_dynamic_shared_memory":           {14, 18},
	"multixact_member_buffers":            {17, 18},
	"multixact_offset_buffers":            {17, 18},
	"notify_buffers":                      {17, 18},
	"old_snapshot_threshold":              {13, 16},
	"port":                                all,
	"recovery_target":                     all,
	"recovery_target_action":              all,
	"recovery_target_inclusive":           all,
	"recovery_target_lsn":                 all,
	"recovery_target_name":                all,
	"recovery_target_time":                all,
	"recovery_target_timeline":            all,
	"recovery_target_xid":                 all,
	"reserved_connections":                {16, 18},
	"restore_command":                     {13, 13},
	"serializable_buffers":                {17, 18},
	"shared_buffers":                      all,
	"shared_memory_type":                  all,
	"shared_preload_libraries":            all,
	"subtransaction_buffers":              {17, 18},
	"superuser_reserved_connections":      all,
	"track_activity_query_size":           all,
	"track_commit_timestamp":              all,
	"transaction_buffers":                 {17, 18},
	"unix_socket_directories":             all,
	"unix_socket_group":                   all,
	"unix_socket_permissions":             all,
	"wal_buffers":                         all,
	"wal_decode_buffer_size":              {15, 18},
	"wal_level":                           all,
	"wal_log_hints":                       all,
	// pg_stat_statements sizes its shared hash table at start.
	"pg_stat_statements.max": all,
}

// reloadRemoved are reloadable parameters the 18 list does not name:
// those older majors had and 18 removed, and ssl_ecdh_curve, which 18
// keeps only as an alias of ssl_groups.
var reloadRemoved = map[string]span{
	"ssl_ecdh_curve":           all,
	"force_parallel_mode":      {13, 15},
	"promote_trigger_file":     {13, 15},
	"stats_temp_directory":     {13, 14},
	"vacuum_defer_cleanup_age": {13, 15},
	"db_user_namespace":        {13, 16},
	"trace_recovery_messages":  {13, 16},
}

// reloadExtensionPrefixes are extension parameter namespaces whose
// parameters (other than those listed in postmasterGUCs) apply on a
// reload: pg_stat_statements' tracking switches, auto_explain and
// pgaudit are superuser or sighup throughout.
var reloadExtensionPrefixes = []string{"pg_stat_statements.", "auto_explain.", "pgaudit."}

// hotStandbySensitive are the parameters a standby must hold at least as
// high as its primary, with the value an instance runs with when the spec
// does not set them: removing one from the spec returns it there, which
// may be a decrease. CloudNativePG restarts the primary first when one of
// them decreases and the replicas only once the new value is in
// pg_controldata (pkg/management/postgres/probes.go,
// GetDecreasedSensibleSettings, v1.30.0); an increase follows
// the usual order, replicas first. max_worker_processes defaults to 32 in
// CloudNativePG (pkg/postgres/configuration.go, CnpgConfigurationSettings
// GlobalDefaultSettings, v1.30.0); the others keep
// PostgreSQL's defaults, the same in 13 to 18.
var hotStandbySensitive = map[string]string{
	"max_connections":           "100",
	"max_locks_per_transaction": "64",
	"max_prepared_transactions": "0",
	"max_wal_senders":           "10",
	"max_worker_processes":      "32",
}

// reloadSince is the first major of the reloadable parameters of the 18
// list that older majors do not have (PostgreSQL release notes, checked
// against `postgres --describe-config` of 14.24, 17.5 and 18.6). On an
// older major the parameter is absent: PostgreSQL rejects a configuration
// file that sets it. A name not listed exists in every covered major.
var reloadSince = map[string]int{
	"client_connection_check_interval": 14, "compute_query_id": 14, "default_toast_compression": 14,
	"enable_async_append": 14, "enable_memoize": 14, "idle_session_timeout": 14, "log_recovery_conflict_waits": 14,
	"recovery_init_sync_method": 14, "ssl_crl_dir": 14, "track_wal_io_timing": 14, "vacuum_failsafe_age": 14,
	"vacuum_multixact_failsafe_age": 14,

	"archive_library": 15, "log_startup_progress_interval": 15, "recovery_prefetch": 15,
	"recursive_worktable_factor": 15, "stats_fetch_consistency": 15,

	"createrole_self_grant": 16, "enable_presorted_aggregate": 16, "gss_accept_delegation": 16,
	"icu_validation_level": 16, "max_parallel_apply_workers_per_subscription": 16, "scram_iterations": 16,
	"vacuum_buffer_usage_limit": 16,

	"allow_alter_system": 17, "enable_group_by_reordering": 17, "event_triggers": 17, "io_combine_limit": 17,
	"summarize_wal": 17, "sync_replication_slots": 17, "synchronized_standby_slots": 17, "transaction_timeout": 17,
	"wal_summary_keep_time": 17,

	"autovacuum_vacuum_max_threshold": 18, "enable_distinct_reordering": 18, "enable_self_join_elimination": 18,
	"extension_control_path": 18, "file_copy_method": 18, "file_extend_method": 18,
	"idle_replication_slot_timeout": 18, "io_workers": 18, "log_lock_failures": 18, "md5_password_warnings": 18,
	"oauth_validator_libraries": 18, "output_plugin_libraries": 18, "ssl_groups": 18, "ssl_tls13_ciphers": 18,
	"track_cost_delay_timing": 18, "vacuum_max_eager_freeze_failure_rate": 18, "vacuum_truncate": 18,
}

// reloadSpan is the span of majors that have a parameter of the 18 list.
func reloadSpan(name string) span {
	if since, ok := reloadSince[name]; ok {
		return span{since, maxMajor}
	}
	return all
}

// guc classifies one parameter for a PostgreSQL major (0: unknown).
// Names are lowercased: PostgreSQL matches parameter names
// case-insensitively, and every table is lowercase.
func guc(name string, major int) (Effect, string) {
	name = strings.ToLower(name)
	_, inReload := slices.BinarySearch(reloadGUCs18, name)
	post, inPost := postmasterGUCs[name]
	removed, inRemoved := reloadRemoved[name]
	if major >= minMajor && major <= maxMajor {
		switch {
		case inPost && post.has(major):
			return EffectRestart, ContextPostmaster
		case inRemoved && removed.has(major):
			return EffectReload, ContextReload
		case inReload && reloadSpan(name).has(major):
			return EffectReload, ContextReload
		case inPost || inRemoved || inReload:
			// Known from another major, not this one: PostgreSQL refuses
			// the configuration, so nothing can be predicted.
			return EffectUnknown, ContextAbsent
		}
		return extension(name)
	}
	// Unknown or uncovered major: restart only if postmaster everywhere,
	// reload only if reloadable everywhere, otherwise it depends.
	switch {
	case inPost:
		if post == all {
			return EffectRestart, ContextPostmaster
		}
		return EffectUnknown, ""
	case inReload:
		if reloadSpan(name) == all {
			return EffectReload, ContextReload
		}
		return EffectUnknown, ""
	case inRemoved:
		if removed == all {
			return EffectReload, ContextReload
		}
		return EffectUnknown, ""
	}
	return extension(name)
}

func extension(name string) (Effect, string) {
	for _, prefix := range reloadExtensionPrefixes {
		if strings.HasPrefix(name, prefix) {
			return EffectReload, ContextReload
		}
	}
	return EffectUnknown, ""
}
