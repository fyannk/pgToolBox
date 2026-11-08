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

package mcpoauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"
)

// Client ID metadata documents (draft-ietf-oauth-client-id-metadata-
// document, preferred by the MCP specification since 2025-11-25): the
// client ID is an HTTPS URL, and the document at that URL describes the
// client. Fetching a URL a stranger chose is a server-side request
// forgery risk, so the fetch is fenced:
//
//   - https only, a path other than "/", no user information, no
//     fragment, no IP literal that is not public;
//   - the address actually dialed must be public (checked at connect time,
//     after DNS, so a name resolving to 10.0.0.1 or rebinding to it is
//     refused), with no proxy (a proxy would dial for us);
//   - no redirect is followed;
//   - 5 s for the whole exchange, 32 KiB of body, a JSON content type;
//   - the document must name itself (client_id equal to its URL) and pass
//     the same rules as a dynamic registration (public client, loopback
//     redirect URIs);
//   - optionally, only the hosts an administrator lists
//     (MCP_OAUTH_CLIENT_METADATA_HOSTS);
//   - answers are cached (between 5 minutes and a day, after the
//     response's max-age; one hour by default), failures for a minute,
//     at most 256 URLs; concurrent requests for one URL share one fetch,
//     which runs to its own deadline whoever asked for it, so a caller
//     that hangs up cannot leave a failure in the cache.

const (
	fetchTimeout    = 5 * time.Second
	maxDocument     = 32 << 10
	maxCached       = 256
	defaultCacheTTL = time.Hour
	minCacheTTL     = 5 * time.Minute
	maxCacheTTL     = 24 * time.Hour
	failureTTL      = time.Minute
)

// Document is an admitted client ID metadata document.
type Document struct {
	Name         string
	RedirectURIs []string
}

// FetcherOptions configure a Fetcher.
type FetcherOptions struct {
	// AllowAddress decides whether an address may be dialed; nil admits
	// public unicast addresses only (tests widen it to their loopback
	// server).
	AllowAddress func(netip.Addr) bool
	// RootCAs is for tests; nil is the system pool.
	TLS *tls.Config
	// AllowedHosts, when set, are the only hosts (exact names, any case)
	// whose documents are fetched.
	AllowedHosts []string
	Now          func() time.Time
}

// Fetcher fetches and caches client ID metadata documents.
type Fetcher struct {
	client  *http.Client
	now     func() time.Time
	allowed map[string]bool
	flight  singleflight.Group

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	doc   Document
	err   error
	until time.Time
}

// errForbiddenAddress refuses a dial to a non-public address.
var errForbiddenAddress = errors.New("the client ID metadata document's host is not a public address")

// NewFetcher builds the fenced HTTP client.
func NewFetcher(o FetcherOptions) *Fetcher {
	if o.Now == nil {
		o.Now = time.Now
	}
	allow := o.AllowAddress
	if allow == nil {
		allow = PublicAddress
	}
	dialer := &net.Dialer{
		Timeout: fetchTimeout,
		// Control runs after DNS, on the address about to be dialed.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errForbiddenAddress
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !allow(ip.Unmap()) {
				return errForbiddenAddress
			}
			return nil
		},
	}
	tlsCfg := o.TLS
	if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        tlsCfg,
		TLSHandshakeTimeout:    fetchTimeout,
		ResponseHeaderTimeout:  fetchTimeout,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
	}
	var allowed map[string]bool
	for _, h := range o.AllowedHosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			if allowed == nil {
				allowed = map[string]bool{}
			}
			allowed[h] = true
		}
	}
	return &Fetcher{
		allowed: allowed,
		client: &http.Client{
			Transport: transport,
			Timeout:   fetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("the client ID metadata document redirects: refused")
			},
		},
		now:   o.Now,
		cache: map[string]cached{},
	}
}

// PublicAddress admits global unicast addresses that are not private,
// shared (CGNAT), loopback, link-local or reserved.
func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// nonPublic are special-purpose ranges IsGlobalUnicast admits.
var nonPublic = func() []netip.Prefix {
	ranges := []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7",
	}
	out := make([]netip.Prefix, 0, len(ranges))
	for _, s := range ranges {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// documentURLShape is the shape of a client ID URL: https, an authority
// with no user information, a path, no fragment, no white space. fetch
// checks it where the request is built, then checkDocumentURL.
var documentURLShape = regexp.MustCompile(`^https://[^/?#@\\\s]+/[^#\s]*$`)

// errDocumentURL refuses a client ID URL of the wrong shape.
var errDocumentURL = errors.New("a client ID URL must be https with a path, and no user information or fragment")

// checkDocumentURL validates a client ID URL before any fetch. The fetch
// itself is fenced again after DNS (the dialer's public-address check), with
// no proxy and no redirect.
func checkDocumentURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") ||
		u.Path == "" || u.Path == "/" || u.Opaque != "" {
		return errDocumentURL
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !PublicAddress(ip) {
		return errForbiddenAddress
	}
	return nil
}

// errHostNotAllowed refuses a host outside AllowedHosts.
var errHostNotAllowed = errors.New("the client ID metadata document's host is not one this console admits")

// Fetch returns the document at raw, from the cache or the network.
// Concurrent calls for one URL share one fetch; it runs detached from
// ctx (to its own deadline), and ctx only bounds this caller's wait.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (Document, error) {
	now := f.now()
	f.mu.Lock()
	if c, ok := f.cache[raw]; ok && now.Before(c.until) {
		f.mu.Unlock()
		return c.doc, c.err
	}
	f.mu.Unlock()
	if f.allowed != nil {
		if u, err := url.Parse(raw); err != nil || !f.allowed[strings.ToLower(u.Hostname())] {
			return Document{}, errHostNotAllowed
		}
	}
	ch := f.flight.DoChan(raw, func() (any, error) {
		return f.fetchAndCache(context.WithoutCancel(ctx), raw)
	})
	select {
	case <-ctx.Done():
		return Document{}, errors.New("the client ID metadata document could not be fetched in time")
	case res := <-ch:
		doc, _ := res.Val.(Document)
		return doc, res.Err
	}
}

// fetchAndCache fetches raw and caches the answer: a document, or a
// refusal for failureTTL. A fetch that ran out of time is not cached.
func (f *Fetcher) fetchAndCache(ctx context.Context, raw string) (Document, error) {
	doc, ttl, err := f.fetch(ctx, raw)
	if err != nil {
		ttl = failureTTL
		requests.WithLabelValues("client_metadata", "refused").Inc()
	} else {
		requests.WithLabelValues("client_metadata", "ok").Inc()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return doc, err
	}
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cache) >= maxCached {
		for k, c := range f.cache {
			if !now.Before(c.until) {
				delete(f.cache, k)
			}
		}
		if len(f.cache) >= maxCached {
			clear(f.cache)
		}
	}
	f.cache[raw] = cached{doc: doc, err: err, until: now.Add(ttl)}
	return doc, err
}

func (f *Fetcher) fetch(ctx context.Context, raw string) (Document, time.Duration, error) {
	if !documentURLShape.MatchString(raw) {
		return Document{}, 0, errDocumentURL
	}
	if err := checkDocumentURL(raw); err != nil {
		return Document{}, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	// The URL is a stranger's choice, fenced: its shape and checkDocumentURL above, the
	// dialer's public-address check after DNS, no proxy, no redirect.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil) // #nosec G704 -- fenced as above
	if err != nil {
		return Document{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req) // #nosec G704 -- fenced as above
	if err != nil {
		switch {
		case errors.Is(err, errForbiddenAddress):
			return Document{}, 0, errForbiddenAddress
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			return Document{}, 0, fmt.Errorf("the client ID metadata document could not be fetched: %w", context.DeadlineExceeded)
		}
		return Document{}, 0, errors.New("the client ID metadata document could not be fetched")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Document{}, 0, fmt.Errorf("the client ID metadata document answered HTTP %d", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return Document{}, 0, errors.New("the client ID metadata document is not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil {
		return Document{}, 0, errors.New("the client ID metadata document could not be read")
	}
	if len(body) > maxDocument {
		return Document{}, 0, fmt.Errorf("the client ID metadata document is larger than %d bytes", maxDocument)
	}
	doc, err := parseDocument(raw, body)
	if err != nil {
		return Document{}, 0, err
	}
	return doc, cacheTTL(resp.Header.Get("Cache-Control")), nil
}

// parseDocument applies the registration rules to a fetched document.
func parseDocument(raw string, body []byte) (Document, error) {
	var d struct {
		registerRequest
		ClientID     string  `json:"client_id"`
		ClientSecret *string `json:"client_secret"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return Document{}, errors.New("the client ID metadata document does not parse")
	}
	if d.ClientID != raw {
		return Document{}, errors.New("the client ID metadata document names another client_id")
	}
	if d.ClientSecret != nil {
		return Document{}, errors.New("a client ID metadata document may not carry a client secret")
	}
	name, uris, err := checkMetadata(d.registerRequest)
	if err != nil {
		return Document{}, err
	}
	return Document{Name: name, RedirectURIs: uris}, nil
}

// cacheTTL reads max-age, clamped; no-store and no-cache keep the floor.
func cacheTTL(cc string) time.Duration {
	ttl := defaultCacheTTL
	for d := range strings.SplitSeq(cc, ",") {
		d = strings.TrimSpace(strings.ToLower(d))
		switch {
		case d == "no-store" || d == "no-cache":
			return minCacheTTL
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil && n >= 0 {
				ttl = time.Duration(n) * time.Second
			}
		}
	}
	return min(max(ttl, minCacheTTL), maxCacheTTL)
}
