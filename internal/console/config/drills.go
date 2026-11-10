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

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Restore drills: a person starts one from the
// Backups screen; the drill Cluster is created with that person's own
// token, checked without SQL and deleted by the console's narrow,
// opt-in teardown Role. These variables are the drill's whole
// configuration; scheduled drills are a later step.
const (
	// EnvRestoreDrillEnabled offers the drill (default false). Off, the
	// offer is absent and nothing is ever deleted.
	EnvRestoreDrillEnabled = "RESTORE_DRILL_ENABLED"
	// EnvRestoreDrillNamespace is where drill Clusters are created;
	// unset, the source Cluster's own namespace. It must be a watched
	// namespace: the console checks the drill through its read model.
	EnvRestoreDrillNamespace = "RESTORE_DRILL_NAMESPACE"
	// EnvRestoreDrillPrefix starts every drill Cluster's name (default
	// "drill-").
	EnvRestoreDrillPrefix = "RESTORE_DRILL_PREFIX"
	// EnvRestoreDrillTimeLimit is how long a drill may run before the
	// console records it as timed out and deletes it (default 2h).
	EnvRestoreDrillTimeLimit = "RESTORE_DRILL_TIME_LIMIT"
	// EnvRestoreDrillCPURequest, EnvRestoreDrillMemoryRequest,
	// EnvRestoreDrillCPULimit and EnvRestoreDrillMemoryLimit are the drill
	// instance's resources; unset, the source's.
	EnvRestoreDrillCPURequest    = "RESTORE_DRILL_CPU_REQUEST"
	EnvRestoreDrillMemoryRequest = "RESTORE_DRILL_MEMORY_REQUEST"
	EnvRestoreDrillCPULimit      = "RESTORE_DRILL_CPU_LIMIT"
	EnvRestoreDrillMemoryLimit   = "RESTORE_DRILL_MEMORY_LIMIT"
	// EnvRestoreDrillStorageSize is the drill's data volume size; unset,
	// the source's.
	EnvRestoreDrillStorageSize = "RESTORE_DRILL_STORAGE_SIZE"
	// EnvRestoreDrillStorageClass is the drill's storage class (data,
	// WAL and tablespaces); unset, the source's.
	EnvRestoreDrillStorageClass = "RESTORE_DRILL_STORAGE_CLASS"
	// EnvRestoreDrillMaxConcurrent caps the drills of this console
	// running at once (default 3, 1 to 20): each is a full copy of a
	// database, in storage and compute.
	EnvRestoreDrillMaxConcurrent = "RESTORE_DRILL_MAX_CONCURRENT"
	// EnvRestoreDrillServerCA is where a drill's server certificate is
	// signed, which decides whether the console can verify the drill's
	// /pg/status: "own" (the default) lets CloudNativePG create the
	// drill's own <drill>-ca, which only a bundle kept by the operator
	// (a PgToolBoxConsole's instance CA mirror) can follow; "source" signs it
	// with the source Cluster's server CA, already in an admin-filled
	// bundle; "secret" signs it with the CA Secret
	// RESTORE_DRILL_SERVER_CA_SECRET names, whose ca.crt the admin puts
	// in the bundle. The console never reads any of these Secrets:
	// CloudNativePG does, for the drill.
	EnvRestoreDrillServerCA = "RESTORE_DRILL_SERVER_CA"
	// EnvRestoreDrillServerCASecret is the CA Secret of
	// RESTORE_DRILL_SERVER_CA=secret, in every drill namespace.
	EnvRestoreDrillServerCASecret = "RESTORE_DRILL_SERVER_CA_SECRET" // #nosec G101 -- the name of a variable holding a Secret name, not a credential
	// EnvRestoreDrillSealKeyFile is a mounted file of the drill seal's
	// own keys (one per line, the first seals, all verify), from a
	// Secret of its own. Set, anyone who can read the session Secret
	// cannot forge a seal, so cannot make the console delete a Cluster
	// of their choosing; unset, the keys derive from the session secret
	// (HKDF, a distinct label), as before.
	EnvRestoreDrillSealKeyFile = "RESTORE_DRILL_SEAL_KEY_FILE" // #nosec G101 -- the name of a variable holding a file path, not a credential
)

// The values of RESTORE_DRILL_SERVER_CA.
const (
	DrillServerCAOwn    = "own"
	DrillServerCASource = "source"
	DrillServerCASecret = "secret"
)

// MaxRestoreDrillPrefix bounds the prefix so that a useful part of the
// source's name fits in CloudNativePG's 50-character Cluster name.
const MaxRestoreDrillPrefix = 20

var drillPrefixRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*)?$`)

// RestoreDrill is the validated drill configuration.
type RestoreDrill struct {
	Enabled bool
	// Namespace is the drill namespace; empty means the source's own.
	Namespace string
	Prefix    string
	TimeLimit time.Duration
	// MaxConcurrent caps the running drills of this console.
	MaxConcurrent int
	// Resources are Kubernetes quantities, empty when unset.
	CPURequest, MemoryRequest, CPULimit, MemoryLimit string
	StorageSize, StorageClass                        string
	// ServerCA is RESTORE_DRILL_SERVER_CA (DrillServerCA*), and
	// ServerCASecret the Secret of the "secret" mode.
	ServerCA, ServerCASecret string
	// SealKeyFile is RESTORE_DRILL_SEAL_KEY_FILE, empty when the seal's
	// keys derive from the session secret.
	SealKeyFile string
}

// NamespaceFor is the namespace a drill of a Cluster in source runs in.
func (d RestoreDrill) NamespaceFor(source string) string {
	if d.Namespace != "" {
		return d.Namespace
	}
	return source
}

func restoreDrill(lookup Lookup, watched []string, fail func(name, constraint string)) RestoreDrill {
	d := RestoreDrill{
		Enabled:   boolVar(lookup, EnvRestoreDrillEnabled, false, fail),
		Prefix:    "drill-",
		TimeLimit: durationVar(lookup, EnvRestoreDrillTimeLimit, 2*time.Hour, 10*time.Minute, 24*time.Hour, fail),

		MaxConcurrent: intVar(lookup, EnvRestoreDrillMaxConcurrent, 3, 1, 20, fail),
	}
	if raw, ok := lookup(EnvRestoreDrillNamespace); ok && raw != "" {
		switch {
		case len(raw) > 63 || !dns1123Label.MatchString(raw):
			fail(EnvRestoreDrillNamespace, "must be a DNS-1123 label")
		case len(watched) > 0 && !slices.Contains(watched, raw):
			// The console checks a drill through its own read model
			// (health, /pg/status, exporter) and its teardown Role is
			// bound there: a namespace it does not watch could be
			// created into but never checked or cleaned up.
			fail(EnvRestoreDrillNamespace, "must be one of "+EnvWatchNamespaces)
		default:
			d.Namespace = raw
		}
	}
	if raw, ok := lookup(EnvRestoreDrillPrefix); ok {
		if len(raw) > MaxRestoreDrillPrefix || !drillPrefixRE.MatchString(raw) {
			fail(EnvRestoreDrillPrefix, "must start with a lowercase letter, use only lowercase letters, digits and '-', and be at most "+
				strconv.Itoa(MaxRestoreDrillPrefix)+" characters")
		} else {
			d.Prefix = raw
		}
	}
	quantity := func(name string) string {
		raw, ok := lookup(name)
		if !ok || raw == "" {
			return ""
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil || q.Sign() <= 0 {
			fail(name, "must be a positive Kubernetes quantity")
			return ""
		}
		return q.String()
	}
	d.CPURequest = quantity(EnvRestoreDrillCPURequest)
	d.MemoryRequest = quantity(EnvRestoreDrillMemoryRequest)
	d.CPULimit = quantity(EnvRestoreDrillCPULimit)
	d.MemoryLimit = quantity(EnvRestoreDrillMemoryLimit)
	d.StorageSize = quantity(EnvRestoreDrillStorageSize)
	lessOrEqual := func(req, lim, limName string) {
		if req == "" || lim == "" {
			return
		}
		r, l := resource.MustParse(req), resource.MustParse(lim)
		if r.Cmp(l) > 0 {
			fail(limName, "must not be below the matching request")
		}
	}
	lessOrEqual(d.CPURequest, d.CPULimit, EnvRestoreDrillCPULimit)
	lessOrEqual(d.MemoryRequest, d.MemoryLimit, EnvRestoreDrillMemoryLimit)
	if raw, ok := lookup(EnvRestoreDrillStorageClass); ok && raw != "" {
		if len(raw) > 253 || !dnsSubdomainRE.MatchString(raw) {
			fail(EnvRestoreDrillStorageClass, "must be a DNS-1123 subdomain")
		} else {
			d.StorageClass = raw
		}
	}
	d.ServerCA, d.ServerCASecret = drillServerCA(lookup, d.Namespace, fail)
	if raw, ok := lookup(EnvRestoreDrillSealKeyFile); ok && raw != "" {
		if !filepath.IsAbs(raw) {
			fail(EnvRestoreDrillSealKeyFile, "must be an absolute path")
		} else {
			d.SealKeyFile = filepath.Clean(raw)
		}
	}
	return d
}

// drillServerCA reads RESTORE_DRILL_SERVER_CA and its Secret. "source"
// needs drills in the source's namespace: CloudNativePG reads a Cluster's
// CA Secret in the Cluster's own namespace, and the source's is there.
func drillServerCA(lookup Lookup, namespace string, fail func(name, constraint string)) (mode, secret string) {
	mode = DrillServerCAOwn
	if raw, ok := lookup(EnvRestoreDrillServerCA); ok && raw != "" {
		switch raw {
		case DrillServerCAOwn, DrillServerCASource, DrillServerCASecret:
			mode = raw
		default:
			fail(EnvRestoreDrillServerCA, "must be "+DrillServerCAOwn+", "+DrillServerCASource+" or "+DrillServerCASecret)
		}
	}
	raw, _ := lookup(EnvRestoreDrillServerCASecret)
	switch {
	case mode == DrillServerCASource && namespace != "":
		fail(EnvRestoreDrillServerCA, DrillServerCASource+" needs drills in the source's namespace ("+EnvRestoreDrillNamespace+
			" unset): CloudNativePG reads the source's CA Secret in the drill's namespace; use "+DrillServerCASecret)
	case mode == DrillServerCASecret && raw == "":
		fail(EnvRestoreDrillServerCASecret, "is required with "+EnvRestoreDrillServerCA+"="+DrillServerCASecret)
	case mode != DrillServerCASecret && raw != "":
		fail(EnvRestoreDrillServerCASecret, "is only read with "+EnvRestoreDrillServerCA+"="+DrillServerCASecret)
	case raw != "" && (len(raw) > 253 || !dnsSubdomainRE.MatchString(raw)):
		fail(EnvRestoreDrillServerCASecret, "must be a DNS-1123 subdomain")
	default:
		secret = raw
	}
	return mode, secret
}

var dnsSubdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func (d RestoreDrill) info(into map[string]string) {
	into[EnvRestoreDrillEnabled] = strconv.FormatBool(d.Enabled)
	if !d.Enabled {
		return
	}
	into[EnvRestoreDrillNamespace] = d.Namespace
	into[EnvRestoreDrillPrefix] = d.Prefix
	into[EnvRestoreDrillTimeLimit] = d.TimeLimit.String()
	into[EnvRestoreDrillMaxConcurrent] = strconv.Itoa(d.MaxConcurrent)
	into[EnvRestoreDrillStorageClass] = d.StorageClass
	into[EnvRestoreDrillServerCA] = d.ServerCA
	into[EnvRestoreDrillServerCASecret] = d.ServerCASecret
	into[EnvRestoreDrillSealKeyFile] = d.SealKeyFile
}
