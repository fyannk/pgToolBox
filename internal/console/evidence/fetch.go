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

package evidence

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// Consumer-side bounds. Every channel constant (media type, routes, size
// ceilings, page size, cursor size, deadline, TLS floor) comes from the
// contract module and is never re-declared here; these bound what the
// console assembles from many responses.
const (
	// maximumBackupPages bounds one cursor chain. An agent serving more
	// pages than this is cut visibly (BackupsComplete false).
	maximumBackupPages = 50
	// maximumBackupBytes bounds the encoded bytes of one cursor chain.
	maximumBackupBytes = 16 * 1024 * 1024
	// maximumBackupIDs bounds the assembled backup_id set.
	maximumBackupIDs = 10000
	// MaximumBackupItems bounds the compact backup items kept for the
	// recovery window; the newest by end time win.
	MaximumBackupItems = 1000
	// maximumRecoveryPages, maximumRecoveryBytes and maximumRecoveryPaths
	// bound the recovery-path chain like the backup one.
	maximumRecoveryPages = 20
	maximumRecoveryBytes = 8 * 1024 * 1024
	maximumRecoveryPaths = 2000
	// maximumErrorBytes bounds the error body read to classify a refusal.
	maximumErrorBytes = 4096
	// maximumEndpointBytes mirrors the CRD bound of status.endpoint.
	maximumEndpointBytes = 512
	// maximumCABundleBytes mirrors the CRD bound of status.caBundle.
	maximumCABundleBytes = 64 * 1024
	// maximumResponseHeaderBytes bounds the agent's response headers.
	maximumResponseHeaderBytes = 64 * 1024
)

// failure is the closed, display-safe classification of a refused or
// failed fetch. It is also the outcome label of the fetch counter.
type failure string

// The failure kinds.
const (
	failureTimeout            failure = "timeout"
	failureCanceled           failure = "canceled"
	failureTLS                failure = "tls"
	failureUnauthorized       failure = "unauthorized"
	failureForbidden          failure = "forbidden"
	failureAuthUnavailable    failure = "auth_unavailable"
	failureBusy               failure = "busy"
	failureContract           failure = "contract"
	failureTooLarge           failure = "too_large"
	failureUnavailable        failure = "unavailable"
	failureIdentity           failure = "identity"
	failurePublicationChanged failure = "publication_changed"
	failureEndpoint           failure = "endpoint"
	failureCABundle           failure = "ca_bundle"
	failureTokenMissing       failure = "token_missing"
	failureTokenInvalid       failure = "token_invalid"
	failureAudienceMismatch   failure = "audience_mismatch"
	failureStatusWriter       failure = "status_writer"
	// failureCAFile is the static mode's (static.go): an agent's CA file
	// is missing or holds no certificate.
	failureCAFile failure = "ca_file"
)

// failureReasons are the display-safe reasons of the failure kinds:
// fixed phrases, never transport detail, a path, a header or a body.
//
// #nosec G101 -- display phrases that mention the token, not credentials.
var failureReasons = map[failure]string{
	failureTimeout:            "the evidence agent did not answer within the request deadline",
	failureCanceled:           "the evidence request was canceled",
	failureTLS:                "the evidence agent's certificate did not verify against status.caBundle for its Service name",
	failureUnauthorized:       "the evidence agent refused the console's token (unauthenticated)",
	failureForbidden:          "the console's ServiceAccount may not get this EvidenceAgent",
	failureAuthUnavailable:    "the evidence agent could not review the console's token; it will be retried",
	failureBusy:               "the evidence agent is busy; it will be retried",
	failureContract:           "the evidence agent's answer does not follow the evidence/v1alpha1 contract",
	failureTooLarge:           "the evidence agent's answer exceeds the contract's size bound",
	failureUnavailable:        "the evidence agent could not be reached or has no valid publication",
	failureIdentity:           "the evidence agent describes another cluster or repository than this EvidenceAgent",
	failurePublicationChanged: "the repository evidence changed while it was being read; it will be retried",
	failureEndpoint:           "status.endpoint is not an HTTPS Service name in the EvidenceAgent's own namespace",
	failureCABundle:           "status.caBundle holds no usable PEM certificate",
	failureTokenMissing:       "the console has no evidence-agent token (chart projection missing)",
	failureTokenInvalid:       "the console's evidence-agent token is unreadable or malformed",
	failureAudienceMismatch:   "not verifiable: audience mismatch (the agent accepts only the token of audience pgtoolbox-evidence-agent/<its namespace> and refused the console's; check the console's per-namespace token projection, or an agent older than the console)",
	failureCAFile:             "the evidence agent's CA file is missing or holds no PEM certificate",
	failureStatusWriter:       "not verifiable: status not written by the operator (status.endpoint or status.caBundle was set by another field manager than " + OperatorStatusManager + "); the console does not call this agent",
}

// reason returns the display-safe reason of a failure kind.
func (f failure) reason() string {
	if text, ok := failureReasons[f]; ok {
		return text
	}
	return failureReasons[failureUnavailable]
}

// fetchError carries a closed failure kind. Its message is the kind
// alone; the cause stays wrapped for in-process branching and debug
// logging and never reaches a Result.
type fetchError struct {
	kind  failure
	cause error
}

func (e *fetchError) Error() string { return "evidence agent: " + string(e.kind) }

func (e *fetchError) Unwrap() error { return e.cause }

func fail(kind failure, cause error) error { return &fetchError{kind: kind, cause: cause} }

// kindOf returns the failure kind of an error of this package; anything
// else is unavailable.
func kindOf(err error) failure {
	var typed *fetchError
	if errors.As(err, &typed) {
		return typed.kind
	}
	return failureUnavailable
}

// errInvalidEndpoint is the only error parseEndpoint returns.
var errInvalidEndpoint = errors.New("evidence agent endpoint is not a Service of its own namespace")

// parseEndpoint validates EvidenceAgent.status.endpoint for an agent in
// namespace: https, no userinfo, path, query or fragment, and a host of
// exactly <service>.<namespace>.svc with both labels DNS-1123 labels
// and the namespace the agent's own. It returns the origin to request
// and the TLS server name to verify. Anything else is refused: the
// console calls agents of the agent's namespace only.
func parseEndpoint(raw, namespace string) (origin, serverName string, err error) {
	if raw == "" || len(raw) > maximumEndpointBytes || len(validation.IsDNS1123Label(namespace)) != 0 {
		return "", "", errInvalidEndpoint
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", errInvalidEndpoint
	}
	if parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.RawPath != "" ||
		parsed.Path != "" && parsed.Path != "/" {
		return "", "", errInvalidEndpoint
	}
	host, port := parsed.Hostname(), parsed.Port()
	if port != "" {
		number, convErr := strconv.Atoi(port)
		if convErr != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
			return "", "", errInvalidEndpoint
		}
		if parsed.Host != host+":"+port {
			return "", "", errInvalidEndpoint
		}
	} else if parsed.Host != host {
		return "", "", errInvalidEndpoint
	}
	labels := strings.Split(host, ".")
	if len(labels) != 3 || labels[1] != namespace || labels[2] != "svc" || len(validation.IsDNS1123Label(labels[0])) != 0 {
		return "", "", errInvalidEndpoint
	}
	return "https://" + parsed.Host, host, nil
}

// dialFunc is the transport's dialer; tests replace it to reach an
// httptest server under a Service name.
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// newAgentClient builds an HTTPS client that trusts the agent's CA bundle
// only (no system roots), verifies serverName, offers TLS 1.3 only, uses
// no proxy from the environment and follows no redirect.
func newAgentClient(caBundle []byte, serverName string, dial dialFunc) (*http.Client, error) {
	if len(caBundle) == 0 || len(caBundle) > maximumCABundleBytes {
		return nil, fail(failureCABundle, nil)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fail(failureCABundle, nil)
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: evidencev1alpha1.RequestTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	transport := &http.Transport{
		// The agent is in-cluster: an HTTPS_PROXY from the environment
		// must never see the bearer token.
		Proxy:       nil,
		DialContext: dial,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: serverName,
			MinVersion: evidencev1alpha1.MinimumTLSVersion,
		},
		TLSHandshakeTimeout:    evidencev1alpha1.RequestTimeout,
		ResponseHeaderTimeout:  evidencev1alpha1.RequestTimeout,
		MaxResponseHeaderBytes: maximumResponseHeaderBytes,
		DisableCompression:     true,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    2,
		IdleConnTimeout:        90 * time.Second,
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// agentSession is one agent's client and credentials for one pass.
type agentSession struct {
	client *http.Client
	origin string
	token  evidencev1alpha1.BearerToken
}

// get performs one authenticated, bounded GET under the contract's
// per-request deadline. Every error carries a closed failure kind.
func (s agentSession) get(ctx context.Context, pathAndQuery string, maximum int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, evidencev1alpha1.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origin+pathAndQuery, http.NoBody)
	if err != nil {
		return nil, fail(failureContract, err)
	}
	request.Header.Set("Accept", evidencev1alpha1.MediaType)
	request.Header.Set("Authorization", s.token.AuthorizationHeader())
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fail(transportKind(ctx, err), err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumErrorBytes))
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBytes))
		return nil, fail(statusKind(response.StatusCode, body), errors.New("evidence agent status "+strconv.Itoa(response.StatusCode)))
	}
	if !mediaTypeOK(response.Header.Get("Content-Type")) {
		return nil, fail(failureContract, errors.New("unexpected media type"))
	}
	if response.ContentLength > int64(maximum) {
		return nil, fail(failureTooLarge, nil)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maximum)+1))
	if err != nil {
		return nil, fail(transportKind(ctx, err), err)
	}
	if len(body) > maximum {
		return nil, fail(failureTooLarge, nil)
	}
	return body, nil
}

// mediaTypeOK accepts the contract's media type, with parameters.
func mediaTypeOK(header string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && mediaType == evidencev1alpha1.MediaType
}

// statusKind classifies a non-200 answer. The error body is consulted
// only to tell a token-review outage from another unavailability.
func statusKind(status int, body []byte) failure {
	switch status {
	case http.StatusUnauthorized:
		if code, ok := decodeAPIError(body); ok && code == evidencev1alpha1.ErrorAudienceMismatch {
			return failureAudienceMismatch
		}
		return failureUnauthorized
	case http.StatusForbidden:
		return failureForbidden
	case http.StatusTooManyRequests:
		return failureBusy
	case http.StatusConflict:
		return failurePublicationChanged
	case http.StatusRequestEntityTooLarge:
		return failureTooLarge
	case http.StatusServiceUnavailable:
		if code, ok := decodeAPIError(body); ok && code == evidencev1alpha1.ErrorAuthenticationUnavailable {
			return failureAuthUnavailable
		}
		return failureUnavailable
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return failureUnavailable
	default:
		// Redirects, 400, 404, 405 and anything undeclared: the agent
		// and the console disagree on the channel.
		return failureContract
	}
}

// transportKind classifies a transport error.
func transportKind(ctx context.Context, err error) failure {
	var (
		unknownAuthority x509.UnknownAuthorityError
		hostname         x509.HostnameError
		invalid          x509.CertificateInvalidError
		verification     *tls.CertificateVerificationError
		alert            tls.AlertError
		header           tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failureTimeout
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return failureCanceled
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid),
		errors.As(err, &verification), errors.As(err, &alert), errors.As(err, &header):
		return failureTLS
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return failureTimeout
		}
		return failureUnavailable
	}
}

// decodeJSON decodes exactly one JSON value.
func decodeJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// decodeAPIError reads an agent error body; ok only when it validates.
func decodeAPIError(body []byte) (evidencev1alpha1.ErrorCode, bool) {
	if len(body) == 0 || len(body) > maximumErrorBytes {
		return "", false
	}
	var value evidencev1alpha1.EvidenceAPIError
	if decodeJSON(body, &value) != nil || value.Validate() != nil {
		return "", false
	}
	return value.Code, true
}

// decodeSnapshot decodes one snapshot body and validates it with the
// contract's own invariants; it never returns a snapshot that fails
// Validate.
func decodeSnapshot(body []byte) (*evidencev1alpha1.RepositoryEvidenceSnapshot, error) {
	if len(body) > evidencev1alpha1.MaximumSnapshotBytes {
		return nil, fail(failureTooLarge, nil)
	}
	var snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	if err := decodeJSON(body, &snapshot); err != nil {
		return nil, fail(failureContract, err)
	}
	if err := snapshot.Validate(); err != nil {
		return nil, fail(failureContract, err)
	}
	return &snapshot, nil
}

// decodePage decodes one collection page of any kind and validates it
// with the contract's own invariants, cursor bound included.
func decodePage[P any, PT interface {
	*P
	Validate() error
}](body []byte, header func(PT) evidencev1alpha1.PageHeader) (PT, error) {
	if len(body) > evidencev1alpha1.MaximumCollectionBytes {
		return nil, fail(failureTooLarge, nil)
	}
	page := PT(new(P))
	if err := decodeJSON(body, page); err != nil {
		return nil, fail(failureContract, err)
	}
	if err := page.Validate(); err != nil {
		return nil, fail(failureContract, err)
	}
	if next := header(page).NextCursor; next != nil && (*next == "" || len(*next) > evidencev1alpha1.MaximumCursorBytes) {
		return nil, fail(failureContract, errors.New("page cursor is out of bounds"))
	}
	return page, nil
}

// decodeBackupPage decodes one Barman backup page.
func decodeBackupPage(body []byte) (*evidencev1alpha1.BarmanBackupPage, error) {
	return decodePage(body, func(p *evidencev1alpha1.BarmanBackupPage) evidencev1alpha1.PageHeader { return p.PageHeader })
}

// checkIdentity binds a validated snapshot to the EvidenceAgent that
// published its endpoint: the snapshot's cluster namespace is the
// agent's, its cluster name (when present) is spec.clusterRef.name, and
// its repository fingerprint and scope match the status when the status
// has them: the Barman server for a barman-cloud repository, the backup
// stanza (status.pgbackrest.stanza) for a pgBackRest one, whose format
// must then be pgbackrest. Evidence about anything else is a mismatch,
// never a lesser answer.
func checkIdentity(agent *pgtoolboxv1alpha1.EvidenceAgent, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) error {
	identity := snapshot.Identity
	pgbackrest := agent.Status.PgBackRest
	switch {
	case identity.Cluster.Namespace != agent.Namespace:
		return fail(failureIdentity, errors.New("cluster namespace mismatch"))
	case identity.Cluster.Name != nil && *identity.Cluster.Name != agent.Spec.ClusterRef.Name:
		return fail(failureIdentity, errors.New("cluster name mismatch"))
	case agent.Status.Fingerprint != "" && identity.Repository.DestinationFingerprint != agent.Status.Fingerprint:
		return fail(failureIdentity, errors.New("destination fingerprint mismatch"))
	case agent.Status.ServerName != "" && (identity.Repository.Format != evidencev1alpha1.FormatBarmanCloud || identity.Repository.Scope.Name != agent.Status.ServerName):
		return fail(failureIdentity, errors.New("barman server mismatch"))
	case pgbackrest != nil && identity.Repository.Format != evidencev1alpha1.FormatPGBackRest:
		return fail(failureIdentity, errors.New("the agent resolved a pgBackRest repository and answers for another format"))
	case pgbackrest != nil && pgbackrest.Stanza != "" && identity.Repository.Scope.Name != pgbackrest.Stanza:
		return fail(failureIdentity, errors.New("pgBackRest stanza mismatch"))
	case pgbackrest != nil && pgbackrest.Repository > 0 && snapshot.Details.PGBackRest != nil && snapshot.Details.PGBackRest.Repository != uint64(pgbackrest.Repository):
		return fail(failureIdentity, errors.New("pgBackRest repository number mismatch"))
	}
	return nil
}

// epochCurrent reports whether a snapshot comes from the configuration
// the EvidenceAgent status says is running. A status without an epoch
// (rollout not complete) cannot vouch for any snapshot.
func epochCurrent(agent *pgtoolboxv1alpha1.EvidenceAgent, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) bool {
	epoch := agent.Status.ProducerEpoch
	return epoch > 0 && uint64(epoch) == snapshot.ProducerEpoch
}

// backups is the assembled backup collection of one publication, and
// the recovery coverage of the same publication.
type backups struct {
	ids      map[string]bool
	complete bool
	items    []BackupItem
	recovery Recovery
}

// fetch reads one agent: the snapshot, its identity, then the backup
// collection of that publication. A publication change mid-chain is
// retried once from the snapshot. Any failure fails the whole fetch, so
// a partial answer never replaces good evidence.
func (s agentSession) fetch(ctx context.Context, agent *pgtoolboxv1alpha1.EvidenceAgent) (*evidencev1alpha1.RepositoryEvidenceSnapshot, backups, error) {
	var lastErr error
	for range 2 {
		body, err := s.get(ctx, evidencev1alpha1.SnapshotPath, evidencev1alpha1.MaximumSnapshotBytes)
		if err != nil {
			return nil, backups{}, err
		}
		snapshot, err := decodeSnapshot(body)
		if err != nil {
			return nil, backups{}, err
		}
		if err := checkIdentity(agent, snapshot); err != nil {
			return nil, backups{}, err
		}
		var collection backups
		if snapshot.Identity.Repository.Format == evidencev1alpha1.FormatPGBackRest {
			collection, err = s.pgbackrestBackups(ctx, snapshot)
			if err == nil {
				collection.recovery, err = s.pgbackrestRecoveryPaths(ctx, snapshot, collection.items)
			}
		} else {
			collection, err = s.backups(ctx, snapshot)
			if err == nil {
				collection.recovery, err = s.recoveryPaths(ctx, snapshot)
			}
		}
		if err == nil {
			collection.recovery = capRecovery(snapshot, collection.recovery, collection.items)
			return snapshot, collection, nil
		}
		if kindOf(err) != failurePublicationChanged {
			return nil, backups{}, err
		}
		lastErr = err
	}
	return nil, backups{}, lastErr
}

// collectPages walks one generation-consistent cursor chain of the
// snapshot's publication, bounded by pages and bytes: each page is
// decoded and checked against the snapshot, then each item is handed to
// keep, which returns false at its own bound. complete is true only when
// the chain ended, nothing was cut, and the agent's total agrees with
// what was read.
func collectPages[T any](ctx context.Context, s agentSession, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot, path string,
	maxPages, maxBytes int, decode func([]byte) (evidencev1alpha1.PageHeader, []T, error), keep func(T) bool,
) (complete bool, err error) {
	revision := strconv.FormatUint(snapshot.Revision, 10)
	cursor, read, seen := "", 0, uint64(0)
	cursors := map[string]bool{}
	for range maxPages {
		query := url.Values{"revision": {revision}, "limit": {strconv.Itoa(evidencev1alpha1.MaximumPageSize)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		body, err := s.get(ctx, path+"?"+query.Encode(), evidencev1alpha1.MaximumCollectionBytes)
		if err != nil {
			return false, err
		}
		read += len(body)
		header, items, err := decode(body)
		if err != nil {
			return false, err
		}
		if header.Revision != snapshot.Revision || header.EvidenceGeneration != snapshot.EvidenceGeneration {
			return false, fail(failureContract, errors.New("page publication identity differs from the snapshot"))
		}
		seen += uint64(len(items))
		for _, item := range items {
			if !keep(item) {
				return false, nil
			}
		}
		if header.NextCursor == nil {
			return header.TotalItems == nil || *header.TotalItems == seen, nil
		}
		if cursors[*header.NextCursor] {
			return false, fail(failureContract, errors.New("cursor chain repeats"))
		}
		cursors[*header.NextCursor] = true
		if read >= maxBytes {
			return false, nil
		}
		cursor = *header.NextCursor
	}
	return false, nil
}

// backups assembles the backup_id set of the snapshot's Barman server
// through generation-consistent pages, bounded by pages, bytes and
// items. complete is true only when the chain ended, nothing was cut,
// and the agent's total agrees with what was read. A snapshot without a
// completed generation has no collection: absence then proves nothing.
func (s agentSession) backups(ctx context.Context, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) (backups, error) {
	if snapshot.EvidenceGeneration == 0 {
		return backups{ids: map[string]bool{}}, nil
	}
	server := snapshot.Identity.Repository.Scope.Name
	decode := func(body []byte) (evidencev1alpha1.PageHeader, []evidencev1alpha1.BarmanBackup, error) {
		page, err := decodeBackupPage(body)
		if err != nil {
			return evidencev1alpha1.PageHeader{}, nil, err
		}
		return page.PageHeader, page.Items, nil
	}
	return collectBackups(ctx, s, snapshot, decode, func(item evidencev1alpha1.BarmanBackup) (string, bool) {
		return item.BackupID, item.Server == server
	}, func(item evidencev1alpha1.BarmanBackup) BackupItem {
		endTimeline, endPosition := walPosition(item.EndWAL, item.WALSegmentSizeBytes)
		return BackupItem{
			ID: item.BackupID, Status: item.Status, State: item.State, Reason: item.Reason, BeginAt: item.BeginAt, EndAt: item.EndAt,
			EndTimeline: endTimeline, EndPosition: endPosition, Failed: item.Status != nil && *item.Status == "FAILED",
		}
	})
}

// collectBackups walks the backup collection of either format: key gives
// an item's identity and whether it belongs to the snapshot's scope (a
// server or a stanza), project its compact form. Each identity is kept
// once and at most maximumBackupIDs of them; reaching the bound ends the
// walk incomplete. The result keeps the newest items.
func collectBackups[T any](
	ctx context.Context, s agentSession, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot,
	decode func([]byte) (evidencev1alpha1.PageHeader, []T, error),
	key func(T) (string, bool), project func(T) BackupItem,
) (backups, error) {
	result := backups{ids: map[string]bool{}}
	complete, err := collectPages(ctx, s, snapshot, evidencev1alpha1.BackupsPath, maximumBackupPages, maximumBackupBytes, decode, func(item T) bool {
		id, inScope := key(item)
		if !inScope {
			return true
		}
		if len(result.ids) >= maximumBackupIDs && !result.ids[id] {
			return false
		}
		if !result.ids[id] {
			result.items = append(result.items, project(item))
		}
		result.ids[id] = true
		return true
	})
	if err != nil {
		return backups{}, err
	}
	result.complete = complete
	result.items = newestItems(result.items)
	return result, nil
}

// newestItems orders the items by end time (unknown first) and keeps
// the newest MaximumBackupItems.
func newestItems(items []BackupItem) []BackupItem {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].EndAt, items[j].EndAt
		switch {
		case a == nil || b == nil:
			return a == nil && b != nil
		default:
			return a.Before(*b)
		}
	})
	if len(items) > MaximumBackupItems {
		items = append([]BackupItem(nil), items[len(items)-MaximumBackupItems:]...)
	}
	return items
}

// walPosition parses a WAL segment name into its timeline and its
// position counted in segments (log * segments-per-log + segment), the
// unit of the agent's recovery paths. Anything that does not parse, or a
// segment size that is not a power of two from 1 MiB to 1 GiB, gives nil:
// the console then cannot check the path reached the backup's end.
func walPosition(name *string, segmentSize *uint64) (*uint64, *uint64) {
	if name == nil || segmentSize == nil || len(*name) != 24 {
		return nil, nil
	}
	size := *segmentSize
	if size < 1<<20 || size > 1<<30 || size&(size-1) != 0 {
		return nil, nil
	}
	var parts [3]uint64
	for i := range parts {
		v, err := strconv.ParseUint((*name)[i*8:i*8+8], 16, 32)
		if err != nil {
			return nil, nil
		}
		parts[i] = v
	}
	perLog := uint64(1<<32) / size
	if parts[0] == 0 || parts[2] >= perLog {
		return nil, nil
	}
	timeline, position := parts[0], parts[1]*perLog+parts[2]
	return &timeline, &position
}

// recoveryPaths reads the observed recovery coverage of the snapshot's
// publication, bounded like the backups. Only a publication change is an
// error (the caller retries from the snapshot); any other failure leaves
// the recovery part unavailable with its reason and keeps the rest of
// the evidence, so an agent without the collection still serves the
// snapshot.
func (s agentSession) recoveryPaths(ctx context.Context, snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) (Recovery, error) {
	if out, done := recoveryPrecheck(snapshot); done {
		return out, nil
	}
	server := snapshot.Identity.Repository.Scope.Name
	out := Recovery{State: RecoveryAvailable}
	decode := func(body []byte) (evidencev1alpha1.PageHeader, []evidencev1alpha1.BarmanRecoveryPath, error) {
		page, err := decodeRecoveryPage(body)
		if err != nil {
			return evidencev1alpha1.PageHeader{}, nil, err
		}
		return page.PageHeader, page.Items, nil
	}
	complete, err := collectPages(ctx, s, snapshot, evidencev1alpha1.RecoveryPathsPath, maximumRecoveryPages, maximumRecoveryBytes, decode, func(item evidencev1alpha1.BarmanRecoveryPath) bool {
		if item.Server != server {
			return true
		}
		if len(out.Paths) >= maximumRecoveryPaths {
			return false
		}
		out.Paths = append(out.Paths, RecoveryPath{
			BackupID: item.BackupID, TargetTimeline: item.TargetTimeline, State: item.State, Reason: item.Reason, Stop: item.Stop,
			LowerBoundAt: item.LowerBoundAt, FrontierReceiptAt: item.FrontierReceiptAt, Assumptions: item.AssumptionCodes,
			StartTimeline: item.StartTimeline, FrontierTimeline: item.FrontierTimeline, FrontierPosition: item.FrontierPosition,
			OffTarget: item.OffTarget,
		})
		return true
	})
	return recoveryOutcome(out, complete, err)
}

// recoveryPrecheck answers without a fetch when the publication has no
// completed scan or does not claim recovery coverage.
func recoveryPrecheck(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) (Recovery, bool) {
	if snapshot.EvidenceGeneration == 0 {
		return Recovery{State: RecoveryNotScanned, Reason: "the evidence agent has not completed a scan of the repository yet"}, true
	}
	if !recoverySupported(snapshot) {
		return Recovery{State: RecoveryUnsupported, Reason: "the evidence agent does not claim observed recovery coverage for this repository"}, true
	}
	return Recovery{}, false
}

// recoveryOutcome turns a collected chain into the Recovery answer: a
// publication change is the caller's to retry, any other failure leaves
// the recovery part unavailable with its reason.
func recoveryOutcome(out Recovery, complete bool, err error) (Recovery, error) {
	if err != nil {
		if kindOf(err) == failurePublicationChanged {
			return Recovery{}, err
		}
		return Recovery{State: RecoveryUnavailable, Reason: kindOf(err).reason()}, nil
	}
	out.Complete = complete
	return out, nil
}

// recoverySupported reports whether the snapshot claims the observed
// recovery coverage capability.
func recoverySupported(snapshot *evidencev1alpha1.RepositoryEvidenceSnapshot) bool {
	for _, c := range snapshot.Capabilities {
		if c.ID == evidencev1alpha1.RecoveryCoverage {
			return c.Support == evidencev1alpha1.Supported
		}
	}
	return false
}

// decodeRecoveryPage decodes one Barman recovery-path page.
func decodeRecoveryPage(body []byte) (*evidencev1alpha1.BarmanRecoveryPathPage, error) {
	return decodePage(body, func(p *evidencev1alpha1.BarmanRecoveryPathPage) evidencev1alpha1.PageHeader { return p.PageHeader })
}
