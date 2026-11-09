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

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/authz"
	"github.com/fyannk/pgToolBox/internal/console/sse"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel/fixtures"
)

// FixtureSource serves the deterministic scenarios of viewmodel/fixtures:
// team-a/orders is healthy, team-a/billing is failing over. Sub-views of
// a known Cluster reuse the failing-over scenario under its reference.
type FixtureSource struct{}

func refFor(ns, name string) (viewmodel.ClusterRef, error) {
	for _, row := range fixtures.ClusterList().Items {
		if row.Ref.Namespace == ns && row.Ref.Name == name {
			return row.Ref, nil
		}
	}
	return viewmodel.ClusterRef{}, ErrNotFound
}

// ClusterList implements Source.
func (FixtureSource) ClusterList(context.Context) (viewmodel.ClusterList, error) {
	return fixtures.ClusterList(), nil
}

// ClusterOverview implements Source.
func (FixtureSource) ClusterOverview(_ context.Context, ns, name string) (viewmodel.ClusterOverview, error) {
	for _, ov := range []viewmodel.ClusterOverview{fixtures.HealthyOverview(), fixtures.FailingOverOverview()} {
		if ov.Ref.Namespace == ns && ov.Ref.Name == name {
			return ov, nil
		}
	}
	return viewmodel.ClusterOverview{}, ErrNotFound
}

// Instances implements Source.
func (FixtureSource) Instances(_ context.Context, ns, name string) (viewmodel.InstanceList, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Instances()
	v.Ref = ref
	return v, err
}

// Events implements Source.
func (FixtureSource) Events(_ context.Context, ns, name string) (viewmodel.EventList, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Events()
	v.Ref = ref
	return v, err
}

// Diagnostics implements Source.
func (FixtureSource) Diagnostics(_ context.Context, ns, name string) (viewmodel.Diagnostics, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Diagnostics()
	v.Ref = ref
	return v, err
}

// Triage implements Source.
func (FixtureSource) Triage(_ context.Context, ns, name string) (viewmodel.Triage, error) {
	ref, err := refFor(ns, name)
	v := fixtures.Triage()
	v.Ref = ref
	return v, err
}

// FixtureAuthorizer answers from fixed sets: Clusters in Denied are
// forbidden, in Unchecked undecided, everything else allowed with the
// operator capabilities.
type FixtureAuthorizer struct {
	Denied    map[authz.ObjectRef]bool
	Unchecked map[authz.ObjectRef]bool
	// DeniedLogs denies every pods/log read.
	DeniedLogs bool
	// DeniedAdmin denies listing PgAdmins and EvidenceAgents in these
	// namespaces.
	DeniedAdmin map[string]bool
	// DeniedCreate denies creating these resources (databaseroles).
	DeniedCreate map[string]bool
}

// Allowed implements Authorizer.
func (f FixtureAuthorizer) Allowed(_ context.Context, _ authz.User, at authz.Attributes) authz.Decision {
	ref := authz.ObjectRef{Namespace: at.Namespace, Name: at.Name}
	switch {
	case f.DeniedLogs && at.Subresource == "log":
		return authz.Decision{}
	case f.DeniedAdmin[at.Namespace] && at.Group == authz.PgToolBoxGroup && at.Verb == "list":
		return authz.Decision{}
	case at.Verb == "create" && f.DeniedCreate[at.Resource]:
		return authz.Decision{}
	case f.Unchecked[ref]:
		return authz.Decision{Err: fmt.Errorf("fixture: no answer for %s", at)}
	case f.Denied[ref]:
		return authz.Decision{}
	}
	return authz.Decision{Allowed: true}
}

// VisibleClusters implements Authorizer.
func (f FixtureAuthorizer) VisibleClusters(ctx context.Context, u authz.User, refs []authz.ObjectRef) (visible, undecided []authz.ObjectRef, err error) {
	for _, r := range refs {
		d := f.Allowed(ctx, u, authz.ClusterView(r.Namespace, r.Name))
		switch {
		case d.Err != nil:
			undecided = append(undecided, r)
		case d.Allowed:
			visible = append(visible, r)
		}
	}
	return visible, undecided, nil
}

// ClusterCapabilities implements Authorizer: viewer on billing (logs, no
// operations), operator elsewhere, so both button states have a fixture.
func (f FixtureAuthorizer) ClusterCapabilities(_ context.Context, _ authz.User, ns, name string) authz.Capabilities {
	if name == "billing" {
		return authz.Capabilities{View: true, Logs: true}
	}
	return authz.Capabilities{View: true, Logs: true, Operations: true, Backup: true, Reload: true, Restart: true, Promote: true}
}

// FixturePrincipal is the signed-in user of every fixture.
func FixturePrincipal() *auth.Principal {
	return &auth.Principal{
		User:           authz.User{Name: "oidc:jane@corp.example", Groups: []string{"dba", "system:authenticated"}},
		Provider:       auth.ProviderOIDC,
		SessionID:      "5f0c2a9e8d7b6a5f4e3d2c1b0a998877",
		TokenExpiry:    fixtures.Now.Add(5 * time.Minute),
		Expiry:         fixtures.Now.Add(5 * time.Minute),
		AbsoluteExpiry: fixtures.Now.Add(12 * time.Hour),
		Refreshable:    true,
	}
}

// Fixture is one recorded API response.
type Fixture struct {
	// File is the name under testdata/fixtures.
	File        string `json:"file"`
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	Scenario    string `json:"scenario"`
	// Body is the request body of an unsafe method.
	Body string `json:"body,omitempty"`
}

type fixtureCase struct {
	file, op, path, scenario string
	authz                    FixtureAuthorizer
	// method defaults to GET; body is sent as JSON with the CSRF header.
	method, body string
}

// FixtureFeatures are the console-wide switches of every fixture.
var FixtureFeatures = Features{Logs: true, History: true, Metrics: true, Evidence: true, Operations: true, Digest: true, MCP: true}

// fixtureCases are the scenarios the UI's MSW handlers replay.
func fixtureCases() []fixtureCase {
	partial := FixtureAuthorizer{Unchecked: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "billing"}: true}}
	denied := FixtureAuthorizer{Denied: map[authz.ObjectRef]bool{{Namespace: "team-a", Name: "billing"}: true}}
	base := "/api/" + Version
	cl := base + "/namespaces/team-a/clusters/"
	return []fixtureCase{
		{"getSession.json", OpGetSession, base + "/session", "a refreshable OIDC session five minutes before reauth", FixtureAuthorizer{}, "", ""},
		{"listClusters.json", OpListClusters, base + "/clusters", "two visible clusters, one namespace the console may not list", FixtureAuthorizer{}, "", ""},
		{"listClusters.partial.json", OpListClusters, base + "/clusters", "billing could not be checked and is left out", partial, "", ""},
		{"listClusters.denied.json", OpListClusters, base + "/clusters", "billing is not visible to this user", denied, "", ""},
		{"getCluster.orders.json", OpGetCluster, cl + "orders", "healthy cluster, operator capabilities", FixtureAuthorizer{}, "", ""},
		{"getCluster.billing.json", OpGetCluster, cl + "billing", "failing over, viewer capabilities", FixtureAuthorizer{}, "", ""},
		{"getCluster.forbidden.json", OpGetCluster, cl + "billing", "403: the user may not get this cluster", denied, "", ""},
		{"getCluster.unchecked.json", OpGetCluster, cl + "billing", "503: the API server could not be asked", partial, "", ""},
		{"getCluster.notFound.json", OpGetCluster, cl + "gone", "404: not in the watched namespaces", FixtureAuthorizer{}, "", ""},
		{"listInstances.billing.json", OpListInstances, cl + "billing/instances", "one row per status-read state", FixtureAuthorizer{}, "", ""},
		{"listEvents.billing.json", OpListEvents, cl + "billing/events", "recent warnings", FixtureAuthorizer{}, "", ""},
		{"getDiagnostics.billing.json", OpGetDiagnostics, cl + "billing/diagnostics", "one incident, unavailable checks", FixtureAuthorizer{}, "", ""},
		{"getTriage.billing.json", OpGetTriage, cl + "billing/triage", "one playbook walked", FixtureAuthorizer{}, "", ""},
		{"getVocabulary.json", OpGetVocabulary, base + "/vocabulary", "the health order and source-state mapping", FixtureAuthorizer{}, "", ""},
		{"compareClusters.json", OpCompareClusters, base + "/compare?leftNamespace=team-a&leftName=orders&rightNamespace=team-a&rightName=billing", "orders against billing: memory, backups and synchronous replication differ", FixtureAuthorizer{}, "", ""},
		{"compareClusters.forbidden.json", OpCompareClusters, base + "/compare?leftNamespace=team-a&leftName=orders&rightNamespace=team-a&rightName=billing", "403: the user may not get one of the two", denied, "", ""},
		{"compareClusters.notFound.json", OpCompareClusters, base + "/compare?leftNamespace=team-a&leftName=orders&rightNamespace=team-a&rightName=gone", "404: one is not in the watched namespaces", FixtureAuthorizer{}, "", ""},
		{"getReplication.orders.json", OpGetReplication, cl + "orders/replication", "quorum sync, one catching-up standby, one replica not verifiable", FixtureAuthorizer{}, "", ""},
		{"getBackups.orders.json", OpGetBackups, cl + "orders/backups", "plugin backups: running, completed, failed; evidence live", FixtureAuthorizer{}, "", ""},
		{"getBackups.billing.json", OpGetBackups, cl + "billing/backups", "pgBackRest (Dalibo): a full, differential and incremental chain, a failed incremental, the agent's recovery window and one finding", FixtureAuthorizer{}, "", ""},
		{"getRestoreCheck.orders.json", OpGetRestoreCheck, cl + "orders/backups/restore-check?at=2026-10-29T10:00:00Z", "covered: base backup and contiguous archived WAL, with caveats", FixtureAuthorizer{}, "", ""},
		{"getStorage.orders.json", OpGetStorage, cl + "orders/storage", "one PVC resizing, usage not reported", FixtureAuthorizer{}, "", ""},
		{"getStorage.billing.json", OpGetStorage, cl + "billing/storage", "volumes reported: forecasts toward autoGrow.maxSize, WAL growing while archiving fails, too few points", FixtureAuthorizer{}, "", ""},
		{"getObjects.orders.json", OpGetObjects, cl + "orders/objects", "a failed Database, an unreported managed role, a catalog not readable", FixtureAuthorizer{}, "", ""},
		{"listHistory.orders.json", OpListHistory, cl + "orders/history", "three revisions", FixtureAuthorizer{}, "", ""},
		{"getRevision.orders.json", OpGetRevision, cl + "orders/history/42", "a spec change with its diff", FixtureAuthorizer{}, "", ""},
		{"getRevision.notFound.json", OpGetRevision, cl + "orders/history/9", "404: not a revision of this Cluster", FixtureAuthorizer{}, "", ""},
		{"getTimeline.billing.json", OpGetTimeline, cl + "billing/timeline", "a failover with its findings, a restart, a failed backup and a console gap", FixtureAuthorizer{}, "", ""},
		{"getTimeline.billing.filtered.json", OpGetTimeline, cl + "billing/timeline?kind=finding,primary", "the same window, findings and primary changes only", FixtureAuthorizer{}, "", ""},
		{"getTimeline.badWindow.json", OpGetTimeline, cl + "billing/timeline?from=2026-10-29T12:00:00Z&to=2026-10-29T11:00:00Z", "422: from after to", FixtureAuthorizer{}, "", ""},
		{"listDigests.json", OpListDigests, base + "/digests", "two digests, the newer with billing needing attention", FixtureAuthorizer{}, "", ""},
		{"listDigests.denied.json", OpListDigests, base + "/digests", "billing is not visible: counted out, only the hidden count remains", denied, "", ""},
		{"getDigest.json", OpGetDigest, base + "/digests/" + fixtures.DigestID, "one digest with billing's failover and orders' revision", FixtureAuthorizer{}, "", ""},
		{"getDigest.notFound.json", OpGetDigest, base + "/digests/20250101T0700Z", "404: left the retention", FixtureAuthorizer{}, "", ""},
		{"getMetrics.orders.json", OpGetMetrics, cl + "orders/metrics", "tiles with an unreported instance, a chart with a gap", FixtureAuthorizer{}, "", ""},
		{"getTopology.orders.json", OpGetTopology, cl + "orders/topology", "the wiring diagram with geometry", FixtureAuthorizer{}, "", ""},
		{"getLogTail.orders.json", OpGetLogTail, cl + "orders/instances/orders-1/logs?container=postgres", "two lines", FixtureAuthorizer{}, "", ""},
		{"getLogTail.forbidden.json", OpGetLogTail, cl + "orders/instances/orders-1/logs", "403: the user may not get pods/log", FixtureAuthorizer{DeniedLogs: true}, "", ""},
		{"listOperations.orders.json", OpListOperations, cl + "orders/operations", "operator: every offer allowed, promote roster", FixtureAuthorizer{}, "", ""},
		{"listOperations.billing.json", OpListOperations, cl + "billing/operations", "viewer: nothing allowed", FixtureAuthorizer{}, "", ""},
		{"requestOperation.backup.json", OpRequestOperation, cl + "orders/operations", "202: a backup accepted", FixtureAuthorizer{}, http.MethodPost, `{"kind":"backup","confirmation":"orders"}`},
		{"requestOperation.restoreDrill.json", OpRequestOperation, cl + "orders/operations", "202: a restore drill accepted", FixtureAuthorizer{}, http.MethodPost, `{"kind":"restoreDrill","confirmation":"orders"}`},
		{"requestOperation.badConfirmation.json", OpRequestOperation, cl + "orders/operations", "422: the confirmation does not match", FixtureAuthorizer{}, http.MethodPost, `{"kind":"restart","confirmation":"order"}`},
		{"requestOperation.refused.json", OpRequestOperation, cl + "orders/operations", "409: a precondition is not met", FixtureAuthorizer{}, http.MethodPost, `{"kind":"promote","target":"orders-3","confirmation":"orders-3"}`},
		{"getOperation.json", OpGetOperation, base + "/operations/" + fixtureOperationID(), "a running backup", FixtureAuthorizer{}, "", ""},
		{"getOperation.notFound.json", OpGetOperation, base + "/operations/not-an-id", "404: not an operation id", FixtureAuthorizer{}, "", ""},
		{"createDatabaseRole.json", OpCreateDatabaseRole, cl + "orders/database-roles", "201: a personal role for a named pgAdmin user", FixtureAuthorizer{}, http.MethodPost, `{"role":"alice_ro","pgAdminUser":"alice@example.com","inRoles":["reporting"]}`},
		{"createDatabaseRole.forbidden.json", OpCreateDatabaseRole, cl + "orders/database-roles", "403: the user may not create databaseroles", FixtureAuthorizer{DeniedCreate: map[string]bool{"databaseroles": true}}, http.MethodPost, `{"role":"alice_ro"}`},
		{"createDatabaseRole.exists.json", OpCreateDatabaseRole, cl + "orders/database-roles", "409: a DatabaseRole of that name exists", FixtureAuthorizer{}, http.MethodPost, `{"role":"` + FixtureExistingRole + `"}`},
		{"createDatabaseRole.invalid.json", OpCreateDatabaseRole, cl + "orders/database-roles", "422: a reserved role name", FixtureAuthorizer{}, http.MethodPost, `{"role":"pg_monitor"}`},
		{"createDatabaseRole.unavailable.json", OpCreateDatabaseRole, cl + "billing/database-roles", "501: CloudNativePG before 1.31 cannot generate the password", FixtureAuthorizer{}, http.MethodPost, `{"role":"alice_ro"}`},
		{"getInsight.orders.json", OpGetInsight, cl + "orders/insight", "the pack enabled: wraparound overdue, a lock chain, a regression", FixtureAuthorizer{}, "", ""},
		{"getInsight.billing.json", OpGetInsight, cl + "billing/insight", "the pack not enabled: the spec change to opt in", FixtureAuthorizer{}, "", ""},
		{"getInsightBlocking.orders.json", OpGetInsightBlocking, cl + "orders/insight/blocking?refresh=true", "a lock chain behind an idle-in-transaction session, read on demand", FixtureAuthorizer{}, "", ""},
		{"getFleetStatements.json", OpGetFleetStatements, base + "/insight/statements", "orders compared, billing not opted in", FixtureAuthorizer{}, "", ""},
		{"getFleetStatements.denied.json", OpGetFleetStatements, base + "/insight/statements", "billing is not visible to this user", denied, "", ""},
		{"getAdmin.json", OpGetAdmin, base + "/admin", "a PgAdmin, an EvidenceAgent, a namespace the user may not list", FixtureAuthorizer{DeniedAdmin: map[string]bool{"team-a-staging": true}}, "", ""},
	}
}

// StreamFixture is an example event sequence of GET /api/v1/stream.
type StreamFixture struct {
	Event string `json:"event,omitempty"`
	Data  any    `json:"data,omitempty"`
	// Comment is a keep-alive line.
	Comment string `json:"comment,omitempty"`
}

// Generate renders the committed artifacts: openapi.yaml and the MSW
// fixtures (responses of the real handlers over FixtureSource), keyed by
// path relative to this package. make openapi writes them; a test and CI
// fail when the committed copies differ.
func Generate() (map[string][]byte, error) {
	out := map[string][]byte{}
	_, a := New(Deps{})
	spec, err := a.OpenAPI().YAML()
	if err != nil {
		return nil, fmt.Errorf("rendering OpenAPI: %w", err)
	}
	out["openapi.yaml"] = spec

	var manifest []Fixture
	for _, c := range fixtureCases() {
		h, _ := New(Deps{Source: FixtureSource{}, Authz: c.authz, Logs: FixtureLogs{}, Operator: FixtureOperator{}, DatabaseRoles: FixtureDatabaseRoles{}, Features: FixtureFeatures})
		method := c.method
		if method == "" {
			method = http.MethodGet
		}
		code, body, err := serveFixture(h, method, c.path, c.body, c.body != "")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.file, err)
		}
		out["testdata/fixtures/"+c.file] = body
		manifest = append(manifest, Fixture{File: c.file, OperationID: c.op, Method: method, Path: c.path, Status: code, Scenario: c.scenario, Body: c.body})
	}
	accessManifest, err := generateAccessFixtures(out)
	if err != nil {
		return nil, err
	}
	manifest = append(manifest, accessManifest...)
	assistantManifest, err := generateAssistantFixtures(out)
	if err != nil {
		return nil, err
	}
	manifest = append(manifest, assistantManifest...)
	stream := []StreamFixture{
		{Event: "hello", Data: sse.Hello{ServerTime: fixtures.Now, HeartbeatSeconds: 15, PollMinSeconds: 2, PollMaxSeconds: 10}},
		{Event: "invalidate", Data: sse.Invalidate{Topic: sse.TopicCluster, Namespace: "team-a", Name: "billing"}},
		{Comment: "heartbeat"},
		{Event: "invalidate", Data: sse.Invalidate{Topic: sse.TopicClusters}},
		{Event: "reauth", Data: sse.Reauth{ExpiresAt: fixtures.Now.Add(5 * time.Minute), Refreshable: true}},
		{Event: "resync", Data: sse.Resync{Reason: "events were dropped for this client"}},
	}
	b, err := json.MarshalIndent(stream, "", "  ")
	if err != nil {
		return nil, err
	}
	out["testdata/fixtures/stream.events.json"] = append(b, '\n')
	manifest = append(manifest, Fixture{File: "stream.events.json", OperationID: OpStream, Method: http.MethodGet, Path: "/api/" + Version + "/stream", Status: http.StatusOK, Scenario: "an example event sequence (one JSON object per SSE message)"})
	sort.SliceStable(manifest, func(i, j int) bool { return manifest[i].File < manifest[j].File })
	b, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	out["testdata/fixtures/manifest.json"] = append(b, '\n')
	return out, nil
}

// serveFixture runs one fixture request through h as the fixture
// principal and returns the status and the indented body. It builds the
// request the way net/http/httptest would (host example.com, a
// documentation remote address) without importing it: the generator is
// linked into the console's package, and httptest has no place in the
// console binary. unsafe marks a request with a JSON body and the CSRF
// header.
func serveFixture(h http.Handler, method, path, body string, unsafe bool) (int, []byte, error) {
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	r, err := http.NewRequestWithContext(auth.WithPrincipal(context.Background(), FixturePrincipal()), method, "http://example.com"+path, reqBody)
	if err != nil {
		return 0, nil, err
	}
	r.RequestURI, r.RemoteAddr = path, "192.0.2.1:1234"
	if unsafe {
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set(auth.RequestHeader, "1")
	}
	w := &fixtureRecorder{header: http.Header{}}
	h.ServeHTTP(w, r)
	out, err := indent(w.body.Bytes())
	return w.status(), out, err
}

// fixtureRecorder is the part of httptest.ResponseRecorder the
// generator reads: the status and the body.
type fixtureRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (w *fixtureRecorder) Header() http.Header { return w.header }

func (w *fixtureRecorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *fixtureRecorder) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

// Flush lets a handler that flushes run unchanged.
func (w *fixtureRecorder) Flush() { w.WriteHeader(http.StatusOK) }

func (w *fixtureRecorder) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func indent(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}
