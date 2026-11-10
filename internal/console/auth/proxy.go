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

package auth

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// maxProxyHops bounds TRUSTED_PROXY_HOPS.
const maxProxyHops = 8

// maxProxyCIDRs bounds TRUSTED_PROXY_CIDRS.
const maxProxyCIDRs = 32

// ProxyTrust says which X-Forwarded-For entries the console believes
// (TRUSTED_PROXY_HOPS, TRUSTED_PROXY_CIDRS). The header is read only when
// the direct peer is inside one of CIDRs: a peer outside them is the
// client, whatever it claims. From such a peer the chain is walked from
// the right, at most Hops entries, and stops at the first address that
// is not itself inside CIDRs. Hops without CIDRs trusts no peer, so the
// header is ignored: anyone who can reach the console's port directly
// (any Pod of the cluster, without a NetworkPolicy) could otherwise name
// a fresh address on every request and escape every per-address budget.
// A deployment whose policy admits only its proxies says so explicitly
// with 0.0.0.0/0,::/0.
type ProxyTrust struct {
	// Hops is how many reverse proxies append to X-Forwarded-For, 0 to 8.
	Hops int
	// CIDRs are the proxies' own addresses.
	CIDRs []netip.Prefix
	// PeerShared says the direct peer is not known to be the client:
	// no proxy is declared (Hops 0) but the console is published under a
	// non-loopback PUBLIC_URL, so something (a Service, an Ingress the
	// operator did not render, a cloud load balancer) may stand between
	// the clients and the Pod, and every client may then have its
	// address. FromEnv sets it; the per-address budgets are then off as
	// with Hops and no CIDRs (SharedClientKey).
	PeerShared bool
}

// trusts reports whether addr is a trusted proxy.
func (p ProxyTrust) trusts(addr netip.Addr) bool {
	addr = addr.Unmap()
	return slices.ContainsFunc(p.CIDRs, func(c netip.Prefix) bool { return c.Contains(addr) })
}

// Enabled reports whether X-Forwarded-For is read at all.
func (p ProxyTrust) Enabled() bool { return p.Hops > 0 && len(p.CIDRs) > 0 }

// SharedClientKey reports whether ClientKey may name a shared address
// rather than a client: proxies are declared (Hops) but none is trusted
// (no CIDRs), so X-Forwarded-For is ignored and every client behind the
// proxies has the proxy's address; or none is declared while the console
// is published under a non-loopback URL (PeerShared), where a proxy the
// console was not told about would do the same. A per-address budget
// then is one budget for everyone, which anyone can spend, so the
// sign-in paths do not charge it (see Handler.addressBudget).
func (p ProxyTrust) SharedClientKey() bool {
	return (p.Hops > 0 && len(p.CIDRs) == 0) || (p.Hops == 0 && p.PeerShared)
}

// String lists the CIDRs, comma-separated (pgtoolbox_console_config_info).
func (p ProxyTrust) String() string {
	parts := make([]string, len(p.CIDRs))
	for i, c := range p.CIDRs {
		parts[i] = c.String()
	}
	return strings.Join(parts, ",")
}

// ParseProxyCIDRs reads TRUSTED_PROXY_CIDRS: CIDRs (or single addresses)
// separated by commas or spaces.
func ParseProxyCIDRs(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		var p netip.Prefix
		var err error
		if strings.Contains(f, "/") {
			p, err = netip.ParsePrefix(f)
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(f); err == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil || p.Addr().Zone() != "" {
			return nil, fmt.Errorf("%q is not a CIDR", f)
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("%q is not a CIDR", f)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) > maxProxyCIDRs {
		return nil, fmt.Errorf("%d CIDRs, at most %d", len(out), maxProxyCIDRs)
	}
	return out, nil
}

// parseHop reads one address: a peer (host:port) or an X-Forwarded-For
// entry (an address, possibly bracketed or with a port).
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

// clientIP returns the client's address under trust: the direct peer,
// unless the peer is a trusted proxy; then the X-Forwarded-For chain is
// walked from the right, at most Hops entries, and the first entry that
// is not a trusted proxy is the client. Entries further left were
// written by the client and are never read. A chain that runs out, or an
// entry that is not an address, yields the last trusted hop seen.
func clientIP(r *http.Request, trust ProxyTrust) string {
	peerText := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peerText); err == nil {
		peerText = host
	}
	peer, ok := parseHop(r.RemoteAddr)
	if !ok {
		return peerText
	}
	if !trust.Enabled() || !trust.trusts(peer) {
		return peer.String()
	}
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				chain = append(chain, part)
			}
		}
	}
	current := peer
	for i := range min(trust.Hops, maxProxyHops) {
		if i >= len(chain) {
			break
		}
		hop, ok := parseHop(chain[len(chain)-1-i])
		if !ok {
			break
		}
		current = hop
		if !trust.trusts(hop) {
			break
		}
	}
	return current.String()
}
