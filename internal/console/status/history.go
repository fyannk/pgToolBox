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

package status

import (
	"bytes"
	"context"
	"encoding/json"

	cnpgv1 "github.com/cloudnative-pg/api/pkg/api/v1"

	"github.com/fyannk/pgToolBox/internal/console/cache"
	consolehistory "github.com/fyannk/pgToolBox/internal/console/history"
	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/history"
	"github.com/fyannk/pgToolBox/pkg/cnpgstatus/preview"
)

// HistoryReader is the revision history as the read model uses it
// (internal/console/history.History).
type HistoryReader interface {
	Snapshot() (history.Snapshot, bool)
	Revision(seq uint64) (history.Revision, bool)
	Diff(seq uint64) (history.Diff, bool)
	OwnerOf(uid, kind, namespace, name string) (consolehistory.Owner, bool)
	Persistent() bool
}

// maxManifestBytes bounds one manifest served in a revision view.
const maxManifestBytes = 256 * 1024

func (s *Service) historyReader() HistoryReader {
	if s.opts.History == nil {
		return nil
	}
	return s.opts.History
}

func historyEntry(e history.Entry) viewmodel.HistoryEntry {
	out := viewmodel.HistoryEntry{
		Seq: int64(min(e.Seq, uint64(1<<62))), Kind: e.Kind, Name: e.Name, Change: string(e.Change), AfterGap: e.AfterGap,
		Manager: bound(e.Actor.Manager), Operation: e.Actor.Operation, ObservedAt: e.ObservedAt.UTC(),
		Coalesced: e.Coalesced, HasManifest: e.HasManifest,
	}
	if !e.PrevObservedAt.IsZero() {
		t := e.PrevObservedAt.UTC()
		out.PrevObservedAt = &t
	}
	return out
}

func (s *Service) belongs(h HistoryReader, uid, kind, ns, name string, key cache.ClusterKey) bool {
	o, ok := h.OwnerOf(uid, kind, ns, name)
	return ok && o.Namespace == key.Namespace && o.Cluster == key.Name
}

// History is the /history screen of one Cluster.
func (s *Service) History(ctx context.Context, namespace, name string) (viewmodel.History, error) {
	key := cache.ClusterKey{Namespace: namespace, Name: name}
	g, err := s.gather(ctx, key, false)
	if err != nil {
		return viewmodel.History{}, err
	}
	out := viewmodel.History{Ref: ref(g.cluster), GeneratedAt: s.now().UTC(), Items: []viewmodel.HistoryEntry{}}
	h := s.historyReader()
	if h == nil {
		out.Source = viewmodel.SourceState{Source: "history", State: viewmodel.SourceDisabled, Reason: "HISTORY_ENABLED is false"}
		return out, nil
	}
	out.Source = viewmodel.SourceState{Source: "history", State: viewmodel.SourceLive}
	out.Persistent = h.Persistent()
	snap, _ := h.Snapshot()
	out.Evicted = snap.Evicted
	for _, e := range snap.Entries {
		if e.Namespace == namespace && s.belongs(h, e.UID, e.Kind, e.Namespace, e.Name, key) {
			out.Items = append(out.Items, historyEntry(e))
		}
	}
	return out, nil
}

// Revision is one revision of the Cluster or one of its children.
func (s *Service) Revision(ctx context.Context, namespace, name string, seq int64) (viewmodel.Revision, error) {
	key := cache.ClusterKey{Namespace: namespace, Name: name}
	g, err := s.gather(ctx, key, false)
	if err != nil {
		return viewmodel.Revision{}, err
	}
	h := s.historyReader()
	if h == nil || seq <= 0 {
		return viewmodel.Revision{}, ErrNotFound
	}
	rev, ok := h.Revision(uint64(seq))
	if !ok || rev.Namespace != namespace || !s.belongs(h, rev.UID, rev.Kind, rev.Namespace, rev.Name, key) {
		return viewmodel.Revision{}, ErrNotFound
	}
	out := viewmodel.Revision{
		Ref: ref(g.cluster),
		Entry: historyEntry(history.Entry{
			Seq: rev.Seq, Scope: rev.Scope, Kind: rev.Kind, Namespace: rev.Namespace, Name: rev.Name, UID: rev.UID,
			Change: rev.Change, AfterGap: rev.AfterGap, Actor: rev.Actor, ObservedAt: rev.ObservedAt,
			PrevObservedAt: rev.PrevObservedAt, Coalesced: rev.Coalesced, HasManifest: rev.Manifest != nil,
		}),
		Manifest: pretty(rev.Manifest),
		Diff:     viewmodel.RevisionDiff{Entries: []viewmodel.DiffEntry{}},
	}
	if d, ok := h.Diff(rev.Seq); ok {
		out.Diff.HasBase, out.Diff.Truncated = d.HasBase, d.Truncated
		if d.HasBase {
			base := int64(min(d.BaseSeq, uint64(1<<62)))
			at := d.BaseObservedAt.UTC()
			out.Diff.BaseSeq, out.Diff.BaseObservedAt = &base, &at
			if b, ok := h.Revision(d.BaseSeq); ok {
				out.BaseManifest = pretty(b.Manifest)
				if rev.Kind == "Cluster" && rev.Group == cnpgv1.SchemeGroupVersion.Group {
					out.Preview = changePreview(b.Manifest, rev.Manifest, g.minor)
				}
			}
		}
		for _, e := range d.Entries {
			out.Diff.Entries = append(out.Diff.Entries, viewmodel.DiffEntry{Path: e.Path, Op: string(e.Op), Before: e.Before, After: e.After, Manager: bound(e.Manager)})
		}
	}
	return out, nil
}

// pretty indents a canonical manifest for a line diff, bounded.
func pretty(manifest []byte) string {
	if len(manifest) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if json.Indent(&buf, manifest, "", "  ") != nil {
		return ""
	}
	if buf.Len() > maxManifestBytes {
		return buf.String()[:maxManifestBytes] + "\n… (cut at 256 KiB)"
	}
	return buf.String()
}

// changePreview is what a Cluster revision set in motion over its base;
// nil when either definition cannot be read.
func changePreview(before, after []byte, minor cnpgstatus.Minor) *viewmodel.ChangePreview {
	p, err := preview.Compute(preview.Input{Before: before, After: after, CNPGMinor: minor})
	if err != nil {
		return nil
	}
	out := &viewmodel.ChangePreview{
		Effect: string(p.Effect), AtLeast: string(p.AtLeast), Strategy: p.Strategy, Method: p.Method, Primary: p.Primary,
		PostgresMajor: p.PostgresMajor, Verified: p.Verified, Unknown: p.Unknown, CNPGMinor: string(minor),
		Parameters: make([]viewmodel.PreviewParameter, 0, len(p.Parameters)),
		Fields:     make([]viewmodel.PreviewField, 0, len(p.Fields)),
		Steps:      make([]viewmodel.PreviewStep, 0, len(p.Steps)),
	}
	for _, c := range p.Parameters {
		out.Parameters = append(out.Parameters, viewmodel.PreviewParameter{
			Name: bound(c.Name), Before: bound(c.Before), After: bound(c.After), Effect: string(c.Effect), Context: c.Context, Decrease: c.Decrease,
		})
	}
	for _, f := range p.Fields {
		out.Fields = append(out.Fields, viewmodel.PreviewField{Path: bound(f.Path), Effect: string(f.Effect), Reason: f.Reason})
	}
	for _, st := range p.Steps {
		out.Steps = append(out.Steps, viewmodel.PreviewStep{Role: st.Role, Instances: append([]string{}, st.Instances...), Action: st.Action})
	}
	return out
}
