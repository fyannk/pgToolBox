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
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// EnvEvidenceAgentsFile names the fixed list of evidence agents the
// pgtools chart deploys without the operator: a JSON or
// YAML list of {namespace, cluster, url, tokenFile, caFile}. Set, the
// console reads these agents instead of listing EvidenceAgent objects.
const EnvEvidenceAgentsFile = "EVIDENCE_AGENTS_FILE"

const (
	// maximumAgentsFileBytes bounds the agents file (a ConfigMap key).
	maximumAgentsFileBytes = 256 * 1024
	// maximumStaticAgents bounds the list: one agent per Cluster.
	maximumStaticAgents = 256
)

// StaticAgent is one evidence agent of EVIDENCE_AGENTS_FILE.
type StaticAgent struct {
	// Namespace is the agent's and its Cluster's namespace, a watched one.
	Namespace string `json:"namespace"`
	// Cluster names the CloudNativePG Cluster whose evidence it serves.
	Cluster string `json:"cluster"`
	// URL is the agent's Service: http(s)://<service>.<namespace>.svc[:port].
	URL string `json:"url"`
	// TokenFile holds the shared token the console presents; it is re-read
	// on every pass, so a rotated Secret needs no restart.
	TokenFile string `json:"tokenFile"`
	// CAFile is the PEM bundle the agent's certificate verifies against:
	// required for https, refused for http. Re-read on every pass.
	CAFile string `json:"caFile,omitempty"`
}

// loadStaticAgents reads and validates EVIDENCE_AGENTS_FILE. Errors name
// the entry by index and the rule, never a value from the file.
func loadStaticAgents(path string, watched []string, fail func(name, constraint string)) []StaticAgent {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		fail(EnvEvidenceAgentsFile, "must be an absolute, clean path")
		return nil
	}
	file, err := os.Open(path) //nolint:gosec // an operator-configured mount path, validated absolute and clean
	if err != nil {
		fail(EnvEvidenceAgentsFile, "must name a readable file (the chart's evidence agents ConfigMap)")
		return nil
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximumAgentsFileBytes+1))
	if err != nil || len(content) > maximumAgentsFileBytes {
		fail(EnvEvidenceAgentsFile, fmt.Sprintf("must be a readable file of at most %d bytes", maximumAgentsFileBytes))
		return nil
	}
	var agents []StaticAgent
	// UnmarshalStrict refuses unknown and duplicated keys: a misspelled
	// caFile must not silently become plain trust in nothing.
	if err := yaml.UnmarshalStrict(content, &agents); err != nil {
		fail(EnvEvidenceAgentsFile, "must hold a JSON or YAML list of {namespace, cluster, url, tokenFile, caFile} without other keys")
		return nil
	}
	if len(agents) == 0 || len(agents) > maximumStaticAgents {
		fail(EnvEvidenceAgentsFile, fmt.Sprintf("must list between 1 and %d agents", maximumStaticAgents))
		return nil
	}
	seen := map[string]bool{}
	valid := true
	for index, agent := range agents {
		entry := func(constraint string) {
			fail(EnvEvidenceAgentsFile, "entry "+strconv.Itoa(index)+": "+constraint)
			valid = false
		}
		if len(validation.IsDNS1123Label(agent.Namespace)) != 0 || !slices.Contains(watched, agent.Namespace) {
			entry("namespace must be one of " + EnvWatchNamespaces)
			continue
		}
		if len(validation.IsDNS1123Subdomain(agent.Cluster)) != 0 {
			entry("cluster must be a Cluster name")
		}
		key := agent.Namespace + "/" + agent.Cluster
		if seen[key] {
			entry("names the same Cluster as an earlier entry")
		}
		seen[key] = true
		scheme, err := StaticAgentScheme(agent.URL, agent.Namespace)
		if err != nil {
			entry(err.Error())
		}
		if !absoluteClean(agent.TokenFile) {
			entry("tokenFile must be an absolute, clean path")
		}
		switch {
		case scheme == "https" && !absoluteClean(agent.CAFile):
			entry("caFile must be an absolute, clean path for an https url")
		case scheme == "http" && agent.CAFile != "":
			entry("caFile must be empty for an http url (there is no certificate to verify)")
		}
	}
	if !valid {
		return nil
	}
	return agents
}

func absoluteClean(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// StaticAgentScheme validates a static agent URL for an agent in
// namespace and returns its scheme: http or https, no userinfo, path,
// query or fragment, and a host of exactly <service>.<namespace>.svc with
// an optional port. Same-namespace only, like EvidenceAgent.status.endpoint.
func StaticAgentScheme(raw, namespace string) (string, error) {
	invalid := fmt.Errorf("url must be http(s)://<service>.%s.svc[:port] without a path", namespace)
	if raw == "" || len(raw) > 512 {
		return "", invalid
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", invalid
	}
	host, port := parsed.Hostname(), parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port || parsed.Host != host+":"+port {
			return "", invalid
		}
	} else if parsed.Host != host {
		return "", invalid
	}
	labels := strings.Split(host, ".")
	if len(labels) != 3 || labels[1] != namespace || labels[2] != "svc" || len(validation.IsDNS1123Label(labels[0])) != 0 {
		return "", invalid
	}
	return parsed.Scheme, nil
}
