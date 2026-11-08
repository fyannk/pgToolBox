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

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type scrapeOpts struct {
	kube        kubeFlags
	consoleURL  string
	operatorURL string
	// operatorSkipVerify accepts any certificate from the operator: the
	// kind cluster's operator serves controller-runtime's self-signed one.
	operatorSkipVerify bool
	saNamespace        string
	every              time.Duration
	count              int
	topNS              string
}

func runScrape(ctx context.Context, args []string) error {
	var o scrapeOpts
	fs := flag.NewFlagSet("scrape", flag.ContinueOnError)
	o.kube.register(fs, 20, 40)
	fs.StringVar(&o.consoleURL, "console-url", os.Getenv("LOADTEST_CONSOLE_PROBE_URL"), "console probe port URL, http://<node ip>:<nodePort> (env LOADTEST_CONSOLE_PROBE_URL); /metrics is read from it")
	fs.StringVar(&o.operatorURL, "operator-url", os.Getenv("LOADTEST_OPERATOR_METRICS_URL"), "operator metrics URL, https://<node ip>:<nodePort> (env LOADTEST_OPERATOR_METRICS_URL); see --operator-skip-verify")
	fs.BoolVar(&o.operatorSkipVerify, "operator-skip-verify", os.Getenv("LOADTEST_OPERATOR_SKIP_VERIFY") == "1", "accept any certificate from the operator (env LOADTEST_OPERATOR_SKIP_VERIFY=1, which hack/loadtest/cluster.sh sets for its throwaway cluster, whose operator serves a self-signed certificate); the bearer token still decides")
	fs.StringVar(&o.saNamespace, "sa-namespace", "loadtest", "namespace of the ServiceAccount 'loadtest' that may get /metrics")
	fs.DurationVar(&o.every, "every", 0, "repeat at this interval (0: once)")
	fs.IntVar(&o.count, "count", 0, "stop after this many lines when --every is set (0: until interrupted)")
	fs.StringVar(&o.topNS, "top-namespaces", "pgtoolbox-console,pgtoolbox,loadtest", "namespaces whose Pods are read from the metrics API (kubectl top)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := o.kube.connect()
	if err != nil {
		return err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.operatorSkipVerify {
		// Opted in: the operator authenticates the caller by token, and the
		// certificate of a throwaway cluster's operator is self-signed.
		tlsConfig.InsecureSkipVerify = true //nolint:gosec // --operator-skip-verify
	}
	httpc := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: tlsConfig,
		},
	}
	for n := 1; ; n++ {
		tr, err := k.clset.CoreV1().ServiceAccounts(o.saNamespace).CreateToken(ctx, "loadtest", &authv1.TokenRequest{
			Spec: authv1.TokenRequestSpec{ExpirationSeconds: new(int64(600))},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("token for ServiceAccount %s/loadtest: %w", o.saNamespace, err)
		}
		fmt.Println(scrapeLine(ctx, k, httpc, &o, tr.Status.Token))
		if o.every <= 0 || (o.count > 0 && n >= o.count) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(o.every):
		}
	}
}

func scrapeLine(ctx context.Context, k *kube, httpc *http.Client, o *scrapeOpts, token string) string {
	parts := []string{time.Now().UTC().Format("15:04:05")}
	if o.consoleURL == "" {
		parts = append(parts, "console n/a (no URL)")
	} else {
		m, err := fetchMetrics(ctx, httpc, strings.TrimRight(o.consoleURL, "/")+"/metrics", token)
		if err != nil {
			parts = append(parts, "console n/a ("+err.Error()+")")
		} else {
			parts = append(parts, consoleSummary(m))
		}
	}
	if o.operatorURL == "" {
		parts = append(parts, "operator n/a (no URL)")
	} else {
		m, err := fetchMetrics(ctx, httpc, strings.TrimRight(o.operatorURL, "/")+"/metrics", token)
		if err != nil {
			parts = append(parts, "operator n/a ("+err.Error()+")")
		} else {
			parts = append(parts, operatorSummary(m))
		}
	}
	parts = append(parts, topSummary(ctx, k, strings.Split(o.topNS, ",")))
	return strings.Join(parts, " | ")
}

// metricSet is a parsed text exposition: name -> series.
type metricSet map[string][]metricSeries

type metricSeries struct {
	labels map[string]string
	value  float64
}

func fetchMetrics(ctx context.Context, c *http.Client, target, token string) (metricSet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Do(req)
	if err != nil {
		return nil, errors.New("unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseExposition(io.LimitReader(resp.Body, 64<<20))
}

// parseExposition reads the Prometheus text format: enough of it for gauge,
// counter and histogram lines (comments are skipped, timestamps ignored).
func parseExposition(r io.Reader) (metricSet, error) {
	out := metricSet{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, rest, ok := splitSeries(line)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[name] = append(out[name], metricSeries{labels: labels, value: v})
	}
	return out, sc.Err()
}

// splitSeries cuts "name{a="b",c="d"} 1" into its parts.
func splitSeries(line string) (name string, labels map[string]string, rest string, ok bool) {
	brace := strings.IndexByte(line, '{')
	space := strings.IndexByte(line, ' ')
	if brace < 0 || (space >= 0 && space < brace) {
		if space < 0 {
			return "", nil, "", false
		}
		return line[:space], nil, line[space+1:], true
	}
	labels = map[string]string{}
	i := brace + 1
	for i < len(line) && line[i] != '}' {
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 || i+eq+1 >= len(line) || line[i+eq+1] != '"' {
			return "", nil, "", false
		}
		key := strings.Trim(strings.TrimSpace(line[i:i+eq]), ",")
		j := i + eq + 2
		var val strings.Builder
		for j < len(line) && line[j] != '"' {
			if line[j] == '\\' && j+1 < len(line) {
				j++
			}
			val.WriteByte(line[j])
			j++
		}
		if j >= len(line) {
			return "", nil, "", false
		}
		labels[key] = val.String()
		i = j + 1
		for i < len(line) && (line[i] == ',' || line[i] == ' ') {
			i++
		}
	}
	if i >= len(line) {
		return "", nil, "", false
	}
	return line[:brace], labels, strings.TrimSpace(line[i+1:]), true
}

func (m metricSet) sum(name string) (float64, bool) {
	series, ok := m[name]
	if !ok {
		return 0, false
	}
	t := 0.0
	for _, s := range series {
		t += s.value
	}
	return t, true
}

func (m metricSet) bySource(name, label string) string {
	series := m[name]
	if len(series) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(series))
	for _, s := range series {
		parts = append(parts, fmt.Sprintf("%s:%.0f", s.labels[label], s.value))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func mib(v float64) string { return fmt.Sprintf("%.0fMiB", v/(1<<20)) }

func (m metricSet) num(name string) string {
	v, ok := m.sum(name)
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.0f", v)
}

func processSummary(m metricSet) string {
	rss, ok := m.sum("process_resident_memory_bytes")
	rssText := "-"
	if ok {
		rssText = mib(rss)
	}
	heap := "-"
	if v, ok := m.sum("go_memstats_heap_inuse_bytes"); ok {
		heap = mib(v)
	}
	return fmt.Sprintf("rss=%s heap=%s goroutines=%s", rssText, heap, m.num("go_goroutines"))
}

func consoleSummary(m metricSet) string {
	var cached float64
	for _, s := range m["pgtoolbox_console_cached_objects"] {
		cached += s.value
	}
	return fmt.Sprintf("console %s cached=%.0f[%s] sse_clients=%s sse_dropped=%s sar_reviews=%s sar_cache_lookups=%s api_reqs=%s",
		processSummary(m), cached, m.bySource("pgtoolbox_console_cached_objects", "source"),
		m.num("pgtoolbox_console_sse_clients"), m.num("pgtoolbox_console_sse_dropped_events_total"),
		m.num("pgtoolbox_console_sar_reviews_total"), m.num("pgtoolbox_console_sar_cache_lookups_total"),
		m.num("pgtoolbox_console_api_request_duration_seconds_count"))
}

func operatorSummary(m metricSet) string {
	return fmt.Sprintf("operator %s reconciles=%s queue=%s",
		processSummary(m), m.num("controller_runtime_reconcile_total"), m.num("workqueue_depth"))
}

// topSummary is `kubectl top pod` for the harness namespaces, read from the
// metrics API (metrics-server): per Pod, CPU in millicores and memory.
func topSummary(ctx context.Context, k *kube, namespaces []string) string {
	var parts []string
	for _, ns := range namespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		raw, err := k.clset.Discovery().RESTClient().Get().AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods").DoRaw(ctx)
		if err != nil {
			parts = append(parts, ns+"=n/a")
			continue
		}
		var list struct {
			Items []struct {
				Metadata   struct{ Name string } `json:"metadata"`
				Containers []struct {
					Usage map[string]string `json:"usage"`
				} `json:"containers"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			parts = append(parts, ns+"=n/a")
			continue
		}
		for _, it := range list.Items {
			var cpu, mem int64
			for _, c := range it.Containers {
				if q, err := resource.ParseQuantity(c.Usage["cpu"]); err == nil {
					cpu += q.MilliValue()
				}
				if q, err := resource.ParseQuantity(c.Usage["memory"]); err == nil {
					mem += q.Value()
				}
			}
			parts = append(parts, fmt.Sprintf("%s=%dm/%s", shortPodName(it.Metadata.Name), cpu, mib(float64(mem))))
		}
	}
	if len(parts) == 0 {
		return "top n/a"
	}
	return "top[" + strings.Join(parts, " ") + "]"
}

// shortPodName drops the ReplicaSet and Pod hash of a Deployment's Pod.
func shortPodName(name string) string {
	segs := strings.Split(name, "-")
	if len(segs) > 2 {
		return strings.Join(segs[:len(segs)-2], "-")
	}
	return name
}
