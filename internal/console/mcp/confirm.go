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
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fyannk/pgToolBox/internal/console/auth"
	"github.com/fyannk/pgToolBox/internal/console/mcpauth"
	"github.com/fyannk/pgToolBox/internal/console/sharedstate"
)

// The confirmation page of restart and promote: what a URL elicitation
// sends the person to. It is a page of the console, on the session mux:
// it reads the person's own browser session (a model or an MCP client
// holds no such cookie), its form posts only from this origin (the CSRF
// rules), it is bound to the person the tool asked for, and it takes one
// answer (single-use values in the shared index). The tool runs the
// operation only once the page recorded the approval.

// PathConfirm is the confirmation page.
const PathConfirm = "/mcp/confirm"

const (
	// maxBudgetKeys bounds the confirmation budgets' tables.
	maxBudgetKeys = 4096
	// pageViewsPerMinute bounds page loads per client address (per person
	// when the address is shared, auth.BudgetKey).
	pageViewsPerMinute = 30
	// pageAnswersPerMinute bounds answers per person.
	pageAnswersPerMinute = 10
	// maxConfirmForm bounds the page's form.
	maxConfirmForm = 16 << 10
)

// SessionSource reads the person's browser session (auth.Handler).
type SessionSource interface {
	CurrentSession(w http.ResponseWriter, r *http.Request) (*auth.SessionGrant, string, error)
}

// openPage opens the page's sealed request: unexpired, or nothing.
func (e *Endpoint) openPage(sealed string) (confirmPage, bool) {
	var pg confirmPage
	if sealed == "" || e.o.Sealer.OpenPurpose(purposeConfirmPage, sealed, &pg) != nil ||
		pg.Nonce == "" || pg.User == "" || e.o.Now().Unix() >= pg.Expiry {
		return confirmPage{}, false
	}
	return pg, true
}

// confirmView is the page for pg.
func confirmView(pg confirmPage, sealed, problem string) auth.MCPConfirmView {
	assistant := pg.Assistant
	if assistant == "" {
		assistant = "an MCP client that gave no name"
	}
	return auth.MCPConfirmView{
		Subject: pg.User, Assistant: assistant, Title: pg.Title, Summary: pg.Summary, Consequence: pg.Consequence,
		Duration: pg.Duration, Namespace: pg.Namespace, Name: pg.Name, Instance: pg.Instance, Typed: pg.Typed,
		Expires: time.Unix(pg.Expiry, 0).UTC().Format("15:04:05"), Problem: problem, Action: PathConfirm, Request: sealed,
		Credential: pg.Credential, Asked: time.Unix(pg.Asked, 0).UTC().Format("15:04:05"), From: pg.From,
	}
}

// handleConfirmPage shows the confirmation to the person it was asked of.
func (e *Endpoint) handleConfirmPage(w http.ResponseWriter, r *http.Request) {
	// Page loads are budgeted per client address, or, behind a proxy the
	// console does not trust (every client then has its address), per
	// signed-in person once the session is known: an anonymous load only
	// redirects to the sign-in, which has budgets of its own.
	key, perAddress := auth.BudgetKey(r, e.o.TrustedProxy)
	if perAddress && !e.pageViews.Allow(key) {
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many confirmation pages from this address; wait a minute.")
		return
	}
	sess, _, err := e.o.Sessions.CurrentSession(w, r)
	if err != nil {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session; please retry.")
		return
	}
	if !perAddress && sess != nil && !e.pageViews.Allow("user:"+e.userKey(sess.Principal.User.Name)) {
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many confirmation pages; wait a minute.")
		return
	}
	if sess == nil {
		// As the OAuth authorization endpoint: back from a cross-site
		// sign-in, a page of this origin continues so that the
		// SameSite=Strict cookie is sent.
		if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
			auth.ContinuePage(w, r.URL.RequestURI())
			return
		}
		http.Redirect(w, r, auth.LoginURL(r.URL.RequestURI()), http.StatusFound) // #nosec G710 -- same-origin target
		return
	}
	sealed := r.URL.Query().Get("r")
	pg, ok := e.openPage(sealed)
	switch {
	case !ok:
		auth.ErrorPage(w, http.StatusBadRequest, "This confirmation link expired or is not valid; nothing was done. Ask the assistant again.")
	case sess.Principal.User.Name != pg.User:
		auth.ErrorPage(w, http.StatusForbidden, "This confirmation was asked of another person; nothing was done. Sign in as that person, or ask the assistant again.")
	case e.o.Confirmations.CodeUsed(codeAnswered + pg.Nonce):
		auth.NoticePage(w, http.StatusConflict, "Already answered", "This confirmation was already answered. Return to the assistant.")
	default:
		auth.MCPConfirmPage(w, http.StatusOK, confirmView(pg, sealed, ""))
	}
}

// handleConfirmAnswer records the person's one answer: approved (with the
// name typed) or declined.
func (e *Endpoint) handleConfirmAnswer(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxConfirmForm)
	if err := r.ParseForm(); err != nil {
		auth.ErrorPage(w, http.StatusBadRequest, "The form could not be read.")
		return
	}
	sealed := r.PostForm.Get("r")
	pg, ok := e.openPage(sealed)
	if !ok {
		auth.ErrorPage(w, http.StatusBadRequest, "This confirmation page expired; nothing was done. Ask the assistant again.")
		return
	}
	sess, _, err := e.o.Sessions.CurrentSession(w, r)
	if err != nil {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The Kubernetes API server could not be reached to check your session; please retry.")
		return
	}
	if sess == nil || sess.Principal.User.Name != pg.User {
		auth.ErrorPage(w, http.StatusForbidden, "This confirmation belongs to another sign-in; nothing was done. Sign in as "+pg.User+" and open the link again.")
		return
	}
	key := e.userKey(pg.User)
	if !e.pageAnswers.Allow(key) {
		auth.ErrorPage(w, http.StatusTooManyRequests, "Too many answers; wait a minute.")
		return
	}
	approve := r.PostForm.Get("decision") == "approve"
	if approve && strings.TrimSpace(r.PostForm.Get(confirmField)) != pg.Typed {
		auth.MCPConfirmPage(w, http.StatusUnprocessableEntity, confirmView(pg, sealed, "That is not "+pg.Typed+
			": type it exactly to approve, or decline."))
		return
	}
	store := e.o.Confirmations
	if !store.GrantsUsable() {
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The console cannot record confirmations right now; nothing was done. Retry in a minute.")
		return
	}
	until := time.Unix(pg.Expiry, 0).Add(time.Minute)
	switch err := store.SpendCode(r.Context(), codeAnswered+pg.Nonce, key, until); {
	case errors.Is(err, sharedstate.ErrSpent):
		auth.NoticePage(w, http.StatusConflict, "Already answered", "This confirmation was already answered. Return to the assistant.")
		return
	case errors.Is(err, sharedstate.ErrBusy):
		auth.ErrorPage(w, http.StatusServiceUnavailable, "Too many confirmations in progress; nothing was done. Retry in a minute.")
		return
	case err != nil:
		auth.ErrorPage(w, http.StatusServiceUnavailable, "The console cannot record confirmations right now; nothing was done. Retry in a minute.")
		return
	}
	marker, outcome := codeDeclined, "declined"
	if approve {
		marker, outcome = codeApproved, "approved"
	}
	if err := store.SpendCode(r.Context(), marker+pg.Nonce, key, until); err != nil {
		// The answer is spent but not recorded: the tool finds no
		// approval and does nothing.
		auth.ErrorPage(w, http.StatusServiceUnavailable, "Your answer could not be recorded; nothing was done. Ask the assistant again.")
		return
	}
	e.audit.Record(r.Context(), auth.ClientOf(r, e.o.TrustedProxy), auth.AuditMCPConfirm,
		"user", pg.User, "session", sess.Principal.SessionID, "outcome", outcome, "operation", string(pg.Kind),
		"namespace", pg.Namespace, "cluster", pg.Name, "target", pg.Instance, "assistant", pg.Assistant,
		"confirmation", confirmID(pg.Nonce))
	if !approve {
		auth.NoticePage(w, http.StatusOK, "Declined", "Nothing will be done. You can close this page.")
		return
	}
	auth.NoticePage(w, http.StatusOK, "Approved", "Return to the assistant and continue: the "+string(pg.Kind)+
		" runs when it calls the console again, before "+time.Unix(pg.Expiry, 0).UTC().Format("15:04:05")+" UTC. You can close this page.")
}

// filterTools hides the write tools from the tools/list of a principal
// that may not operate (a read-only grant): calling one is refused anyway
// (requireOperate), and a model should not be
// offered what it cannot do.
func (e *Endpoint) filterTools(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		list, ok := res.(*mcp.ListToolsResult)
		if !ok {
			return res, err
		}
		p, _, ok := caller(req.GetExtra())
		if ok && mcpauth.HasScope(p, mcpauth.ScopeOperate) {
			return res, nil
		}
		out := *list
		out.Tools = slices.DeleteFunc(slices.Clone(list.Tools), func(t *mcp.Tool) bool { return e.operateTools[t.Name] })
		return &out, nil
	}
}
