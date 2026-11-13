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

// Package configuration is the operator's configuration: one
// struct, defaults in code, overlaid by the environment, then the operator
// ConfigMap, then the operator Secret (the Secret wins), loaded before the
// manager is built so it can shape the cache, the watches and the probes,
// validated once with a single structured error, and exported as
// pgtoolbox_config_info with Secret-sourced values redacted. There is no hot
// reload: edit the ConfigMap, then roll the operator.
package configuration

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultPgAdminImage is the pgAdmin image a PgAdmin without spec.image
// runs: the hardened image, pinned by digest. Overridable at
// build time (-ldflags -X) and by PGADMIN_IMAGE. The digest is the
// multi-arch index of ghcr.io/fyannk/pgadmin:9.18-hardened, whose
// entrypoint runs on a read-only root filesystem.
var DefaultPgAdminImage = "ghcr.io/fyannk/pgadmin:9.18-hardened@sha256:96576df8aba0d0057248644d6d5ad398869e42bcf3fcfba2c0f7c13d151ea17c"

// DefaultOAuthProxyImage is the OpenShift oauth-proxy image, pinned by
// digest (the 4.18 manifest list of 2026-11-02). OpenShift clusters normally
// override it with the release payload's own image (OAUTH_PROXY_IMAGE),
// which is the one their registry mirrors.
var DefaultOAuthProxyImage = "quay.io/openshift/origin-oauth-proxy:4.18@sha256:63b0344b903162519efdb63052c1dde81aa17cfc46ba63cc87300db47f0fb5df"

// DefaultOperatorImage is the operator's own image, copied into pgAdmin
// Pods by an init container; set by -ldflags at build time.
var DefaultOperatorImage = ""

// Config is the operator configuration.
type Config struct {
	// WatchNamespaces restricts the manager's caches (DefaultNamespaces);
	// empty watches every namespace the operator's RBAC allows.
	WatchNamespaces []string
	// OperatorNamespace is where the operator runs (downward API).
	OperatorNamespace string
	// OperatorImage is copied into pgAdmin Pods for the passfile-sync
	// sidecar and into registration Jobs.
	OperatorImage string
	// PgAdminImage is the default pgAdmin image.
	PgAdminImage string
	// OAuthProxyImage is the default OpenShift oauth-proxy image.
	OAuthProxyImage string
	// PgAdminImageRepositories are the repositories a PgAdmin's
	// spec.image and oauth-proxy image may name (exact, or a prefix ending
	// in "/"). Empty: each image's default repository only. Whoever may
	// create a PgAdmin chooses which image holds the offered credentials,
	// so the choice is bounded here, by the operator's installer.
	PgAdminImageRepositories []string
	// ExposureAllowedHostSuffixes are the DNS suffixes a PgAdmin's or a
	// PgToolBoxConsole's spec.exposure.hostname must fall under (the suffix
	// itself or a name below it). Empty: any hostname, as before. Whoever
	// may create those kinds otherwise chooses which name the cluster's
	// router or gateway publishes, a name another team or a login page
	// may own.
	ExposureAllowedHostSuffixes []string
	// MaxConcurrentReconciles per controller.
	MaxConcurrentReconciles int
	// ReconciliationTimeout bounds one Reconcile call.
	ReconciliationTimeout time.Duration
	// Leader election timings.
	LeaseDuration, RenewDeadline, RetryPeriod time.Duration
	// UpgradeBackupsKept is how many pre-upgrade settings backups stay on
	// the settings volume.
	UpgradeBackupsKept int
	// Admission* are the allow-lists of the admission webhook
	// (--admission-webhook): who may set spec.image and
	// spec.workloadIdentity, as the chart's operator.admissionPolicy.
	AdmissionImageUsers, AdmissionImageGroups       []string
	AdmissionIdentityUsers, AdmissionIdentityGroups []string
}

// Defaults returns the configuration before any overlay.
func Defaults() Config {
	return Config{
		OperatorImage:           DefaultOperatorImage,
		PgAdminImage:            DefaultPgAdminImage,
		OAuthProxyImage:         DefaultOAuthProxyImage,
		MaxConcurrentReconciles: 4,
		ReconciliationTimeout:   2 * time.Minute,
		LeaseDuration:           30 * time.Second,
		RenewDeadline:           20 * time.Second,
		RetryPeriod:             5 * time.Second,
		UpgradeBackupsKept:      3,
		AdmissionImageGroups:    []string{"system:masters"},
		AdmissionIdentityGroups: []string{"system:masters"},
	}
}

// field binds one configuration key to the struct.
type field struct {
	key string
	set func(*Config, string) error
	get func(*Config) string
}

var fields = []field{
	{
		"WATCH_NAMESPACES", func(c *Config, v string) error { c.WatchNamespaces = splitList(v); return nil },
		func(c *Config) string { return strings.Join(c.WatchNamespaces, ",") },
	},
	{
		"POD_NAMESPACE", func(c *Config, v string) error { c.OperatorNamespace = strings.TrimSpace(v); return nil },
		func(c *Config) string { return c.OperatorNamespace },
	},
	{
		"OPERATOR_IMAGE", func(c *Config, v string) error { c.OperatorImage = strings.TrimSpace(v); return nil },
		func(c *Config) string { return c.OperatorImage },
	},
	{
		"PGADMIN_IMAGE", func(c *Config, v string) error { c.PgAdminImage = strings.TrimSpace(v); return nil },
		func(c *Config) string { return c.PgAdminImage },
	},
	{
		"OAUTH_PROXY_IMAGE", func(c *Config, v string) error { c.OAuthProxyImage = strings.TrimSpace(v); return nil },
		func(c *Config) string { return c.OAuthProxyImage },
	},
	{
		"PGADMIN_IMAGE_REPOSITORIES", func(c *Config, v string) error { c.PgAdminImageRepositories = splitList(v); return nil },
		func(c *Config) string { return strings.Join(c.PgAdminImageRepositories, ",") },
	},
	{
		"EXPOSURE_ALLOWED_HOST_SUFFIXES", func(c *Config, v string) error {
			c.ExposureAllowedHostSuffixes = splitList(strings.ToLower(v))
			return nil
		},
		func(c *Config) string { return strings.Join(c.ExposureAllowedHostSuffixes, ",") },
	},
	{
		"MAX_CONCURRENT_RECONCILES", intSetter(func(c *Config) *int { return &c.MaxConcurrentReconciles }),
		func(c *Config) string { return strconv.Itoa(c.MaxConcurrentReconciles) },
	},
	{
		"RECONCILIATION_TIMEOUT", durationSetter(func(c *Config) *time.Duration { return &c.ReconciliationTimeout }),
		func(c *Config) string { return c.ReconciliationTimeout.String() },
	},
	{
		"LEADER_ELECTION_LEASE_DURATION", durationSetter(func(c *Config) *time.Duration { return &c.LeaseDuration }),
		func(c *Config) string { return c.LeaseDuration.String() },
	},
	{
		"LEADER_ELECTION_RENEW_DEADLINE", durationSetter(func(c *Config) *time.Duration { return &c.RenewDeadline }),
		func(c *Config) string { return c.RenewDeadline.String() },
	},
	{
		"LEADER_ELECTION_RETRY_PERIOD", durationSetter(func(c *Config) *time.Duration { return &c.RetryPeriod }),
		func(c *Config) string { return c.RetryPeriod.String() },
	},
	{
		"UPGRADE_BACKUPS_KEPT", intSetter(func(c *Config) *int { return &c.UpgradeBackupsKept }),
		func(c *Config) string { return strconv.Itoa(c.UpgradeBackupsKept) },
	},
	listField("ADMISSION_IMAGE_OVERRIDE_USERS", func(c *Config) *[]string { return &c.AdmissionImageUsers }),
	listField("ADMISSION_IMAGE_OVERRIDE_GROUPS", func(c *Config) *[]string { return &c.AdmissionImageGroups }),
	listField("ADMISSION_WORKLOAD_IDENTITY_USERS", func(c *Config) *[]string { return &c.AdmissionIdentityUsers }),
	listField("ADMISSION_WORKLOAD_IDENTITY_GROUPS", func(c *Config) *[]string { return &c.AdmissionIdentityGroups }),
}

// listField binds a comma list.
func listField(key string, list func(*Config) *[]string) field {
	return field{
		key, func(c *Config, v string) error { *list(c) = splitList(v); return nil },
		func(c *Config) string { return strings.Join(*list(c), ",") },
	}
}

// Source names where a key's effective value came from.
type Source string

// Sources of a value, in overlay order.
const (
	SourceDefault   Source = "default"
	SourceEnv       Source = "env"
	SourceConfigMap Source = "configmap"
	SourceSecret    Source = "secret"
	// SourceFlag is a command-line flag the binary applied after Load
	// (Loaded.OverrideFlag).
	SourceFlag Source = "flag"
)

// Loaded is a loaded configuration and where each value came from.
type Loaded struct {
	Config  Config
	Sources map[string]Source
}

// Objects names the operator ConfigMap and Secret; empty names skip that
// overlay. Both live in the operator's namespace.
type Objects struct {
	Namespace, ConfigMap, Secret string
}

// Load builds the configuration: defaults, then lookupEnv, then the
// ConfigMap's data, then the Secret's data. A missing ConfigMap or Secret is
// not an error (both are optional); any other read error is. reader must
// read past any cache (the manager does not exist yet). The result is
// validated.
func Load(ctx context.Context, reader client.Reader, lookupEnv func(string) (string, bool), objects Objects) (Loaded, error) {
	loaded := Loaded{Config: Defaults(), Sources: map[string]Source{}}
	for _, f := range fields {
		loaded.Sources[f.key] = SourceDefault
	}
	var problems []error
	overlay := func(source Source, values func(string) (string, bool)) {
		for _, f := range fields {
			value, ok := values(f.key)
			if !ok {
				continue
			}
			if err := f.set(&loaded.Config, value); err != nil {
				problems = append(problems, fmt.Errorf("%s (from %s): %w", f.key, source, err))
				continue
			}
			loaded.Sources[f.key] = source
		}
	}
	overlay(SourceEnv, lookupEnv)

	if objects.Namespace == "" {
		objects.Namespace = loaded.Config.OperatorNamespace
	}
	if objects.ConfigMap != "" && objects.Namespace != "" {
		var configMap corev1.ConfigMap
		err := reader.Get(ctx, client.ObjectKey{Namespace: objects.Namespace, Name: objects.ConfigMap}, &configMap)
		switch {
		case err == nil:
			overlay(SourceConfigMap, func(key string) (string, bool) { v, ok := configMap.Data[key]; return v, ok })
		case !apierrors.IsNotFound(err):
			return loaded, fmt.Errorf("read operator ConfigMap %s/%s: %w", objects.Namespace, objects.ConfigMap, err)
		}
	}
	if objects.Secret != "" && objects.Namespace != "" {
		var secret corev1.Secret
		err := reader.Get(ctx, client.ObjectKey{Namespace: objects.Namespace, Name: objects.Secret}, &secret)
		switch {
		case err == nil:
			overlay(SourceSecret, func(key string) (string, bool) { v, ok := secret.Data[key]; return string(v), ok })
		case !apierrors.IsNotFound(err):
			return loaded, fmt.Errorf("read operator Secret %s/%s: %w", objects.Namespace, objects.Secret, err)
		}
	}
	problems = append(problems, loaded.Config.validate()...)
	if len(problems) > 0 {
		return loaded, &InvalidError{Problems: problems}
	}
	return loaded, nil
}

// InvalidError is the single structured start-up error.
type InvalidError struct{ Problems []error }

func (e *InvalidError) Error() string {
	messages := make([]string, 0, len(e.Problems))
	for _, problem := range e.Problems {
		messages = append(messages, problem.Error())
	}
	return "invalid operator configuration: " + strings.Join(messages, "; ")
}

// Unwrap exposes the individual problems to errors.Is/As.
func (e *InvalidError) Unwrap() []error { return e.Problems }

func (c *Config) validate() []error {
	var problems []error
	for _, namespace := range c.WatchNamespaces {
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			problems = append(problems, fmt.Errorf("WATCH_NAMESPACES: %q: %s", namespace, strings.Join(errs, ", ")))
		}
	}
	for _, suffix := range c.ExposureAllowedHostSuffixes {
		name := strings.TrimSuffix(strings.TrimPrefix(suffix, "."), ".")
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			problems = append(problems, fmt.Errorf("EXPOSURE_ALLOWED_HOST_SUFFIXES: %q: %s", suffix, strings.Join(errs, ", ")))
		}
	}
	if c.OperatorImage == "" {
		problems = append(problems, errors.New("OPERATOR_IMAGE: empty; the pgAdmin Pod's passfile-sync sidecar is copied from it"))
	}
	if c.PgAdminImage == "" {
		problems = append(problems, errors.New("PGADMIN_IMAGE: empty"))
	}
	if c.MaxConcurrentReconciles < 1 || c.MaxConcurrentReconciles > 64 {
		problems = append(problems, fmt.Errorf("MAX_CONCURRENT_RECONCILES: %d is outside 1..64", c.MaxConcurrentReconciles))
	}
	if c.ReconciliationTimeout < 10*time.Second {
		problems = append(problems, fmt.Errorf("RECONCILIATION_TIMEOUT: %s is under 10s", c.ReconciliationTimeout))
	}
	if !(c.RetryPeriod > 0 && c.RetryPeriod < c.RenewDeadline && c.RenewDeadline < c.LeaseDuration) {
		problems = append(problems, fmt.Errorf("leader election: need 0 < retry (%s) < renew (%s) < lease (%s)",
			c.RetryPeriod, c.RenewDeadline, c.LeaseDuration))
	}
	if c.UpgradeBackupsKept < 1 || c.UpgradeBackupsKept > 20 {
		problems = append(problems, fmt.Errorf("UPGRADE_BACKUPS_KEPT: %d is outside 1..20", c.UpgradeBackupsKept))
	}
	return problems
}

// Redacted is the placeholder of a Secret-sourced value in exported
// configuration.
const Redacted = "<redacted>"

// Effective returns every key with its effective value, Secret-sourced
// values redacted, in key order.
func (l Loaded) Effective() [][2]string {
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		value := f.get(&l.Config)
		if l.Sources[f.key] == SourceSecret {
			value = Redacted
		}
		out = append(out, [2]string{f.key, value})
	}
	return out
}

// RegisterInfoMetric registers pgtoolbox_config_info{name,value,source} = 1
// for every key on registry.
func (l Loaded) RegisterInfoMetric(registry prometheus.Registerer) error {
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "pgtoolbox_config_info",
		Help: "The operator's effective configuration; Secret-sourced values are redacted.",
	}, []string{"name", "value", "source"})
	for _, kv := range l.Effective() {
		info.WithLabelValues(kv[0], kv[1], string(l.Sources[kv[0]])).Set(1)
	}
	return registry.Register(info)
}

func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" && !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	slices.Sort(out)
	return out
}

func intSetter(target func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("not an integer: %q", v)
		}
		*target(c) = n
		return nil
	}
}

func durationSetter(target func(*Config) *time.Duration) func(*Config, string) error {
	return func(c *Config, v string) error {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("not a duration: %q", v)
		}
		*target(c) = d
		return nil
	}
}

// OverrideFlag applies a command-line flag on top of the loaded
// configuration: the flag wins over every other source, the value is
// validated like any other, and its source reads "flag" in
// pgtoolbox_config_info. An empty value leaves the key as loaded.
func (l *Loaded) OverrideFlag(key, value string) error {
	if value == "" {
		return nil
	}
	for _, f := range fields {
		if f.key != key {
			continue
		}
		if err := f.set(&l.Config, value); err != nil {
			return fmt.Errorf("%s (from %s): %w", key, SourceFlag, err)
		}
		if problems := l.Config.validate(); len(problems) > 0 {
			return &InvalidError{Problems: problems}
		}
		l.Sources[key] = SourceFlag
		return nil
	}
	return fmt.Errorf("no configuration key %s", key)
}
