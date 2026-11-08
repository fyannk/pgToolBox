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

package podlogs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/fyannk/pgToolBox/internal/console/viewmodel"
)

type fakeOpener struct {
	body string
	err  error
	got  *corev1.PodLogOptions
}

func (f *fakeOpener) Open(_ context.Context, _, _ string, o *corev1.PodLogOptions) (io.ReadCloser, error) {
	f.got = o
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.body)), nil
}

func pod(name, cluster, owner string) *corev1.Pod {
	t := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, Labels: map[string]string{"cnpg.io/cluster": cluster},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: owner, UID: "u", Controller: &t}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgres"}}},
	}
}

func reader(t *testing.T, o *fakeOpener) *Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pod("orders-1", "orders", "orders"), pod("liar-1", "orders", "liar")).Build()
	return New(c, o, func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) })
}

func TestTail(t *testing.T) {
	ctx := context.Background()
	body := "2026-10-29T11:59:00.123456789Z first line\nno timestamp here\n2026-10-29T11:59:01Z " + strings.Repeat("x", MaxLineBytes+10) + "\n"
	o := &fakeOpener{body: body}
	tail, err := reader(t, o).Tail(ctx, "team-a", "orders", "orders-1", Options{Container: "postgres", TailLines: 50})
	if err != nil {
		t.Fatal(err)
	}
	if *o.got.TailLines != 50 || o.got.LimitBytes != nil || !o.got.Timestamps || o.got.Follow {
		t.Errorf("options %+v", o.got)
	}
	if len(tail.Lines) != 3 || tail.Lines[0].At == nil || tail.Lines[0].Text != "first line" || tail.Lines[1].At != nil {
		t.Fatalf("lines %+v", tail.Lines)
	}
	if !strings.HasSuffix(tail.Lines[2].Text, "(line cut)") || len(tail.Lines[2].Text) > MaxLineBytes+20 {
		t.Errorf("overlong line not cut: %d bytes", len(tail.Lines[2].Text))
	}

	for name, tc := range map[string]struct {
		pod, container string
		err            error
	}{
		"not a member":    {"liar-1", "postgres", nil},
		"missing pod":     {"orders-9", "postgres", nil},
		"wrong container": {"orders-1", "bootstrap", nil},
		"api not found":   {"orders-1", "postgres", apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x")},
	} {
		_, err := reader(t, &fakeOpener{err: tc.err}).Tail(ctx, "team-a", "orders", tc.pod, Options{Container: tc.container, TailLines: 1})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err = reader(t, &fakeOpener{err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("secret detail"))}).
		Tail(ctx, "team-a", "orders", "orders-1", Options{Container: "postgres", TailLines: 1})
	if err == nil || strings.Contains(err.Error(), "secret detail") {
		t.Errorf("forbidden: %v", err)
	}
}

func TestTailKeepsTheNewestLines(t *testing.T) {
	var b strings.Builder
	line := strings.Repeat("y", 1000)
	for i := range 2000 {
		fmt.Fprintf(&b, "2026-10-29T11:59:00Z %04d %s\n", i, line)
	}
	tail, err := reader(t, &fakeOpener{body: b.String()}).Tail(context.Background(), "team-a", "orders", "orders-1", Options{Container: "postgres", TailLines: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !tail.Truncated || !strings.HasPrefix(tail.Lines[len(tail.Lines)-1].Text, "1999 ") {
		t.Fatalf("truncated=%v last=%q", tail.Truncated, tail.Lines[len(tail.Lines)-1].Text[:8])
	}
	size := 0
	for _, l := range tail.Lines {
		size += len(l.Text)
	}
	if size > MaxTailBytes {
		t.Errorf("kept %d bytes", size)
	}
}

func TestReadLineCuts(t *testing.T) {
	exact := strings.Repeat("a", MaxLineBytes)
	got, n, _ := readLine(bufio.NewReader(strings.NewReader(exact + "\nnext")))
	if got != exact || n != MaxLineBytes+1 {
		t.Errorf("a line of exactly MaxLineBytes: %d bytes, n=%d, marker=%v", len(got), n, strings.HasSuffix(got, cutMarker))
	}
	split := strings.Repeat("a", MaxLineBytes-1) + "é" + "tail"
	got, _, _ = readLine(bufio.NewReader(strings.NewReader(split + "\n")))
	if !utf8.ValidString(got) || !strings.HasSuffix(got, cutMarker) {
		t.Errorf("cut inside a rune: valid=%v", utf8.ValidString(got))
	}
}

func TestFollowEnds(t *testing.T) {
	var events []any
	err := reader(t, &fakeOpener{body: "2026-10-29T11:59:00Z a\n2026-10-29T11:59:01Z b\n"}).Follow(context.Background(), "team-a", "orders", "orders-1",
		Options{Container: "postgres", TailLines: 100}, func(ev any) error { events = append(events, ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events %+v", events)
	}
	if _, ok := events[2].(viewmodel.LogStreamEnd); !ok {
		t.Errorf("last event %T", events[2])
	}
}

// FuzzParseLine: the text is never lost and a time is only reported for
// a well-formed prefix.
func FuzzParseLine(f *testing.F) {
	for _, s := range []string{"2026-10-29T11:59:00.1Z x", "x", "", "2026-13-99T99:99:99Z y", "2026-10-29T11:59:00Z"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		l := ParseLine(raw)
		if l.At == nil && l.Text != raw {
			t.Fatalf("text lost without a time: %q -> %q", raw, l.Text)
		}
		if l.At != nil && !strings.HasSuffix(raw, l.Text) {
			t.Fatalf("text %q is not the tail of %q", l.Text, raw)
		}
	})
}

// FuzzScan: every line is bounded and nothing panics on any byte stream.
func FuzzScan(f *testing.F) {
	f.Add("a\nb\r\n\n" + strings.Repeat("z", 70000))
	f.Fuzz(func(t *testing.T, body string) {
		n := 0
		_ = scan(strings.NewReader(body), func(l viewmodel.LogLine, _ int) error {
			n++
			if len(l.Text) > MaxLineBytes+len(" … (line cut)") {
				t.Fatalf("line of %d bytes", len(l.Text))
			}
			_ = utf8.ValidString(l.Text)
			return nil
		})
		if n > strings.Count(body, "\n")+1 {
			t.Fatalf("%d lines from %d newlines", n, strings.Count(body, "\n"))
		}
	})
}
