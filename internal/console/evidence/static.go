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

// This file is the runner's static mode: the pgtools chart
// deploys one evidence agent per Cluster without the operator and hands
// the console their fixed list (EVIDENCE_AGENTS_FILE). There is no
// EvidenceAgent object, so:
//
//   - the agents are the file's entries, not a listing;
//   - the console presents a shared token (re-read from its file on every
//     pass, so a rotated Secret needs no restart), not its bound
//     ServiceAccount token;
//   - an https agent is verified against the entry's CA file (re-read on
//     every pass) for its Service name; an http agent is a choice the
//     chart's values made (evidence.tls.mode none);
//   - there is no status.producerEpoch to compare an answer with (see
//     advance);
//   - the failure reasons name the chart's objects instead of the
//     EvidenceAgent's status.
//
// Results, states, the source state and the metrics keep the shapes of
// the EvidenceAgent mode; an agent's "name" is its Cluster's.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	pgtoolboxv1alpha1 "github.com/fyannk/pgToolBox/api/v1alpha1"
	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

// StaticAgent is one fixed agent (config.StaticAgent).
type StaticAgent struct {
	Namespace string
	Cluster   string
	// URL is http(s)://<service>.<namespace>.svc[:port].
	URL string
	// TokenFile holds the shared token; CAFile the PEM bundle of an https
	// agent (empty for http).
	TokenFile string
	CAFile    string
}

// reasonNoStaticAgent is ForCluster's answer for a Cluster the file does
// not name.
const reasonNoStaticAgent = "no evidence agent of EVIDENCE_AGENTS_FILE names this Cluster (evidence.agents in the chart values)"

// staticReasons replace the EvidenceAgent-mode reasons of the kinds whose
// cause or remedy differs without the operator.
//
// #nosec G101 -- display phrases that mention the token, not credentials.
var staticReasons = map[failure]string{
	failureTimeout:         "the evidence agent the chart deployed for this Cluster did not answer within the request deadline",
	failureUnavailable:     "the evidence agent the chart deployed for this Cluster does not answer (check its Deployment and Service) or has no valid publication",
	failureTLS:             "the evidence agent's certificate did not verify against its CA file for its Service name (evidence.tls)",
	failureUnauthorized:    "the evidence agent refused the console's shared token: the console and the agent do not hold the same evidence token",
	failureAuthUnavailable: "the evidence agent has no usable shared token file and refuses every request until its token Secret is mounted",
	failureIdentity:        "the evidence agent describes another cluster than the one EVIDENCE_AGENTS_FILE names for it",
	failureEndpoint:        "the agent url in EVIDENCE_AGENTS_FILE is not an http(s) Service of the Cluster's namespace",
	failureTokenMissing:    "the console has no shared evidence token (the chart's evidence token Secret is not mounted)",
	failureTokenInvalid:    "the console's shared evidence token is unreadable or shorter than 32 characters",
	failureCAFile:          "the CA file EVIDENCE_AGENTS_FILE names for this agent is missing or holds no PEM certificate (evidence.tls)",
}

// staticReason is the display-safe reason of a failure kind in the static
// mode.
func (f failure) staticReason() string {
	if text, ok := staticReasons[f]; ok {
		return text
	}
	return f.reason()
}

// staticAgents validates the fixed list (config validated it already;
// this is the runner's own guard) and keys it by "namespace/cluster".
func staticAgents(agents []StaticAgent, namespaces []string) (map[string]StaticAgent, error) {
	out := make(map[string]StaticAgent, len(agents))
	for index, agent := range agents {
		invalid := func(rule string) error {
			return fmt.Errorf("evidence: static agent %d: %s", index, rule)
		}
		if _, watched := slices.BinarySearch(namespaces, agent.Namespace); !watched {
			return nil, invalid("its namespace is not watched")
		}
		if len(validation.IsDNS1123Subdomain(agent.Cluster)) != 0 {
			return nil, invalid("the cluster is not a Cluster name")
		}
		key := stateKey(agent.Namespace, agent.Cluster)
		if _, duplicate := out[key]; duplicate {
			return nil, invalid("names the same Cluster as an earlier agent")
		}
		scheme, _, _, err := parseStaticURL(agent.URL, agent.Namespace)
		if err != nil {
			return nil, invalid("the url is not an http(s) Service of its namespace")
		}
		if !cleanAbsolute(agent.TokenFile) {
			return nil, invalid("the token file is not an absolute, clean path")
		}
		if (scheme == "https") != cleanAbsolute(agent.CAFile) || (scheme == "http" && agent.CAFile != "") {
			return nil, invalid("an https agent needs an absolute CA file and an http agent none")
		}
		out[key] = agent
	}
	return out, nil
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// staticRefs is the fixed listing ForCluster reads: one agent per Cluster,
// named after it.
func staticRefs(static map[string]StaticAgent) map[string][]agentRef {
	refs := map[string][]agentRef{}
	for _, agent := range static {
		refs[agent.Namespace] = append(refs[agent.Namespace], agentRef{name: agent.Cluster, cluster: agent.Cluster})
	}
	for namespace := range refs {
		slices.SortFunc(refs[namespace], func(a, b agentRef) int { return strings.Compare(a.name, b.name) })
	}
	return refs
}

// parseStaticURL validates a static agent URL as parseEndpoint does
// status.endpoint, with http also allowed, and returns its scheme, the
// origin to request and the TLS server name.
func parseStaticURL(raw, namespace string) (scheme, origin, serverName string, err error) {
	if raw == "" || len(raw) > maximumEndpointBytes {
		return "", "", "", errInvalidEndpoint
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", "", errInvalidEndpoint
	}
	// parseEndpoint holds the rest of the grammar; it is https-only, so
	// an http URL is checked under the https scheme.
	secured := *parsed
	secured.Scheme = "https"
	origin, serverName, err = parseEndpoint(secured.String(), namespace)
	if err != nil {
		return "", "", "", err
	}
	return parsed.Scheme, parsed.Scheme + strings.TrimPrefix(origin, "https"), serverName, nil
}

// syntheticAgent stands for a fixed agent where the shared code reads an
// EvidenceAgent: its namespace, its name (the Cluster's), spec.clusterRef
// (checkIdentity binds the answer to it) and a stable UID.
func syntheticAgent(agent StaticAgent) *pgtoolboxv1alpha1.EvidenceAgent {
	return &pgtoolboxv1alpha1.EvidenceAgent{
		ObjectMeta: metav1.ObjectMeta{
			Name: agent.Cluster, Namespace: agent.Namespace,
			UID: types.UID("static:" + agent.Namespace + "/" + agent.Cluster + "/" + agent.URL),
		},
		Spec: pgtoolboxv1alpha1.EvidenceAgentSpec{ClusterRef: pgtoolboxv1alpha1.LocalObjectReference{Name: agent.Cluster}},
	}
}

// sharedToken is one token file's read in a pass.
type sharedToken struct {
	token   evidencev1alpha1.BearerToken
	failure failure
}

// readSharedToken re-reads a shared token file.
func readSharedToken(path string) sharedToken {
	token, err := evidencev1alpha1.ReadSharedTokenFile(path)
	if err == nil {
		return sharedToken{token: token}
	}
	if _, statErr := os.Stat(path); errors.Is(statErr, fs.ErrNotExist) {
		return sharedToken{failure: failureTokenMissing}
	}
	return sharedToken{failure: failureTokenInvalid}
}

// refreshStatic is one pass over the fixed agents.
func (r *Runner) refreshStatic(ctx context.Context) {
	keys := make([]string, 0, len(r.static))
	for key := range r.static {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	listed := map[string][]*pgtoolboxv1alpha1.EvidenceAgent{}
	live := map[string]bool{}
	tokens := map[string]sharedToken{}
	tokenFailures := map[string]failure{}
	jobs := make([]*job, 0, len(keys))
	for _, key := range keys {
		agent := r.static[key]
		synthetic := syntheticAgent(agent)
		listed[agent.Namespace] = append(listed[agent.Namespace], synthetic)
		live[key] = true
		token, read := tokens[agent.TokenFile]
		if !read {
			token = readSharedToken(agent.TokenFile)
			tokens[agent.TokenFile] = token
		}
		if token.failure != "" {
			tokenFailures[agent.Namespace] = token.failure
		}
		jobs = append(jobs, r.prepareStatic(synthetic, agent, key, token))
	}
	r.pruneClients(live, nil)
	r.runJobs(ctx, jobs)
	if ctx.Err() != nil {
		return
	}
	r.merge(jobs, listed, map[string]bool{}, map[string]bool{}, tokenFailures, false)
}

// prepareStatic builds a fixed agent's session: its CA file is re-read
// and its client rebuilt when the URL or the CA changes.
func (r *Runner) prepareStatic(synthetic *pgtoolboxv1alpha1.EvidenceAgent, agent StaticAgent, key string, token sharedToken) *job {
	j := &job{agent: synthetic, static: true}
	if token.failure != "" {
		j.out.err = fail(token.failure, nil)
		return j
	}
	scheme, origin, serverName, err := parseStaticURL(agent.URL, agent.Namespace)
	if err != nil {
		j.out.err = fail(failureEndpoint, err)
		return j
	}
	var caBundle []byte
	if scheme == "https" {
		if caBundle, err = readCAFile(agent.CAFile); err != nil {
			j.out.err = fail(failureCAFile, err)
			return j
		}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(agent.URL + "\x00"))
	_, _ = hash.Write(caBundle)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	cached := r.clients[key]
	if cached == nil || cached.fingerprint != fingerprint {
		if cached != nil {
			cached.client.CloseIdleConnections()
			delete(r.clients, key)
		}
		var httpClient *http.Client
		if scheme == "https" {
			if httpClient, err = newAgentClient(caBundle, serverName, r.dial); err != nil {
				j.out.err = fail(failureCAFile, err)
				return j
			}
		} else {
			httpClient = newPlainAgentClient(r.dial)
		}
		cached = &cachedClient{fingerprint: fingerprint, client: httpClient}
		r.clients[key] = cached
	}
	j.session = &agentSession{client: cached.client, origin: origin, token: token.token}
	return j
}

// readCAFile reads an agent's CA bundle, bounded like status.caBundle.
func readCAFile(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // a path the chart mounts, validated absolute and clean
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximumCABundleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) == 0 || len(content) > maximumCABundleBytes {
		return nil, errors.New("the CA file is empty or larger than " + strconv.Itoa(maximumCABundleBytes) + " bytes")
	}
	return content, nil
}

// newPlainAgentClient is the client of an http agent: no TLS, otherwise
// the HTTPS client's bounds (no proxy from the environment, no redirect,
// bounded headers). The shared token then crosses the pod network in
// clear text; that is the evidence.tls.mode none choice the chart's
// README states, with the NetworkPolicy as the mitigation.
func newPlainAgentClient(dial dialFunc) *http.Client {
	if dial == nil {
		dial = (&net.Dialer{Timeout: evidencev1alpha1.RequestTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            dial,
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
	}
}
