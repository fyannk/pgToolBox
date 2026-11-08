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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/operations"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// The write tools: backup, reload, restart and
// promote, registered only with MCP_WRITE_TOOLS. Each is the console's own
// operation, reached through the function the REST route calls
// (api.Reads.RequestOperation): the typed confirmation, the live read and
// its preconditions, the write made with the person's own token (the API
// server authorizes and audits it under their name), the console's
// "operation" audit line (via=mcp) and the operation tracker, whose ID the
// tool returns for operation_status. There is no second implementation.
//
// What MCP adds is the person's confirmation, asked for with MCP
// elicitation as an input request of the multi round-trip protocol
// (2026-07-28, SEP-2322), which works over this stateless endpoint: the
// call returns the question, the client shows it, and retries the call
// with the answer and the request state the console sealed. Two kinds of
// confirmation, by what the operation costs:
//
//   - backup and reload ask in a form, in the client: the person types the
//     Cluster name. The client relays the answer, so a client (or a model
//     driving it) could answer by itself: this proves the client asked, not
//     that a person agreed. It is kept for the two operations that
//     interrupt nobody.
//   - restart and promote ask for a URL: a page of this console, which
//     needs the person's own browser session (the CSRF rules of the session
//     mux, a page bound to them, single-use answers kept server side). The
//     tool runs only once that page recorded the approval. A client that
//     cannot open a URL elicitation gets these tools refused.
//
// Every confirmation carries a random nonce, spent once in the shared
// index (sharedstate, as the consent answers are) before the operation
// runs: one confirmation runs one operation. When the index cannot record
// it, the tool refuses (fails closed). The state binds the person, the
// credential (grant or bearer reference, and provider), the tool, the
// target and the Cluster as it was (operations.Expected), for confirmTTL.
//
// A model choosing to call a write tool is not the person agreeing to it:
// there is no confirm argument a model could fill in.
//
// A console grant consented to read-only (or while the write tools were
// off) carries only mcp:read: it is refused here at call time, whatever the
// switch says now, and tools/list does not show it the write tools.
// Sessions and raw API-server tokens act with the person's own rights in
// full; the API server's authorization (SelfSubjectAccessReview, then the
// write) is the gate for them.

const (
	// confirmInput is the input request's key.
	confirmInput = "confirm"
	// confirmField is the elicitation form's one field.
	confirmField = "confirmation"
	// purposeConfirm seals a confirmation round's request state.
	purposeConfirm = "mcp-confirm"
	// purposeConfirmPage seals the confirmation page's request.
	purposeConfirmPage = "mcp-confirm-page"
	// userKeyPurpose keys the user hashes of the spent-value index.
	userKeyPurpose = "mcp-confirm-user"
	// confirmTTL bounds how long the person has to answer.
	confirmTTL = 2 * time.Minute
	// maxURLRounds bounds how often a URL confirmation is asked again
	// while the page has not recorded an answer (the person came back
	// before approving, or another replica has not read it yet).
	maxURLRounds = 3
	// protocolMultiRoundTrip is the first protocol version whose clients
	// answer input requests (SEP-2322).
	protocolMultiRoundTrip = "2026-07-28"
	// DefaultWritesPerMinute is each person's budget of confirmations
	// asked. It is counted per replica (a person can be asked up to this
	// many times a minute by each replica the Service sends calls to) and
	// charged when a confirmation is first asked: the re-asks of one
	// confirmation, its answer and the operation are not charged.
	DefaultWritesPerMinute = 5

	modeForm = "form"
	modeURL  = "url"
)

// Prefixes of the single-use values a confirmation spends in the index.
const (
	// codeSpent: the operation ran (or was refused) on this confirmation.
	codeSpent = "mcp-confirm/"
	// codeAnswered: the confirmation page took its one answer.
	codeAnswered = "mcp-confirm-answer/"
	codeApproved = "mcp-confirm-approve/"
	codeDeclined = "mcp-confirm-decline/"
)

// Outcomes of a write call in the mcp.call line, beside "ok" and the
// status codes.
const (
	outcomeAsked       = "confirmation_requested"
	outcomeDeclined    = "declined"
	outcomeUnconfirmed = "unconfirmed"
)

// Sealer seals the request state of a confirmation round and keys the
// person in the spent-value index: the session codec (auth.Codec), so
// any replica opens what another sealed.
type Sealer interface {
	SealPurpose(purpose string, v any) (string, error)
	OpenPurpose(purpose, s string, v any) error
	KeyedHashes(purpose, value string) []string
}

// ConfirmStore keeps the single-use values of confirmations: the shared
// index of MCP grants (sharedstate.Store), or this replica's memory.
// SpendCode is written through to the shared ConfigMap before it returns
// nil; CodeUsed reads this replica's copy (a replica behind re-asks).
type ConfirmStore interface {
	SpendCode(ctx context.Context, code, userHash string, until time.Time) error
	CodeUsed(code string) bool
	GrantsUsable() bool
}

// writeArgs name the Cluster of backup, reload and restart.
type writeArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Cluster"`
	Name      string `json:"name" jsonschema:"name of the Cluster"`
}

// promoteArgs name the Cluster and the replica to promote.
type promoteArgs struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the Cluster"`
	Name      string `json:"name" jsonschema:"name of the Cluster"`
	Target    string `json:"target" jsonschema:"the replica (instance pod name) to promote, from cluster_status or replication"`
}

// writeSpec describes one write tool.
type writeSpec[In any] struct {
	kind              viewmodel.OperationKind
	name, title, desc string
	// destructive: the operation interrupts clients (restart, promote).
	destructive bool
	// idempotent: calling again with the same arguments changes nothing
	// more (reload: PostgreSQL reloads only changed files).
	idempotent bool
	// mode is how the person confirms: modeForm or modeURL.
	mode   string
	schema func(*jsonschema.Schema)
	args   func(In) operationTarget
}

// operationTarget is what one call acts on.
type operationTarget struct {
	Namespace, Name, Instance string
}

// writeTool is what operate needs of a writeSpec.
type writeTool struct {
	kind       viewmodel.OperationKind
	name, mode string
	// live: the question is asked again on the live Cluster
	// (askAgainLive).
	live bool
}

// confirmState is the sealed request state of a confirmation round: the
// answer counts only for the same person and credential, tool and
// target, within confirmTTL of the question, once (Nonce).
type confirmState struct {
	Nonce     string `json:"x"`
	User      string `json:"u"`
	Session   string `json:"s"`
	Provider  string `json:"p"`
	Tool      string `json:"t"`
	Namespace string `json:"ns"`
	Name      string `json:"n"`
	Instance  string `json:"i,omitempty"`
	Mode      string `json:"m"`
	// Round counts the URL questions asked on this nonce.
	Round int `json:"r,omitempty"`
	// Expect is the Cluster the person confirms on.
	Expect viewmodel.ClusterRevision `json:"e"`
	Expiry int64                     `json:"exp"`
	// Live: Expect is the live Cluster the write was refused on (the
	// question asked again), not the console's cached view of it.
	Live bool `json:"l,omitempty"`
}

// confirmPage is the sealed request of the confirmation page.
type confirmPage struct {
	Nonce       string                  `json:"x"`
	User        string                  `json:"u"`
	Assistant   string                  `json:"a,omitempty"`
	Kind        viewmodel.OperationKind `json:"k"`
	Namespace   string                  `json:"ns"`
	Name        string                  `json:"n"`
	Instance    string                  `json:"i,omitempty"`
	Typed       string                  `json:"y"`
	Title       string                  `json:"tt"`
	Summary     string                  `json:"sm"`
	Consequence string                  `json:"cq"`
	Duration    string                  `json:"d"`
	Expiry      int64                   `json:"exp"`
	// Credential is what asked (credentialOf), Asked when (Unix seconds),
	// From the primary a promote moves the role from.
	Credential string `json:"cr"`
	Asked      int64  `json:"at"`
	From       string `json:"fr,omitempty"`
}

// writeResult is one write call's result and its mcp.call fields.
type writeResult struct {
	res     *mcp.CallToolResult
	v       any
	outcome string
	// confirmation is the confirmation's ID (confirmID), when there is one.
	confirmation string
}

// addWriteTools registers the write tools; New calls it only with
// MCP_WRITE_TOOLS on and the operation service served.
func (e *Endpoint) addWriteTools(s *mcp.Server) {
	cluster := func(in writeArgs) operationTarget { return operationTarget{Namespace: in.Namespace, Name: in.Name} }
	addWriteTool(e, s, writeSpec[writeArgs]{
		kind: viewmodel.OperationBackup, name: "backup", title: "Take an on-demand backup", mode: modeForm,
		desc: "Creates one Backup of the Cluster with the method it backs up with, as you (create backups). The person " +
			"confirms in the client first. Returns an operationId: follow it with operation_status.",
		schema: clusterSchema(), args: cluster,
	})
	addWriteTool(e, s, writeSpec[writeArgs]{
		kind: viewmodel.OperationReload, name: "reload", title: "Reload the configuration", mode: modeForm,
		desc: "Asks every instance to reload its configuration (annotation cnpg.io/reloadedAt), as you (patch clusters). " +
			"CloudNativePG reports no outcome for a reload. The person confirms in the client first.",
		idempotent: true, schema: clusterSchema(), args: cluster,
	})
	addWriteTool(e, s, writeSpec[writeArgs]{
		kind: viewmodel.OperationRestart, name: "restart", title: "Restart the Cluster", mode: modeURL,
		desc: "Rolls every instance, replicas first, then the primary (annotation kubectl.kubernetes.io/restartedAt), as " +
			"you (patch clusters). Clients of the primary are disconnected. The person confirms on a console page the " +
			"client opens (URL elicitation); clients that cannot open one are refused. Returns an operationId: follow " +
			"it with operation_status.",
		destructive: true, schema: clusterSchema(), args: cluster,
	})
	addWriteTool(e, s, writeSpec[promoteArgs]{
		kind: viewmodel.OperationPromote, name: "promote", title: "Promote a replica (switchover)", mode: modeURL,
		desc: "Moves the primary role to the chosen replica, as you (patch clusters/status). Clients of the primary are " +
			"disconnected; the old primary becomes a replica. The person confirms on a console page the client opens " +
			"(URL elicitation); clients that cannot open one are refused. Returns an operationId: follow it with " +
			"operation_status.",
		destructive: true,
		schema: func(sc *jsonschema.Schema) {
			clusterSchema()(sc)
			setString(sc, "target", namePattern, 253)
		},
		args: func(in promoteArgs) operationTarget {
			return operationTarget{Namespace: in.Namespace, Name: in.Name, Instance: in.Target}
		},
	})
}

// addWriteTool registers one write tool.
func addWriteTool[In any](e *Endpoint, s *mcp.Server, spec writeSpec[In]) {
	in, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: the input schema of %s: %v", spec.name, err))
	}
	if spec.schema != nil {
		spec.schema(in)
	}
	tool := &mcp.Tool{
		Name: spec.name, Title: spec.title, Description: spec.desc, InputSchema: in,
		Annotations: &mcp.ToolAnnotations{
			Title: spec.title, ReadOnlyHint: false, IdempotentHint: spec.idempotent,
			DestructiveHint: new(spec.destructive), OpenWorldHint: new(false),
		},
	}
	e.register(tool, true)
	mcp.AddTool(s, tool, func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, any, error) {
		start := e.o.Now()
		ctx, span := e.startToolSpan(ctx, req, spec.name)
		defer span.End()
		p, client, ok := caller(req.Extra)
		if !ok {
			e.finishCall(span, callRecord{tool: spec.name, args: args, outcome: "unauthenticated", took: e.o.Now().Sub(start)})
			return nil, nil, errors.New("unauthenticated: the request carries no principal")
		}
		ctx = auth.WithPrincipal(ctx, p)
		ctx = operations.WithOrigin(ctx, operations.Origin{Via: operations.ViaMCP, Assistant: assistantName(p, req), Grant: grantOf(p)})
		ctx, cancel := context.WithTimeout(ctx, e.o.CallTimeout)
		defer cancel()
		r, err := e.operate(ctx, req, p, writeTool{kind: spec.kind, name: spec.name, mode: spec.mode}, spec.args(args))
		// One audit line, and the call's metrics and span, per answer.
		audit := func(outcome string, size int, truncated bool, result any) {
			var extra []any
			if r.confirmation != "" {
				extra = []any{"confirmation", r.confirmation}
			}
			took := e.o.Now().Sub(start)
			e.auditCall(ctx, client, p, spec.name, false, args, outcome, size, truncated, took, extra...)
			e.finishCall(span, callRecord{
				tool: spec.name, args: args, user: p.User.Name, outcome: outcome,
				size: size, truncated: truncated, took: took, result: result,
			})
		}
		if err != nil {
			failure := asCallError(ctx, err)
			if r.outcome == "" {
				r.outcome = failure.outcome()
			}
			audit(r.outcome, 0, false, nil)
			return nil, nil, failure
		}
		if r.res != nil {
			audit(r.outcome, 0, false, nil)
			return r.res, nil, nil
		}
		// The write is done: its receipt is rendered without the call's
		// deadline, which the write may have outlasted.
		env, size, err := render(context.WithoutCancel(ctx), r.v, nil, e.o.MaxResultBytes)
		if err != nil {
			audit("ok", 0, false, nil)
			return nil, nil, &callError{kind: "error", text: "the operation was accepted but its receipt could not be encoded; see the console's Operations page"}
		}
		audit("ok", size, env.Truncated != nil, env)
		return nil, env, nil
	})
}

// asCallError is err as the client reads it.
func asCallError(ctx context.Context, err error) *callError {
	var ce *callError
	if errors.As(err, &ce) {
		return ce
	}
	return toolFailure(ctx, err)
}

// errReadOnlyGrant refuses a write tool to a grant without mcp:operate.
func errReadOnlyGrant() *callError {
	return &callError{status: http.StatusForbidden, text: "this assistant's grant is read-only (it was given while the write tools were off, " +
		"or for reading only): disconnect it on the console's session page and connect it again to grant read and operate"}
}

// errUnavailable refuses a confirmation the console cannot record: never
// a refusal by policy, and nothing was done.
func errUnavailable() *callError {
	return &callError{status: http.StatusServiceUnavailable, text: "nothing was done: the console cannot record confirmations " +
		"right now (its shared state is not readable or writable, or too many confirmations are pending); retry in a minute"}
}

// operate runs one write call: the grant's scope, the offer (the Cluster
// authorized, the API server's answer for the verbs, the preconditions),
// the person's confirmation, then the operation. It returns either an
// input-required result (the confirmation question), or the accepted
// operation, or an error with its audit outcome when it is not a status.
func (e *Endpoint) operate(ctx context.Context, req *mcp.CallToolRequest, p *auth.Principal, w writeTool, t operationTarget) (writeResult, error) {
	kind, tool := w.kind, w.name
	// requireOperate refused this already; kept as a second check.
	if !mcpauth.HasScope(p, mcpauth.ScopeOperate) {
		return writeResult{}, errReadOnlyGrant()
	}
	if kind == viewmodel.OperationPromote && t.Instance == "" {
		return writeResult{}, errInvalid("promote needs a target: the replica to promote")
	}
	r := e.o.Reads
	offers, err := r.OperationOffers(ctx, t.Namespace, t.Name)
	if err != nil {
		return writeResult{}, err
	}
	offer, err := usableOffer(offers, kind, t)
	if err != nil {
		return writeResult{}, err
	}
	st, answer, answered := e.confirmation(req, p, tool, t)
	if !answered {
		return e.ask(req, p, w, offer, t, operations.Expected(kind, offers.Revision))
	}
	id := confirmID(st.Nonce)
	if answer.Action != "accept" {
		return writeResult{outcome: outcomeDeclined, confirmation: id},
			&callError{kind: outcomeDeclined, text: "nothing was done: the person did not confirm the " + string(kind)}
	}
	typed, _ := answer.Content[confirmField].(string)
	if st.Mode == modeURL {
		store := e.o.Confirmations
		switch {
		case store.CodeUsed(codeDeclined + st.Nonce):
			return writeResult{outcome: outcomeDeclined, confirmation: id},
				&callError{kind: outcomeDeclined, text: "nothing was done: the person declined the " + string(kind) + " on the console's confirmation page"}
		case store.CodeUsed(codeApproved + st.Nonce):
			typed = typedFor(kind, t)
		case st.Round+1 < maxURLRounds && e.o.Now().Unix() < st.Expiry:
			st.Round++
			return e.askURL(p, assistantName(p, req), st, offer, t)
		default:
			return writeResult{outcome: outcomeUnconfirmed, confirmation: id}, &callError{kind: outcomeUnconfirmed, text: "nothing was done: " +
				"the " + string(kind) + " was not approved on the console's confirmation page in time; call the tool again to ask anew"}
		}
	}
	// One confirmation, one operation: the nonce is spent, written through
	// to the shared ConfigMap, before the write, so a replayed state finds
	// it spent on any replica, even one whose watch is behind.
	if !e.o.Confirmations.GrantsUsable() {
		return writeResult{confirmation: id}, errUnavailable()
	}
	switch err := e.o.Confirmations.SpendCode(ctx, codeSpent+st.Nonce, e.userKey(p.User.Name), time.Unix(st.Expiry, 0).Add(time.Minute)); {
	case errors.Is(err, sharedstate.ErrSpent):
		return writeResult{confirmation: id}, &callError{status: http.StatusConflict, text: "nothing was done: this confirmation was " +
			"already used; one confirmation runs one operation. Call the tool again to ask the person anew"}
	case err != nil:
		return writeResult{confirmation: id}, errUnavailable()
	}
	o := operations.OriginFrom(ctx)
	var observed viewmodel.ClusterRevision
	o.Confirmation, o.Confirmed, o.Observed = id, &st.Expect, &observed
	// The write gets its own deadline, detached from the call's: a client
	// that gives up must not cut a write the API server is completing.
	wctx, cancel := context.WithTimeout(operations.WithOrigin(context.WithoutCancel(ctx), o), e.o.CallTimeout)
	defer cancel()
	accepted, err := r.RequestOperation(wctx, t.Namespace, t.Name, viewmodel.OperationRequest{
		Kind: kind, Target: t.Instance, Confirmation: strings.TrimSpace(typed),
	})
	if err != nil {
		if observed.UID != "" && !st.Live {
			// The person confirmed on the console's cached view of the
			// Cluster, and the live one differs: changed since, or the
			// cache was behind. Nothing was done; they are asked again
			// on the live state, once (a second refusal is the 409).
			return e.askAgainLive(req, p, w, offer, t, observed)
		}
		return writeResult{confirmation: id}, writeFailure(wctx, err)
	}
	return writeResult{v: accepted, confirmation: id}, nil
}

// askAgainLive asks the person again, on the live Cluster the write was
// refused on.
func (e *Endpoint) askAgainLive(req *mcp.CallToolRequest, p *auth.Principal, w writeTool, offer viewmodel.OperationOffer, t operationTarget, live viewmodel.ClusterRevision) (writeResult, error) {
	w.live = true
	r, err := e.ask(req, p, w, offer, t, live)
	if err != nil || r.res == nil {
		return r, err
	}
	if q, ok := r.res.InputRequests[confirmInput].(*mcp.ElicitParams); ok {
		q.Message = "Nothing was done: the Cluster is not as it was when you were asked (it changed, or the console's " +
			"view of it was behind). Here is the question again, on the Cluster as it is now.\n\n" + q.Message
	}
	return r, nil
}

// writeFailure is a failed write as the client reads it: a write that ran
// out of time may still have been applied, and must not be repeated
// blindly.
func writeFailure(ctx context.Context, err error) *callError {
	var ce *callError
	if errors.As(err, &ce) {
		return ce
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return &callError{kind: "timeout", text: "the write took longer than the console allows and may have been applied: " +
			"check operation_status (or the console's Operations page) before anything else; do not retry it"}
	}
	return toolFailure(ctx, err)
}

// ask is the first question of a confirmation, if the person's budget and
// the client allow it.
func (e *Endpoint) ask(req *mcp.CallToolRequest, p *auth.Principal, w writeTool, offer viewmodel.OperationOffer, t operationTarget, expect viewmodel.ClusterRevision) (writeResult, error) {
	mode := w.mode
	if mode == modeURL && !canElicitURL(req) {
		return writeResult{outcome: outcomeUnconfirmed}, &callError{kind: outcomeUnconfirmed, text: "nothing was done: " +
			w.name + " is confirmed on a page of the console, which this client cannot open (MCP URL elicitation, " +
			"protocol " + protocolMultiRoundTrip + " or later). Use a client that supports it, or the console's Operations page"}
	}
	if mode == modeForm && !canElicitForm(req) {
		return writeResult{outcome: outcomeUnconfirmed}, &callError{kind: outcomeUnconfirmed, text: "nothing was done: this tool needs the " +
			"person's confirmation, and this client cannot ask for it (MCP form elicitation, protocol " + protocolMultiRoundTrip +
			" or later). Use a client that supports it, or the console's Operations page"}
	}
	if !e.o.Confirmations.GrantsUsable() {
		return writeResult{}, errUnavailable()
	}
	if !e.writes.Allow(e.userKey(p.User.Name)) {
		return writeResult{}, &callError{status: http.StatusTooManyRequests, text: fmt.Sprintf("nothing was done: at most %d "+
			"operations a minute may be asked for, per person; wait a minute", e.o.WritesPerMinute)}
	}
	nonce, err := newNonce()
	if err != nil {
		return writeResult{}, &callError{kind: "error", text: "the confirmation could not be prepared; retry"}
	}
	st := confirmState{
		Nonce: nonce, User: p.User.Name, Session: p.SessionID, Provider: p.Provider,
		Tool: w.name, Namespace: t.Namespace, Name: t.Name, Instance: t.Instance, Mode: mode,
		Expect: expect, Expiry: e.o.Now().Add(confirmTTL).Unix(), Live: w.live,
	}
	if mode == modeURL {
		return e.askURL(p, assistantName(p, req), st, offer, t)
	}
	return e.askForm(p, st, offer, t)
}

// canElicitForm reports whether the client answers a form elicitation
// embedded in a tool result (SEP-2322): a 2026-07-28 client that
// declared elicitation. With neither form nor url declared, form is
// assumed, as the specification says.
func canElicitForm(req *mcp.CallToolRequest) bool {
	if req.ProtocolVersion() < protocolMultiRoundTrip {
		return false
	}
	caps := req.ClientCapabilities()
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

// canElicitURL reports whether the client opens a URL elicitation
// embedded in a tool result: a 2026-07-28 client that declared it.
func canElicitURL(req *mcp.CallToolRequest) bool {
	if req.ProtocolVersion() < protocolMultiRoundTrip {
		return false
	}
	caps := req.ClientCapabilities()
	return caps != nil && caps.Elicitation != nil && caps.Elicitation.URL != nil
}

// confirmation is the person's answer carried by a retried call: present
// only with the request state this console sealed for the same person and
// credential, tool and target, unexpired. Anything else counts as not
// answered, and the person is asked (again).
func (e *Endpoint) confirmation(req *mcp.CallToolRequest, p *auth.Principal, tool string, t operationTarget) (confirmState, *mcp.ElicitResult, bool) {
	if req.Params == nil || req.Params.RequestState == "" {
		return confirmState{}, nil, false
	}
	answer, ok := req.Params.InputResponses[confirmInput].(*mcp.ElicitResult)
	if !ok || answer == nil {
		return confirmState{}, nil, false
	}
	var st confirmState
	if err := e.o.Sealer.OpenPurpose(purposeConfirm, req.Params.RequestState, &st); err != nil {
		// Not this console's state (another key, purpose or a forgery):
		// no answer, the person is asked.
		return confirmState{}, nil, false
	}
	if st.Nonce == "" || st.User != p.User.Name || st.Session != p.SessionID || st.Provider != p.Provider ||
		st.Tool != tool || st.Namespace != t.Namespace || st.Name != t.Name || st.Instance != t.Instance ||
		e.o.Now().Unix() >= st.Expiry {
		return confirmState{}, nil, false
	}
	return st, answer, true
}

// typedFor is what the person types to confirm kind on t: the Cluster's
// name, the instance's for promote (operations.CheckConfirmation).
func typedFor(kind viewmodel.OperationKind, t operationTarget) string {
	if kind == viewmodel.OperationPromote {
		return t.Instance
	}
	return t.Name
}

// describe is the question's text: the operation, where, as whom, what it
// does and costs.
func describe(p *auth.Principal, offer viewmodel.OperationOffer, t operationTarget) string {
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s on %s/%s", offer.Title, t.Namespace, t.Name)
	if t.Instance != "" {
		fmt.Fprintf(&msg, ", instance %s", t.Instance)
	}
	fmt.Fprintf(&msg, ", as %s.\n\n%s\n%s\nExpected duration: %s.", p.User.Name, offer.Summary, offer.Consequence, offer.ExpectedDuration)
	return msg.String()
}

// askForm is the input-required result asking the person to type the
// name in the client.
func (e *Endpoint) askForm(p *auth.Principal, st confirmState, offer viewmodel.OperationOffer, t operationTarget) (writeResult, error) {
	id := confirmID(st.Nonce)
	state, err := e.o.Sealer.SealPurpose(purposeConfirm, st)
	if err != nil {
		return writeResult{}, &callError{kind: "error", text: "the confirmation could not be prepared; retry"}
	}
	typed, what := typedFor(offer.Kind, t), "the Cluster name"
	if offer.Kind == viewmodel.OperationPromote {
		what = "the instance name"
	}
	msg := describe(p, offer, t) + fmt.Sprintf("\n\nType %s, %s, to confirm.", what, typed)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			confirmField: map[string]any{
				"type": "string", "title": "Type " + typed + " to confirm",
				"description": "The " + string(offer.Kind) + " runs only if this is exactly " + typed + ".",
				"minLength":   1, "maxLength": 253,
			},
		},
		"required": []string{confirmField},
	}
	return writeResult{outcome: outcomeAsked, confirmation: id, res: &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{confirmInput: &mcp.ElicitParams{Mode: modeForm, Message: msg, RequestedSchema: schema}},
		RequestState:  state,
	}}, nil
}

// askURL is the input-required result sending the person to the console's
// confirmation page.
func (e *Endpoint) askURL(p *auth.Principal, assistant string, st confirmState, offer viewmodel.OperationOffer, t operationTarget) (writeResult, error) {
	id := confirmID(st.Nonce)
	state, err := e.o.Sealer.SealPurpose(purposeConfirm, st)
	if err != nil {
		return writeResult{}, &callError{kind: "error", text: "the confirmation could not be prepared; retry"}
	}
	page, err := e.o.Sealer.SealPurpose(purposeConfirmPage, confirmPage{
		Nonce: st.Nonce, User: st.User, Assistant: assistant, Kind: offer.Kind,
		Namespace: t.Namespace, Name: t.Name, Instance: t.Instance, Typed: typedFor(offer.Kind, t),
		Title: offer.Title, Summary: offer.Summary, Consequence: offer.Consequence, Duration: offer.ExpectedDuration,
		Expiry: st.Expiry, Credential: credentialOf(p), Asked: e.o.Now().Unix(), From: promotedFrom(offer.Kind, st.Expect),
	})
	if err != nil {
		return writeResult{}, &callError{kind: "error", text: "the confirmation could not be prepared; retry"}
	}
	msg := describe(p, offer, t) + "\n\nConfirm on the console's page, signed in as " + p.User.Name + ", then continue here. " +
		"Nothing is done unless you approve there, before " + time.Unix(st.Expiry, 0).UTC().Format("15:04:05") + " UTC."
	if st.Round > 0 {
		msg = "The console has not recorded an answer yet. Open the page, approve or decline, then continue.\n\n" + msg
	}
	link := strings.TrimRight(e.o.PublicURL, "/") + PathConfirm + "?r=" + url.QueryEscape(page)
	return writeResult{outcome: outcomeAsked, confirmation: id, res: &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{confirmInput: &mcp.ElicitParams{Mode: modeURL, Message: msg, URL: link, ElicitationID: id}},
		RequestState:  state,
	}}, nil
}

// usableOffer is the offer of kind, refused early when the API server
// says no or a precondition is not met: the person is not asked to
// confirm what cannot run. The operation re-checks both on the write.
func usableOffer(offers viewmodel.Operations, kind viewmodel.OperationKind, t operationTarget) (viewmodel.OperationOffer, error) {
	i := slices.IndexFunc(offers.Offers, func(o viewmodel.OperationOffer) bool { return o.Kind == kind })
	if i < 0 {
		return viewmodel.OperationOffer{}, &callError{status: http.StatusNotImplemented, text: string(kind) + " is not offered by this console"}
	}
	offer := offers.Offers[i]
	if !offer.Allowed && !offer.Undecided {
		return offer, &callError{status: http.StatusForbidden, text: "the Kubernetes API server does not let you " +
			strings.Join(offer.Requires, ", ") + " on " + t.Namespace + "/" + t.Name}
	}
	blockers := offer.Blockers
	if kind == viewmodel.OperationPromote {
		j := slices.IndexFunc(offer.Targets, func(c viewmodel.PromoteTarget) bool { return c.Name == t.Instance })
		if j < 0 {
			return offer, &callError{status: http.StatusConflict, text: "promote refused: " + t.Instance + " is not a replica of " + t.Namespace + "/" + t.Name}
		}
		blockers = append(slices.Clone(blockers), offer.Targets[j].Blockers...)
	}
	if len(blockers) > 0 {
		texts := make([]string, 0, len(blockers))
		for _, b := range blockers {
			texts = append(texts, b.Text)
		}
		return offer, &callError{status: http.StatusConflict, text: string(kind) + " refused: " + strings.Join(texts, "; ")}
	}
	return offer, nil
}

// newNonce is a confirmation's single-use value: 128 random bits.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// confirmID names a confirmation in the audit lines (mcp.call,
// mcp.confirm, operation) without the nonce itself: a hash of it.
func confirmID(nonce string) string {
	if nonce == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("pgtoolbox-console/mcp-confirm-id/" + nonce))
	return hex.EncodeToString(sum[:8])
}

// userKey is the person in the spent-value index and the write budget: a
// keyed hash, never the name.
func (e *Endpoint) userKey(user string) string {
	if keys := e.o.Sealer.KeyedHashes(userKeyPurpose, user); len(keys) > 0 {
		return keys[0]
	}
	// No key (cannot happen with a loaded codec): a value SpendCode
	// refuses, so the confirmation fails closed.
	return ""
}

// assistantName is the client as the audit line and the confirmation page
// name it: the grant's declared client name, else the client's
// self-declared name on this request. Both are unverified; format and
// control characters are removed.
func assistantName(p *auth.Principal, req *mcp.CallToolRequest) string {
	if p.Assistant != "" {
		return operations.CleanName(p.Assistant)
	}
	if info := req.ClientInfo(); info != nil {
		return operations.CleanName(info.Name)
	}
	return ""
}

// credentialOf names, for the confirmation page, the credential the call
// came with: a console grant (its client's declared name, unverified, and
// the callback host its consent sent the code to), or a raw API-server
// token.
func credentialOf(p *auth.Principal) string {
	if p.Provider != auth.GrantProvider {
		return "a Kubernetes API-server token presented directly (no console grant)"
	}
	name := operations.CleanName(p.Assistant)
	if name == "" {
		name = "a client that gave no name"
	}
	host := operations.CleanName(p.AssistantRedirect)
	if host == "" {
		host = "not recorded (a grant from before the console kept it)"
	}
	return "the console grant to " + name + " (unverified name), callback host " + host
}

// promotedFrom is, for a promote, the primary when the person was asked.
func promotedFrom(kind viewmodel.OperationKind, r viewmodel.ClusterRevision) string {
	if kind != viewmodel.OperationPromote {
		return ""
	}
	if r.CurrentPrimary == "" {
		return "not reported"
	}
	return r.CurrentPrimary
}

// grantOf is the console grant the request holds, if any.
func grantOf(p *auth.Principal) string {
	if p.Provider == auth.GrantProvider {
		return p.SessionID
	}
	return ""
}
