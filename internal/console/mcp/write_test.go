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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/fyannk/pgToolBox/internal/console/api"
	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

var writeToolNames = []string{"backup", "reload", "restart", "promote"}

func testSealer(t *testing.T) Sealer {
	t.Helper()
	c, err := auth.NewCodec([][]byte{[]byte("0123456789abcdef0123456789abcdef")}, true, auth.Limits{MaxAge: time.Hour, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testSealerOther(t *testing.T) Sealer {
	t.Helper()
	c, err := auth.NewCodec([][]byte{[]byte("fedcba9876543210fedcba9876543210")}, true, auth.Limits{MaxAge: time.Hour, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sessionCookie names the browser's session in the tests; its value is a
// key of browserUsers.
const sessionCookie = "test-session"

var browserUsers = map[string]string{"dba": "oidc:dba@example.test", "ops": "oidc:ops@example.test"}

// cookieSessions is the sign-in layer of the tests.
type cookieSessions struct{}

func (cookieSessions) CurrentSession(_ http.ResponseWriter, r *http.Request) (*auth.SessionGrant, string, error) {
	key := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		key = c.Value
	}
	user, ok := browserUsers[key]
	if !ok {
		return nil, "", nil
	}
	return &auth.SessionGrant{Provider: "oidc", Principal: &auth.Principal{
		User: authz.User{Name: user}, Provider: "oidc", SessionID: "browser-" + key,
	}}, "", nil
}

// enableWrites turns the write tools on with what they need.
func enableWrites(t *testing.T, o *Options) {
	t.Helper()
	o.WriteTools, o.Sealer, o.Sessions = true, testSealer(t), cookieSessions{}
	o.Confirmations = sharedstate.New(sharedstate.Options{Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return fixtures.Now }})
}

// requested is one operation request the fake service received.
type requested struct {
	user      string
	namespace string
	name      string
	req       viewmodel.OperationRequest
	origin    operations.Origin
	deadline  bool
}

// testRevision is the Cluster the fixture's offers read.
var testRevision = viewmodel.ClusterRevision{UID: "uid-orders", Generation: 3, Phase: "Cluster in healthy state", CurrentPrimary: "orders-1"}

// recordingOperator is the fixture operation service that records what
// reached it: a request that never reaches it was never written.
type recordingOperator struct {
	api.FixtureOperator
	mu sync.Mutex
	// forbidden users are refused by the API server on the write itself
	// (the offer said yes: an RBAC change in between, or an undecided SAR).
	forbidden map[string]bool
	// slow writes wait for their deadline.
	slow bool
	// live, when set, is the Cluster the API server holds at each request
	// (the last one from then on): a write confirmed on another is
	// refused as the service's changedSince does.
	live []viewmodel.ClusterRevision
	got  []requested
}

func (r *recordingOperator) Offers(ctx context.Context, p *auth.Principal, caps authz.Capabilities, ns, name string) (viewmodel.Operations, error) {
	v, err := r.FixtureOperator.Offers(ctx, p, caps, ns, name)
	v.Revision = testRevision
	return v, err
}

func (r *recordingOperator) Request(ctx context.Context, p *auth.Principal, ns, name string, req viewmodel.OperationRequest) (viewmodel.OperationAccepted, error) {
	_, deadline := ctx.Deadline()
	r.mu.Lock()
	r.got = append(r.got, requested{user: p.User.Name, namespace: ns, name: name, req: req, origin: operations.OriginFrom(ctx), deadline: deadline})
	slow := r.slow
	var live *viewmodel.ClusterRevision
	if n := len(r.live); n > 0 {
		live = &r.live[min(len(r.got)-1, n-1)]
	}
	r.mu.Unlock()
	if o := operations.OriginFrom(ctx); live != nil && o.Confirmed != nil && *o.Confirmed != operations.Expected(req.Kind, *live) {
		if o.Observed != nil {
			*o.Observed = operations.Expected(req.Kind, *live)
		}
		return viewmodel.OperationAccepted{}, operations.Rejection(http.StatusConflict, "the Cluster changed since the person confirmed")
	}
	if slow {
		<-ctx.Done()
		return viewmodel.OperationAccepted{}, ctx.Err()
	}
	if r.forbidden[p.User.Name] {
		return viewmodel.OperationAccepted{}, operations.Rejection(http.StatusForbidden, "the Kubernetes API server refused the write")
	}
	if req.Kind == viewmodel.OperationPromote {
		// The fixture refuses every promote; accept a confirmed one here.
		if err := operations.CheckConfirmation(name, req); err != nil {
			return viewmodel.OperationAccepted{}, err
		}
		id := operations.ID{Kind: req.Kind, Namespace: ns, Cluster: name, Instance: req.Target, AcceptedAt: fixtures.Now.Unix()}
		return viewmodel.OperationAccepted{OperationID: id.Encode(), Kind: req.Kind, Instance: req.Target, AcceptedAt: fixtures.Now}, nil
	}
	return r.FixtureOperator.Request(ctx, p, ns, name, req)
}

func (r *recordingOperator) requests() []requested {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// writeHarness is the endpoint with the write tools on.
func writeHarness(t *testing.T, extra ...options) (harness, *recordingOperator) {
	t.Helper()
	op := &recordingOperator{forbidden: map[string]bool{}}
	opts := append([]options{func(o *Options, d *api.Deps) {
		enableWrites(t, o)
		d.Operator = op
	}}, extra...)
	return newHarness(t, opts...), op
}

// page is what the person does on the console's confirmation page: as
// whom (a browserUsers key), approve or deny, and the name typed.
type page struct {
	as, decision, typed string
}

// answer is what the person does with the confirmation in the client
// (action, and for a form the name typed), and on the page for a URL.
type answer struct {
	action string
	typed  string
	page   *page
	// formOnly: the client declares form elicitation only.
	formOnly bool
}

// recorder keeps the questions a client was asked and what the pages
// answered.
type recorder struct {
	mu    sync.Mutex
	asked []*mcp.ElicitParams
	pages []int
}

func (r *recorder) questions() []*mcp.ElicitParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

func (r *recorder) statuses() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.pages)
}

// browse is the person's browser on the confirmation page: it loads the
// page with their session, then posts the answer as a same-origin form.
// It returns the status of the last response and its body.
func (h harness) browse(ctx context.Context, t *testing.T, link string, pg page) (int, string) {
	t.Helper()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cookie := &http.Cookie{Name: sessionCookie, Value: pg.as}
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	get.AddCookie(cookie)
	status, body := do(t, noRedirect, get)
	if status != http.StatusOK {
		return status, body
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"r": {u.Query().Get("r")}, "decision": {pg.decision}, confirmField: {pg.typed}}
	return h.post(ctx, t, form, cookie, h.url)
}

// post sends the page's form with a session cookie and an Origin.
func (h harness) post(ctx context.Context, t *testing.T, form url.Values, cookie *http.Cookie, origin string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url+PathConfirm, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return do(t, http.DefaultClient, req)
}

func do(t *testing.T, c *http.Client, req *http.Request) (int, string) {
	t.Helper()
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// elicitClient connects as token with a client that shows the person the
// question: a form it answers with a.typed, a URL it opens in the
// person's browser (a.page) before answering a.action.
func (h harness) elicitClient(t *testing.T, token string, a answer) (*mcp.ClientSession, *recorder) {
	t.Helper()
	rec := &recorder{}
	caps := &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}
	if a.formOnly {
		caps.URL = nil
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "assistant-under-test", Version: "v0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: caps},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			rec.mu.Lock()
			rec.asked = append(rec.asked, req.Params)
			rec.mu.Unlock()
			if req.Params.Mode == modeURL {
				if a.page != nil {
					status, _ := h.browse(ctx, t, req.Params.URL, *a.page)
					rec.mu.Lock()
					rec.pages = append(rec.pages, status)
					rec.mu.Unlock()
				}
				return &mcp.ElicitResult{Action: a.action}, nil
			}
			res := &mcp.ElicitResult{Action: a.action}
			if a.action == "accept" {
				res.Content = map[string]any{confirmField: a.typed}
			}
			return res, nil
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, rec
}

// auditLines are the log's lines with audit=event.
func auditLines(t *testing.T, log, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["audit"] == event {
			out = append(out, m)
		}
	}
	return out
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		out[tl.Name] = tl
	}
	return out
}

func TestWriteToolsRegistration(t *testing.T) {
	t.Parallel()
	off := toolNames(t, newHarness(t).connect(t, "tok-dba"))
	for _, n := range writeToolNames {
		if _, ok := off[n]; ok {
			t.Errorf("MCP_WRITE_TOOLS off: %s is registered", n)
		}
	}
	// Off, a call to a write tool is an unknown tool, not a refusal.
	if _, err := newHarness(t).connect(t, "tok-dba").CallTool(context.Background(), &mcp.CallToolParams{
		Name: "backup", Arguments: map[string]any{"namespace": "team-a", "name": "orders"},
	}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("backup with the write tools off: %v", err)
	}

	h, _ := writeHarness(t)
	on := toolNames(t, h.connect(t, "tok-dba"))
	hints := map[string]struct{ destructive, idempotent bool }{
		"backup": {false, false}, "reload": {false, true}, "restart": {true, false}, "promote": {true, false},
	}
	for n, want := range hints {
		tl, ok := on[n]
		if !ok {
			t.Errorf("MCP_WRITE_TOOLS on: %s is missing", n)
			continue
		}
		a := tl.Annotations
		if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint != want.destructive ||
			a.IdempotentHint != want.idempotent || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want destructive %v idempotent %v", n, a, want.destructive, want.idempotent)
		}
	}
	if !on["list_clusters"].Annotations.ReadOnlyHint {
		t.Error("the read tools lost their read-only hint")
	}

	// The switch without the operation service registers nothing.
	bare, _ := writeHarness(t, func(_ *Options, d *api.Deps) { d.Operator = nil })
	for n := range toolNames(t, bare.connect(t, "tok-dba")) {
		if slices.Contains(writeToolNames, n) {
			t.Errorf("operations off: %s is registered", n)
		}
	}

	// The switch needs the codec, the confirmation store and the sign-in
	// layer.
	_, _, reads := api.NewWithReads(api.Deps{Source: api.FixtureSource{}, Authz: api.FixtureAuthorizer{}, Operator: api.FixtureOperator{}})
	for name, f := range map[string]func(*Options){
		"no sealer":   func(o *Options) { o.Sealer = nil },
		"no store":    func(o *Options) { o.Confirmations = nil },
		"no sessions": func(o *Options) { o.Sessions = nil },
	} {
		o := Options{Reads: reads, Authenticator: tokens(), PublicURL: "https://console.example"}
		enableWrites(t, &o)
		f(&o)
		if _, err := New(o); err == nil {
			t.Errorf("New accepted the write tools with %s", name)
		}
	}
}

// A grant that may only read is not shown the write tools; one that may
// operate, and a raw API-server token, are.
func TestToolsListFollowsTheScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		scopes []string
		token  string
		want   bool
	}{
		{"a read-only grant", []string{mcpauth.ScopeRead}, "tok-grant", false},
		{"an operate grant", []string{mcpauth.ScopeRead, mcpauth.ScopeOperate}, "tok-grant", true},
		{"a raw API-server token", nil, "tok-dba", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := writeHarness(t, func(o *Options, _ *api.Deps) { o.Authenticator = grantOrRaw(tc.scopes...) })
			names := toolNames(t, h.connect(t, tc.token))
			for _, n := range writeToolNames {
				if _, ok := names[n]; ok != tc.want {
					t.Errorf("%s listed: %v, want %v", n, ok, tc.want)
				}
			}
			if _, ok := names["cluster_status"]; !ok {
				t.Error("the read tools are hidden")
			}
		})
	}
}

func TestWriteToolConfirmed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		tool  string
		args  map[string]any
		typed string
		kind  viewmodel.OperationKind
		mode  string
	}{
		{"backup", map[string]any{"namespace": "team-a", "name": "orders"}, " orders ", viewmodel.OperationBackup, modeForm},
		{"reload", map[string]any{"namespace": "team-a", "name": "orders"}, "orders", viewmodel.OperationReload, modeForm},
		{"restart", map[string]any{"namespace": "team-a", "name": "orders"}, "orders", viewmodel.OperationRestart, modeURL},
		{"promote", map[string]any{"namespace": "team-a", "name": "orders", "target": "orders-2"}, "orders-2", viewmodel.OperationPromote, modeURL},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			t.Parallel()
			h, op := writeHarness(t)
			a := answer{action: "accept", typed: tc.typed}
			if tc.mode == modeURL {
				a.page = &page{as: "dba", decision: "approve", typed: tc.typed}
			}
			cs, rec := h.elicitClient(t, "tok-dba", a)
			res, env := callTool(t, cs, tc.tool, tc.args)
			if res.IsError {
				t.Fatalf("%s: %s", tc.tool, text(res))
			}
			asked := rec.questions()
			if len(asked) != 1 || asked[0].Mode != tc.mode {
				t.Fatalf("the person was asked %d times (%v), want once in %s mode", len(asked), asked, tc.mode)
			}
			q := asked[0]
			want := strings.TrimSpace(tc.typed)
			for _, s := range []string{"team-a/orders", "oidc:dba@example.test", want} {
				if !strings.Contains(q.Message, s) {
					t.Errorf("the question %q does not name %q", q.Message, s)
				}
			}
			if tc.mode == modeURL {
				if !strings.HasPrefix(q.URL, h.url+PathConfirm+"?r=") || q.ElicitationID == "" || q.RequestedSchema != nil {
					t.Errorf("URL question = %+v", q)
				}
				if st := rec.statuses(); len(st) != 1 || st[0] != http.StatusOK {
					t.Errorf("the page answered %v", st)
				}
			}
			data, _ := env["data"].(map[string]any)
			if id, _ := data["operationId"].(string); id == "" {
				t.Errorf("no operationId in %v", env)
			}
			got := op.requests()
			if len(got) != 1 || got[0].req.Kind != tc.kind || got[0].req.Confirmation != want || got[0].user != "oidc:dba@example.test" {
				t.Fatalf("the operation service received %+v", got)
			}
			o := got[0].origin
			if o.Via != operations.ViaMCP || o.Assistant != "assistant-under-test" || o.Grant != "" || o.Confirmation == "" {
				t.Errorf("origin = %+v", o)
			}
			if o.Confirmed == nil || *o.Confirmed != operations.Expected(tc.kind, testRevision) {
				t.Errorf("confirmed on %+v, want %+v", o.Confirmed, operations.Expected(tc.kind, testRevision))
			}
			if !got[0].deadline {
				t.Error("the write ran without a deadline")
			}
			// One mcp.call line per round: the question, then the operation,
			// both with the confirmation's ID; the page's answer has its own.
			calls := auditLines(t, h.log.String(), auth.AuditMCPCall)
			if len(calls) != 2 || calls[0]["outcome"] != outcomeAsked || calls[1]["outcome"] != "ok" || calls[1]["tool"] != tc.tool ||
				calls[0]["confirmation"] != o.Confirmation || calls[1]["confirmation"] != o.Confirmation {
				t.Errorf("mcp.call lines = %v", calls)
			}
			pages := auditLines(t, h.log.String(), auth.AuditMCPConfirm)
			if tc.mode == modeURL && (len(pages) != 1 || pages[0]["outcome"] != "approved" || pages[0]["confirmation"] != o.Confirmation ||
				pages[0]["assistant"] != "assistant-under-test") {
				t.Errorf("mcp.confirm lines = %v", pages)
			}
			if q.URL != "" && strings.Contains(h.log.String(), q.URL) {
				t.Error("the confirmation link is in the log")
			}
		})
	}
}

func TestWriteToolRefusals(t *testing.T) {
	t.Parallel()
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	for _, tc := range []struct {
		name    string
		token   string
		tool    string
		args    map[string]any
		answer  *answer // nil: a client without elicitation
		harness []options
		// want is a substring of the refusal; asked how often the person
		// was asked; reached whether the operation service was called.
		want    string
		asked   int
		reached bool
		outcome string
	}{
		{
			name: "a client that cannot ask the person", token: "tok-dba", tool: "backup", args: orders,
			want: "nothing was done", outcome: outcomeUnconfirmed,
		},
		{
			name: "a client that cannot open a URL: restart", token: "tok-dba", tool: "restart", args: orders,
			answer: &answer{action: "accept", typed: "orders", formOnly: true},
			want:   "confirmed on a page of the console, which this client cannot open", outcome: outcomeUnconfirmed,
		},
		{
			name: "a client that cannot open a URL: promote", token: "tok-dba", tool: "promote",
			args:   map[string]any{"namespace": "team-a", "name": "orders", "target": "orders-2"},
			answer: &answer{action: "accept", typed: "orders-2", formOnly: true},
			want:   "which this client cannot open", outcome: outcomeUnconfirmed,
		},
		{
			name: "the person declines in the client", token: "tok-dba", tool: "backup", args: orders, answer: &answer{action: "decline"},
			want: "did not confirm", asked: 1, outcome: outcomeDeclined,
		},
		{
			name: "the person cancels", token: "tok-dba", tool: "reload", args: orders, answer: &answer{action: "cancel"},
			want: "did not confirm", asked: 1, outcome: outcomeDeclined,
		},
		{
			name: "the person declines on the page", token: "tok-dba", tool: "restart", args: orders,
			answer: &answer{action: "accept", page: &page{as: "dba", decision: "deny"}},
			want:   "declined the restart on the console's confirmation page", asked: 1, outcome: outcomeDeclined,
		},
		{
			name: "the client says accept, nobody approved on the page", token: "tok-dba", tool: "restart", args: orders,
			answer: &answer{action: "accept"},
			want:   "was not approved on the console's confirmation page", asked: maxURLRounds, outcome: outcomeUnconfirmed,
		},
		{
			name: "another person approves on the page", token: "tok-dba", tool: "restart", args: orders,
			answer: &answer{action: "accept", page: &page{as: "ops", decision: "approve", typed: "orders"}},
			want:   "was not approved", asked: maxURLRounds, outcome: outcomeUnconfirmed,
		},
		{
			name: "the page approval with the wrong name", token: "tok-dba", tool: "restart", args: orders,
			answer: &answer{action: "accept", page: &page{as: "dba", decision: "approve", typed: "billing"}},
			want:   "was not approved", asked: maxURLRounds, outcome: outcomeUnconfirmed,
		},
		{
			name: "the typed name is wrong", token: "tok-dba", tool: "backup", args: orders, answer: &answer{action: "accept", typed: "billing"},
			want: "422", asked: 1, reached: true, outcome: "422",
		},
		{
			name: "a Cluster the person may not get", token: "tok-stranger", tool: "backup", args: orders,
			answer: &answer{action: "accept", typed: "orders"}, want: "403", outcome: "403",
		},
		{
			name: "the API server says no to the verbs (SAR)", token: "tok-ops", tool: "backup",
			args: map[string]any{"namespace": "team-a", "name": "billing"}, answer: &answer{action: "accept", typed: "billing"},
			want: "does not let you create backups", outcome: "403",
		},
		{
			name: "the API server refuses the write", token: "tok-dba", tool: "reload", args: orders,
			answer: &answer{action: "accept", typed: "orders"}, want: "403", asked: 1, reached: true, outcome: "403",
			harness: []options{func(_ *Options, d *api.Deps) {
				d.Operator.(*recordingOperator).forbidden["oidc:dba@example.test"] = true
			}},
		},
		{
			name: "promote without a target", token: "tok-dba", tool: "promote", args: orders,
			// The input schema refuses it before the tool runs: no line.
			answer: &answer{action: "accept", typed: "orders"}, want: `missing properties: ["target"]`,
		},
		{
			name: "promote of an ineligible replica", token: "tok-dba", tool: "promote",
			args:   map[string]any{"namespace": "team-a", "name": "orders", "target": "orders-3"},
			answer: &answer{action: "accept", typed: "orders-3"}, want: "orders-3 is fenced", outcome: "409",
		},
		{
			name: "promote of an instance that is not a replica", token: "tok-dba", tool: "promote",
			args:   map[string]any{"namespace": "team-a", "name": "orders", "target": "orders-1"},
			answer: &answer{action: "accept", typed: "orders-1"}, want: "not a replica", outcome: "409",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, op := writeHarness(t, tc.harness...)
			var (
				cs  *mcp.ClientSession
				rec = &recorder{}
			)
			if tc.answer == nil {
				cs = h.connect(t, tc.token)
			} else {
				cs, rec = h.elicitClient(t, tc.token, *tc.answer)
			}
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("%s: %v", tc.tool, err)
			}
			if !res.IsError || !strings.Contains(text(res), tc.want) {
				t.Errorf("%s: error=%v %q, want a refusal with %q", tc.tool, res.IsError, text(res), tc.want)
			}
			if got := len(rec.questions()); got != tc.asked {
				t.Errorf("the person was asked %d times, want %d", got, tc.asked)
			}
			if got := len(op.requests()) > 0; got != tc.reached {
				t.Errorf("the operation service was called: %v, want %v", got, tc.reached)
			}
			calls := auditLines(t, h.log.String(), auth.AuditMCPCall)
			if tc.outcome == "" {
				return
			}
			if len(calls) == 0 || calls[len(calls)-1]["outcome"] != tc.outcome {
				t.Errorf("mcp.call lines = %v, want the last with outcome %q", calls, tc.outcome)
			}
		})
	}
}

// grantAuthenticator authenticates console grants with the given scopes,
// as mcpoauth.Server.Authenticate does.
func grantAuthenticator(scopes ...string) mcpauth.Authenticator {
	return mcpauth.AuthenticatorFunc(func(_ context.Context, token string) (*auth.Principal, error) {
		if token != "tok-grant" {
			return nil, auth.ErrTokenRejected
		}
		return &auth.Principal{
			User: authz.User{Name: "oidc:dba@example.test"}, Provider: auth.GrantProvider, SessionID: "grant-0123456789abcdef0123456789abcdef",
			Token: "upstream", Scopes: scopes, Assistant: "Claude\u202e Code\u200b", AssistantRedirect: "127.0.0.1:33418",
		}, nil
	})
}

// grantOrRaw is the console's chain: the grant, else raw tokens.
func grantOrRaw(scopes ...string) mcpauth.Authenticator {
	g, raw := grantAuthenticator(scopes...), tokens()
	return mcpauth.AuthenticatorFunc(func(ctx context.Context, tok string) (*auth.Principal, error) {
		if tok == "tok-grant" {
			return g.Authenticate(ctx, tok)
		}
		return raw.Authenticate(ctx, tok)
	})
}

// Every tool the endpoint serves that does not declare itself read-only
// is registered as a write, so the dispatcher holds it to mcp:operate: a
// new write tool that forgot the flag fails here (and at registration).
func TestEveryWriteToolNeedsOperate(t *testing.T) {
	t.Parallel()
	h, _ := writeHarness(t)
	writes := 0
	for name, tl := range toolNames(t, h.connect(t, "tok-dba")) {
		if tl.Annotations != nil && tl.Annotations.ReadOnlyHint {
			if h.e.operateTools[name] {
				t.Errorf("%s declares itself read-only but is registered as a write", name)
			}
			continue
		}
		writes++
		if !h.e.operateTools[name] {
			t.Errorf("%s is not read-only (readOnlyHint false) and not registered as a write: mcp:operate is not enforced", name)
		}
	}
	if writes != len(writeToolNames) {
		t.Errorf("%d tools are not read-only, want the %d write tools", writes, len(writeToolNames))
	}

	for name, tc := range map[string]struct {
		tool    *mcp.Tool
		operate bool
	}{
		"a write not marked":       {&mcp.Tool{Name: "w", Annotations: &mcp.ToolAnnotations{}}, false},
		"no annotations":           {&mcp.Tool{Name: "w"}, false},
		"a read marked as a write": {&mcp.Tool{Name: "r", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, true},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("register accepted %s", name)
				}
			}()
			(&Endpoint{}).register(tc.tool, tc.operate)
		}()
	}
}

// The dispatcher refuses a write tool to a read-only grant before its
// handler runs, and audits the refusal.
func TestRequireOperateRunsBeforeTheHandler(t *testing.T) {
	t.Parallel()
	h, _ := writeHarness(t, func(o *Options, _ *api.Deps) { o.Authenticator = grantOrRaw(mcpauth.ScopeRead) })
	var ran atomic.Int32
	tool := &mcp.Tool{Name: "future_write", Annotations: &mcp.ToolAnnotations{}}
	h.e.register(tool, true)
	mcp.AddTool(h.e.server, tool, func(context.Context, *mcp.CallToolRequest, writeArgs) (*mcp.CallToolResult, any, error) {
		ran.Add(1)
		return nil, map[string]string{"done": "yes"}, nil
	})
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	res, _ := callTool(t, h.connect(t, "tok-grant"), "future_write", orders)
	if !res.IsError || !strings.Contains(text(res), "403") || ran.Load() != 0 {
		t.Errorf("a read grant called a write tool: error %v, %q, handler ran %d times", res.IsError, text(res), ran.Load())
	}
	if log := h.log.String(); !strings.Contains(log, `"tool":"future_write"`) || !strings.Contains(log, `"outcome":"403"`) {
		t.Errorf("the refusal is not audited: %s", log)
	}
	if res, _ := callTool(t, h.connect(t, "tok-dba"), "future_write", orders); res.IsError || ran.Load() != 1 {
		t.Errorf("a raw token was refused: %q, handler ran %d times", text(res), ran.Load())
	}
}

func TestWriteToolGrantScope(t *testing.T) {
	t.Parallel()
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	// A grant consented while the write tools were off: read only, even
	// with the switch on now.
	h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.Authenticator = grantAuthenticator(mcpauth.ScopeRead) })
	cs, rec := h.elicitClient(t, "tok-grant", answer{action: "accept", typed: "orders"})
	res, _ := callTool(t, cs, "reload", orders)
	if !res.IsError || !strings.Contains(text(res), "403") || !strings.Contains(text(res), "read-only") {
		t.Errorf("a read grant called reload: %v %q", res.IsError, text(res))
	}
	if len(rec.questions()) != 0 || len(op.requests()) != 0 {
		t.Errorf("a read grant reached the person (%d) or the service (%d)", len(rec.questions()), len(op.requests()))
	}
	// Its read tools still answer.
	if res, _ := callTool(t, cs, "cluster_status", orders); res.IsError {
		t.Errorf("a read grant cannot read: %s", text(res))
	}

	// A grant consented with the write tools on operates, and the audit
	// names its client (without the characters that display other than
	// they read) and grant.
	h, op = writeHarness(t, func(o *Options, _ *api.Deps) {
		o.Authenticator = grantAuthenticator(mcpauth.ScopeRead, mcpauth.ScopeOperate)
	})
	cs, _ = h.elicitClient(t, "tok-grant", answer{action: "accept", typed: "orders"})
	if res, _ := callTool(t, cs, "reload", orders); res.IsError {
		t.Fatalf("an operate grant: %s", text(res))
	}
	got := op.requests()
	if len(got) != 1 || got[0].origin.Via != operations.ViaMCP || got[0].origin.Assistant != "Claude Code" ||
		got[0].origin.Grant != "grant-0123456789abcdef0123456789abcdef" {
		t.Errorf("requests = %+v", got)
	}
}

// stateSniffer adds the bearer and keeps the last request state the client
// sent back, to replay it in a forged request.
type stateSniffer struct {
	token string
	mu    sync.Mutex
	state string
}

func (s *stateSniffer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+s.token)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Params struct {
				RequestState string `json:"requestState"`
			} `json:"params"`
		}
		if json.Unmarshal(b, &m) == nil && m.Params.RequestState != "" {
			s.mu.Lock()
			s.state = m.Params.RequestState
			s.mu.Unlock()
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (s *stateSniffer) last() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// selfAnsweringClient is a client that answers a form by itself, reading
// the name to type from the question: no person involved. It is what a
// form confirmation cannot tell from a person, and why it is single use
// and kept for backup and reload.
func selfAnsweringClient(t *testing.T, h harness, token string) (*mcp.ClientSession, *stateSniffer) {
	t.Helper()
	sn := &stateSniffer{token: token}
	c := mcp.NewClient(&mcp.Implementation{Name: "Claude Code", Version: "v0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{
			Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{},
		}},
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if req.Params.Mode == modeURL {
				// It cannot approve the console's page: it has no session.
				return &mcp.ElicitResult{Action: "accept"}, nil
			}
			props := req.Params.RequestedSchema.(map[string]any)["properties"].(map[string]any)
			title := props[confirmField].(map[string]any)["title"].(string)
			typed := strings.TrimSuffix(strings.TrimPrefix(title, "Type "), " to confirm")
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmField: typed}}, nil
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: sn},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, sn
}

// replay retries a tool call with a captured state and an accept.
func replay(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any, state, typed string) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: tool, Arguments: args, RequestState: state,
		InputResponses: mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmField: typed}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// One confirmation, one operation: a replayed request state finds its
// nonce spent.
func TestConfirmationReplay(t *testing.T) {
	t.Parallel()
	args := map[string]any{"namespace": "team-a", "name": "orders"}
	t.Run("form", func(t *testing.T) {
		t.Parallel()
		h, op := writeHarness(t)
		cs, sn := selfAnsweringClient(t, h, "tok-dba")
		if res, _ := callTool(t, cs, "backup", args); res.IsError {
			t.Fatalf("first: %s", text(res))
		}
		if sn.last() == "" {
			t.Fatal("no state captured")
		}
		for i := range 3 {
			if res := replay(t, cs, "backup", args, sn.last(), "orders"); !res.IsError || !strings.Contains(text(res), "already used") {
				t.Errorf("replay %d: error=%v %q", i, res.IsError, text(res))
			}
		}
		if n := len(op.requests()); n != 1 {
			t.Errorf("%d operations ran on one confirmation", n)
		}
	})
	t.Run("url", func(t *testing.T) {
		t.Parallel()
		h, op := writeHarness(t)
		// The approved round's state, replayed: the approval stands, the
		// nonce is spent.
		cs, sn := h.approvingSniffedClient(t, "tok-dba", "orders")
		if res, _ := callTool(t, cs, "restart", args); res.IsError {
			t.Fatalf("first: %s", text(res))
		}
		for i := range 3 {
			if res := replay(t, cs, "restart", args, sn.last(), ""); !res.IsError || !strings.Contains(text(res), "already used") {
				t.Errorf("replay %d: error=%v %q", i, res.IsError, text(res))
			}
		}
		if n := len(op.requests()); n != 1 {
			t.Errorf("%d operations ran on one confirmation", n)
		}
	})
}

// approvingSniffedClient is elicitClient approving every URL question as
// dba, with its request states captured.
func (h harness) approvingSniffedClient(t *testing.T, token, typed string) (*mcp.ClientSession, *stateSniffer) {
	t.Helper()
	sn := &stateSniffer{token: token}
	c := mcp.NewClient(&mcp.Implementation{Name: "assistant-under-test", Version: "v0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			h.browse(ctx, t, req.Params.URL, page{as: "dba", decision: "approve", typed: typed})
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: sn}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, sn
}

// A state minted for one credential (a raw API-server token) is no answer
// under another credential of the same person (an operate grant of another
// client): that call asks anew.
func TestConfirmationCrossCredential(t *testing.T) {
	t.Parallel()
	h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.Authenticator = grantOrRaw(mcpauth.ScopeRead, mcpauth.ScopeOperate) })
	args := map[string]any{"namespace": "team-a", "name": "orders"}
	// The raw-token client is asked, and gives up before answering.
	sn := &stateSniffer{token: "tok-dba"}
	c := mcp.NewClient(&mcp.Implementation{Name: "raw", Version: "v0"}, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "cancel"}, nil
		},
	})
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.url + "/mcp", HTTPClient: &http.Client{Transport: sn}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if res, _ := callTool(t, cs, "backup", args); !res.IsError {
		t.Fatal("a cancelled confirmation ran")
	}
	if sn.last() == "" {
		t.Fatal("no state captured")
	}
	// The grant's client sends the raw token's state with an answer: no
	// answer for it (a client without elicitation is then refused).
	res := replay(t, h.connect(t, "tok-grant"), "backup", args, sn.last(), "orders")
	if !res.IsError || !strings.Contains(text(res), "nothing was done") {
		t.Errorf("cross-credential replay: error=%v %q", res.IsError, text(res))
	}
	if n := len(op.requests()); n != 0 {
		t.Errorf("a state minted for a raw token ran under a grant: %+v", op.requests())
	}
}

// failingStore is a confirmation store that cannot record.
type failingStore struct {
	usable bool
	err    error
}

func (f failingStore) SpendCode(context.Context, string, string, time.Time) error { return f.err }
func (f failingStore) MarkCode(context.Context, string, string, string) error     { return f.err }
func (failingStore) CodeUsed(string) bool                                         { return false }
func (failingStore) CodeMarked(string, string) bool                               { return false }
func (f failingStore) GrantsUsable() bool                                         { return f.usable }

// When the console cannot record a confirmation, nothing runs, and the
// refusal says so: never as a refusal by policy.
func TestConfirmationStoreFailsClosed(t *testing.T) {
	t.Parallel()
	args := map[string]any{"namespace": "team-a", "name": "orders"}
	for _, tc := range []struct {
		name  string
		store failingStore
		asked int
	}{
		{"the index is not usable", failingStore{usable: false, err: sharedstate.ErrBusy}, 0},
		{"the index is full", failingStore{usable: true, err: sharedstate.ErrBusy}, 1},
		{"the spend cannot be written through", failingStore{usable: true, err: sharedstate.ErrNotRecorded}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.Confirmations = tc.store })
			cs, rec := h.elicitClient(t, "tok-dba", answer{action: "accept", typed: "orders"})
			res, _ := callTool(t, cs, "backup", args)
			if !res.IsError || !strings.Contains(text(res), "503") || !strings.Contains(text(res), "cannot record confirmations") {
				t.Errorf("error=%v %q", res.IsError, text(res))
			}
			if len(rec.questions()) != tc.asked || len(op.requests()) != 0 {
				t.Errorf("asked %d, ran %d", len(rec.questions()), len(op.requests()))
			}
		})
	}
}

// Each person may ask for a few operations a minute.
func TestWriteBudget(t *testing.T) {
	t.Parallel()
	h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.WritesPerMinute = 2 })
	cs, rec := h.elicitClient(t, "tok-dba", answer{action: "accept", typed: "orders"})
	args := map[string]any{"namespace": "team-a", "name": "orders"}
	for i := range 2 {
		if res, _ := callTool(t, cs, "reload", args); res.IsError {
			t.Fatalf("call %d: %s", i, text(res))
		}
	}
	res, _ := callTool(t, cs, "reload", args)
	if !res.IsError || !strings.Contains(text(res), "429") || !strings.Contains(text(res), "at most 2") {
		t.Errorf("third: error=%v %q", res.IsError, text(res))
	}
	if len(rec.questions()) != 2 || len(op.requests()) != 2 {
		t.Errorf("asked %d, ran %d", len(rec.questions()), len(op.requests()))
	}
	// Another person has their own budget.
	other, _ := h.elicitClient(t, "tok-ops", answer{action: "accept", typed: "orders"})
	if res, _ := callTool(t, other, "reload", args); res.IsError {
		t.Errorf("ops: %s", text(res))
	}
}

// A write that outlives its deadline may have been applied: the tool says
// so and not to retry.
func TestWriteTimeout(t *testing.T) {
	t.Parallel()
	h, op := writeHarness(t, func(o *Options, d *api.Deps) {
		o.CallTimeout = 300 * time.Millisecond
		d.Operator.(*recordingOperator).slow = true
	})
	cs, _ := h.elicitClient(t, "tok-dba", answer{action: "accept", typed: "orders"})
	res, _ := callTool(t, cs, "reload", map[string]any{"namespace": "team-a", "name": "orders"})
	for _, s := range []string{"may have been applied", "operation_status", "do not retry"} {
		if !res.IsError || !strings.Contains(text(res), s) {
			t.Errorf("error=%v %q lacks %q", res.IsError, text(res), s)
		}
	}
	if got := op.requests(); len(got) != 1 || !got[0].deadline {
		t.Errorf("requests = %+v", got)
	}
}

// The confirmation page: the person's own session, the right person, a
// same-origin form, one answer.
func TestConfirmPage(t *testing.T) {
	t.Parallel()
	h, op := writeHarness(t)
	// A question the client does not follow: its link is valid.
	cs, rec := h.elicitClient(t, "tok-dba", answer{action: "cancel"})
	if res, _ := callTool(t, cs, "restart", map[string]any{"namespace": "team-a", "name": "orders"}); !res.IsError {
		t.Fatal("a cancelled restart ran")
	}
	asked := rec.questions()
	if len(asked) != 1 || asked[0].Mode != modeURL {
		t.Fatalf("asked %v", asked)
	}
	link := asked[0].URL
	u, _ := url.Parse(link)
	sealed := u.Query().Get("r")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(as, target string) (int, string, http.Header) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		if as != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: as})
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b), res.Header
	}
	dba := &http.Cookie{Name: sessionCookie, Value: "dba"}

	if status, _, hdr := get("", link); status != http.StatusFound || !strings.HasPrefix(hdr.Get("Location"), "/auth/login?rd=") {
		t.Errorf("no session: %d %v", status, hdr)
	}
	if status, body, _ := get("ops", link); status != http.StatusForbidden || !strings.Contains(body, "another person") {
		t.Errorf("another person: %d", status)
	}
	if status, _, _ := get("dba", h.url+PathConfirm+"?r=garbage"); status != http.StatusBadRequest {
		t.Errorf("a forged link: %d", status)
	}
	status, body, hdr := get("dba", link)
	if status != http.StatusOK {
		t.Fatalf("the page: %d %s", status, body)
	}
	for _, s := range []string{
		"oidc:dba@example.test", "assistant-under-test", "(unverified)", "team-a/orders", "Type <strong>orders</strong>",
		"Asked with: a Kubernetes API-server token presented directly (no console grant), at 12:00:00 UTC",
	} {
		if !strings.Contains(body, s) {
			t.Errorf("the page lacks %q", s)
		}
	}
	if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", hdr)
	}

	form := url.Values{"r": {sealed}, "decision": {"approve"}, confirmField: {"orders"}}
	// The CSRF rules: a form from elsewhere, or without an origin, is refused.
	if status, _ := h.post(t.Context(), t, form, dba, "https://evil.example"); status != http.StatusForbidden {
		t.Errorf("a cross-site answer: %d", status)
	}
	if status, _ := h.post(t.Context(), t, form, dba, ""); status != http.StatusForbidden {
		t.Errorf("an answer without an origin: %d", status)
	}
	// Without the person's session, or as another person.
	if status, _ := h.post(t.Context(), t, form, nil, h.url); status != http.StatusForbidden {
		t.Errorf("an answer without a session: %d", status)
	}
	if status, _ := h.post(t.Context(), t, form, &http.Cookie{Name: sessionCookie, Value: "ops"}, h.url); status != http.StatusForbidden {
		t.Errorf("an answer as another person: %d", status)
	}
	// The wrong name is not an answer; the right one is, once.
	wrong := url.Values{"r": {sealed}, "decision": {"approve"}, confirmField: {"billing"}}
	if status, body := h.post(t.Context(), t, wrong, dba, h.url); status != http.StatusUnprocessableEntity || !strings.Contains(body, "That is not orders") {
		t.Errorf("the wrong name: %d", status)
	}
	if status, body := h.post(t.Context(), t, form, dba, h.url); status != http.StatusOK || !strings.Contains(body, "Approved") {
		t.Errorf("the approval: %d %s", status, body)
	}
	if status, _ := h.post(t.Context(), t, form, dba, h.url); status != http.StatusConflict {
		t.Errorf("a second answer: %d", status)
	}
	if status, _, _ := get("dba", link); status != http.StatusConflict {
		t.Errorf("the page once answered: %d", status)
	}
	if len(op.requests()) != 0 {
		t.Error("the page ran the operation itself")
	}
	if lines := auditLines(t, h.log.String(), auth.AuditMCPConfirm); len(lines) != 1 || lines[0]["outcome"] != "approved" {
		t.Errorf("mcp.confirm lines = %v", lines)
	}
}

// TestConfirmationState: an answer counts only with the state the
// console sealed for the same person, credential, tool and target,
// unexpired; any other is no answer, and the person is asked.
func TestConfirmationState(t *testing.T) {
	t.Parallel()
	now := fixtures.Now
	e := &Endpoint{o: Options{Sealer: testSealer(t), Now: func() time.Time { return now }}}
	p := &auth.Principal{User: authz.User{Name: "oidc:dba@example.test"}, Provider: auth.BearerProvider, SessionID: "bearer-1"}
	target := operationTarget{Namespace: "team-a", Name: "orders"}
	seal := func(st confirmState) string {
		s, err := e.o.Sealer.SealPurpose(purposeConfirm, st)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	good := confirmState{
		Nonce: "n1", User: p.User.Name, Session: p.SessionID, Provider: p.Provider, Tool: "restart",
		Namespace: "team-a", Name: "orders", Mode: modeURL, Expiry: now.Add(time.Minute).Unix(),
	}
	with := func(f func(*confirmState)) string { s := good; f(&s); return seal(s) }
	other := testSealerOther(t)
	accept := mcp.InputResponseMap{confirmInput: &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmField: "orders"}}}
	for _, tc := range []struct {
		name      string
		state     string
		responses mcp.InputResponseMap
		want      bool
	}{
		{"sealed for this call", seal(good), accept, true},
		{"no state", "", accept, false},
		{"no answer", seal(good), nil, false},
		{"no nonce", with(func(s *confirmState) { s.Nonce = "" }), accept, false},
		{"another person", with(func(s *confirmState) { s.User = "oidc:ops@example.test" }), accept, false},
		{"another credential", with(func(s *confirmState) { s.Session = "grant-1" }), accept, false},
		{"another provider", with(func(s *confirmState) { s.Provider = auth.GrantProvider }), accept, false},
		{"another tool", with(func(s *confirmState) { s.Tool = "reload" }), accept, false},
		{"another Cluster", with(func(s *confirmState) { s.Name = "billing" }), accept, false},
		{"another instance", with(func(s *confirmState) { s.Instance = "orders-2" }), accept, false},
		{"expired", with(func(s *confirmState) { s.Expiry = now.Unix() }), accept, false},
		{"another console's key", func() string { s, _ := other.SealPurpose(purposeConfirm, good); return s }(), accept, false},
		{"another purpose", func() string { s, _ := e.o.Sealer.SealPurpose(purposeConfirmPage, good); return s }(), accept, false},
		{"forged", "pgc_garbage", accept, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "restart", RequestState: tc.state, InputResponses: tc.responses}}
			if _, _, ok := e.confirmation(req, p, "restart", target); ok != tc.want {
				t.Errorf("answered = %v, want %v", ok, tc.want)
			}
		})
	}
}

func TestCanElicit(t *testing.T) {
	t.Parallel()
	caps := func(v string, elicitation any) mcp.Meta {
		m := mcp.Meta{mcp.MetaKeyProtocolVersion: v, mcp.MetaKeyClientCapabilities: map[string]any{}}
		if elicitation != nil {
			m[mcp.MetaKeyClientCapabilities] = map[string]any{"elicitation": elicitation}
		}
		return m
	}
	empty := map[string]any{}
	for _, tc := range []struct {
		name      string
		meta      mcp.Meta
		form, url bool
	}{
		{"no meta: an older protocol", nil, false, false},
		{"2025-11-25 with elicitation", caps("2025-11-25", map[string]any{"form": empty, "url": empty}), false, false},
		{"2026-07-28 with elicitation {}", caps("2026-07-28", empty), true, false},
		{"2026-07-28 with form", caps("2026-07-28", map[string]any{"form": empty}), true, false},
		{"2026-07-28 with url only", caps("2026-07-28", map[string]any{"url": empty}), false, true},
		{"2026-07-28 with both", caps("2026-07-28", map[string]any{"form": empty, "url": empty}), true, true},
		{"2026-07-28 without elicitation", caps("2026-07-28", nil), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: tc.meta, Name: "restart"}}
			if got := canElicitForm(req); got != tc.form {
				t.Errorf("canElicitForm = %v, want %v", got, tc.form)
			}
			if got := canElicitURL(req); got != tc.url {
				t.Errorf("canElicitURL = %v, want %v", got, tc.url)
			}
		})
	}
}

// A write tool's answers are counted like a read tool's (and traced, by the
// same finishCall): the question, then the done operation. Not parallel: it
// reads a package-wide counter.
func TestWriteToolCallMetrics(t *testing.T) {
	h, _ := writeHarness(t)
	cs, _ := h.elicitClient(t, "tok-dba", answer{action: "accept", typed: "orders"})
	asked := testutil.ToFloat64(calls.WithLabelValues("reload", outcomeAsked))
	ok := testutil.ToFloat64(calls.WithLabelValues("reload", "ok"))
	if res, _ := callTool(t, cs, "reload", map[string]any{"namespace": "team-a", "name": "orders"}); res.IsError {
		t.Fatalf("reload: %s", text(res))
	}
	if got := testutil.ToFloat64(calls.WithLabelValues("reload", outcomeAsked)) - asked; got != 1 {
		t.Errorf("pgtoolbox_console_mcp_calls_total{tool=reload,outcome=%s} grew by %v, want 1", outcomeAsked, got)
	}
	if got := testutil.ToFloat64(calls.WithLabelValues("reload", "ok")) - ok; got != 1 {
		t.Errorf("pgtoolbox_console_mcp_calls_total{tool=reload,outcome=ok} grew by %v, want 1", got)
	}
}

// Past 80 % of the codes table (80 identities each at their late bound),
// a person's confirmations on the page still go through, one after the
// other: a confirmation is one entry of the table, so its approval and
// its run are never refused for want of room, and the person may have
// the late bound of them in flight (round-12 security P3-1: each step was
// an entry, and the second confirmation's approval was refused, leaving
// its answer spent).
func TestPageConfirmationsPastTheLateBound(t *testing.T) {
	t.Parallel()
	store := sharedstate.New(sharedstate.Options{Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return fixtures.Now }})
	codes := 0
	for p := range 80 {
		sum := sha256.Sum256(fmt.Append(nil, "hoarder", p))
		user := hex.EncodeToString(sum[:16])
		for store.UseCode(fmt.Sprint(user, "/", codes), user, fixtures.Now.Add(time.Hour)) == nil {
			codes++
		}
	}
	h, op := writeHarness(t, func(o *Options, _ *api.Deps) { o.Confirmations = store })
	const confirmations = sharedstate.MaxCodesPerUser / 4
	for i := range confirmations {
		cs, rec := h.elicitClient(t, "tok-dba", answer{action: "accept", page: &page{as: "dba", decision: "approve", typed: "orders"}})
		res, _ := callTool(t, cs, "restart", map[string]any{"namespace": "team-a", "name": "orders"})
		if res.IsError {
			t.Fatalf("confirmation %d (page %v): %s", i+1, rec.statuses(), text(res))
		}
	}
	if n := len(op.requests()); n != confirmations {
		t.Errorf("%d operations ran, want %d", n, confirmations)
	}
}
