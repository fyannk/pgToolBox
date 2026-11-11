//go:build e2echart

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

package chart

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// passwordToken takes a person's id_token from dex with the password grant:
// the token an agent would be given. The API server trusts dex, so the
// console's MCP endpoint accepts it as a raw API-server token.
func (f *dexFixture) passwordToken(t *testing.T, hosts map[string]string, who string) string {
	t.Helper()
	u := f.Users[who]
	br := newBrowser(t, hosts, f.CA)
	form := url.Values{
		"grant_type": {"password"}, "username": {u.Email}, "password": {u.Password},
		"scope": {"openid email"},
	}
	req, err := http.NewRequestWithContext(ctx(), http.MethodPost, dexIssuer()+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(dexClientID, f.client)
	resp, err := br.client.Do(req)
	if err != nil {
		t.Fatalf("dex password grant for %s: %v", who, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.IDToken == "" {
		t.Fatalf("dex password grant for %s: HTTP %d, error %q (%v)", who, resp.StatusCode, tok.Error, err)
	}
	return tok.IDToken
}

// bearerRoundTripper sends the token with every request.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// mcpSession connects an MCP SDK client to base's /mcp with the token, no
// cookie jar: the endpoint must not need one.
func mcpSession(t *testing.T, hosts map[string]string, base, token string) *mcp.ClientSession {
	t.Helper()
	br := newBrowser(t, hosts, nil)
	hc := br.build(false)
	hc.Jar = nil
	hc.Transport = bearerRoundTripper{token: token, next: hc.Transport}
	c := mcp.NewClient(&mcp.Implementation{Name: "pgtoolbox-e2e", Version: "v0"}, nil)
	cs, err := connectWaitingOut429(func() (*mcp.ClientSession, error) {
		return c.Connect(ctx(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	})
	if err != nil {
		t.Fatalf("MCP connect to %s: %v", base, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// acceptedEverywhere waits until six requests in a row on new connections
// (the Service picks a replica each time) are answered 200, so that every
// replica has accepted token once and no longer counts it against the
// per-address budget for unrecognised tokens. A 429 (that budget, spent
// by an earlier scenario) is waited out; any other status fails.
func acceptedEverywhere(t *testing.T, hosts map[string]string, base, token string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var seen []int
	for ok := 0; ok < 6; {
		if time.Now().After(deadline) {
			t.Fatalf("%s: a token was not accepted by every replica within 2 minutes (statuses %v)", base, seen)
		}
		st := bearerStatus(t, hosts, base, token)
		seen = append(seen, st)
		switch st {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			ok = 0
			time.Sleep(2 * time.Second)
		default:
			t.Fatalf("%s: a valid token was answered %d (statuses %v)", base, st, seen)
		}
	}
}

// mcpClusterNames is list_clusters as namespace/name, sorted.
func mcpClusterNames(t *testing.T, hosts map[string]string, base, token string) []string {
	t.Helper()
	res, err := mcpSession(t, hosts, base, token).CallTool(ctx(), &mcp.CallToolParams{Name: "list_clusters", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_clusters on %s: %v %+v", base, err, res)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct {
			Items []struct {
				Ref struct{ Namespace, Name string } `json:"ref"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("list_clusters on %s: %v: %s", base, err, tailBytes(raw))
	}
	out := make([]string, 0, len(env.Data.Items))
	for _, it := range env.Data.Items {
		out = append(out, it.Ref.Namespace+"/"+it.Ref.Name)
	}
	return sortedStrings(out)
}

// mcpForbidden asserts that cluster_status of a Cluster the token's owner
// may not get is a 403 tool error carrying no data.
func mcpForbidden(t *testing.T, hosts map[string]string, base, token, namespace, name string) {
	t.Helper()
	res, err := mcpSession(t, hosts, base, token).CallTool(ctx(), &mcp.CallToolParams{
		Name: "cluster_status", Arguments: map[string]any{"namespace": namespace, "name": name},
	})
	if err != nil {
		t.Fatalf("cluster_status %s/%s: %v", namespace, name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !res.IsError || !strings.Contains(text.String(), "403") {
		t.Errorf("cluster_status %s/%s on %s: error=%v %q, want a 403 tool error", namespace, name, base, res.IsError, text.String())
	}
}

// mcpChallenge asserts the unauthenticated answer: 401 with the RFC 9728
// challenge, and the metadata it points at naming the endpoint.
func mcpChallenge(t *testing.T, hosts map[string]string, base string) {
	t.Helper()
	br := newBrowser(t, hosts, nil)
	hc := br.build(false)
	hc.Jar = nil
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	req, err := http.NewRequestWithContext(ctx(), http.MethodPost, base+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	meta := base + "/.well-known/oauth-protected-resource/mcp"
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+meta+`"`) {
		t.Fatalf("POST /mcp without a token = %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	r := br.get(meta)
	var m struct {
		Resource string `json:"resource"`
	}
	if r.Status != http.StatusOK || json.Unmarshal(r.Body, &m) != nil || m.Resource != base+"/mcp" {
		t.Errorf("metadata %s = %d %s", meta, r.Status, tailBytes(r.Body))
	}
	// A cookie alone is no credential here.
	sso := newBrowser(t, hosts, nil)
	if r := sso.do(http.MethodPost, base+"/mcp", nil, map[string]string{"Cookie": "pgtoolbox_console_session=forged"}); r.Status != http.StatusUnauthorized {
		t.Errorf("POST /mcp with a cookie and no token = %d", r.Status)
	}
}

// mcpWriteSession is mcpSession with a client that elicits: the person
// answers every confirmation by typing typed (accept), or declines when
// typed is empty; asked counts the questions.
func mcpWriteSession(t *testing.T, hosts map[string]string, base, token, typed string, asked *int) *mcp.ClientSession {
	t.Helper()
	br := newBrowser(t, hosts, nil)
	hc := br.build(false)
	hc.Jar = nil
	hc.Transport = bearerRoundTripper{token: token, next: hc.Transport}
	c := mcp.NewClient(&mcp.Implementation{Name: "pgtoolbox-e2e-write", Version: "v0"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			*asked++
			if typed == "" {
				return &mcp.ElicitResult{Action: "decline"}, nil
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirmation": typed}}, nil
		},
	})
	cs, err := connectWaitingOut429(func() (*mcp.ClientSession, error) {
		return c.Connect(ctx(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	})
	if err != nil {
		t.Fatalf("MCP connect to %s: %v", base, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return text.String()
}

// reloadedAt is orders' cnpg.io/reloadedAt annotation ("" when absent).
func reloadedAt(t *testing.T) string {
	t.Helper()
	return ordersAnnotation(t, "cnpg.io/reloadedAt")
}

func ordersAnnotation(t *testing.T, key string) string {
	t.Helper()
	c, err := newDynamic(t).Resource(clusterGVR).Namespace("team-a").Get(ctx(), "orders", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get orders: %v", err)
	}
	return c.GetAnnotations()[key]
}

// mcpURLSession is an MCP client (bearer token) that declares URL
// elicitation and hands each link to open.
func mcpURLSession(t *testing.T, hosts map[string]string, base, token string, formOnly bool, open func(link string)) *mcp.ClientSession {
	t.Helper()
	br := newBrowser(t, hosts, nil)
	hc := br.build(false)
	hc.Jar = nil
	hc.Transport = bearerRoundTripper{token: token, next: hc.Transport}
	caps := &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}, URL: &mcp.URLElicitationCapabilities{}}
	if formOnly {
		caps.URL = nil
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "pgtoolbox-e2e-url", Version: "v0"}, &mcp.ClientOptions{
		Capabilities: &mcp.ClientCapabilities{Elicitation: caps},
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if req.Params.Mode == "url" {
				open(req.Params.URL)
			}
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	})
	cs, err := connectWaitingOut429(func() (*mcp.ClientSession, error) {
		return c.Connect(ctx(), &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	})
	if err != nil {
		t.Fatalf("MCP connect to %s: %v", base, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// mcpRestartDeclined is the URL confirmation of restart, end to end
// without rolling orders: a client that cannot open a URL is refused; the
// link opens the console's page in dba's own browser session (dex
// sign-in), dba declines there, and the tool does nothing.
func mcpRestartDeclined(t *testing.T, dex *dexFixture, hosts map[string]string, base, dba string) {
	t.Helper()
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	before := ordersAnnotation(t, "kubectl.kubernetes.io/restartedAt")
	res, err := mcpURLSession(t, hosts, base, dba, true, func(string) { t.Error("a form-only client was sent a link") }).
		CallTool(ctx(), &mcp.CallToolParams{Name: "restart", Arguments: orders})
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "cannot open") {
		t.Fatalf("restart from a form-only client: %v %q", err, toolText(res))
	}

	br := newBrowser(t, hosts, dex.CA)
	br.signInSSO(base, dex.Users["dba"])
	pages := 0
	res, err = mcpURLSession(t, hosts, base, dba, false, func(link string) {
		pages++
		if pages > 1 {
			// Asked again: the call reached a replica that had not read
			// the answer yet (the index replicates in about a second). A
			// person takes longer than that to click "continue" again.
			time.Sleep(3 * time.Second)
			return
		}
		page := br.get(link)
		if page.Status != http.StatusOK || !strings.Contains(string(page.Body), "team-a/orders") {
			t.Errorf("the confirmation page answered %d: %s", page.Status, tailBytes(page.Body))
			return
		}
		u, _ := url.Parse(link)
		// The CSRF rules: the same answer without this origin is refused.
		if r := br.do(http.MethodPost, base+"/mcp/confirm", url.Values{"r": {u.Query().Get("r")}, "decision": {"deny"}}, nil); r.Status != http.StatusForbidden {
			t.Errorf("an answer without an Origin: %d", r.Status)
		}
		r := br.do(http.MethodPost, base+"/mcp/confirm", url.Values{"r": {u.Query().Get("r")}, "decision": {"deny"}},
			map[string]string{"Origin": origin(base)})
		if r.Status != http.StatusOK || !strings.Contains(string(r.Body), "Declined") {
			t.Errorf("the decline answered %d: %s", r.Status, tailBytes(r.Body))
		}
	}).CallTool(ctx(), &mcp.CallToolParams{Name: "restart", Arguments: orders})
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "declined the restart") || pages == 0 {
		t.Fatalf("restart declined on the page: %v %q (pages %d)", err, toolText(res), pages)
	}
	if got := ordersAnnotation(t, "kubectl.kubernetes.io/restartedAt"); got != before {
		t.Errorf("a declined restart wrote restartedAt: %q -> %q", before, got)
	}
}

// mcpWriteScenario proves the MCP write path on the reload tool of console
// A (writeTools on): refused without the person's confirmation, refused to
// a person without the right, run and tracked with both; console B
// (writeTools off) lists no write tool.
func mcpWriteScenario(t *testing.T, dex *dexFixture, hosts map[string]string, baseA, baseB string) {
	t.Helper()
	dba, stranger := dex.passwordToken(t, hosts, "dba"), dex.passwordToken(t, hosts, "stranger")
	// The OAuth scenario before this one spends the per-address budget
	// for unrecognised tokens (its revoked grants, asked of every
	// replica); these tokens are new to every replica.
	for _, tok := range []string{dba, stranger} {
		acceptedEverywhere(t, hosts, baseA, tok)
	}
	acceptedEverywhere(t, hosts, baseB, dba)
	orders := map[string]any{"namespace": "team-a", "name": "orders"}
	before := reloadedAt(t)

	// A client that cannot ask the person: refused, nothing written.
	res, err := mcpSession(t, hosts, baseA, dba).CallTool(ctx(), &mcp.CallToolParams{Name: "reload", Arguments: orders})
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "nothing was done") {
		t.Fatalf("reload without elicitation: %v %q", err, toolText(res))
	}
	// The person declines: refused, nothing written.
	asked := 0
	res, err = mcpWriteSession(t, hosts, baseA, dba, "", &asked).CallTool(ctx(), &mcp.CallToolParams{Name: "reload", Arguments: orders})
	if err != nil || !res.IsError || asked != 1 {
		t.Fatalf("reload declined: %v %q, asked %d", err, toolText(res), asked)
	}
	// A person who may not get orders: 403, never asked.
	asked = 0
	res, err = mcpWriteSession(t, hosts, baseA, stranger, "orders", &asked).CallTool(ctx(), &mcp.CallToolParams{Name: "reload", Arguments: orders})
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "403") || asked != 0 {
		t.Fatalf("stranger's reload: %v %q, asked %d", err, toolText(res), asked)
	}
	if got := reloadedAt(t); got != before {
		t.Fatalf("a refused reload wrote cnpg.io/reloadedAt: %q -> %q", before, got)
	}

	// dba, who may patch orders, confirms by typing its name: run and tracked.
	asked = 0
	cs := mcpWriteSession(t, hosts, baseA, dba, "orders", &asked)
	res, err = cs.CallTool(ctx(), &mcp.CallToolParams{Name: "reload", Arguments: orders})
	if err != nil || res.IsError || asked != 1 {
		t.Fatalf("dba's confirmed reload: %v %q, asked %d", err, toolText(res), asked)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Data struct {
			OperationID string `json:"operationId"`
			Kind        string `json:"kind"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Data.OperationID == "" || env.Data.Kind != "reload" {
		t.Fatalf("reload receipt: %s", tailBytes(raw))
	}
	if got := reloadedAt(t); got == "" || got == before {
		t.Errorf("the confirmed reload did not write cnpg.io/reloadedAt (%q)", got)
	}
	st, err := cs.CallTool(ctx(), &mcp.CallToolParams{Name: "operation_status", Arguments: map[string]any{"operationId": env.Data.OperationID}})
	if err != nil || st.IsError || !strings.Contains(toolText(st), "reload") {
		t.Errorf("operation_status of the reload: %v %q", err, toolText(st))
	}

	mcpRestartDeclined(t, dex, hosts, baseA, dba)

	// Console B serves the endpoint without the write tools.
	tools, err := mcpSession(t, hosts, baseB, dba).ListTools(ctx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "reload" || tl.Name == "backup" || tl.Name == "restart" || tl.Name == "promote" {
			t.Errorf("console B (writeTools off) lists %s", tl.Name)
		}
	}
}

// connectWaitingOut429 retries an MCP connect the console answered 429 for
// up to two minutes. Every client of this test reaches the consoles from
// one address (kind's port forward), so the per-address budget for
// unrecognised tokens, which a new access token spends once on each
// replica and which the sign-in form shares, is spent by the scenarios
// together where real people would each have their own; any other error
// is returned at once.
func connectWaitingOut429(connect func() (*mcp.ClientSession, error)) (*mcp.ClientSession, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cs, err := connect()
		if err == nil || !strings.Contains(err.Error(), http.StatusText(http.StatusTooManyRequests)) || time.Now().After(deadline) {
			return cs, err
		}
		time.Sleep(10 * time.Second)
	}
}
