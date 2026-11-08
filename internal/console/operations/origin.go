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

package operations

import (
	"context"
	"net/http"
	"strings"
	"unicode"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

// Faces an operation can be requested through. The audit line names it
// (via=...), so a write an assistant made is told apart from one made on
// the console's own screens.
const (
	// ViaAPI is the console's JSON API (its screens): the default.
	ViaAPI = "api"
	// ViaMCP is an MCP write tool.
	ViaMCP = "mcp"
)

// maxAssistantName bounds the client-declared name in an audit line.
const maxAssistantName = 128

// Origin is the face an operation was requested through, and for MCP the
// client that asked: its declared name (unverified, as the consent page
// says), the console grant it holds when it holds one, the confirmation
// the person gave (its ID, for the audit line) and the Cluster as it was
// when they confirmed.
type Origin struct {
	Via       string
	Assistant string
	Grant     string
	// Confirmation identifies the person's confirmation (MCP): the same
	// ID is on the mcp.call lines of the question and of the answer.
	Confirmation string
	// Confirmed is the Cluster the person confirmed on; nil: no
	// expectation (the console's own screens confirm on the live page).
	Confirmed *viewmodel.ClusterRevision
	// Observed, when set, receives the live Cluster (as Expected keeps
	// it) when the write is refused because it is not Confirmed: the
	// caller can ask the person again on the live state, since what they
	// confirmed on came from the console's cache, which may lag.
	Observed *viewmodel.ClusterRevision
}

type originKey struct{}

// WithOrigin records o in ctx for the operation's audit line.
func WithOrigin(ctx context.Context, o Origin) context.Context {
	return context.WithValue(ctx, originKey{}, o)
}

// OriginFrom is ctx's origin: ViaAPI when none was recorded.
func OriginFrom(ctx context.Context) Origin {
	o, ok := ctx.Value(originKey{}).(Origin)
	if !ok || o.Via == "" {
		o.Via = ViaAPI
	}
	o.Assistant = CleanName(o.Assistant)
	return o
}

// CleanName is a client-declared name fit for an audit line or a page:
// format characters (bidirectional overrides, zero-width joiners: text
// that displays other than it reads) and control characters removed,
// valid UTF-8, at most maxAssistantName bytes.
func CleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	if len(name) > maxAssistantName {
		name = strings.ToValidUTF8(name[:maxAssistantName], "")
	}
	return strings.TrimSpace(name)
}

// RevisionOf is what a confirmation seals of c.
func RevisionOf(c *cnpgv1.Cluster) viewmodel.ClusterRevision {
	if c == nil {
		return viewmodel.ClusterRevision{}
	}
	return viewmodel.ClusterRevision{
		UID: string(c.UID), Generation: c.Generation, Phase: c.Status.Phase, CurrentPrimary: c.Status.CurrentPrimary,
	}
}

// Expected keeps of r what an operation of kind depends on: the Cluster's
// identity always; for a restart its spec generation, phase and primary
// (a restart confirmed on a healthy Cluster must not land on one that is
// now rolling out, or on another primary); for a promote the primary it
// moves the role from. Not the resourceVersion: CloudNativePG writes the
// status often enough that every confirmation would go stale.
func Expected(kind viewmodel.OperationKind, r viewmodel.ClusterRevision) viewmodel.ClusterRevision {
	out := viewmodel.ClusterRevision{UID: r.UID}
	switch kind {
	case viewmodel.OperationRestart:
		out.Generation, out.Phase, out.CurrentPrimary = r.Generation, r.Phase, r.CurrentPrimary
	case viewmodel.OperationPromote:
		out.CurrentPrimary = r.CurrentPrimary
	case viewmodel.OperationBackup, viewmodel.OperationReload, viewmodel.OperationRestoreDrill:
	}
	return out
}

// changedSince refuses a write when the live Cluster is not the one the
// person confirmed on (Origin.Confirmed).
func changedSince(ctx context.Context, kind viewmodel.OperationKind, live *cnpgv1.Cluster) error {
	o := OriginFrom(ctx)
	want := o.Confirmed
	if want == nil {
		return nil
	}
	got := Expected(kind, RevisionOf(live))
	var what []string
	switch {
	case got.UID != want.UID:
		what = append(what, "it was deleted and created again")
	default:
		if got.Generation != want.Generation {
			what = append(what, "its spec was changed")
		}
		if got.Phase != want.Phase {
			what = append(what, "its phase is now \""+got.Phase+"\"")
		}
		if got.CurrentPrimary != want.CurrentPrimary {
			what = append(what, "its primary is now "+got.CurrentPrimary)
		}
	}
	if len(what) == 0 {
		return nil
	}
	if o.Observed != nil {
		*o.Observed = got
	}
	return Rejection(http.StatusConflict, "the Cluster changed since the person confirmed ("+strings.Join(what, "; ")+
		"): nothing was done; check its state and ask the person to confirm again")
}
