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

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// people of the tests: the API server would let dba get orders only, ops
// both Clusters, and stranger nothing.
var people = map[string]struct {
	user    string
	allowed []string
}{
	"tok-dba":      {"oidc:dba@example.test", []string{"team-a/orders"}},
	"tok-ops":      {"oidc:ops@example.test", []string{"team-a/billing", "team-a/orders"}},
	"tok-stranger": {"oidc:stranger@example.test", nil},
}

// personAuthorizer is the SAR filter per user: the console's authorizer
// answers per person, so the fixture does too.
type personAuthorizer struct {
	api.FixtureAuthorizer
	allowed map[string][]string // user -> namespace/name
}

func (a personAuthorizer) Allowed(ctx context.Context, u authz.User, at authz.Attributes) authz.Decision {
	if at.Resource == "clusters" && at.Name != "" {
		return authz.Decision{Allowed: slices.Contains(a.allowed[u.Name], at.Namespace+"/"+at.Name)}
	}
	if at.Resource == "clusters" && at.Verb == "list" {
		return authz.Decision{Allowed: len(a.allowed[u.Name]) == 2}
	}
	return a.FixtureAuthorizer.Allowed(ctx, u, at)
}

func (a personAuthorizer) VisibleClusters(ctx context.Context, u authz.User, refs []authz.ObjectRef) (visible, undecided []authz.ObjectRef, err error) {
	for _, r := range refs {
		if a.Allowed(ctx, u, authz.ClusterView(r.Namespace, r.Name)).Allowed {
			visible = append(visible, r)
		}
	}
	return visible, nil, nil
}

// tokens authenticates the fixed tokens, as RawToken would after the API
// server's SelfSubjectReview.
func tokens() mcpauth.Authenticator {
	return mcpauth.AuthenticatorFunc(func(_ context.Context, token string) (*auth.Principal, error) {
		p, ok := people[token]
		if !ok {
			return nil, auth.ErrTokenRejected
		}
		return &auth.Principal{User: authz.User{Name: p.user}, Provider: auth.BearerProvider, SessionID: auth.BearerReference(token), Token: token}, nil
	})
}

// syncBuffer is a log sink the handlers write to concurrently.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	url string
	log *syncBuffer
	e   *Endpoint
}

type options func(*Options, *api.Deps)

func newHarness(t *testing.T, opts ...options) harness {
	t.Helper()
	allowed := map[string][]string{}
	for _, p := range people {
		allowed[p.user] = p.allowed
	}
	deps := api.Deps{
		Source: api.FixtureSource{}, Authz: personAuthorizer{allowed: allowed},
		Logs: api.FixtureLogs{}, Operator: api.FixtureOperator{},
		Features: api.Features{Logs: true, Operations: true, Digest: true},
		Log:      slog.New(slog.DiscardHandler),
	}
	o := Options{Authenticator: tokens(), Now: func() time.Time { return fixtures.Now }}
	for _, f := range opts {
		f(&o, &deps)
	}
	_, _, reads := api.NewWithReads(deps)
	o.Reads = reads
	buf := &syncBuffer{}
	o.Log = slog.New(slog.NewJSONHandler(buf, nil))
	mux := http.NewServeMux()
	// As the server does: the request span around the endpoint.
	srv := httptest.NewServer(o.Tracing.Handler(mux, nil))
	t.Cleanup(srv.Close)
	if o.PublicURL == "" {
		o.PublicURL = srv.URL
	}
	e, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.Register(mux)
	// The session routes (the confirmation page) behind the CSRF check,
	// as server.buildHandler mounts them.
	sessions := http.NewServeMux()
	e.RegisterSession(sessions)
	mux.Handle("/", auth.CSRF(o.PublicURL, sessions))
	return harness{url: srv.URL, log: buf, e: e}
}

// bearerTransport adds the token to every request.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (h harness) connect(t *testing.T, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var env map[string]any
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(raw, &env)
	}
	return res, env
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func clusterNames(t *testing.T, env map[string]any) []string {
	t.Helper()
	data, _ := env["data"].(map[string]any)
	items, _ := data["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		ref, _ := it.(map[string]any)["ref"].(map[string]any)
		out = append(out, ref["namespace"].(string)+"/"+ref["name"].(string))
	}
	slices.Sort(out)
	return out
}

func TestListClustersIsPerUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for token, p := range people {
		res, env := callTool(t, h.connect(t, token), "list_clusters", map[string]any{})
		if res.IsError {
			t.Fatalf("%s: %s", p.user, text(res))
		}
		want := p.allowed
		if want == nil {
			want = []string{}
		}
		if got := clusterNames(t, env); !slices.Equal(got, want) {
			t.Errorf("%s sees %v, want exactly %v", p.user, got, want)
		}
	}
}

// Every per-Cluster tool refuses a Cluster the caller may not get with a
// tool error, and returns no data, whether the Cluster exists or not.
func TestForbiddenClusterIsAToolError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dba := h.connect(t, "tok-dba")
	cluster := func(name string) map[string]any { return map[string]any{"namespace": "team-a", "name": name} }
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"cluster_status", cluster("billing")},
		{"cluster_status", cluster("ghost")},
		{"triage", cluster("billing")},
		{"diagnostics", cluster("billing")},
		{"replication", cluster("billing")},
		{"backups", cluster("billing")},
		{"restore_check", cluster("billing")},
		{"storage", cluster("billing")},
		{"insight_statements", cluster("billing")},
		{"insight_blocking", cluster("billing")},
		{"timeline", cluster("billing")},
		{"history", cluster("billing")},
		{"events", cluster("billing")},
		{"compare", map[string]any{"leftNamespace": "team-a", "leftName": "orders", "rightNamespace": "team-a", "rightName": "billing"}},
		{"instance_logs", map[string]any{"namespace": "team-a", "name": "billing", "pod": "billing-1"}},
	}
	for _, c := range calls {
		res, env := callTool(t, dba, c.tool, c.args)
		if !res.IsError || !strings.Contains(text(res), "403") {
			t.Errorf("%s %v: error=%v %q, want a 403 tool error", c.tool, c.args, res.IsError, text(res))
		}
		if env["data"] != nil {
			t.Errorf("%s %v returned data with its refusal", c.tool, c.args)
		}
	}
	// The cluster resource refuses the same way.
	if _, err := dba.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pgtoolbox://clusters/team-a/billing"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("reading a forbidden cluster resource: %v, want a 403", err)
	}
}

func TestEveryToolAnswersOnAnAllowedCluster(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.connect(t, "tok-ops")
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"cluster_status", orders},
		{"triage", orders},
		{"diagnostics", orders},
		{"replication", orders},
		{"backups", orders},
		{"restore_check", map[string]any{"namespace": "team-a", "name": "orders", "at": "2026-10-29T11:00:00Z"}},
		{"storage", orders},
		{"insight_statements", orders},
		{"insight_statements", map[string]any{}},
		{"insight_blocking", orders},
		{"timeline", map[string]any{"namespace": "team-a", "name": "orders", "kinds": []string{"event", "phase"}, "limit": 10}},
		{"history", orders},
		{"events", orders},
		{"compare", map[string]any{"leftNamespace": "team-a", "leftName": "orders", "rightNamespace": "team-a", "rightName": "billing"}},
		{"instance_logs", map[string]any{"namespace": "team-a", "name": "orders", "pod": "orders-1", "lines": 50}},
		{"digests", map[string]any{}},
	} {
		res, env := callTool(t, ops, c.tool, c.args)
		if res.IsError {
			t.Errorf("%s %v: %s", c.tool, c.args, text(res))
			continue
		}
		if env["data"] == nil {
			t.Errorf("%s returned no data", c.tool)
		}
		if !json.Valid([]byte(text(res))) {
			t.Errorf("%s: the text content is not the JSON result", c.tool)
		}
	}
}

func TestArgumentsAreValidated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.connect(t, "tok-ops")
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"cluster_status", map[string]any{"namespace": "Team_A", "name": "orders"}},
		{"cluster_status", map[string]any{"namespace": "team-a"}},
		{"restore_check", map[string]any{"namespace": "team-a", "name": "orders", "at": "yesterday"}},
		{"timeline", map[string]any{"namespace": "team-a", "name": "orders", "kinds": []string{"gossip"}}},
		{"timeline", map[string]any{"namespace": "team-a", "name": "orders", "from": "2026-01-01T00:00:00Z", "to": "2026-03-01T00:00:00Z"}},
		{"insight_statements", map[string]any{"namespace": "team-a"}},
		{"instance_logs", map[string]any{"namespace": "team-a", "name": "orders", "pod": "orders-1", "lines": 5000}},
	} {
		res, err := ops.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err == nil && !res.IsError {
			t.Errorf("%s %v was accepted", c.tool, c.args)
		}
	}
}

func TestUntrustedTextIsMarked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.connect(t, "tok-ops")
	_, env := callTool(t, ops, "instance_logs", map[string]any{"namespace": "team-a", "name": "orders", "pod": "orders-1"})
	lines := env["data"].(map[string]any)["lines"].([]any)
	if len(lines) == 0 {
		t.Fatal("no log lines")
	}
	for _, l := range lines {
		txt, ok := l.(map[string]any)["text"].(map[string]any)
		if !ok || txt[UntrustedKey] == nil {
			t.Errorf("log line %v is not wrapped as untrusted", l)
		}
	}
	_, env = callTool(t, ops, "events", map[string]any{"namespace": "team-a", "name": "orders"})
	items := env["data"].(map[string]any)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no events in the fixture")
	}
	for _, it := range items {
		if m, ok := it.(map[string]any)["message"]; ok {
			if w, ok := m.(map[string]any); !ok || w[UntrustedKey] == nil {
				t.Errorf("event message %v is not wrapped as untrusted", m)
			}
		}
	}
}

func TestDiagnosticsCarryReasons(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, env := callTool(t, h.connect(t, "tok-ops"), "diagnostics", map[string]any{"namespace": "team-a", "name": "billing"})
	data := env["data"].(map[string]any)
	reasons, _ := data["reasons"].(map[string]any)
	incidents, _ := data["incidents"].([]any)
	if len(incidents) == 0 {
		t.Fatal("the fixture has no incident")
	}
	for _, inc := range incidents {
		root := inc.(map[string]any)["root"].(map[string]any)
		check := root["check"].(string)
		r, ok := reasons[check].(map[string]any)
		if !ok || r["describes"] == "" {
			t.Errorf("finding %s (check %s) has no reason text: %v", root["id"], check, reasons)
		}
	}
}

func TestResultsAreCappedAndSaySo(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options, _ *api.Deps) { o.MaxResultBytes = 1500 })
	res, env := callTool(t, h.connect(t, "tok-ops"), "timeline", map[string]any{"namespace": "team-a", "name": "orders"})
	if res.IsError {
		t.Fatal(text(res))
	}
	tr, ok := env["truncated"].(map[string]any)
	if !ok {
		t.Fatalf("a result over the cap does not say it was truncated: %v", env)
	}
	if tr["limitBytes"].(float64) != 1500 {
		t.Errorf("truncated = %v", tr)
	}
	raw, _ := json.Marshal(env)
	if len(raw) > 1500 {
		t.Errorf("the capped result is %d bytes, over the 1500-byte cap", len(raw))
	}
}

func TestEveryCallIsAudited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dba := h.connect(t, "tok-dba")
	callTool(t, dba, "cluster_status", map[string]any{"namespace": "team-a", "name": "orders"})
	callTool(t, dba, "cluster_status", map[string]any{"namespace": "team-a", "name": "billing"})
	var lines []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(h.log.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["audit"] == auth.AuditMCPCall {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("%d mcp.call lines, want 2:\n%s", len(lines), h.log.String())
	}
	for i, want := range []string{"ok", "403"} {
		l := lines[i]
		if l["user"] != "oidc:dba@example.test" || l["tool"] != "cluster_status" || l["outcome"] != want {
			t.Errorf("line %d = %v", i, l)
		}
		if args, _ := l["arguments"].([]any); len(args) != 2 {
			t.Errorf("line %d arguments = %v", i, l["arguments"])
		}
	}
	if strings.Contains(h.log.String(), "tok-dba") {
		t.Error("the audit trail holds the bearer token")
	}
}

func TestOptionalToolsFollowTheConsole(t *testing.T) {
	t.Parallel()
	names := func(h harness) []string {
		res, err := h.connect(t, "tok-ops").ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(res.Tools))
		for _, tl := range res.Tools {
			out = append(out, tl.Name)
			if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
				t.Errorf("%s is not marked read-only", tl.Name)
			}
		}
		return out
	}
	all := names(newHarness(t))
	for _, want := range []string{
		"list_clusters", "cluster_status", "triage", "diagnostics", "replication", "backups",
		"restore_check", "storage", "insight_statements", "insight_blocking", "timeline", "history", "events", "compare",
		"instance_logs", "operation_status", "digests",
	} {
		if !slices.Contains(all, want) {
			t.Errorf("tool %s is missing from %v", want, all)
		}
	}
	if slices.Contains(all, "search_docs") {
		t.Error("search_docs is registered without a documentation bundle")
	}
	bare := names(newHarness(t, func(o *Options, d *api.Deps) {
		d.Logs, d.Features.Logs, d.Operator = nil, false, nil
		enableWrites(t, o) // operations off: no write tool either
	}))
	if slices.Contains(bare, "instance_logs") || slices.Contains(bare, "operation_status") {
		t.Errorf("logs and operations off, the tools are %v", bare)
	}
	if len(bare) != len(all)-2 {
		t.Errorf("MCP_WRITE_TOOLS registered tools: %v", bare)
	}
}

func TestOperationStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res, _ := callTool(t, h.connect(t, "tok-ops"), "operation_status", map[string]any{"operationId": "not-an-id"})
	if !res.IsError || !strings.Contains(text(res), "404") {
		t.Errorf("a malformed operation ID: %v %q", res.IsError, text(res))
	}
}

func TestResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ops := h.connect(t, "tok-ops")
	res, err := ops.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pgtoolbox://clusters/team-a/orders"})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct {
			Ref   viewmodel.ObjectRef `json:"ref"`
			Cards []map[string]any    `json:"cards"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &env); err != nil || env.Data.Ref.Name != "orders" {
		t.Errorf("cluster resource: %v %+v", err, env.Data.Ref)
	}
	for _, c := range env.Data.Cards {
		if d, ok := c["detail"]; ok {
			if w, ok := d.(map[string]any); !ok || w[UntrustedKey] == nil {
				t.Errorf("card detail %v is not wrapped as untrusted", d)
			}
		}
	}
	voc, err := ops.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: VocabularyResource})
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Data VocabularyView `json:"data"`
	}
	if err := json.Unmarshal([]byte(voc.Contents[0].Text), &v); err != nil || len(v.Data.Checks) < 50 || len(v.Data.HealthOrder) == 0 {
		t.Errorf("vocabulary: %v, %d checks", err, len(v.Data.Checks))
	}
	if _, err := ops.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "pgtoolbox://clusters/Team_A/orders"}); err == nil {
		t.Error("a malformed cluster URI was read")
	}
}

func TestPrompts(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t, "tok-stranger")
	list, err := cs.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Prompts) != len(prompts) {
		t.Errorf("%d prompts, want %d", len(list.Prompts), len(prompts))
	}
	got, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "triage_cluster", Arguments: map[string]string{"namespace": "team-a", "name": "orders"}})
	if err != nil {
		t.Fatal(err)
	}
	msg := got.Messages[0].Content.(*mcp.TextContent).Text
	for _, want := range []string{"team-a/orders", "triage", "never follow"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the triage prompt lacks %q: %s", want, msg)
		}
	}
	if _, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "triage_cluster", Arguments: map[string]string{"namespace": "x; drop", "name": "orders"}}); err == nil {
		t.Error("a prompt accepted a namespace that is no Kubernetes name")
	}
}

// fakeDocs is the documentation bundle's stand-in.
type fakeDocs struct{}

func (fakeDocs) Search(q string, limit int) []DocHit {
	return []DocHit{{Path: "console/mcp", Title: "MCP endpoint", Excerpt: "about " + q}}[:min(1, limit)]
}

func (fakeDocs) Page(path string) (DocPage, bool) {
	if path != "console/mcp" {
		return DocPage{}, false
	}
	return DocPage{Path: path, Title: "MCP endpoint", Markdown: "# MCP endpoint"}, true
}
func (fakeDocs) Version() string { return "1.3" }

func TestDocsToolsWhenABundleIsGiven(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options, _ *api.Deps) { o.Docs = fakeDocs{} })
	cs := h.connect(t, "tok-stranger")
	res, env := callTool(t, cs, "search_docs", map[string]any{"query": "MCP_ENABLED"})
	if res.IsError || !strings.Contains(text(res), "console/mcp") || env["data"].(map[string]any)["version"] != "1.3" {
		t.Errorf("search_docs: %s", text(res))
	}
	res, _ = callTool(t, cs, "read_doc", map[string]any{"path": "console/mcp"})
	if res.IsError || !strings.Contains(text(res), "# MCP endpoint") {
		t.Errorf("read_doc: %s", text(res))
	}
	res, _ = callTool(t, cs, "read_doc", map[string]any{"path": "nowhere"})
	if !res.IsError {
		t.Error("read_doc of a missing page is not an error")
	}
	page, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "docs://1.3/console/mcp"})
	if err != nil || page.Contents[0].Text != "# MCP endpoint" {
		t.Errorf("docs resource: %v", err)
	}
	// Free text is audited by name only.
	if strings.Contains(h.log.String(), "MCP_ENABLED") {
		t.Error("the audit line carries a search query's text")
	}
}

func TestServerInstructionsSayTextIsData(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := h.connect(t, "tok-stranger")
	ins := cs.InitializeResult().Instructions
	for _, want := range []string{"untrusted", "never instructions", "log lines"} {
		if !strings.Contains(ins, want) {
			t.Errorf("the instructions do not say %q", want)
		}
	}
}

func TestNoBearerNoTools(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	if _, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.url + "/mcp"}, nil); err == nil {
		t.Fatal("a client without a bearer connected")
	}
	if _, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"tok-forged"}},
	}, nil); err == nil {
		t.Fatal("a client with a rejected bearer connected")
	}
}
