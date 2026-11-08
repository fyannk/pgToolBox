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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maxStreamsPerSession is the console's own bound (internal/console/api
// maxStreamsPerSession); streams beyond it get a resync and close.
const maxStreamsPerSession = 8

type httpOpts struct {
	kube        kubeFlags
	base        string
	users       int
	duration    time.Duration
	mix         string
	sse         int
	concurrency int
	rps         float64
	detailPaths string
	saNamespace string
	sameUser    bool
	jsonOut     string
}

func runHTTP(ctx context.Context, args []string) error {
	var o httpOpts
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	o.kube.register(fs, 50, 100)
	fs.StringVar(&o.base, "base", os.Getenv("LOADTEST_CONSOLE_URL"), "console URL, http://<node ip>:<nodePort> (env LOADTEST_CONSOLE_URL)")
	fs.IntVar(&o.users, "users", 4, "virtual users: one session each, logged in once and paced under the console's 10 logins/min limit")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "how long the load runs (after the logins)")
	fs.StringVar(&o.mix, "mix", "1:4", "list:detail request ratio, e.g. 1:4 is one cluster list for four cluster detail reads")
	fs.IntVar(&o.sse, "sse", 0, "open this many SSE streams (/api/v1/stream), at most 8 per session")
	fs.IntVar(&o.concurrency, "concurrency", 4, "concurrent requests per user (the console admits 4 SelfSubjectAccessReview workers per user)")
	fs.Float64Var(&o.rps, "rps", 0, "total requests/s (open loop); 0 is closed loop, every worker as fast as the console answers")
	fs.StringVar(&o.detailPaths, "detail-paths", ",instances,diagnostics", "comma list of sub-paths a detail request visits (empty entry is the overview)")
	fs.StringVar(&o.saNamespace, "sa-namespace", "loadtest", "namespace of the ServiceAccounts the users sign in as (cluster.sh binds the group system:serviceaccounts:<ns> to the viewer role)")
	fs.BoolVar(&o.sameUser, "same-user", false, "every session signs in as the single ServiceAccount 'loadtest' (one SelfSubjectAccessReview cache) instead of one ServiceAccount per user")
	fs.StringVar(&o.jsonOut, "json", "", "also write the result as JSON to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if o.base == "" {
		return errors.New("--base is required (or source hack/loadtest/.state/env)")
	}
	o.base = strings.TrimRight(o.base, "/")
	if o.users < 0 || o.sse < 0 || o.users+o.sse == 0 || o.concurrency < 1 {
		return errors.New("need --users and/or --sse above zero, and --concurrency at least 1")
	}
	wList, wDetail, err := parseMix(o.mix)
	if err != nil {
		return err
	}
	k, err := o.kube.connect()
	if err != nil {
		return err
	}
	return httpLoad(ctx, k, &o, wList, wDetail)
}

// parseMix reads "list:detail" weights.
func parseMix(s string) (list, detail int, err error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("--mix %q: want list:detail, like 1:4", s)
	}
	list, errA := strconv.Atoi(a)
	detail, errB := strconv.Atoi(b)
	if errA != nil || errB != nil || list < 0 || detail < 0 || list+detail == 0 {
		return 0, 0, fmt.Errorf("--mix %q: two non-negative integers, not both zero", s)
	}
	return list, detail, nil
}

// session is one signed-in virtual user.
type session struct {
	name   string
	client *http.Client
}

type clusterRef struct{ ns, name string }

// loginPacer keeps the logins under the console's limit (10 per minute per
// source address, burst 10): the first few go at once, then one every 6.2 s.
type loginPacer struct {
	burst    int
	interval time.Duration
	n        int
}

func (p *loginPacer) wait(ctx context.Context) error {
	p.n++
	if p.n <= p.burst {
		return nil
	}
	select {
	case <-time.After(p.interval):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type loginStats struct {
	logins, retries int
	took            time.Duration
}

func httpLoad(ctx context.Context, k *kube, o *httpOpts, wList, wDetail int) error {
	transport := &http.Transport{
		MaxIdleConns: 0, MaxIdleConnsPerHost: o.users*o.concurrency + o.sse + 8,
		IdleConnTimeout: 90 * time.Second, DisableCompression: false,
		Proxy: nil, // the console is reached directly on the kind node
	}
	sseSessions := (o.sse + maxStreamsPerSession - 1) / maxStreamsPerSession
	total := o.users + sseSessions

	// Sign in. Logins are the slow part of a big run, so say so.
	pacer := &loginPacer{burst: 8, interval: 6200 * time.Millisecond}
	if total > pacer.burst {
		fmt.Printf("http: %d logins, paced at one per %s after the first %d (console limit: 10/min per address): about %s\n",
			total, pacer.interval, pacer.burst, (time.Duration(total-pacer.burst) * pacer.interval).Round(time.Second))
	}
	var ls loginStats
	loginStart := time.Now()
	sessions := make([]*session, 0, total)
	for i := range total {
		if err := pacer.wait(ctx); err != nil {
			return err
		}
		user := "loadtest"
		if !o.sameUser {
			user = fmt.Sprintf("lt-user-%03d", i)
		}
		s, retries, err := login(ctx, k, o, transport, user)
		ls.retries += retries
		if err != nil {
			return fmt.Errorf("login %d (%s): %w", i, user, err)
		}
		ls.logins++
		sessions = append(sessions, s)
	}
	ls.took = time.Since(loginStart)
	fmt.Printf("http: %d sessions signed in in %s (%d rate-limit retries)\n", ls.logins, ls.took.Round(time.Millisecond), ls.retries)

	var refs []clusterRef
	if o.users > 0 {
		var err error
		if refs, err = listClusters(ctx, sessions[0], o.base); err != nil {
			return fmt.Errorf("first cluster list: %w", err)
		}
		if len(refs) == 0 {
			return errors.New("the cluster list is empty for the virtual users: is the viewer RoleBinding in the watched namespaces, and is populate done?")
		}
		fmt.Printf("http: the list shows %d Clusters\n", len(refs))
	}

	runCtx, cancel := context.WithTimeout(ctx, o.duration)
	defer cancel()
	started := time.Now()

	// Streams first, so the request load runs against open streams.
	sseRes := &sseResult{}
	var sseWG sync.WaitGroup
	for i := range o.sse {
		sseWG.Add(1)
		go func() {
			defer sseWG.Done()
			runStream(runCtx, sessions[o.users+i/maxStreamsPerSession], o.base, sseRes)
		}()
	}

	paths := strings.Split(o.detailPaths, ",")
	var pace <-chan time.Time
	if o.rps > 0 {
		t := time.NewTicker(time.Duration(float64(time.Second) / o.rps))
		defer t.Stop()
		pace = t.C
	}
	var mu sync.Mutex
	merged := map[string]*epStats{}
	var wg sync.WaitGroup
	for u := range o.users {
		for range o.concurrency {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := map[string]*epStats{}
				for runCtx.Err() == nil {
					if pace != nil {
						select {
						case <-pace:
						case <-runCtx.Done():
						}
						if runCtx.Err() != nil {
							break
						}
					}
					label, path := pickRequest(refs, paths, wList, wDetail)
					smp := timedGet(runCtx, sessions[u], o.base+path)
					if runCtx.Err() != nil && smp.status == 0 {
						break // cut off by the end of the run, not a failure
					}
					e := local[label]
					if e == nil {
						e = &epStats{}
						local[label] = e
					}
					e.add(smp)
				}
				mu.Lock()
				defer mu.Unlock()
				for label, e := range local {
					if merged[label] == nil {
						merged[label] = &epStats{}
					}
					merged[label].merge(e)
				}
			}()
		}
	}
	wg.Wait()
	<-runCtx.Done()
	sseWG.Wait()
	elapsed := time.Since(started)

	fmt.Printf("\nhttp: %d users x %d concurrent, mix %s, %s%s\n", o.users, o.concurrency, o.mix, elapsed.Round(time.Millisecond), rpsNote(o.rps))
	if len(merged) > 0 {
		printEndpointTable(os.Stdout, merged, elapsed)
	}
	if o.sse > 0 {
		sseRes.print(os.Stdout, o.sse, elapsed)
	}
	if o.jsonOut != "" {
		return writeHTTPJSON(o, merged, sseRes, ls, elapsed)
	}
	return nil
}

func rpsNote(rps float64) string {
	if rps <= 0 {
		return ", closed loop"
	}
	return fmt.Sprintf(", open loop at %.0f req/s", rps)
}

// pickRequest draws the next request by the mix weights.
func pickRequest(refs []clusterRef, paths []string, wList, wDetail int) (label, path string) {
	if len(refs) == 0 || rand.IntN(wList+wDetail) < wList { //nolint:gosec // load shape, not security
		return "list", "/api/v1/clusters"
	}
	ref := refs[rand.IntN(len(refs))]                      //nolint:gosec // load shape, not security
	sub := strings.TrimSpace(paths[rand.IntN(len(paths))]) //nolint:gosec // load shape, not security
	path = "/api/v1/namespaces/" + ref.ns + "/clusters/" + ref.name
	if sub == "" {
		return "detail", path
	}
	return "detail/" + sub, path + "/" + sub
}

// timedGet reads one URL to the end and times it.
func timedGet(ctx context.Context, s *session, target string) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return sample{}
	}
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := s.client.Do(req)
	if err != nil {
		return sample{d: time.Since(start)}
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	d := time.Since(start)
	if copyErr != nil {
		return sample{d: d}
	}
	return sample{d: d, status: resp.StatusCode}
}

func listClusters(ctx context.Context, s *session, base string) ([]clusterRef, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/clusters", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			Ref struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"ref"`
		} `json:"items"`
		Unchecked int `json:"unchecked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	refs := make([]clusterRef, 0, len(body.Items))
	for _, it := range body.Items {
		refs = append(refs, clusterRef{it.Ref.Namespace, it.Ref.Name})
	}
	if body.Unchecked > 0 {
		fmt.Printf("http: warning: %d Clusters left out of the list (SelfSubjectAccessReview did not answer)\n", body.Unchecked)
	}
	return refs, nil
}

// login signs one ServiceAccount in with a pasted token (POST /auth/token,
// the break-glass path hack/console/test-e2e.sh uses) and returns the
// session. A 429 waits and retries.
func login(ctx context.Context, k *kube, o *httpOpts, transport *http.Transport, user string) (*session, int, error) {
	if err := ensureServiceAccount(ctx, k, o.saNamespace, user); err != nil {
		return nil, 0, err
	}
	tr, err := k.clset.CoreV1().ServiceAccounts(o.saNamespace).CreateToken(ctx, user, &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{ExpirationSeconds: new(int64(3600))},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("token for %s: %w", user, err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, 0, err
	}
	c := &http.Client{
		Transport: transport, Jar: jar, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	form := url.Values{"token": {tr.Status.Token}, "rd": {"/"}}
	retries := 0
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/auth/token", strings.NewReader(form.Encode()))
		if err != nil {
			return nil, retries, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", o.base)
		resp, err := c.Do(req)
		if err != nil {
			return nil, retries, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther:
			return &session{name: user, client: c}, retries, nil
		case resp.StatusCode == http.StatusTooManyRequests && retries < 6:
			retries++
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return nil, retries, ctx.Err()
			}
		default:
			return nil, retries, fmt.Errorf("sign-in answered HTTP %d", resp.StatusCode)
		}
	}
}

func ensureServiceAccount(ctx context.Context, k *kube, ns, name string) error {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	err := k.client.Create(ctx, sa)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("service account %s/%s: %w", ns, name, err)
	}
	return nil
}

// sseResult counts what the open streams received.
type sseResult struct {
	events, bytes, reconnects, connectErrors atomic.Int64
	byType                                   sync.Map // string -> *atomic.Int64
	firstEventNanos                          atomic.Int64
	firstEvents                              atomic.Int64
}

func (r *sseResult) count(eventType string) {
	v, _ := r.byType.LoadOrStore(eventType, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func (r *sseResult) types() map[string]int64 {
	out := map[string]int64{}
	r.byType.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

func (r *sseResult) print(w io.Writer, streams int, elapsed time.Duration) {
	_, _ = fmt.Fprintf(w, "\nsse: %d streams for %s: %d events (%.1f/s total, %.2f/s per stream), %.1f KiB, %d reconnects, %d connect errors\n",
		streams, elapsed.Round(time.Millisecond), r.events.Load(), float64(r.events.Load())/elapsed.Seconds(),
		float64(r.events.Load())/elapsed.Seconds()/float64(streams), float64(r.bytes.Load())/1024, r.reconnects.Load(), r.connectErrors.Load())
	_, _ = fmt.Fprintf(w, "sse: events by type: %v\n", r.types())
	if n := r.firstEvents.Load(); n > 0 {
		_, _ = fmt.Fprintf(w, "sse: mean time to the first event (hello) %sms\n", ms(time.Duration(r.firstEventNanos.Load()/n)))
	}
}

// runStream holds one stream open until ctx ends, reconnecting when it drops
// (the SPA's own behaviour: it polls while the stream is down). A stream that
// is silent for 40 s is dead: the server sends a heartbeat comment every 15 s.
func runStream(ctx context.Context, s *session, base string, res *sseResult) {
	backoff := 500 * time.Millisecond
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			res.reconnects.Add(1)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 5*time.Second)
		}
		if err := readStream(ctx, s, base, res); err != nil && ctx.Err() == nil {
			res.connectErrors.Add(1)
		}
	}
}

func readStream(ctx context.Context, s *session, base string, res *sseResult) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, base+"/api/v1/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	// The session client has a request timeout, which would cut a stream.
	c := &http.Client{Transport: s.client.Transport, Jar: s.client.Jar}
	opened := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream answered HTTP %d", resp.StatusCode)
	}
	var lastByte atomic.Int64
	lastByte.Store(time.Now().UnixNano())
	go func() { // watchdog
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastByte.Load())) > 40*time.Second {
					cancel()
					return
				}
			}
		}
	}()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		lastByte.Store(time.Now().UnixNano())
		res.bytes.Add(int64(len(line)) + 1)
		if t, ok := strings.CutPrefix(line, "event:"); ok {
			t = strings.TrimSpace(t)
			res.events.Add(1)
			res.count(t)
			if first {
				first = false
				res.firstEventNanos.Add(int64(time.Since(opened)))
				res.firstEvents.Add(1)
			}
		}
	}
	if err := sc.Err(); err != nil && sctx.Err() == nil {
		return err
	}
	return nil
}

func writeHTTPJSON(o *httpOpts, stats map[string]*epStats, sse *sseResult, ls loginStats, elapsed time.Duration) error {
	type endpoint struct {
		Count, Errors             int
		P50ms, P95ms, P99ms, MaxM float64
		Statuses                  map[int]int
	}
	out := struct {
		Users, SSE        int
		Mix               string
		ElapsedSeconds    float64
		Logins, Retries   int
		LoginSeconds      float64
		Endpoints         map[string]endpoint
		SSEEvents         int64
		SSEEventsByType   map[string]int64
		SSEReconnects     int64
		SSEConnectErrors  int64
		SSEEventsPerSec   float64
		SSEBytes          int64
		RequestsPerSecond float64
	}{
		Users: o.users, SSE: o.sse, Mix: o.mix, ElapsedSeconds: elapsed.Seconds(),
		Logins: ls.logins, Retries: ls.retries, LoginSeconds: ls.took.Seconds(),
		Endpoints: map[string]endpoint{}, SSEEvents: sse.events.Load(), SSEEventsByType: sse.types(),
		SSEReconnects: sse.reconnects.Load(), SSEConnectErrors: sse.connectErrors.Load(),
		SSEEventsPerSec: float64(sse.events.Load()) / elapsed.Seconds(), SSEBytes: sse.bytes.Load(),
	}
	totalReq := 0
	for name, e := range stats {
		var maxD time.Duration
		for _, d := range e.durations {
			maxD = max(maxD, d)
		}
		msf := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
		out.Endpoints[name] = endpoint{
			Count: e.count(), Errors: e.errors, Statuses: e.statuses,
			P50ms: msf(percentile(e.durations, 50)), P95ms: msf(percentile(e.durations, 95)), P99ms: msf(percentile(e.durations, 99)), MaxM: msf(maxD),
		}
		totalReq += e.count()
	}
	out.RequestsPerSecond = float64(totalReq) / elapsed.Seconds()
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(o.jsonOut, append(b, '\n'), 0o600)
}
