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

package viewmodel

import "time"

// Admin is the /admin screen (the namespace-admin persona):
// the PgAdmin and EvidenceAgent objects of the watched namespaces the
// caller may list, and the console's own health for those who may list
// pods in the console's namespace.
type Admin struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Namespaces  []AdminNamespace `json:"namespaces"`
	// Console is absent when the caller may not see it.
	Console *ConsoleHealth `json:"console,omitempty"`
}

// AdminNamespace is one watched namespace.
type AdminNamespace struct {
	Name string `json:"name"`
	// PgAdminsVisible and EvidenceAgentsVisible say whether the caller
	// may list the kind there; a list the caller may not see is empty
	// and false, never "none".
	PgAdminsVisible       bool               `json:"pgAdminsVisible"`
	PgAdmins              []PgAdminRow       `json:"pgAdmins"`
	EvidenceAgentsVisible bool               `json:"evidenceAgentsVisible"`
	EvidenceAgents        []EvidenceAgentRow `json:"evidenceAgents"`
	Sources               []SourceState      `json:"sources"`
}

// PgAdminRow is one PgAdmin object.
type PgAdminRow struct {
	Name       string          `json:"name"`
	Health     Health          `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info" doc:"From the Ready condition; unknown when absent or stale."`
	URL        string          `json:"url,omitempty"`
	Version    string          `json:"version,omitempty"`
	Image      string          `json:"image,omitempty"`
	Servers    []PgAdminServer `json:"servers"`
	Conditions []Condition     `json:"conditions"`
	// Stale reports status.observedGeneration behind metadata.generation.
	Stale      bool       `json:"stale,omitempty"`
	Provenance Provenance `json:"provenance"`
}

// PgAdminServer is one server registered in a pgAdmin.
type PgAdminServer struct {
	Cluster  string `json:"cluster"`
	Endpoint string `json:"endpoint"`
	State    string `json:"state"`
	Message  string `json:"message,omitempty"`
}

// EvidenceAgentRow is one EvidenceAgent object.
type EvidenceAgentRow struct {
	Name          string      `json:"name"`
	Health        Health      `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	Endpoint      string      `json:"endpoint,omitempty"`
	Fingerprint   string      `json:"fingerprint,omitempty" doc:"The repository destination fingerprint (not a secret)."`
	ProducerEpoch *int64      `json:"producerEpoch,omitempty"`
	Conditions    []Condition `json:"conditions"`
	Stale         bool        `json:"stale,omitempty"`
	Provenance    Provenance  `json:"provenance"`
}

// ConsoleHealth is the console's account of itself.
type ConsoleHealth struct {
	Version         string        `json:"version,omitempty"`
	Namespace       string        `json:"namespace,omitempty"`
	WatchNamespaces []string      `json:"watchNamespaces"`
	Runners         []RunnerState `json:"runners"`
	// Sources are the console-wide sources: the instance sweep, the
	// diagnostics runner, the history, the evidence reader.
	Sources []SourceState `json:"sources"`
	// Digest is the daily digest's schedule and delivery outcomes; absent
	// when history (and so the digest) is disabled.
	Digest     *DigestStatus `json:"digest,omitempty"`
	Provenance Provenance    `json:"provenance"`
}

// RunnerState is one supervised background runner.
type RunnerState struct {
	Name      string `json:"name"`
	Core      bool   `json:"core" doc:"Its failure flips readiness."`
	Running   bool   `json:"running"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"lastError,omitempty" doc:"Bounded, secret-free."`
}

// PgAdminLink is a pgAdmin a Cluster is registered in that the caller
// may open (get pgadmins/<name>: dba-only).
type PgAdminLink struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty" doc:"status.url; absent until the operator publishes it."`
	Health Health `json:"health" enum:"ok,progressing,warning,critical,inactive,unknown,info"`
	// ServerState is the Cluster's server entry state in that pgAdmin.
	ServerState string `json:"serverState,omitempty"`
}
