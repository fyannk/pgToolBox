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

// Package adminsync is the libpq credential file writer: the connection
// service file and the password file (0600, written atomically) that carry
// the passwords of the offered PostgreSQL roles on a memory-backed volume
// outside pgAdmin's storage directory, with their bounds and the pgpass
// escaping proven against libpq's own parser. The PgAdmin Pod's
// passfile-sync sidecar uses it: it writes local files only and serves
// nothing.
package adminsync

// DefaultPassFilePath is where the sidecar writes the pgpass file, the same
// credentials for a libpq client in the Pod that names no service. The
// pgAdmin container names it in PGPASSFILE. It is on a memory-backed emptyDir, outside pgAdmin's storage
// directory, so no account can browse or download it.
// #nosec G101 -- filesystem path; no credential material.
const DefaultPassFilePath = "/run/pgadmin/passfile/pgpass"

// DefaultServiceFilePath is where the sidecar writes the libpq connection
// service file, one section per managed server; the pgAdmin container names
// it in PGSERVICEFILE, and each server definition names its section. It
// shares the passfile's memory-backed emptyDir.
// #nosec G101 -- filesystem path; no credential material.
const DefaultServiceFilePath = "/run/pgadmin/passfile/pg_service.conf"

// MaxServers bounds the managed connections of one console, and with it
// the credential files: the operator offers no more, the sidecar refuses
// a request with more.
const MaxServers = 64

// MaxCredentialFilesBytes bounds the service file and the pgpass file
// together. The request bound does not imply it: pgpass escaping can double
// a value, and the service file repeats the host. A request whose files
// would be larger is offered nowhere.
const MaxCredentialFilesBytes = 1 << 20

// PassFileVolumeBytes sizes the memory-backed volume holding the two
// credential files. An atomic replace writes a full temporary copy of one
// file beside the old pair, so the peak is the old pair plus one new file:
// at most 2 × MaxCredentialFilesBytes. One MiB more covers tmpfs page
// rounding with a wide margin.
const PassFileVolumeBytes = 2*MaxCredentialFilesBytes + 1<<20

// Server is one connection pgAdmin should offer, built from a credential
// the CloudNativePG cluster itself publishes. Every pgAdmin account gets
// the same list: the credentials belong to the cluster, not to whoever
// signed in, and everyone who reaches pgAdmin has already been admitted as
// dba by the proxy.
type Server struct {
	Name          string `json:"name"`
	Group         string `json:"group"`
	Host          string `json:"host"`
	Port          int32  `json:"port"`
	MaintenanceDB string `json:"maintenanceDB"`
	Username      string `json:"username"`
	SSLMode       string `json:"sslMode"`
	// Password is written into the service and pgpass files, never into
	// the server definition, so it stays out of pgAdmin's settings
	// database and out of its storage directory.
	Password string `json:"password"`
	// Service, when set, is the service-file section name; otherwise one
	// is derived from Name (ServiceNames). The PgAdmin controller names
	// the section itself so the registration Job and the passfile-sync
	// sidecar agree without sharing state.
	Service string `json:"service,omitempty"`
	// SSLRootCert, when set, is written as the section's sslrootcert: the
	// CA file of this server's Cluster, so one service file verifies
	// several Clusters with different CAs.
	SSLRootCert string `json:"sslRootCert,omitempty"`
}
