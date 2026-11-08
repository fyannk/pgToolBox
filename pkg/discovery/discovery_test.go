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

package discovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var poolerGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Pooler"}

type fakeLister struct {
	err  error
	seen []client.ListOption
	list client.ObjectList
}

func (f *fakeLister) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	f.seen, f.list = opts, list
	return f.err
}

func TestProbeListable(t *testing.T) {
	gr := schema.GroupResource{Group: "postgresql.cnpg.io", Resource: "poolers"}
	tests := []struct {
		name      string
		err       error
		namespace string
		want      State
		wantErr   bool
		reason    string
	}{
		{name: "available", want: Available, namespace: "team-a"},
		{name: "forbidden", err: apierrors.NewForbidden(gr, "", errors.New("denied")), namespace: "team-a", want: Forbidden, reason: "not allowed to list Pooler.postgresql.cnpg.io in team-a"},
		{name: "unauthorized counts as forbidden", err: apierrors.NewUnauthorized("no"), namespace: "team-a", want: Forbidden},
		{name: "no kind match", err: &meta.NoKindMatchError{GroupKind: poolerGVK.GroupKind(), SearchedVersions: []string{"v1"}}, want: NotInstalled, reason: "Pooler.postgresql.cnpg.io is not served by this cluster"},
		{name: "no resource match", err: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: "postgresql.cnpg.io", Resource: "poolers"}}, want: NotInstalled},
		{name: "not found", err: apierrors.NewNotFound(gr, ""), want: NotInstalled},
		{name: "timeout fails the start", err: apierrors.NewTimeoutError("slow", 1), wantErr: true},
		{name: "transport error fails the start", err: errors.New("connection refused"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{err: tt.err}
			got, err := ProbeListable(context.Background(), lister, poolerGVK, tt.namespace)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v", got)
				}
				if !errors.Is(err, tt.err) {
					t.Errorf("error %v does not wrap %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tt.want {
				t.Errorf("state = %s, want %s", got.State, tt.want)
			}
			if got.Usable() != (tt.want == Available) {
				t.Errorf("Usable() = %v", got.Usable())
			}
			if tt.reason != "" && got.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.reason)
			}
			if got.State == Available && got.Reason != "" {
				t.Errorf("available carries a reason %q", got.Reason)
			}
		})
	}
}

// TestProbeIsScopedAndMetadataOnly asserts the probe is the cheap one:
// Limit 1, the namespace when given, and a metadata-only list type of
// the right kind.
func TestProbeIsScopedAndMetadataOnly(t *testing.T) {
	lister := &fakeLister{}
	if _, err := ProbeListable(context.Background(), lister, poolerGVK, "team-a"); err != nil {
		t.Fatal(err)
	}
	opts := &client.ListOptions{}
	opts.ApplyOptions(lister.seen)
	if opts.Limit != 1 {
		t.Errorf("limit = %d, want 1", opts.Limit)
	}
	if opts.Namespace != "team-a" {
		t.Errorf("namespace = %q", opts.Namespace)
	}
	list, ok := lister.list.(*metav1.PartialObjectMetadataList)
	if !ok {
		t.Fatalf("list type %T, want metadata only", lister.list)
	}
	if got := list.GroupVersionKind().Kind; got != "PoolerList" {
		t.Errorf("list kind = %q", got)
	}

	if _, err := ProbeListable(context.Background(), lister, SCCGVK, ""); err != nil {
		t.Fatal(err)
	}
	opts = &client.ListOptions{}
	opts.ApplyOptions(lister.seen)
	if opts.Namespace != "" {
		t.Errorf("cluster-scoped probe got namespace %q", opts.Namespace)
	}
}

func TestProbeSCC(t *testing.T) {
	gr := schema.GroupResource{Group: "security.openshift.io", Resource: "securitycontextconstraints"}
	tests := []struct {
		name    string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "listable is OpenShift", want: true},
		{name: "forbidden is still OpenShift", err: apierrors.NewForbidden(gr, "", errors.New("denied")), want: true},
		{name: "not served is Kubernetes", err: &meta.NoKindMatchError{GroupKind: SCCGVK.GroupKind()}, want: false},
		{name: "other error fails", err: errors.New("boom"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ProbeSCC(context.Background(), &fakeLister{err: tt.err})
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if got != tt.want {
				t.Errorf("ProbeSCC = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReasonsNameNoSecretAndAreBounded(t *testing.T) {
	gr := schema.GroupResource{Resource: "pods"}
	res, err := ProbeListable(context.Background(), &fakeLister{err: apierrors.NewForbidden(gr, "", errors.New("token=abc123"))}, schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Reason, "abc123") {
		t.Errorf("reason leaks the server message: %q", res.Reason)
	}
	if res.Reason != "not allowed to list Pod in team-a" {
		t.Errorf("reason = %q", res.Reason)
	}
}

// namespaceLister answers per namespace; a namespace absent from errs
// lists fine.
type namespaceLister struct {
	errs map[string]error
	seen []string
}

func (n *namespaceLister) List(_ context.Context, _ client.ObjectList, opts ...client.ListOption) error {
	o := &client.ListOptions{}
	o.ApplyOptions(opts)
	n.seen = append(n.seen, o.Namespace)
	return n.errs[o.Namespace]
}

func TestProbeNamespaces(t *testing.T) {
	gr := schema.GroupResource{Group: "barmancloud.cnpg.io", Resource: "objectstores"}
	forbidden := apierrors.NewForbidden(gr, "", errors.New("denied"))
	tests := []struct {
		name       string
		namespaces []string
		errs       map[string]error
		want       State
		wantErr    bool
		wantSeen   []string
		wantReason string
	}{
		{name: "cluster-wide when no namespace", want: Available, wantSeen: []string{""}},
		{name: "every namespace listable", namespaces: []string{"a", "b"}, want: Available, wantSeen: []string{"a", "b"}},
		{
			name: "one forbidden namespace turns the feature off", namespaces: []string{"a", "b", "c"},
			errs: map[string]error{"b": forbidden}, want: Forbidden, wantSeen: []string{"a", "b"},
			wantReason: "not allowed to list ObjectStore.barmancloud.cnpg.io in b",
		},
		{
			name: "not served stops at the first namespace", namespaces: []string{"a", "b"},
			errs: map[string]error{"a": &meta.NoKindMatchError{GroupKind: ObjectStoreGVK.GroupKind()}}, want: NotInstalled, wantSeen: []string{"a"},
		},
		{
			name: "any other error fails", namespaces: []string{"a", "b"},
			errs: map[string]error{"b": errors.New("connection refused")}, wantErr: true, wantSeen: []string{"a", "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &namespaceLister{errs: tt.errs}
			got, err := ProbeObjectStoreAPI(context.Background(), lister, tt.namespaces)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if !tt.wantErr && got.State != tt.want {
				t.Errorf("state = %s, want %s", got.State, tt.want)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if strings.Join(lister.seen, ",") != strings.Join(tt.wantSeen, ",") {
				t.Errorf("probed %q, want %q", lister.seen, tt.wantSeen)
			}
			if !tt.wantErr && got.GVK != ObjectStoreGVK {
				t.Errorf("GVK = %v", got.GVK)
			}
		})
	}
}

// TestProbePgBackRestAPIs: the two pgBackRest plugin kinds are probed like
// the ObjectStore, each under its own GVK and resource name.
func TestProbePgBackRestAPIs(t *testing.T) {
	probes := []struct {
		name  string
		probe func(context.Context, Lister, []string) (Result, error)
		gvk   schema.GroupVersionKind
		gr    schema.GroupResource
	}{
		{"Stanza", ProbeStanzaAPI, StanzaGVK, schema.GroupResource{Group: "pgbackrest.dalibo.com", Resource: "stanzas"}},
		{"Archive", ProbeArchiveAPI, ArchiveGVK, schema.GroupResource{Group: "pgbackrest.cnpg.opera.com", Resource: "archives"}},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			for _, tt := range []struct {
				errs   map[string]error
				want   State
				reason string
			}{
				{want: Available},
				{errs: map[string]error{"b": apierrors.NewForbidden(p.gr, "", errors.New("denied"))}, want: Forbidden, reason: "not allowed to list " + p.gvk.Kind + "." + p.gvk.Group + " in b"},
				{errs: map[string]error{"a": &meta.NoKindMatchError{GroupKind: p.gvk.GroupKind()}}, want: NotInstalled, reason: p.gvk.Kind + "." + p.gvk.Group + " is not served by this cluster"},
			} {
				got, err := p.probe(context.Background(), &namespaceLister{errs: tt.errs}, []string{"a", "b"})
				if err != nil || got.State != tt.want || got.Reason != tt.reason || got.GVK != p.gvk {
					t.Fatalf("probe = %+v, %v; want %s %q", got, err, tt.want, tt.reason)
				}
			}
			if _, err := p.probe(context.Background(), &namespaceLister{errs: map[string]error{"a": errors.New("connection refused")}}, []string{"a"}); err == nil {
				t.Fatal("an unclassified error did not fail the probe")
			}
		})
	}
}
