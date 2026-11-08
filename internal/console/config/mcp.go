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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The MCP endpoint: two switches, both off by default, and
// the per-call budgets.
const (
	// EnvMCPEnabled serves the read-only MCP endpoint at /mcp.
	EnvMCPEnabled = "MCP_ENABLED"
	// EnvMCPWriteTools adds the MCP write tools;
	// accepted only with MCP_ENABLED, effective when operations are
	// served (OPERATIONS_ENABLED).
	EnvMCPWriteTools = "MCP_WRITE_TOOLS" // #nosec G101 -- a switch's name, not a credential
	// EnvMCPMaxResultBytes caps each tool result's JSON (default 256 KiB);
	// a larger result is shortened and says so.
	EnvMCPMaxResultBytes = "MCP_MAX_RESULT_BYTES"
	// EnvMCPCallTimeout bounds each tool call (default 20s).
	EnvMCPCallTimeout = "MCP_CALL_TIMEOUT"
	// EnvMCPOAuthAccessTTL is the lifetime of an access token the console
	// issues to an MCP client (default 1h, 10s to 1h). The
	// client renews it with its refresh token.
	EnvMCPOAuthAccessTTL = "MCP_OAUTH_ACCESS_TTL"
	// EnvMCPOAuthClientMetadata accepts client ID metadata documents (an
	// HTTPS URL as client_id, which the console fetches); off by default,
	// since it makes the console reach out to the Internet. Dynamic
	// registration needs no egress and is always accepted.
	EnvMCPOAuthClientMetadata = "MCP_OAUTH_CLIENT_METADATA_DOCUMENTS"
	// EnvMCPOAuthClientMetadataHosts restricts the client ID metadata
	// documents fetched to these hosts (a comma-separated list of DNS
	// names); empty fetches from any public address.
	EnvMCPOAuthClientMetadataHosts = "MCP_OAUTH_CLIENT_METADATA_HOSTS"
)

// MCP configures the MCP endpoint.
type MCP struct {
	Enabled        bool
	WriteTools     bool
	MaxResultBytes int
	CallTimeout    time.Duration
	// AccessTTL of the console's OAuth access tokens.
	AccessTTL time.Duration
	// ClientMetadataDocuments accepts CIMD client IDs.
	ClientMetadataDocuments bool
	// ClientMetadataHosts are the only hosts CIMD documents are fetched
	// from; empty: any public address.
	ClientMetadataHosts []string
}

func mcpConfig(lookup Lookup, fail func(name, constraint string)) MCP {
	m := MCP{
		Enabled:        boolVar(lookup, EnvMCPEnabled, false, fail),
		WriteTools:     boolVar(lookup, EnvMCPWriteTools, false, fail),
		MaxResultBytes: intVar(lookup, EnvMCPMaxResultBytes, 256<<10, 16<<10, 4<<20, fail),
		CallTimeout:    durationVar(lookup, EnvMCPCallTimeout, 20*time.Second, time.Second, 50*time.Second, fail),
		AccessTTL:      durationVar(lookup, EnvMCPOAuthAccessTTL, time.Hour, 10*time.Second, time.Hour, fail),

		ClientMetadataDocuments: boolVar(lookup, EnvMCPOAuthClientMetadata, false, fail),
		ClientMetadataHosts:     hostList(lookup, EnvMCPOAuthClientMetadataHosts, fail),
	}
	if m.WriteTools && !m.Enabled {
		fail(EnvMCPWriteTools, "requires "+EnvMCPEnabled+"=true")
	}
	return m
}

func (m MCP) info(into map[string]string) {
	into[EnvMCPEnabled] = strconv.FormatBool(m.Enabled)
	into[EnvMCPWriteTools] = strconv.FormatBool(m.WriteTools)
	if !m.Enabled {
		return
	}
	into[EnvMCPMaxResultBytes] = strconv.Itoa(m.MaxResultBytes)
	into[EnvMCPCallTimeout] = m.CallTimeout.String()
	into[EnvMCPOAuthAccessTTL] = m.AccessTTL.String()
	into[EnvMCPOAuthClientMetadata] = strconv.FormatBool(m.ClientMetadataDocuments)
	into[EnvMCPOAuthClientMetadataHosts] = strings.Join(m.ClientMetadataHosts, ",")
}

// hostList reads a comma-separated list of DNS names (lower-cased, no
// duplicates).
func hostList(lookup Lookup, name string, fail func(name, constraint string)) []string {
	raw, _ := lookup(name)
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		h := strings.ToLower(strings.TrimSpace(part))
		switch {
		case h == "":
			continue
		case len(h) > 253 || !dnsName.MatchString(h):
			fail(name, "must be a comma-separated list of DNS names")
			return nil
		case slices.Contains(out, h):
			continue
		}
		out = append(out, h)
	}
	return out
}

// dnsName is a DNS name: dot-separated DNS-1123 labels.
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)
