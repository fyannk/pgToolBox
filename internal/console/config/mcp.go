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
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
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
	// EnvMCPLease names the coordination.k8s.io Lease, in the console's
	// namespace, whose holder alone serves MCP and writes the grant index;
	// it is also the name of the selector-less Service the exposure sends
	// the MCP paths to. Set by the install; empty: every replica serves.
	EnvMCPLease = "MCP_LEASE"
	// EnvMCPEndpointSlices names that Service's EndpointSlices (one per
	// address family, comma-separated), which the leader points at its Pod.
	EnvMCPEndpointSlices = "MCP_ENDPOINT_SLICES"
	// EnvPodIP is the Pod's IP (downward API), written into the slice.
	EnvPodIP = "POD_IP"
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
	// Lease, EndpointSlices and PodIP elect the replica that serves MCP
	// (MCP_LEASE); all empty when every replica serves.
	Lease          string
	EndpointSlices []string
	PodIP          string
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
	m.leader(lookup, fail)
	return m
}

// leader reads MCP_LEASE and, with it, the slices and the Pod IP.
func (m *MCP) leader(lookup Lookup, fail func(name, constraint string)) {
	lease, _ := lookup(EnvMCPLease)
	names, _ := lookup(EnvMCPEndpointSlices)
	if lease == "" {
		if names != "" {
			fail(EnvMCPEndpointSlices, "requires "+EnvMCPLease)
		}
		return
	}
	switch {
	case !m.Enabled:
		fail(EnvMCPLease, "requires "+EnvMCPEnabled+"=true")
		return
	case len(validation.IsDNS1123Subdomain(lease)) != 0:
		fail(EnvMCPLease, "must be a Lease name")
		return
	}
	var list []string
	for part := range strings.SplitSeq(names, ",") {
		switch name := strings.TrimSpace(part); {
		case name == "", slices.Contains(list, name):
		case len(validation.IsDNS1123Subdomain(name)) != 0:
			fail(EnvMCPEndpointSlices, "must be a comma-separated list of EndpointSlice names")
			return
		default:
			list = append(list, name)
		}
	}
	ip, _ := lookup(EnvPodIP)
	addr, err := netip.ParseAddr(ip)
	switch {
	case len(list) == 0 || len(list) > 2:
		fail(EnvMCPEndpointSlices, "must name one or two EndpointSlices with "+EnvMCPLease)
	case err != nil || addr.Zone() != "":
		fail(EnvPodIP, "must be the Pod's IP address with "+EnvMCPLease)
	default:
		m.Lease, m.EndpointSlices, m.PodIP = lease, list, addr.String()
	}
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
	into[EnvMCPLease] = m.Lease
	into[EnvMCPEndpointSlices] = strings.Join(m.EndpointSlices, ",")
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
